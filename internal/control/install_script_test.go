package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

/*
The one-line install's script runs as root on a server, so what it writes into
itself is checked before it is written, and its order is the one that keeps a
server reachable.
*/

// installAPI serves the script with no database: it reads none.
func installAPI() *API {
	return &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func installScript(t *testing.T, a *API, token, version, control string) (int, string) {
	t.Helper()
	q := url.Values{"token": {token}, "version": {version}, "url": {control}}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/install/agent.sh?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	a.getInstallScript(w, r)
	return w.Code, w.Body.String()
}

const goodToken = "argus-enrol-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func TestTheInstallScriptCarriesTheTokenAndAddress(t *testing.T) {
	a := installAPI()
	a.Enroll.Pin = func() (string, error) { return "sha256:" + strings.Repeat("ab", 32), nil }

	code, body := installScript(t, a, goodToken, "1.4.0", "https://argus.example.com:8443")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{
		"TOKEN='" + goodToken + "'",
		"CONTROL='https://argus.example.com:8443'",
		"PIN='sha256:" + strings.Repeat("ab", 32) + "'",
		`RELEASES="https://github.com/gsoultan/argus/releases"`,
		"VERSION='1.4.0'",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
}

// Anything that is not exactly the shape expected is refused, never quoted
// into a script that runs as root.
func TestTheInstallScriptRefusesWhatAShellWouldRead(t *testing.T) {
	a := installAPI()
	cases := map[string][3]string{
		"a token with a quote":    {goodToken + "'; rm -rf / #", "1.4.0", "https://argus.example.com"},
		"not an enrollment token": {"argus-agent-abcdefghijklmnopqrstuvwxyz0123", "1.4.0", "https://argus.example.com"},
		"a version with a space":  {goodToken, "1.4.0 && id", "https://argus.example.com"},
		"plain http":              {goodToken, "1.4.0", "http://argus.example.com"},
		"an address with a path":  {goodToken, "1.4.0", "https://argus.example.com/$(id)"},
		"an address with a quote": {goodToken, "1.4.0", "https://argus.example.com'"},
	}
	for name, c := range cases {
		if code, body := installScript(t, a, c[0], c[1], c[2]); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %q", name, code, body)
		}
	}
}

// The order that keeps a server reachable: the agent runs before sshd is
// told to send every login through it, and sshd_config is restored if sshd
// rejects the change.
func TestTheInstallScriptStartsTheAgentBeforeHookingSSHD(t *testing.T) {
	a := installAPI()
	_, body := installScript(t, a, goodToken, "1.4.0", "https://argus.example.com")
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

// stubCurl stands in for curl in the script's tests. It saves an empty file
// for each package it is asked for, and a checksums.txt listing what it
// saved; asked for the release list it fails, as a rate-limited GitHub does,
// and asked where /releases/latest leads it answers with the tag v2.0.0.
const stubCurl = `case "$*" in
  *api.github.com*) exit 22 ;;
  *url_effective*) echo https://github.com/gsoultan/argus/releases/tag/v2.0.0 ;;
  *checksums.txt) for f in argus-agent_*; do [ -f "$f" ] && sha256sum "$f"; done > checksums.txt ;;
  *-fsLO*) for u; do :; done; : > "${u##*/}" ;;
esac`

// runInstallScript runs the script with every command it calls replaced by a
// stub that logs its arguments, against a copy of sshd_config, and returns the
// log and what sshd_config became. uname -m reports machine.
func runInstallScript(t *testing.T, body, machine, sshdConfig string) (log, conf string) {
	t.Helper()
	log, conf, out, err := execInstallScript(t, body, machine, sshdConfig, stubCurl)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	return log, conf
}

// execInstallScript is runInstallScript with curl's stub given, and the
// script's failure returned rather than failing the test.
func execInstallScript(t *testing.T, body, machine, sshdConfig, curl string) (log, conf, out string, err error) {
	t.Helper()
	return execInstallScriptAgent(t, body, machine, sshdConfig, curl, stubAgent)
}

// stubAgent stands in for argus-agent: every enroll works, and install prints
// the ForceCommand line.
const stubAgent = `[ "$1" = install ] && echo 'ForceCommand /usr/bin/argus-agent shim'; exit 0`

// execInstallScriptAgent is execInstallScript with argus-agent's stub given.
func execInstallScriptAgent(t *testing.T, body, machine, sshdConfig, curl, agent string) (log, conf, out string, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script is for Linux; its stubs are POSIX sh")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh to run the script with")
	}
	sha, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Skip("no sha256sum to make the stub release's checksums with")
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
	stub("curl", curl)
	stub("apt-get", "")
	stub("systemctl", "")
	stub("sshd", "")
	stub("argus-agent", agent)

	confPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(confPath, []byte(sshdConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, "CONF=/etc/ssh/sshd_config", "CONF='"+confPath+"'", 1)
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// Only the stubs and what sh itself needs (grep, sed, sort, cp, mktemp,
	// rm, printf, sha256sum).
	cmd := exec.Command("sh", script)
	// sha256sum is in /sbin on macOS.
	cmd.Env = []string{"PATH=" + bin + ":" + filepath.Dir(sha) + ":/usr/bin:/bin"}
	o, err := cmd.CombinedOutput()
	l, _ := os.ReadFile(logFile)
	c, _ := os.ReadFile(confPath)
	return string(l), string(c), string(o), err
}

