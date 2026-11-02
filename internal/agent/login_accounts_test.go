package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

/*
Who can log in, and how each login signed in. These hold that an account with
only a password is listed (it used to be invisible), that sshd's own settings
decide whether a password or key counts, that a key a login claims is named by
its comment only when it is in the account's authorized_keys, and that a
password is never more than "set", "none" or "locked".
*/

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func authorizedLine(k ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment + "\n"
}

// host writes a passwd, shadow and homes: staging has a key and a password,
// sys-admin only a password, deploy only a key, nobody a nologin shell.
func host(t *testing.T, key ssh.PublicKey) ScanConfig {
	t.Helper()
	dir := t.TempDir()
	home := func(name string) string { return filepath.Join(dir, "home", name) }
	passwd := "" +
		"staging:x:1001:1001::" + home("staging") + ":/bin/bash\n" +
		"sys-admin:x:1002:1002::" + home("sys-admin") + ":/bin/bash\n" +
		"deploy:x:1003:1003::" + home("deploy") + ":/bin/sh\n" +
		"nobody:x:65534:65534::/nonexistent:/usr/sbin/nologin\n"
	shadow := "" +
		"staging:$6$salt$hash:19000:0:99999:7:::\n" +
		"sys-admin:$y$j9T$hash:19000:0:99999:7:::\n" +
		"deploy:!:19000:0:99999:7:::\n" +
		"nobody:*:19000:0:99999:7:::\n"
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "passwd"), passwd)
	write(filepath.Join(dir, "shadow"), shadow)
	write(filepath.Join(home("staging"), ".ssh", "authorized_keys"), authorizedLine(key, "gembit@laptop"))
	write(filepath.Join(home("deploy"), ".ssh", "authorized_keys"),
		authorizedLine(newKey(t), "ci")+authorizedLine(newKey(t), "old-ci"))
	write(filepath.Join(dir, "sshd_config"), "ForceCommand /usr/bin/argus-agent shim\n")
	return ScanConfig{
		SSHDConfig: filepath.Join(dir, "sshd_config"),
		PasswdFile: filepath.Join(dir, "passwd"),
		ShadowFile: filepath.Join(dir, "shadow"),
	}
}

func byUser(list []LoginAccount) map[string]LoginAccount {
	m := map[string]LoginAccount{}
	for _, a := range list {
		m[a.User] = a
	}
	return m
}

// Every account with a login shell is listed, a password-only one included,
// and what sshd allows decides who can log in.
func TestLoginAccountsListPasswordsAndKeys(t *testing.T) {
	cfg := host(t, newKey(t))

	got := byUser(scanLoginAccounts(cfg, SSHDAuth{Passwords: true, Keys: true}))
	if _, ok := got["nobody"]; ok {
		t.Error("a nologin account was listed")
	}
	want := map[string]LoginAccount{
		"staging":   {User: "staging", Password: PasswordSet, Keys: 1, CanLogin: true},
		"sys-admin": {User: "sys-admin", Password: PasswordSet, Keys: 0, CanLogin: true},
		"deploy":    {User: "deploy", Password: PasswordLocked, Keys: 2, CanLogin: true},
	}
	for u, w := range want {
		if got[u] != w {
			t.Errorf("%s = %+v, want %+v", u, got[u], w)
		}
	}

	// With passwords off, an account with only a password cannot get in.
	got = byUser(scanLoginAccounts(cfg, SSHDAuth{Passwords: false, Keys: true}))
	if got["sys-admin"].CanLogin {
		t.Error("sys-admin can log in with passwords off and no key")
	}
	if !got["staging"].CanLogin || !got["deploy"].CanLogin {
		t.Error("an account with a key cannot log in with keys on")
	}
}

// Shadow that cannot be read leaves a password unknown, and unknown counts
// as a way in: an account is never left off for want of reading it.
func TestAnUnreadableShadowStillListsTheAccount(t *testing.T) {
	cfg := host(t, newKey(t))
	cfg.ShadowFile = filepath.Join(t.TempDir(), "missing")
	got := byUser(scanLoginAccounts(cfg, SSHDAuth{Passwords: true, Keys: true}))
	if a := got["sys-admin"]; a.Password != PasswordUnknown || !a.CanLogin {
		t.Errorf("sys-admin = %+v", a)
	}
}

