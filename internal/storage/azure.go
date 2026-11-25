package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

/*
Azure Blob Storage, spoken directly over its REST API and signed with the
storage account's shared key. The cloud integrations in internal/control/cloud
talk to Azure the same way, and a whole SDK for seven calls would be most of
the dependency for none of the benefit.

A recording is a block blob in one container. Its key is the same as an S3
object's -- 2026/11/24/<id>.cast.zst, tenants/<org>/..., archive/... -- so
everything that reasons about keys (organizations, the archive, backups)
works unchanged. What does differ:

  - Metadata names must be C# identifiers, so argus-chain-head is stored as
    argus_chain_head. ChainHead reads either.
  - Storage classes are access tiers: Hot, Cool and Cold. Archive is refused
    for the same reason Glacier is: a recording there cannot be replayed until
    a rehydration that takes hours.
  - A presigned link is a service SAS for one blob, read-only, signed with the
    same key.
*/

// azureVersion is the REST API version every request names. 2021-12-02 is
// the first with the Cold tier.
const azureVersion = "2021-12-02"

// azureSASVersion is the version a SAS is signed for. Its string-to-sign is
// the one Azure documents for 2020-12-06 and later.
const azureSASVersion = "2022-11-02"

// azureSingleShot is the largest recording sent in one request. Larger ones
// go up in azureBlockSize blocks, so a failure costs one block, not the file.
// Variables so a test can cross the line without a 256 MB file.
var (
	azureSingleShot int64 = 256 << 20
	azureBlockSize  int64 = 64 << 20
)

// AzureTiers are the access tiers a recording may be stored in: those it can
// be read back from at once.
var AzureTiers = []string{"Hot", "Cool", "Cold"}

var errBlobNotFound = errors.New("blob not found")

// Azure keeps recordings in an Azure Blob Storage container.
type Azure struct {
	account   string
	key       []byte
	base      *url.URL // https://<account>.blob.core.windows.net, or an emulator's /<account>
	container string
	http      *http.Client
	log       *slog.Logger
	now       func() time.Time
}

// NewAzure builds a client for the container cfg names. A Config with no
// account returns nil, as New does with no endpoint.
func NewAzure(cfg Config, log *slog.Logger) (*Azure, error) {
	account := strings.TrimSpace(cfg.AccessKey)
	if account == "" {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.SecretKey))
	if err != nil || len(key) == 0 {
		return nil, errors.New("the Azure account key is not a key: paste key1 or key2 from the storage account's Access keys")
	}
	container := cfg.Bucket
	if container == "" {
		container = "argus-recordings"
	}
	if !validContainer(container) {
		return nil, fmt.Errorf("%q cannot name an Azure container: 3 to 63 lowercase letters, digits and single hyphens", container)
	}
	base, err := azureBase(cfg.Endpoint, account, cfg.UseSSL)
	if err != nil {
		return nil, err
	}
	return &Azure{account: account, key: key, base: base, container: container,
		http: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		log:  log, now: time.Now}, nil
}

// azureBase is the account's blob endpoint. An endpoint left out is the
// public cloud's; one given without a scheme takes it from useSSL, so an
// operator can name a sovereign cloud by host alone.
func azureBase(endpoint, account string, useSSL bool) (*url.URL, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return &url.URL{Scheme: "https", Host: account + ".blob.core.windows.net"}, nil
	}
	if !strings.Contains(endpoint, "://") {
		scheme := "http"
		if useSSL {
			scheme = "https"
		}
		endpoint = scheme + "://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%q is not a blob endpoint: use https://<account>.blob.core.windows.net", endpoint)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

