package tunnel

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"
)

func testKeys(t *testing.T) (secret string) {
	t.Helper()
	return strings.Repeat("s", 40)
}

func writeDump(t *testing.T, secret, org string, plain []byte) (bytes.Buffer, DumpHeader) {
	t.Helper()
	priv, err := DeriveBackupKey(secret, org)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, h, err := NewDumpWriter(&out, priv.PublicKey(), DumpHeader{BackupID: "b1", DatabaseID: "d1",
		Organization: org, DBName: "orders", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Written != int64(out.Len()) {
		t.Fatalf("Written = %d, file is %d", w.Written, out.Len())
	}
	return out, h
}

func open(t *testing.T, secret, org string, file []byte, expect string) ([]byte, error) {
	t.Helper()
	priv, _ := DeriveBackupKey(secret, org)
	h, _, err := ReadDumpHeader(bufio.NewReader(bytes.NewReader(file)))
	if err != nil {
		return nil, err
	}
	key, err := UnwrapBackupKey(priv, h)
	if err != nil {
		return nil, err
	}
	r, err := NewDumpReader(bytes.NewReader(file), key, expect)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestDumpRoundTripsAcrossFrames(t *testing.T) {
	secret := testKeys(t)
	plain := make([]byte, 2*DumpFrameSize+12345)
	_, _ = rand.Read(plain)
	file, h := writeDump(t, secret, "default", plain)
	if h.KeyID == "" || h.WrappedKey == "" {
		t.Fatal("the header does not carry the wrap")
	}
	got, err := open(t, secret, "default", file.Bytes(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("plaintext differs")
	}
}

func TestEmptyDumpStillHasAFinalFrame(t *testing.T) {
	secret := testKeys(t)
	file, _ := writeDump(t, secret, "default", nil)
	got, err := open(t, secret, "default", file.Bytes(), "")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d bytes, %v", len(got), err)
	}
}

func TestTruncatedDumpIsRefusedNotRestoredAsAPrefix(t *testing.T) {
	secret := testKeys(t)
	plain := make([]byte, DumpFrameSize+100)
	file, _ := writeDump(t, secret, "default", plain)
	b := file.Bytes()
	// Cut the last frame off entirely: what remains decrypts fine frame by
	// frame and must still fail.
	cut := b[:len(b)-(4+100+16)]
	_, err := open(t, secret, "default", cut, "")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("a truncated file opened: %v", err)
	}
}

func TestAlteredByteIsRefused(t *testing.T) {
	secret := testKeys(t)
	file, _ := writeDump(t, secret, "default", []byte("hello backup"))
	b := file.Bytes()
	b[len(b)-3] ^= 1
	if _, err := open(t, secret, "default", b, ""); err == nil {
		t.Fatal("an altered frame opened")
	}
}

func TestHeaderFromAnotherBackupIsRefused(t *testing.T) {
	secret := testKeys(t)
	f1, _ := writeDump(t, secret, "default", []byte("one"))
	f2, _ := writeDump(t, secret, "default", []byte("two"))
	br := bufio.NewReader(bytes.NewReader(f1.Bytes()))
	_, _, _ = ReadDumpHeader(br)
	frames1, _ := io.ReadAll(br)
	// Simpler and exact: f2 header bytes + f1 frames.
	br2 := bufio.NewReader(bytes.NewReader(f2.Bytes()))
	_, hb2, _ := ReadDumpHeader(br2)
	var out bytes.Buffer
	out.WriteString(dumpMagic)
	out.Write([]byte{0, 0, byte(len(hb2) >> 8), byte(len(hb2))})
	out.Write(hb2)
	out.Write(frames1)
	if _, err := open(t, secret, "default", out.Bytes(), ""); err == nil {
		t.Fatal("a spliced file opened")
	}
}

func TestAnotherOrganizationsKeyCannotOpenIt(t *testing.T) {
	secret := testKeys(t)
	file, _ := writeDump(t, secret, "acme", []byte("secret rows"))
	_, err := open(t, secret, "default", file.Bytes(), "")
	if err == nil || !strings.Contains(err.Error(), "encrypted to key") {
		t.Fatalf("another organization's key opened it: %v", err)
	}
}

func TestKeyForOneBackupIsRefusedOnAnother(t *testing.T) {
	secret := testKeys(t)
	file, _ := writeDump(t, secret, "default", []byte("x"))
	_, err := open(t, secret, "default", file.Bytes(), "b2")
	if err == nil || !strings.Contains(err.Error(), "backup b1") {
		t.Fatalf("expected a refusal naming the backup, got %v", err)
	}
}

func TestBackupGrantValidation(t *testing.T) {
	priv, _ := DeriveBackupKey(testKeys(t), "default")
	pub := EncodeBackupPublicKey(priv.PublicKey())
	good := GrantBackup{Job: BackupJobDump, BackupID: "0f0f0f0f-0f0f-0f0f-0f0f-0f0f0f0f0f0f", DatabaseID: "d",
		Name: "orders", Host: "/var/run/postgresql", Port: 5432, Destination: BackupDestServer, PublicKey: pub}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []GrantBackup{
		func() GrantBackup { g := good; g.Host = "10.0.0.5"; return g }(),
		func() GrantBackup { g := good; g.Name = "orders; drop"; return g }(),
		func() GrantBackup { g := good; g.PublicKey = "nope"; return g }(),
		func() GrantBackup { g := good; g.BackupID = "../etc"; return g }(),
		func() GrantBackup {
			g := good
			g.Job = BackupJobRestore
			g.DataKey, g.SHA256 = "k", strings.Repeat("a", 64)
			g.Restore = &GrantRestore{Mode: RestoreReplace, Source: "local", RenameTo: "orders_old"}
			return g
		}(),
		func() GrantBackup {
			g := good
			g.Job = BackupJobRestore
			g.DataKey, g.SHA256 = "k", strings.Repeat("a", 64)
			g.Restore = &GrantRestore{Mode: RestoreNew, Source: "local", NewName: "orders"}
			return g
		}(),
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("case %d validated", i)
		}
	}
}

func TestMaskConnInfo(t *testing.T) {
	got := MaskConnInfo("host=a port=5432 PASSWORD=hunter2 dbname=x")
	if got != "host=a port=5432 password=*** dbname=x" {
		t.Fatal(got)
	}
}
