package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// memBucket is a Bucket in memory, standing in for S3 or Azure.
type memBucket struct {
	mu    sync.Mutex
	objs  map[string][]byte
	heads map[string]string
	fail  map[string]bool // keys an upload of fails
}

func newMemBucket() *memBucket {
	return &memBucket{objs: map[string][]byte{}, heads: map[string]string{}, fail: map[string]bool{}}
}

func (m *memBucket) Name() string                       { return "mem" }
func (m *memBucket) EnsureBucket(context.Context) error { return nil }
func (m *memBucket) PresignedURL(context.Context, string, time.Duration) (*url.URL, error) {
	return nil, errors.New("no")
}
func (m *memBucket) Upload(ctx context.Context, path, sid, head string, at time.Time) (string, error) {
	return m.UploadKey(ctx, path, ObjectKeyExt(at, sid, extOf(path)), sid, head, at)
}
func (m *memBucket) UploadKey(ctx context.Context, path, key, sid, head string, at time.Time) (string, error) {
	return m.UploadKeyWith(ctx, path, key, sid, head, at, PutOptions{})
}
func (m *memBucket) UploadKeyWith(_ context.Context, path, key, _, head string, _ time.Time, _ PutOptions) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail[key] {
		return "", errors.New("refused")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m.objs[key], m.heads[key] = b, head
	return key, nil
}
func (m *memBucket) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, &AzureError{Status: 404, Code: "BlobNotFound"}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memBucket) ChainHead(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heads[key], nil
}
func (m *memBucket) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}
func (m *memBucket) List(context.Context) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for k, b := range m.objs {
		out = append(out, Object{Key: k, Size: int64(len(b))})
	}
	return out, nil
}

func dirWith(t *testing.T, files map[string]string) *Dir {
	t.Helper()
	d, err := NewDir(filepath.Join(t.TempDir(), "recordings"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		p := filepath.Join(d.Root(), filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Turning a bucket on after recordings were kept on this host copies them
// in under the keys the database already names, with their chain heads, and
// leaves this host's copies where they are.
func TestCopyDirMovesRecordingsKeptOnThisHostIntoTheBucket(t *testing.T) {
	ctx := context.Background()
	d := dirWith(t, map[string]string{
		"2026/11/01/s1.cast.zst":                  "one",
		"tenants/acme/2026/11/02/s2.argusrdp.zst": "two",
		"2026/11/03/s3.cast.zst":                  "three",
		"2026/11/04/s4.cast.zst":                  "four",
	})
	b := newMemBucket()
	b.objs["2026/11/03/s3.cast.zst"] = []byte("three") // already there
	b.fail["2026/11/04/s4.cast.zst"] = true            // the bucket refuses this one, for now
	head := func(r io.Reader) (string, error) {
		body, _ := io.ReadAll(r)
		return "head:" + string(body), nil
	}

	res, err := CopyDir(ctx, d, b, head, nil)
	if err == nil {
		t.Error("a pass that left a recording behind reported no error")
	}
	if res.Copied != 2 || res.Already != 1 || res.Failed != 1 {
		t.Errorf("result %+v, want 2 copied, 1 already there, 1 failed", res)
	}
	if got := string(b.objs["tenants/acme/2026/11/02/s2.argusrdp.zst"]); got != "two" {
		t.Errorf("an organization's recording was not copied under its prefix: %q", got)
	}
	if h := b.heads["2026/11/01/s1.cast.zst"]; h != "head:one" {
		t.Errorf("chain head %q, want the one computed from the recording", h)
	}
	if _, err := os.Stat(filepath.Join(d.Root(), "2026", "11", "01", "s1.cast.zst")); err != nil {
		t.Errorf("this host's copy was removed: %v", err)
	}

	// The next pass copies what was refused and nothing twice.
	delete(b.fail, "2026/11/04/s4.cast.zst")
	res, err = CopyDir(ctx, d, b, head, nil)
	if err != nil || res.Copied != 1 || res.Already != 3 {
		t.Errorf("second pass %+v, %v; want only the refused one copied", res, err)
	}
}

// While the copy runs, a recording only this host has still replays, and
// removing one from the console removes both copies.
func TestWithLocalReadsWhatTheBucketDoesNotHaveYet(t *testing.T) {
	ctx := context.Background()
	d := dirWith(t, map[string]string{"2026/11/01/old.cast.zst": "kept here"})
	b := newMemBucket()
	b.objs["2026/11/20/new.cast.zst"] = []byte("in the bucket")
	w := &WithLocal{Bucket: b, Local: d}

	if got := readAll(t, w, "2026/11/01/old.cast.zst"); string(got) != "kept here" {
		t.Errorf("old recording read %q", got)
	}
	if got := readAll(t, w, "2026/11/20/new.cast.zst"); string(got) != "in the bucket" {
		t.Errorf("new recording read %q", got)
	}
	if _, err := w.Get(ctx, "2026/11/21/none.cast.zst"); !IsNotFound(err) {
		t.Errorf("a recording neither has: %v, want not found", err)
	}
	// Scoped still finds its way to the bucket through the wrapper.
	if _, err := ForTenant(w, "").Upload(ctx, writeTemp(t, "n.cast.zst", []byte("x")), "n",
		"h", time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.objs["2026/11/22/n.cast.zst"]; !ok {
		t.Error("a new recording did not go to the bucket")
	}

	if err := ForTenant(w, "").Delete(ctx, "2026/11/01/old.cast.zst"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Get(ctx, "2026/11/01/old.cast.zst"); !IsNotFound(err) {
		t.Errorf("a removed recording still reads from this host: %v", err)
	}
}

func TestS3Defaults(t *testing.T) {
	for _, c := range []struct {
		in   Config
		want Config
	}{
		{Config{Endpoint: "https://abc123.r2.cloudflarestorage.com/"},
			Config{Endpoint: "abc123.r2.cloudflarestorage.com", UseSSL: true, Region: "auto"}},
		{Config{Endpoint: "http://minio.internal:9000"}, Config{Endpoint: "minio.internal:9000"}},
		{Config{Endpoint: "s3.amazonaws.com", Region: "eu-west-1", UseSSL: true},
			Config{Endpoint: "s3.amazonaws.com", Region: "eu-west-1", UseSSL: true}},
		{Config{Endpoint: "abc.r2.cloudflarestorage.com", Region: "weur"},
			Config{Endpoint: "abc.r2.cloudflarestorage.com", Region: "weur"}},
	} {
		if got := s3Defaults(c.in); got != c.want {
			t.Errorf("s3Defaults(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}