func validContainer(c string) bool {
	if len(c) < 3 || len(c) > 63 || strings.Contains(c, "--") ||
		strings.HasPrefix(c, "-") || strings.HasSuffix(c, "-") {
		return false
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// CloseIdleConnections lets go of the connections kept for the next
// request, for a client built once to test a setting.
func (a *Azure) CloseIdleConnections() { a.http.CloseIdleConnections() }

// Name is the account and container.
func (a *Azure) Name() string { return a.account + "/" + a.container }

// blobURL is where key lives, each segment escaped on its own so the
// slashes stay separators.
func (a *Azure) blobURL(key string, q url.Values) *url.URL {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := *a.base
	u.RawPath = a.base.EscapedPath() + "/" + url.PathEscape(a.container) + "/" + strings.Join(segs, "/")
	u.Path, _ = url.PathUnescape(u.RawPath)
	u.RawQuery = q.Encode()
	return &u
}

func (a *Azure) containerURL(q url.Values) *url.URL {
	u := *a.base
	u.Path = a.base.Path + "/" + a.container
	u.RawPath = ""
	u.RawQuery = q.Encode()
	return &u
}

// AzureError is a refusal from the service, with its own code and message.
type AzureError struct {
	Status  int
	Code    string
	Message string
}

func (e *AzureError) Error() string {
	msg := e.Message
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i] // the rest is a request id and a timestamp
	}
	if e.Code == "" {
		return fmt.Sprintf("azure: %s", http.StatusText(e.Status))
	}
	if msg == "" {
		return fmt.Sprintf("azure: %s", e.Code)
	}
	return fmt.Sprintf("azure: %s: %s", e.Code, msg)
}

func (e *AzureError) Is(target error) bool {
	return target == errBlobNotFound && e.Status == http.StatusNotFound
}

// do signs and sends one request. body may be nil; size is its length.
func (a *Azure) do(ctx context.Context, method string, u *url.URL, hdr http.Header,
	body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if hdr != nil {
		req.Header = hdr
	}
	req.ContentLength = size
	if body == nil || size == 0 {
		req.Body = http.NoBody
		req.ContentLength = 0
	}
	req.Header.Set("x-ms-date", a.now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureVersion)
	req.Header.Set("Authorization", "SharedKey "+a.account+":"+a.sign(req))
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAzureError(resp)
	}
	return resp, nil
}

func readAzureError(resp *http.Response) error {
	e := &AzureError{Status: resp.StatusCode, Code: resp.Header.Get("x-ms-error-code")}
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); len(b) > 0 && xml.Unmarshal(b, &body) == nil {
		if e.Code == "" {
			e.Code = body.Code
		}
		e.Message = strings.TrimSpace(body.Message)
	}
	return e
}

// sign is the Shared Key signature of req.
// https://learn.microsoft.com/rest/api/storageservices/authorize-with-shared-key
func (a *Azure) sign(req *http.Request) string {
	h := req.Header
	length := ""
	if req.ContentLength > 0 {
		length = strconv.FormatInt(req.ContentLength, 10)
	}
	var b strings.Builder
	for _, s := range []string{req.Method, h.Get("Content-Encoding"), h.Get("Content-Language"),
		length, h.Get("Content-MD5"), h.Get("Content-Type"), "", h.Get("If-Modified-Since"),
		h.Get("If-Match"), h.Get("If-None-Match"), h.Get("If-Unmodified-Since"), h.Get("Range")} {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	var names []string
	for k := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-") {
			names = append(names, lk)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(h.Get(n)))
		b.WriteByte('\n')
	}
	b.WriteString("/" + a.account + req.URL.EscapedPath())
	q := req.URL.Query()
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		vs := slices.Clone(q[k])
		slices.Sort(vs)
		b.WriteString("\n" + strings.ToLower(k) + ":" + strings.Join(vs, ","))
	}
	return a.mac(b.String())
}

