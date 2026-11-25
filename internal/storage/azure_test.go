package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Azure client is checked against the real service's emulator, Azurite,
// because the one thing a fake cannot tell us is whether Azure accepts the
// signature. Run it with:
//
//	npx azurite-blob --inMemoryPersistence --loose &
//	ARGUS_TEST_AZURITE=http://127.0.0.1:10000/devstoreaccount1 go test ./internal/storage -run Azure
//
// Without the variable these tests skip; the ones below them do not need it.
func azurite(t *testing.T) *Azure {
	t.Helper()
	endpoint := os.Getenv("ARGUS_TEST_AZURITE")
	if endpoint == "" {
		t.Skip("ARGUS_TEST_AZURITE is not set")
	}
	// Azurite's published development account and key.
	a, err := NewAzure(Config{Provider: ProviderAzure, Endpoint: endpoint,
		AccessKey: "devstoreaccount1",
		SecretKey: "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==",
		Bucket:    "argus-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))[:min(30, len(t.Name()))]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.http = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	return a
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readAll(t *testing.T, s Store, key string) []byte {
	t.Helper()
	r, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Everything the control plane does with a bucket, against Azure: the
// recording keeps its key and its chain head, lists, survives an archive
// copy under a tier, opens through a SAS, and is gone after a delete.
func TestAzureKeepsARecordingAsABucketWould(t *testing.T) {
	a := azurite(t)
	ctx := context.Background()
	if err := a.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureBucket(ctx); err != nil {
		t.Fatalf("a second EnsureBucket on an existing container: %v", err)
	}
	at := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	body := []byte("[0.1,\"o\",\"hello\"]\n")
	key, err := ForTenant(a, "acme").Upload(ctx, writeTemp(t, "s.cast.zst", body), "sid-1", "head-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if want := "tenants/acme/2026/11/24/sid-1.cast.zst"; key != want {
		t.Fatalf("key %q, want %q", key, want)
	}
	if got := readAll(t, a, key); !bytes.Equal(got, body) {
		t.Errorf("read back %q", got)
	}
	if h, err := a.ChainHead(ctx, key); err != nil || h != "head-1" {
		t.Errorf("chain head %q, %v", h, err)
	}

	// The archive's copy, in a tier.
	arch := ForTenant(a, "acme").ArchiveKey(at, "sid-1", ".cast.zst")
	if _, err := ForTenant(a, "acme").PutArchive(ctx, writeTemp(t, "a.cast.zst", body), arch, "sid-1",
		"head-1", at, PutOptions{StorageClass: "Cool"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UploadKeyWith(ctx, writeTemp(t, "b", body), "x", "", "", at,
		PutOptions{StorageClass: "Archive"}); err == nil {
		t.Error("the Archive tier was accepted; a recording there cannot be replayed")
	}

	if err := Check(ctx, a); err != nil {
		t.Fatalf("connection check: %v", err)
	}
	objs, err := a.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int64{}
	for _, o := range objs {
		keys[o.Key] = o.Size
	}
	if keys[key] != int64(len(body)) || keys[arch] != int64(len(body)) || len(keys) != 2 {
		t.Errorf("listed %v; want the recording and its archive copy only, not the connection check", keys)
	}

	u, err := a.PresignedURL(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
		t.Errorf("SAS link: %d %q", resp.StatusCode, got)
	}
	// The link is for that blob and for reading: it cannot be turned on
	// another one.
	other := a.blobURL(arch, u.Query())
	if resp, err := a.http.Get(other.String()); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("a SAS for one blob opened another")
		}
	}

	if err := a.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, key); err != nil {
		t.Errorf("deleting what is gone: %v", err)
	}
	if _, err := a.Get(ctx, key); !IsNotFound(err) {
		t.Errorf("after delete, Get: %v, want not found", err)
	}
}

// A recording too big for one request goes up in blocks and reads back
// whole, with its metadata.
func TestAzureUploadsALargeRecordingInBlocks(t *testing.T) {
	a := azurite(t)
	defer func(s, b int64) { azureSingleShot, azureBlockSize = s, b }(azureSingleShot, azureBlockSize)
	azureSingleShot, azureBlockSize = 1000, 300
	ctx := context.Background()
	if err := a.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789abcdef"), 200) // 3200 bytes, 11 blocks
	key, err := a.Upload(ctx, writeTemp(t, "big.argusrdp.zst", body), "sid-big", "head-big", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, a, key); !bytes.Equal(got, body) {
		t.Errorf("read back %d bytes, want %d", len(got), len(body))
	}
	if h, _ := a.ChainHead(ctx, key); h != "head-big" {
		t.Errorf("chain head %q", h)
	}
}

func TestAzureRefusesAWrongKey(t *testing.T) {
	a := azurite(t)
	a.key = []byte("not the key")
	err := a.EnsureBucket(context.Background())
	var ae *AzureError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("EnsureBucket with a wrong key: %v, want a 403 from the service", err)
	}
}

func TestAzureConfig(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("k"))
	for _, c := range []struct {
		name string
		cfg  Config
		base string
		err  string
	}{
		{"public cloud by default", Config{AccessKey: "acct", SecretKey: key, Bucket: "rec"},
			"https://acct.blob.core.windows.net", ""},
		{"a host takes its scheme from UseSSL", Config{AccessKey: "acct", SecretKey: key,
			Endpoint: "acct.blob.core.usgovcloudapi.net", UseSSL: true}, "https://acct.blob.core.usgovcloudapi.net", ""},
		{"an emulator's path is kept", Config{AccessKey: "devstoreaccount1", SecretKey: key,
			Endpoint: "http://127.0.0.1:10000/devstoreaccount1/"}, "http://127.0.0.1:10000/devstoreaccount1", ""},
		{"a key that is not base64", Config{AccessKey: "acct", SecretKey: "not base64!"}, "", "not a key"},
		{"a container Azure will not take", Config{AccessKey: "acct", SecretKey: key, Bucket: "Recordings"},
			"", "cannot name an Azure container"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := NewAzure(c.cfg, nil)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := a.base.String(); got != c.base {
				t.Errorf("base %q, want %q", got, c.base)
			}
		})
	}
	if a, err := NewAzure(Config{}, nil); a != nil || err != nil {
		t.Errorf("no account: %v, %v; want nothing configured", a, err)
	}
}

// Open never hands back a typed nil: a caller's nil check has to work.
func TestOpenWithNothingConfiguredIsANilBucket(t *testing.T) {
	for _, p := range []string{"", ProviderS3, ProviderAzure} {
		b, err := Open(Config{Provider: p}, nil)
		if err != nil || b != nil {
			t.Errorf("provider %q: %v, %v; want a nil interface", p, b, err)
		}
	}
	if _, err := Open(Config{Provider: "gcs", Endpoint: "x"}, nil); err == nil {
		t.Error("an unknown provider was accepted")
	}
}

func TestBlobURLEscapesEachSegment(t *testing.T) {
	a := &Azure{base: &url.URL{Scheme: "https", Host: "acct.blob.core.windows.net"}, container: "rec"}
	got := a.blobURL("tenants/a b/2026/11/24/x#y.cast", nil).String()
	if want := "https://acct.blob.core.windows.net/rec/tenants/a%20b/2026/11/24/x%23y.cast"; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}
