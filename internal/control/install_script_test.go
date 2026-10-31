package control

import (
	"net/http"
	"net/url"
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
