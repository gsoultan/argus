package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// The object storage services recordings can be copied to. S3 covers every
// service that speaks the S3 API -- Amazon S3, Cloudflare R2, MinIO,
// Backblaze B2, Wasabi -- told apart only by their endpoint. Azure Blob
// Storage has an API of its own.
const (
	ProviderS3    = "s3"
	ProviderAzure = "azure"
)

// ValidProvider reports whether p names a provider Open can build. Empty is
// S3, which is what every configuration written before Azure meant.
func ValidProvider(p string) bool {
	switch p {
	case "", ProviderS3, ProviderAzure:
		return true
	}
	return false
}

// Bucket is object storage the control plane keeps recordings in: an S3
// bucket or an Azure container. Everything the control plane does with
// stored recordings goes through it, so a provider is a whole provider or
// none -- the archive, removals, presigned exports and backups all work the
// same on each.
type Bucket interface {
	Store
	UploadKey(ctx context.Context, path, key, sessionID, chainHead string,
		startedAt time.Time) (string, error)
	UploadKeyWith(ctx context.Context, path, key, sessionID, chainHead string,
		startedAt time.Time, opt PutOptions) (string, error)
	// Delete removes an object. A missing one is not an error.
	Delete(ctx context.Context, key string) error
	// ChainHead is the integrity claim stored with an object.
	ChainHead(ctx context.Context, key string) (string, error)
	// PresignedURL is a time-limited direct download link.
	PresignedURL(ctx context.Context, key string, ttl time.Duration) (*url.URL, error)
	// EnsureBucket creates the bucket or container if it does not exist.
	EnsureBucket(ctx context.Context) error
	// Name is the bucket or container, for the status page and logs.
	Name() string
}

var (
	_ Bucket = (*Client)(nil)
	_ Bucket = (*Azure)(nil)
)

// Open builds the Bucket cfg describes, or returns a nil Bucket when cfg
// names none.
//
// The nil is a nil interface, never a nil *Client inside one: every caller
// tests the result against nil, and a typed nil would pass that test and
// then fail every upload (see core.md, "an optional dependency is an
// interface field").
func Open(cfg Config, log *slog.Logger) (Bucket, error) {
	switch cfg.Provider {
	case "", ProviderS3:
		c, err := New(cfg, log)
		if err != nil || c == nil {
			return nil, err
		}
		return c, nil
	case ProviderAzure:
		a, err := NewAzure(cfg, log)
		if err != nil || a == nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unknown object storage provider %q; use %q or %q",
			cfg.Provider, ProviderS3, ProviderAzure)
	}
}

// Release lets go of what a Bucket built for one use holds open between
// requests.
func Release(b Bucket) {
	if c, ok := b.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// IsNotFound reports whether err says an object is not there.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errBlobNotFound) {
		return true
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return true
	}
	return false
}

// CheckKey is the object Check writes. One fixed key, overwritten by every
// check, so testing a bucket leaves one small object behind rather than one
// per click.
const CheckKey = ".argus/connection-check"

// Check proves b can do what the control plane will ask of it: the bucket
// exists (or can be made), an object can be written, and read back as
// written. It answers which of those failed, because "access denied" alone
// does not say whether the key cannot write or cannot read.
func Check(ctx context.Context, b Bucket) error {
	if b == nil {
		return ErrNotConfigured
	}
	if err := b.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("could not reach %s: %w", b.Name(), err)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	want := []byte("argus connection check " + hex.EncodeToString(nonce) + "\n")
	f, err := os.CreateTemp("", "argus-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(want)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if _, err := b.UploadKey(ctx, f.Name(), CheckKey, "", "", time.Now()); err != nil {
		return fmt.Errorf("could not write to %s: %w", b.Name(), err)
	}
	r, err := b.Get(ctx, CheckKey)
	if err != nil {
		return fmt.Errorf("wrote to %s but could not read back: %w", b.Name(), err)
	}
	defer r.Close()
	got, err := io.ReadAll(io.LimitReader(r, int64(len(want))+1))
	if err != nil {
		return fmt.Errorf("wrote to %s but could not read back: %w", b.Name(), err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s gave back something other than what was written", b.Name())
	}
	return nil
}

// isInternal reports whether key is Argus's own bookkeeping, not a
// recording or a backup: the connection check's object.
func isInternal(key string) bool {
	return strings.HasPrefix(key, ".argus/")
}