// sshd's settings come from the file and what it includes, first value wins,
// Match blocks do not count, and OpenSSH's defaults fill the rest.
func TestSSHDAuthFollowsIncludesAndDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "d", "50-cloud.conf"), []byte("PasswordAuthentication no\n"), 0o600)
	cfg := filepath.Join(dir, "sshd_config")
	_ = os.WriteFile(cfg, []byte("Include "+filepath.Join(dir, "d", "*.conf")+"\n"+
		"PasswordAuthentication yes\nExposeAuthInfo yes\nMatch User backup\n  PubkeyAuthentication no\n"), 0o600)
	a := readSSHDAuth(cfg)
	if a.Passwords || !a.Keys || !a.ExposeAuthInfo || a.PermitRootLogin != "prohibit-password" {
		t.Errorf("got %+v", a)
	}
	if d := readSSHDAuth(filepath.Join(dir, "absent")); !d.Passwords || !d.Keys || d.ExposeAuthInfo {
		t.Errorf("defaults %+v", d)
	}
}

// A password login says so; a key login names the key, by its comment only
// when the account's authorized_keys holds it.
func TestLoginAuthNamesTheKeyOnlyWhenTheAccountHasIt(t *testing.T) {
	key := newKey(t)
	cfg := host(t, key)
	c := &Collector{PasswdFile: cfg.PasswdFile}
	line := "publickey " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))

	a := c.loginAuth(SessionStart{Principal: "staging", UserAuth: line + "\n"})
	if a == nil || a.Method != "publickey" || a.KeyFingerprint != ssh.FingerprintSHA256(key) ||
		!a.KeyFound || a.KeyComment != "gembit@laptop" {
		t.Fatalf("staging's own key: %+v", a)
	}

	// The same key claimed for an account that does not hold it is reported,
	// but not found and not named.
	a = c.loginAuth(SessionStart{Principal: "deploy", UserAuth: line + "\n"})
	if a == nil || a.KeyFound || a.KeyComment != "" || a.KeyFingerprint == "" {
		t.Fatalf("a key deploy does not hold: %+v", a)
	}

	a = c.loginAuth(SessionStart{Principal: "sys-admin", UserAuth: "password\n"})
	if a == nil || a.Method != "password" || a.KeyFingerprint != "" {
		t.Fatalf("password login: %+v", a)
	}
	if !strings.Contains(a.Describe(), "password") {
		t.Errorf("describe: %q", a.Describe())
	}

	// Two methods, and lines that are not methods, which are skipped.
	a = c.loginAuth(SessionStart{Principal: "staging", UserAuth: "rm -rf /\n" + line + "\npassword\n"})
	if a == nil || a.Method != "publickey+password" || !a.KeyFound {
		t.Fatalf("two methods: %+v", a)
	}

	if a := c.loginAuth(SessionStart{Principal: "staging"}); a != nil {
		t.Errorf("no SSH_USER_AUTH gave %+v", a)
	}
	if !strings.Contains((*LoginAuth)(nil).Describe(), "ExposeAuthInfo") {
		t.Error("an unknown method does not say why")
	}
}

// A direct login's session record says how it signed in.
func TestADirectSessionCarriesHowItSignedIn(t *testing.T) {
	key := newKey(t)
	cfg := host(t, key)
	c, sock := testCollector(t)
	defer c.CloseWithin(1e9)
	c.PasswdFile = cfg.PasswdFile
	c.SetDirectSSH(DirectSSHAllow)
	allowed := make(chan SessionStart, 1)
	c.OnAllowed = func(m SessionStart) { allowed <- m }

	conn, err := dialStart(sock, SessionStart{
		Type: "start", Principal: "staging", Hostname: "db-01", Origin: Direct,
		UserAuth: "publickey " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	m := <-allowed
	if m.Auth == nil || !m.Auth.KeyFound || m.Auth.KeyComment != "gembit@laptop" {
		t.Fatalf("allowed login reported %+v", m.Auth)
	}
}

func dialStart(sock string, m SessionStart) (net.Conn, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(m); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
