package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// postureHost lays out a host with n login accounts, each holding a few keys
// of the kinds a real fleet has, and returns the scan config for it.
func postureHost(tb testing.TB, n int) ScanConfig {
	tb.Helper()
	dir := tb.TempDir()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	rsaPub, _ := ssh.NewPublicKey(&rsaKey.PublicKey)
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	edSSH, _ := ssh.NewPublicKey(edPub)
	keys := string(ssh.MarshalAuthorizedKey(rsaPub)) + string(ssh.MarshalAuthorizedKey(edSSH)) +
		string(ssh.MarshalAuthorizedKey(edSSH))

	var passwd, shadow strings.Builder
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user%03d", i)
		home := filepath.Join(dir, "home", name)
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(keys), 0o600); err != nil {
			tb.Fatal(err)
		}
		fmt.Fprintf(&passwd, "%s:x:%d:%d::%s:/bin/bash\n", name, 1000+i, 1000+i, home)
		fmt.Fprintf(&shadow, "%s:$6$salt$hash:19000:0:99999:7:::\n", name)
	}
	cfg := ScanConfig{
		SSHDConfig:          filepath.Join(dir, "sshd_config"),
		PasswdFile:          filepath.Join(dir, "passwd"),
		ShadowFile:          filepath.Join(dir, "shadow"),
		ManagedFingerprints: map[string]bool{},
		ShimPath:            "/usr/libexec/argus-agent",
	}
	for path, body := range map[string]string{
		cfg.SSHDConfig: "ForceCommand /usr/libexec/argus-agent shim\nExposeAuthInfo yes\nPasswordAuthentication no\n",
		cfg.PasswdFile: passwd.String(),
		cfg.ShadowFile: shadow.String(),
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return cfg
}

// What the heartbeat paid every 30 seconds before it shared the posture scan:
// a whole scan, every key parsed twice.
func BenchmarkPostureScan(b *testing.B) {
	cfg := postureHost(b, 50)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Scan(cfg); err != nil {
			b.Fatal(err)
		}
	}
}