func (a *Azure) mac(s string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// EnsureBucket creates the container if it does not exist.
func (a *Azure) EnsureBucket(ctx context.Context) error {
	if a == nil {
		return ErrNotConfigured
	}
	resp, err := a.do(ctx, http.MethodHead, a.containerURL(url.Values{"restype": {"container"}}), nil, nil, 0)
	if err == nil {
		resp.Body.Close()
		return nil
	}
	var ae *AzureError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		return fmt.Errorf("check container: %w", err)
	}
	resp, err = a.do(ctx, http.MethodPut, a.containerURL(url.Values{"restype": {"container"}}), nil, nil, 0)
	if err != nil {
		if errors.As(err, &ae) && ae.Code == "ContainerAlreadyExists" {
			return nil
		}
		return fmt.Errorf("create container %s: %w", a.container, err)
	}
	resp.Body.Close()
	a.log.Info("created recordings container", "container", a.Name())
	return nil
}

// Upload stores a recording at the key its session and start make.
func (a *Azure) Upload(ctx context.Context, path, sessionID, chainHead string,
	startedAt time.Time) (string, error) {
	return a.UploadKey(ctx, path, ObjectKeyExt(startedAt, sessionID, extOf(path)), sessionID,
		chainHead, startedAt)
}

// UploadKey is Upload at a key the caller chose; see Scoped.
func (a *Azure) UploadKey(ctx context.Context, path, key, sessionID, chainHead string,
	startedAt time.Time) (string, error) {
	return a.UploadKeyWith(ctx, path, key, sessionID, chainHead, startedAt, PutOptions{})
}

// UploadKeyWith is UploadKey with options. StorageClass is an access tier,
// one of AzureTiers.
func (a *Azure) UploadKeyWith(ctx context.Context, path, key, sessionID, chainHead string,
	startedAt time.Time, opt PutOptions) (string, error) {
	if a == nil {
		return "", ErrNotConfigured
	}
	if opt.StorageClass != "" && !slices.Contains(AzureTiers, opt.StorageClass) {
		return "", fmt.Errorf("%q is not an Azure access tier a recording can be replayed from; use one of %s",
			opt.StorageClass, strings.Join(AzureTiers, ", "))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open recording: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	hdr := http.Header{}
	for k, v := range map[string]string{
		"argus_session":    sessionID,
		"argus_chain_head": chainHead,
		"argus_started_at": startedAt.UTC().Format(time.RFC3339),
	} {
		if v != "" {
			hdr.Set("x-ms-meta-"+k, v)
		}
	}
	if opt.StorageClass != "" {
		hdr.Set("x-ms-access-tier", opt.StorageClass)
	}

	if info.Size() <= azureSingleShot {
		hdr.Set("x-ms-blob-type", "BlockBlob")
		hdr.Set("Content-Type", contentType(key))
		resp, err := a.do(ctx, http.MethodPut, a.blobURL(key, nil), hdr, f, info.Size())
		if err != nil {
			return "", fmt.Errorf("upload %s: %w", key, err)
		}
		resp.Body.Close()
		return key, nil
	}

	// In blocks, then committed as a list. Blocks not committed are
	// discarded by the service after a week, so a failure part way leaves
	// nothing to clean up.
	var ids []string
	for off, n := int64(0), 0; off < info.Size(); off, n = off+azureBlockSize, n+1 {
		size := min(azureBlockSize, info.Size()-off)
		id := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "argus-block-%08d", n))
		resp, err := a.do(ctx, http.MethodPut,
			a.blobURL(key, url.Values{"comp": {"block"}, "blockid": {id}}), http.Header{},
			io.NewSectionReader(f, off, size), size)
		if err != nil {
			return "", fmt.Errorf("upload %s: block %d: %w", key, n, err)
		}
		resp.Body.Close()
		ids = append(ids, id)
	}
	var list bytes.Buffer
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		list.WriteString("<Latest>" + id + "</Latest>")
	}
	list.WriteString("</BlockList>")
	hdr.Set("x-ms-blob-content-type", contentType(key))
	hdr.Set("Content-Type", "application/xml")
	resp, err := a.do(ctx, http.MethodPut, a.blobURL(key, url.Values{"comp": {"blocklist"}}), hdr,
		bytes.NewReader(list.Bytes()), int64(list.Len()))
	if err != nil {
		return "", fmt.Errorf("upload %s: commit: %w", key, err)
	}
	resp.Body.Close()
	return key, nil
}