// The script downloads the package by the name a release publishes it under.
// It asked for argus-agent_<v>_amd64.deb (and -1.x86_64.rpm, _x86_64.apk)
// while goreleaser names them argus-agent_<v>_linux_amd64.<format>, so every
// one-line install stopped at a 404 before the agent was installed.
func TestTheInstallScriptDownloadsTheReleasedPackage(t *testing.T) {
	a := installAPI()
	_, body := installScript(t, a, goodToken, "1.4.0", "https://argus.example.com")
	for machine, arch := range map[string]string{"x86_64": "amd64", "aarch64": "arm64"} {
		log, _ := runInstallScript(t, body, machine, "PasswordAuthentication no\n")
		want := "curl -fsLO https://github.com/gsoultan/argus/releases/download/v1.4.0/argus-agent_1.4.0_linux_" + arch + ".deb"
		if !strings.Contains(log, want) {
			t.Errorf("%s: the script did not fetch the released package\nwant %q\ngot:\n%s", machine, want, log)
		}
	}
	// And the rpm and apk names, which the script builds the same way.
	for _, want := range []string{
		`F="argus-agent_${VERSION}_linux_${ARCH}.${FMT}"`,
		`FMT=rpm`,
		`FMT=apk`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
}

// ForceCommand appended after a Match block applies only to the logins that
// block matches, so every other login would skip the agent.
func TestTheInstallScriptHooksEveryLoginAfterAMatchBlock(t *testing.T) {
	a := installAPI()
	_, body := installScript(t, a, goodToken, "1.4.0", "https://argus.example.com")

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

// A control plane built from source has no release of its own and asks for
// the newest one. It used to ask for 0.1.0, which has no packages, so the
// install stopped at a 404.
func TestTheInstallScriptFindsTheNewestRelease(t *testing.T) {
	a := installAPI()
	code, body := installScript(t, a, goodToken, "latest", "https://argus.example.com")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	log, _ := runInstallScript(t, body, "x86_64", "PasswordAuthentication no\n")
	want := "curl -fsLO https://github.com/gsoultan/argus/releases/download/v2.0.0/argus-agent_2.0.0_linux_amd64.deb"
	if !strings.Contains(log, want) {
		t.Errorf("the script did not fetch the newest release\nwant %q\ngot:\n%s", want, log)
	}
}

// pin=off: the console found a public certificate in front (a Cloudflare
// Tunnel), so the agent must enrol with ordinary verification. A pin of this
// process's key would make it refuse the proxy's.
func TestPinOffLeavesThePinOutOfTheScript(t *testing.T) {
	a := pinAPI("", nil)
	run := func(extra string) string {
		q := url.Values{"token": {goodToken}, "version": {"1.4.0"}, "url": {"https://argus.example.com"}}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/install/agent.sh?"+q.Encode()+extra, nil)
		w := httptest.NewRecorder()
		a.getInstallScript(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if body := run(""); !strings.Contains(body, "PIN='"+ownPin+"'") {
		t.Error("without pin=off the script lost its pin")
	}
	if body := run("&pin=off"); !strings.Contains(body, "PIN=''") {
		t.Error("pin=off still wrote a pin into the script")
	}
}

// releasesJSON is a release list the way GitHub's API returns it: 1.2.0 was
// tagged without a "v" and published with no files, 1.3.0 also has no "v"
// but has its files, v1.9.0 and v1.10.0 have arm64 packages only, and the
// pre-release's packages carry a "-" in the version.
const releasesJSON = `[
 {"tag_name":"v1.10.0-rc.1","prerelease":true,"assets":[
  {"name":"argus-agent_1.10.0-rc.1_linux_amd64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/v1.10.0-rc.1/argus-agent_1.10.0-rc.1_linux_amd64.deb"}]},
 {"tag_name":"v1.10.0","assets":[
  {"name":"argus-agent_1.10.0_linux_arm64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/v1.10.0/argus-agent_1.10.0_linux_arm64.deb"}]},
 {"tag_name":"v1.9.0","assets":[
  {"name":"argus-agent_1.9.0_linux_arm64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/v1.9.0/argus-agent_1.9.0_linux_arm64.deb"}]},
 {"tag_name":"1.3.0","assets":[
  {"name":"argus-agent_1.3.0_linux_amd64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/1.3.0/argus-agent_1.3.0_linux_amd64.deb"},
  {"name":"argus-agent_1.3.0_linux_amd64.rpm","browser_download_url":"https://github.com/gsoultan/argus/releases/download/1.3.0/argus-agent_1.3.0_linux_amd64.rpm"}]},
 {"tag_name":"1.2.0","assets":[]},
 {"tag_name":"v1.2.1","assets":[
  {"name":"argus-agent_1.2.1_linux_amd64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/v1.2.1/argus-agent_1.2.1_linux_amd64.deb"},
  {"name":"argus-agent_1.2.1_linux_arm64.deb","browser_download_url":"https://github.com/gsoultan/argus/releases/download/v1.2.1/argus-agent_1.2.1_linux_arm64.deb"}]}
]`

// curlWithReleases answers the release list with releasesJSON, and fails
// every download whose URL contains one of missing.
func curlWithReleases(t *testing.T, missing ...string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "releases.json")
	if err := os.WriteFile(f, []byte(releasesJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := ""
	for _, m := range missing {
		fail += `case "$*" in *` + m + `*) exit 22 ;; esac` + "\n"
	}
	return fail + strings.Replace(stubCurl, `*api.github.com*) exit 22 ;;`,
		`*api.github.com*) cat '`+f+`' ;;`, 1)
}

// With no version known, the script installs the highest release that has
// this server's package, whatever its tag looks like, and passes over a
// release with nothing attached and a pre-release.
func TestTheInstallScriptPicksTheNewestReleaseWithAPackage(t *testing.T) {
	_, body := installScript(t, installAPI(), goodToken, "latest", "https://argus.example.com")
	for machine, want := range map[string]string{
		// 1.3.0 is tagged without a "v": its URL is the release's own.
		"x86_64": "download/1.3.0/argus-agent_1.3.0_linux_amd64.deb",
		// 1.10.0 is above 1.9.0, which a plain text sort gets wrong.
		"aarch64": "download/v1.10.0/argus-agent_1.10.0_linux_arm64.deb",
	} {
		log, _, out, err := execInstallScript(t, body, machine, "PasswordAuthentication no\n", curlWithReleases(t))
		if err != nil {
			t.Fatalf("%s: script failed: %v\n%s", machine, err, out)
		}
		if !strings.Contains(log, "curl -fsLO https://github.com/gsoultan/argus/releases/"+want) {
			t.Errorf("%s: want a download of %s, got:\n%s", machine, want, log)
		}
		if strings.Contains(log, "rc.1") || strings.Contains(log, "/1.2.0/") {
			t.Errorf("%s: downloaded a pre-release or the empty release:\n%s", machine, log)
		}
	}
}

// When GitHub's list cannot be had, /releases/latest names the tag, with or
// without its "v".
func TestTheInstallScriptReadsATagWithoutAV(t *testing.T) {
	_, body := installScript(t, installAPI(), goodToken, "latest", "https://argus.example.com")
	curl := strings.Replace(stubCurl, "tag/v2.0.0", "tag/1.2.0", 1)
	log, _, out, err := execInstallScript(t, body, "x86_64", "PasswordAuthentication no\n", curl)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	want := "curl -fsLO https://github.com/gsoultan/argus/releases/download/1.2.0/argus-agent_1.2.0_linux_amd64.deb"
	if !strings.Contains(log, want) {
		t.Errorf("want %q, got:\n%s", want, log)
	}
}

// A version asked for by name is looked for under v<version>, then under the
// bare version.
func TestTheInstallScriptTriesTheTagWithoutAV(t *testing.T) {
	_, body := installScript(t, installAPI(), goodToken, "1.4.0", "https://argus.example.com")
	log, _, out, err := execInstallScript(t, body, "x86_64", "PasswordAuthentication no\n", curlWithReleases(t, "/v1.4.0/"))
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "download/1.4.0/argus-agent_1.4.0_linux_amd64.deb") ||
		!strings.Contains(log, "apt-get install -y ./argus-agent_1.4.0_linux_amd64.deb") {
		t.Errorf("1.4.0 was not installed from its bare tag:\n%s", log)
	}
}

// A release with no files (1.2.0 was published that way) does not stop the
// install: the newest release that has the package is installed instead.
func TestTheInstallScriptPassesOverAReleaseWithNoFiles(t *testing.T) {
	_, body := installScript(t, installAPI(), goodToken, "1.2.0", "https://argus.example.com")
	log, _, out, err := execInstallScript(t, body, "x86_64", "PasswordAuthentication no\n", curlWithReleases(t, "1.2.0/"))
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "apt-get install -y ./argus-agent_1.3.0_linux_amd64.deb") {
		t.Errorf("the newest release with a package was not installed:\n%s", log)
	}
}

// The script runs as root: a package that does not match the release's
// checksums is not installed.
func TestTheInstallScriptRefusesAPackageThatDoesNotMatch(t *testing.T) {
	_, body := installScript(t, installAPI(), goodToken, "1.4.0", "https://argus.example.com")
	curl := strings.Replace(stubCurl, `: > "${u##*/}"`, `echo tampered > "${u##*/}"`, 1)
	curl = strings.Replace(curl, `sha256sum "$f"`, `printf '%064d  %s\n' 0 "$f"`, 1)
	log, _, out, err := execInstallScript(t, body, "x86_64", "PasswordAuthentication no\n", curl)
	if err == nil {
		t.Fatalf("the script installed a package that does not match its checksum:\n%s", log)
	}
	if strings.Contains(log, "apt-get") || !strings.Contains(out, "does not match") {
		t.Errorf("want a stop before apt-get, naming the mismatch; log:\n%s\nout:\n%s", log, out)
	}
}

// A new server gets the newest agent release, not the version the console
// asked for: the console asks for its own, which is old on a control plane
// that has not been upgraded.
func TestTheInstallScriptInstallsTheNewestKnownRelease(t *testing.T) {
	a := installAPI()
	a.Releases = &ReleaseWatcher{latest: &AgentRelease{Version: "2.1.0"}}
	for asked, want := range map[string]string{
		"1.4.0":  "2.1.0",
		"v1.4.0": "2.1.0",
		"latest": "2.1.0",
		"3.0.0":  "3.0.0", // newer than any release found: kept
	} {
		code, body := installScript(t, a, goodToken, asked, "https://argus.example.com")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", asked, code, body)
		}
		if !strings.Contains(body, "VERSION='"+want+"'") {
			t.Errorf("asked for %s: want VERSION='%s' in the script", asked, want)
		}
	}
	if _, body := installScript(t, installAPI(), goodToken, "latest", "https://argus.example.com"); !strings.Contains(body, "VERSION='latest'") {
		t.Error("with no release known, latest should be left for the script to find")
	}
}

// Behind a Cloudflare Tunnel with split DNS the control plane meets itself at
// its own address and puts its pin in the command, while a server outside its
// network meets Cloudflare there. The pinned enroll fails on that server, and
// the install used to stop with "public key does not match pinned public key".
// A failed handshake spends no token, so the script enrols again with the
// address's public certificate.
func TestTheInstallScriptEnrolsWithoutThePinWhenThePinFails(t *testing.T) {
	a := pinAPI("", nil)
	_, body := installScript(t, a, goodToken, "1.4.0", "https://argus.example.com")
	if !strings.Contains(body, "PIN='sha256:") {
		t.Fatalf("the script carries no pin to fall back from:\n%s", body)
	}
	pinRefused := `case "$*" in *--pin*) exit 1 ;; esac
` + stubAgent
	log, _, out, err := execInstallScriptAgent(t, body, "x86_64", "PasswordAuthentication no\n", stubCurl, pinRefused)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	unpinned := "argus-agent enroll --control-url https://argus.example.com --token " + goodToken
	if !strings.Contains(log, "--pin sha256:") || !strings.Contains(log, unpinned+"\n") {
		t.Fatalf("did not try the pin and then the public certificate:\n%s", log)
	}
	if !strings.Contains(log, "systemctl enable --now argus-agent") {
		t.Errorf("the agent was not started after enrolling:\n%s", log)
	}

	// A pin that works is used alone.
	log, _ = runInstallScript(t, body, "x86_64", "PasswordAuthentication no\n")
	if strings.Count(log, "argus-agent enroll") != 1 {
		t.Errorf("enrolled more than once with a pin that worked:\n%s", log)
	}
}
