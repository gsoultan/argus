package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSSHPortsFindsEveryWayIn(t *testing.T) {
	dir := t.TempDir()
	cfg := write(t, dir, "sshd_config", `
# Port 9999 is commented out and must not count
Port 22
Port 2222
ListenAddress 0.0.0.0:2022
ListenAddress 10.0.0.5
ListenAddress [::]:2032
PermitRootLogin no
`)
	got := sshPorts(cfg)
	for _, want := range []int{22, 2222, 2022, 2032} {
		if !slices.Contains(got, want) {
			t.Errorf("port %d missing from %v; an unlisted port is an unmonitored way in", want, got)
		}
	}
	if slices.Contains(got, 9999) {
		t.Errorf("commented-out port 9999 was reported in %v", got)
	}
}

// A default sshd config names no port but still listens on 22. Reporting an
// empty list would read as "sshd is not listening", which is the opposite of
// what is true.
func TestSSHPortsDefaultsTo22(t *testing.T) {
	dir := t.TempDir()
	if got := sshPorts(write(t, dir, "empty", "# nothing here\n")); !slices.Equal(got, []int{22}) {
		t.Errorf("empty config gave %v, want [22]", got)
	}
	if got := sshPorts(filepath.Join(dir, "does-not-exist")); !slices.Equal(got, []int{22}) {
		t.Errorf("missing config gave %v, want [22]", got)
	}
}

func TestLoginAccountsExcludesServiceAccounts(t *testing.T) {
	dir := t.TempDir()
	passwd := write(t, dir, "passwd", `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
bin:x:2:2:bin:/bin:/bin/false
ops:x:1000:1000:Ops:/home/ops:/bin/bash
deploy:x:1001:1001:Deploy:/home/deploy:/bin/zsh
broken-line-with-too-few-fields
`)
	shells := write(t, dir, "shells", "/bin/bash\n/bin/zsh\n")

	got := loginAccounts(passwd, shells)
	want := []string{"root", "ops", "deploy"}
	if !slices.Equal(got, want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
}

// Without /etc/shells, anything that is not an explicit refusal counts. A host
// with an unusual login shell must not silently drop out of the account list —
// that would make the least conventional hosts the least visible.
func TestLoginAccountsWithoutShellsFile(t *testing.T) {
	dir := t.TempDir()
	passwd := write(t, dir, "passwd", `ops:x:1000:1000::/home/ops:/usr/bin/fish
svc:x:999:999::/nonexistent:/usr/sbin/nologin
`)
	got := loginAccounts(passwd, filepath.Join(dir, "no-shells-file"))
	if !slices.Equal(got, []string{"ops"}) {
		t.Fatalf("accounts = %v, want [ops]", got)
	}
}

func TestOSNameReadsPrettyName(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "os-release", `NAME="Debian GNU/Linux"
PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
VERSION_ID="13"
`)
	if got := osName(p); got != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("osName = %q", got)
	}
	if got := osName(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing file gave %q, want empty", got)
	}
}

// Every source is independently optional: a host missing os-release still
// reports its accounts, because partial facts still close part of the gap.
func TestDiscoverToleratesMissingSources(t *testing.T) {
	dir := t.TempDir()
	f := Discover(DiscoverConfig{
		OSRelease:  filepath.Join(dir, "nope"),
		Passwd:     write(t, dir, "passwd", "ops:x:1000:1000::/home/ops:/bin/sh\n"),
		Shells:     filepath.Join(dir, "nope"),
		MachineID:  filepath.Join(dir, "nope"),
		SSHDConfig: filepath.Join(dir, "nope"),
	})
	if !slices.Equal(f.Accounts, []string{"ops"}) {
		t.Errorf("accounts = %v, want [ops]", f.Accounts)
	}
	if !slices.Equal(f.SSHPorts, []int{22}) {
		t.Errorf("ports = %v, want [22]", f.SSHPorts)
	}
	if f.Hostname == "" {
		t.Error("hostname should still be reported")
	}
}
