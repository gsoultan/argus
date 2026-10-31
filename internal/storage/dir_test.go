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
