package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func consoleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>console"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "index-abc.js"), []byte("js"), 0o644)
	return dir
}

func consoleGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// The console's Servers pages live under /assets, the same prefix vite writes
// its files to. Treating every /assets/ path as a file 404'd "Add a server" on
// the first packaged build.
func TestConsoleRoutesUnderAssetsAreTheApp(t *testing.T) {
	h, err := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/assets", "/assets/add", "/assets/5b1d6a0e-0000-4000-8000-000000000001", "/sessions"} {
		w := consoleGet(t, h, p)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "console") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	if w := consoleGet(t, h, "/assets/index-abc.js"); w.Body.String() != "js" ||
		!strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("a built asset: %q %q", w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if w := consoleGet(t, h, "/assets/index-gone.js"); w.Code != http.StatusNotFound {
		t.Errorf("a missing built asset was answered with %d", w.Code)
	}
}

// An API client asking for a route that does not exist must get an API
// answer, not the console's HTML with a 200.
func TestAnUnknownAPIPathIsNotTheConsole(t *testing.T) {
	a := NewAPI(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t)})
	a.Console = h
	srv := a.Handler()
	for _, p := range []string{"/api/v1/nope", "/auth/nope"} {
		w := consoleGet(t, srv, p)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "console") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	if w := consoleGet(t, srv, "/users"); !strings.Contains(w.Body.String(), "console") {
		t.Errorf("a console route through the full router: %d", w.Code)
	}
}

func TestConsoleConfigCarriesTheGatewayAndTheCSPAllowsIt(t *testing.T) {
	h, err := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t), GatewayURL: "https://bastion.example:8081"})
	if err != nil {
		t.Fatal(err)
	}
	w := consoleGet(t, h, "/console-config.js")
	if !strings.Contains(w.Body.String(), `"gatewayUrl":"https://bastion.example:8081"`) {
		t.Errorf("config: %s", w.Body)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "wss://bastion.example:8081") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP: %s", csp)
	}
}

func TestAConfiguredConsoleDirectoryMustHoldAConsole(t *testing.T) {
	if _, err := NewConsoleHandler(ConsoleConfig{Dir: t.TempDir()}); err == nil {
		t.Error("an empty directory was accepted as the console")
	}
	if _, err := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t), GatewayURL: "bastion:8081"}); err == nil {
		t.Error("a gateway address with no scheme was accepted")
	}
}

// Behind a Cloudflare Tunnel the console's hostname carries no agent traffic,
// so "Add a server" has to be told where servers enrol instead.
func TestConsoleConfigCarriesTheFleetAddress(t *testing.T) {
	h, err := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t), FleetURL: "https://argus-internal.example:8080"})
	if err != nil {
		t.Fatal(err)
	}
	w := consoleGet(t, h, "/console-config.js")
	if !strings.Contains(w.Body.String(), `"fleetUrl":"https://argus-internal.example:8080"`) {
		t.Errorf("config: %s", w.Body)
	}
	if _, err := NewConsoleHandler(ConsoleConfig{Dir: consoleDir(t), FleetURL: "http://argus:8080"}); err == nil {
		t.Error("a plain-HTTP fleet address was accepted; agents only enrol over HTTPS")
	}
}