// Get opens a recording for reading.
func (a *Azure) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if a == nil {
		return nil, ErrNotConfigured
	}
	resp, err := a.do(ctx, http.MethodGet, a.blobURL(key, nil), nil, nil, 0)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ChainHead reads the integrity claim stored with a blob.
func (a *Azure) ChainHead(ctx context.Context, key string) (string, error) {
	if a == nil {
		return "", ErrNotConfigured
	}
	resp, err := a.do(ctx, http.MethodHead, a.blobURL(key, nil), nil, nil, 0)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if v := resp.Header.Get("x-ms-meta-argus_chain_head"); v != "" {
		return v, nil
	}
	// A blob copied in from an S3 bucket by hand keeps S3's spelling.
	return resp.Header.Get("x-ms-meta-argus-chain-head"), nil
}

// Delete removes a blob, and any snapshots of it. A missing one is not an
// error.
func (a *Azure) Delete(ctx context.Context, key string) error {
	if a == nil {
		return ErrNotConfigured
	}
	hdr := http.Header{}
	hdr.Set("x-ms-delete-snapshots", "include")
	resp, err := a.do(ctx, http.MethodDelete, a.blobURL(key, nil), hdr, nil, 0)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// List returns every blob in the container.
func (a *Azure) List(ctx context.Context) ([]Object, error) {
	if a == nil {
		return nil, ErrNotConfigured
	}
	var out []Object
	marker := ""
	for {
		q := url.Values{"restype": {"container"}, "comp": {"list"}, "maxresults": {"5000"}}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := a.do(ctx, http.MethodGet, a.containerURL(q), nil, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", a.Name(), err)
		}
		var page struct {
			Blobs []struct {
				Name       string `xml:"Name"`
				Properties struct {
					Size int64 `xml:"Content-Length"`
				} `xml:"Properties"`
			} `xml:"Blobs>Blob"`
			NextMarker string `xml:"NextMarker"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", a.Name(), err)
		}
		for _, b := range page.Blobs {
			if !isInternal(b.Name) {
				out = append(out, Object{Key: b.Name, Size: b.Properties.Size})
			}
		}
		if page.NextMarker == "" {
			return out, nil
		}
		marker = page.NextMarker
	}
}

// PresignedURL is a read-only service SAS for one blob, valid for ttl.
// https://learn.microsoft.com/rest/api/storageservices/create-service-sas
func (a *Azure) PresignedURL(_ context.Context, key string, ttl time.Duration) (*url.URL, error) {
	if a == nil {
		return nil, ErrNotConfigured
	}
	now := a.now().UTC()
	// Started a few minutes back: the service's clock and this one differ.
	start := now.Add(-5 * time.Minute).Format(time.RFC3339)
	expiry := now.Add(ttl).Format(time.RFC3339)
	protocol := ""
	if a.base.Scheme == "https" {
		protocol = "https"
	}
	resource := "/blob/" + a.account + "/" + a.container + "/" + key
	toSign := strings.Join([]string{
		"r", start, expiry, resource,
		"",       // signed identifier
		"",       // signed IP
		protocol, // signed protocol
		azureSASVersion,
		"b",                // signed resource: a blob
		"",                 // snapshot time
		"",                 // encryption scope
		"", "", "", "", "", // response headers
	}, "\n")
	q := url.Values{
		"sv":  {azureSASVersion},
		"sp":  {"r"},
		"st":  {start},
		"se":  {expiry},
		"sr":  {"b"},
		"sig": {a.mac(toSign)},
	}
	if protocol != "" {
		q.Set("spr", protocol)
	}
	return a.blobURL(key, q), nil
}
