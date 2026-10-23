package tlsconfig

import (
	"encoding/pem"
	"os"
	"testing"

	"github.com/gsoultan/argus/internal/secrets"
)

func TestManagedPKI(t *testing.T) {
	p := ManagedPKI{Dir: t.TempDir()}
	if made, err := p.EnsureCA(); err != nil || !made {
		t.Fatalf("EnsureCA = %v, %v", made, err)
	}
	if made, _ := p.EnsureCA(); made {
		t.Error("EnsureCA replaced an existing CA")
	}
	if err := p.IssueServer([]string{"localhost", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.CAKey(), p.ServerKey()} {
		if err := secrets.CheckPrivate(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if !p.IssuesServer() {
		t.Fatal("the issued certificate does not verify against the CA")
	}
	// A new address is added; the ones agents enrolled against are kept.
	if renewed, err := p.RenewServerIfDue([]string{"argus.example.com"}); err != nil || !renewed {
		t.Fatalf("renew for a new name = %v, %v", renewed, err)
	}
	sum, err := Summarize(p.ServerCert())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"argus.example.com", "localhost", "127.0.0.1"} {
		found := false
		for _, n := range sum.Names {
			found = found || n == want
		}
		if !found {
			t.Errorf("renewed certificate lost %s: %v", want, sum.Names)
		}
	}
	if renewed, _ := p.RenewServerIfDue([]string{"argus.example.com"}); renewed {
		t.Error("renewed a current certificate")
	}

}

// An uploaded certificate is checked against its key and never replaced by
// renewal behind the operator's back.
func TestUploadedCertificateIsLeftAlone(t *testing.T) {
	dev := t.TempDir()
	if err := GenerateDevPKI(dev, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(dev + "/server.crt")
	key, _ := os.ReadFile(dev + "/server.key")
	other, _ := os.ReadFile(dev + "/client.key")

	p := ManagedPKI{Dir: t.TempDir()}
	if _, err := p.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteServerPair(cert, other); err == nil {
		t.Error("a certificate with the wrong key was accepted")
	}
	if err := p.WriteServerPair(cert, key); err != nil {
		t.Fatal(err)
	}
	if p.IssuesServer() {
		t.Error("an uploaded certificate counted as the control plane's own")
	}
	if renewed, _ := p.RenewServerIfDue([]string{"elsewhere.example.com"}); renewed {
		t.Error("renewal replaced an uploaded certificate")
	}
	if b, _ := pem.Decode(cert); b == nil {
		t.Fatal("bad fixture")
	}
}
