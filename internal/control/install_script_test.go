package control

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

/*
The one-line install's script runs as root on a server, so what it writes into
itself is checked before it is written, and its order is the one that keeps a
server reachable.
*/

func installScript(t *testing.T, h http.Handler, token, version, control string) (int, string) {
	t.Helper()
	q := url.Values{"token": {token}, "version": {version}, "url": {control}}
	w := do(t, h, http.MethodGet, "/api/v1/install/agent.sh?"+q.Encode(), "", "")
	return w.Code, w.Body.String()
}

const goodToken = "argus-enrol-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func TestTheInstallScriptCarriesTheTokenAndAddress(t *testing.T) {
	a, _, h, _ := enrollAPI(t)
	a.Enroll.Pin = func() (string, error) { return "sha256:" + strings.Repeat("ab", 32), nil }

	code, body := installScript(t, h, goodToken, "1.4.0", "https://argus.example.com:8443")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{
		"TOKEN='" + goodToken + "'",
		"CONTROL='https://argus.example.com:8443'",
		"PIN='sha256:" + strings.Repeat("ab", 32) + "'",
		"releases/download/v${VERSION}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
}

// Anything that is not exactly the shape expected is refused, never quoted
// into a script that runs as root.
func TestTheInstallScriptRefusesWhatAShellWouldRead(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	cases := map[string][3]string{
		"a token with a quote":    {goodToken + "'; rm -rf / #", "1.4.0", "https://argus.example.com"},
		"not an enrollment token": {"argus-agent-abcdefghijklmnopqrstuvwxyz0123", "1.4.0", "https://argus.example.com"},
		"a version with a space":  {goodToken, "1.4.0 && id", "https://argus.example.com"},
		"plain http":              {goodToken, "1.4.0", "http://argus.example.com"},
		"an address with a path":  {goodToken, "1.4.0", "https://argus.example.com/$(id)"},
		"an address with a quote": {goodToken, "1.4.0", "https://argus.example.com'"},
	}
	for name, c := range cases {
		if code, body := installScript(t, h, c[0], c[1], c[2]); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %q", name, code, body)
		}
	}
}

// The order that keeps a server reachable: the agent runs before sshd is
// told to send every login through it, and sshd_config is restored if sshd
// rejects the change.
func TestTheInstallScriptStartsTheAgentBeforeHookingSSHD(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	_, body := installScript(t, h, goodToken, "1.4.0", "https://argus.example.com")
	start := strings.Index(body, "systemctl enable --now argus-agent")
	hook := strings.Index(body, "argus-agent install --shim")
	if start < 0 || hook < 0 || start > hook {
		t.Fatalf("the agent must start before sshd is hooked (start at %d, hook at %d)", start, hook)
	}
	if !strings.Contains(body, `cp -p "$CONF.before-argus" "$CONF"`) {
		t.Error("the script does not put sshd_config back when sshd rejects it")
	}
}

func TestCurlPinIsTheSameKey(t *testing.T) {
	if got := curlPin("sha256:" + strings.Repeat("00", 32)); got != "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Errorf("curlPin = %q", got)
	}
	if got := curlPin("sha256:nothex"); got != "" {
		t.Errorf("curlPin of garbage = %q, want empty", got)
	}
}

// runInstallScript runs the script with every command it calls replaced by a
// stub that logs its arguments, against a copy of sshd_config, and returns the
// log and what sshd_config became. uname -m reports machine.
func runInstallScript(t *testing.T, body, machine, sshdConfig string) (log, conf string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh to run the script with")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "calls")
	stub := func(name, script string) {
		t.Helper()
		full := "#!/bin/sh\necho \"" + name + " $*\" >> '" + logFile + "'\n" + script + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(full), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("id", "echo 0")
	stub("uname", "echo "+machine)
	stub("curl", "")
	stub("apt-get", "")
	stub("systemctl", "")
	stub("sshd", "")
	stub("argus-agent", `[ "$1" = install ] && echo 'ForceCommand /usr/bin/argus-agent shim'; exit 0`)

	confPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(confPath, []byte(sshdConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, "CONF=/etc/ssh/sshd_config", "CONF='"+confPath+"'", 1)
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Only the stubs and what sh itself needs (grep, cp, mktemp, rm, printf).
	cmd := exec.Command("sh", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	l, _ := os.ReadFile(logFile)
	c, _ := os.ReadFile(confPath)
	return string(l), string(c)
}

// The script downloads the package by the name a release publishes it under.
// It asked for argus-agent_<v>_amd64.deb (and -1.x86_64.rpm, _x86_64.apk)
// while goreleaser names them argus-agent_<v>_linux_amd64.<format>, so every
// one-line install stopped at a 404 before the agent was installed.
func TestTheInstallScriptDownloadsTheReleasedPackage(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	_, body := installScript(t, h, goodToken, "1.4.0", "https://argus.example.com")
	for machine, arch := range map[string]string{"x86_64": "amd64", "aarch64": "arm64"} {
		log, _ := runInstallScript(t, body, machine, "PasswordAuthentication no\n")
		want := "curl -fsSLO https://github.com/gsoultan/argus/releases/download/v1.4.0/argus-agent_1.4.0_linux_" + arch + ".deb"
		if !strings.Contains(log, want) {
			t.Errorf("%s: the script did not fetch the released package\nwant %q\ngot:\n%s", machine, want, log)
		}
	}
	// And the rpm and apk names, which the script builds the same way.
	for _, want := range []string{
		`F="argus-agent_${VERSION}_linux_${ARCH}.rpm"`,
		`F="argus-agent_${VERSION}_linux_${ARCH}.apk"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
}

// ForceCommand appended after a Match block applies only to the logins that
// block matches, so every other login would skip the agent.
func TestTheInstallScriptHooksEveryLoginAfterAMatchBlock(t *testing.T) {
	_, _, h, _ := enrollAPI(t)
	_, body := installScript(t, h, goodToken, "1.4.0", "https://argus.example.com")

	_, conf := runInstallScript(t, body, "x86_64",
		"PasswordAuthentication no\nMatch User backup\n  AllowTcpForwarding no\n")
	m := strings.Index(conf, "\nMatch all\n")
	f := strings.Index(conf, "ForceCommand /usr/bin/argus-agent shim")
	if m < 0 || f < 0 || m > f {
		t.Fatalf("ForceCommand is not under Match all:\n%s", conf)
	}

	_, conf = runInstallScript(t, body, "x86_64", "PasswordAuthentication no\n#Match User anoncvs\n")
	if strings.Contains(conf, "Match all") {
		t.Errorf("Match all added to a config with no Match block:\n%s", conf)
	}
	if !strings.Contains(conf, "ForceCommand /usr/bin/argus-agent shim") {
		t.Errorf("ForceCommand not added:\n%s", conf)
	}
}
