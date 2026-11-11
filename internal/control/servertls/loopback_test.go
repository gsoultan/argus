package servertls

import (
	"os"
	"testing"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

// The pin an agent holds is of the key, so the key must survive restarts and
// reissues: a new key would cut off the helper that enrolled against it.
func TestLoopbackCertKeepsItsKey(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, err := LoopbackCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := tlsconfig.PinFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(keyFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st.Mode().Perm(), err)
	}

	again, _, err := LoopbackCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := tlsconfig.PinFile(again); p != pin {
		t.Fatalf("a restart changed the pin: %s, was %s", p, pin)
	}

	// A certificate gone or broken is reissued on the same key.
	if err := os.WriteFile(certFile, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoopbackCert(dir); err != nil {
		t.Fatal(err)
	}
	if p, _ := tlsconfig.PinFile(certFile); p != pin {
		t.Fatalf("a reissue changed the pin: %s, was %s", p, pin)
	}

	if _, err := Server(ServerOptions{CertFile: certFile, KeyFile: keyFile}); err != nil {
		t.Fatalf("the pair does not serve: %v", err)
	}
}
