// Package storage moves sealed recordings off the host that produced them.
//
// This is what makes the hash chain worth anything. A recording that lives only
// on the machine it was recorded on is protected by exactly the access controls
// of that machine — so whoever compromises the host can delete the evidence of
// having done so. Uploading to object storage the gateway can write but not
// delete turns "detectable tampering" into "tampering you also cannot hide".
//
// The chain head travels as object metadata, so the artefact carries its own
// integrity claim and can be verified by anyone who can read the bucket.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config describes an S3-compatible endpoint.
type Config struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"use_ssl"`
	Region    string `yaml:"region"`
}

// Client uploads and retrieves recordings.
type Client struct {
	mc     *minio.Client
	bucket string
	log    *slog.Logger
}

// ErrNotConfigured means no object storage is set up, so recordings stay local.
var ErrNotConfigured = errors.New("object storage is not configured")

// New builds a client. A zero Endpoint returns nil, which every caller treats
// as "keep recordings local" rather than as an error — object storage is a
// hardening step, not a prerequisite for recording.
func New(cfg Config, log *slog.Logger) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "argus-recordings"
	}

	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("object storage client: %w", err)
	}
	return &Client{mc: mc, bucket: cfg.Bucket, log: log}, nil
}

// EnsureBucket creates the bucket if it does not exist.
func (c *Client) EnsureBucket(ctx context.Context) error {
	if c == nil {
		return ErrNotConfigured
	}
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("create bucket %s: %w", c.bucket, err)
		}
		c.log.Info("created recordings bucket", "bucket", c.bucket)
	}
	return nil
}

// ObjectKey is where a session's recording lives.
//
// Date-prefixed so a bucket lifecycle rule can expire by retention period
// without listing every object, and so a day's recordings are contiguous.
func ObjectKey(startedAt time.Time, sessionID string) string {
	return ObjectKeyExt(startedAt, sessionID, ".cast")
}

// ObjectKeyExt is ObjectKey for recordings that are not terminal captures.
//
// The extension carries which format the object is, and getting it wrong is not
// cosmetic: the backup drill selects recordings by extension, and an operator
// fetching a .cast expects something a terminal player can open. A Remote
// Desktop recording stored under that name verifies — the chain is
// format-agnostic — and then cannot be played by anything that trusted the
// name.
func ObjectKeyExt(startedAt time.Time, sessionID, ext string) string {
	return fmt.Sprintf("%s/%s%s", startedAt.UTC().Format("2006/01/02"), sessionID, ext)
}

// Upload stores a recording and returns its object key.
//
// The chain head is attached as metadata rather than kept only in the database:
// an artefact that carries its own integrity claim can be verified by anyone
// with read access, including after the control plane is gone.
func (c *Client) Upload(ctx context.Context, path, sessionID, chainHead string,
	startedAt time.Time) (string, error) {

	if c == nil {
		return "", ErrNotConfigured
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open recording: %w", err)
	}
	// The object keeps the extension the artefact has on disk, so the two
	// cannot disagree about what format it is.
	ext := filepath.Ext(path)
	if ext == "" {
		ext = ".cast"
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	key := ObjectKeyExt(startedAt, sessionID, ext)
	_, err = c.mc.PutObject(ctx, c.bucket, key, f, info.Size(), minio.PutObjectOptions{
		ContentType: "application/x-asciicast",
		UserMetadata: map[string]string{
			"argus-session":    sessionID,
			"argus-chain-head": chainHead,
			"argus-started-at": startedAt.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return "", fmt.Errorf("upload %s: %w", key, err)
	}
	return key, nil
}

// Get opens a recording for reading.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// GetObject is lazy: errors surface on first read, so probe now to avoid
	// handing the caller a reader that fails later with no context.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, err
	}
	return obj, nil
}

// ChainHead reads the integrity claim stored with an object.
func (c *Client) ChainHead(ctx context.Context, key string) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}
	info, err := c.mc.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return "", err
	}
	// minio-go normalises user metadata onto the X-Amz-Meta- prefix.
	for k, v := range info.UserMetadata {
		if strings.EqualFold(k, "argus-chain-head") {
			return v, nil
		}
	}
	return "", nil
}

// PresignedURL returns a time-limited direct download link.
//
// Deliberately short-lived and unused by default: streaming through the control
// plane keeps every recording access inside the audit log, whereas a presigned
// URL is a capability that leaves no trace once handed out.
func (c *Client) PresignedURL(ctx context.Context, key string, ttl time.Duration) (*url.URL, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	return c.mc.PresignedGetObject(ctx, c.bucket, key, ttl, url.Values{})
}

// LocalPath is where a recording lives before upload.
func LocalPath(dir, sessionID string) string {
	return LocalPathExt(dir, sessionID, ".cast")
}

// LocalPathExt is LocalPath for recordings that are not terminal captures.
//
// The extension is part of the artefact's identity, not decoration: a
// Remote Desktop recording and a terminal one are different formats, and a
// verifier handed the wrong one reports tampering rather than a mismatch.
func LocalPathExt(dir, sessionID, ext string) string {
	return filepath.Join(dir, sessionID+ext)
}
