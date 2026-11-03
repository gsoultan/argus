package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestADirectoryKeepsARecordingUnderTheKeyABucketWould(t *testing.T) {
	d, err := NewDir(filepath.Join(t.TempDir(), "recordings"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "s.argusrdp")
	if err := os.WriteFile(src, []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 31, 23, 0, 0, 0, time.FixedZone("x", -3600))
	key, err := d.Upload(context.Background(), src, "sid", "head", at)
	if err != nil {
		t.Fatal(err)
	}
	if want := ObjectKeyExt(at, "sid", ".argusrdp"); key != want {
		t.Errorf("key %q, want %q", key, want)
	}
	r, err := d.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "frames" {
		t.Errorf("read back %q", got)
	}
	objs, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != key || objs[0].Size != 6 {
		t.Errorf("list: %+v", objs)
	}
	if info, err := os.Stat(d.Root()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("directory mode: %v %v", info.Mode(), err)
	}
}

// A key comes from the database. One that climbs out of the directory must
// not read whatever else this process can.
func TestADirectoryRefusesAKeyOutsideIt(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "../control.yaml", "2026/../../x", "/etc/passwd"} {
		if r, err := d.Get(context.Background(), key); err == nil {
			_ = r.Close()
			t.Errorf("%q was opened", key)
		}
	}
}

// A compressed recording's key keeps its format and its compression, and an
// organization's archive is inside its own prefix.
func TestKeysOfCompressedAndArchivedRecordings(t *testing.T) {
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for path, want := range map[string]string{
		"/tmp/argus-recording-1.cast.zst":     ".cast.zst",
		"/tmp/argus-recording-1.argusrdp.zst": ".argusrdp.zst",
		"/tmp/argus-recording-1.argusrdp":     ".argusrdp",
		"/tmp/argus-recording-1":              ".cast",
	} {
		if got := extOf(path); got != want {
			t.Errorf("extOf(%s) = %s, want %s", path, got, want)
		}
	}
	d := &Dir{root: t.TempDir()}
	def, org := ForTenant(d, ""), ForTenant(d, "acme")
	if k := def.ArchiveKey(at, "s1", ".cast.zst"); k != "archive/2026/11/03/s1.cast.zst" || !def.Owns(k) || !IsArchived(k) {
		t.Errorf("default archive key %s", k)
	}
	k := org.ArchiveKey(at, "s1", ".cast.zst")
	if k != "tenants/acme/archive/2026/11/03/s1.cast.zst" || !org.Owns(k) || def.Owns(k) || !IsArchived(k) {
		t.Errorf("organization archive key %s", k)
	}
	if IsArchived("2026/11/03/s1.cast.zst") || IsArchived("tenants/acme/2026/11/03/s1.cast") {
		t.Error("a hot key reads as archived")
	}
	if _, err := def.PutArchive(context.Background(), "x", k, "s1", "h", at, PutOptions{}); err == nil {
		t.Error("the default organization archived into another's prefix")
	}
}
