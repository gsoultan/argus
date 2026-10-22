package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

/*
Serving the console.

No release shipped the console. The packages carried the gateway, the control
plane and the agent; web/ was built only by developers, so a packaged install
had an API, a fleet reporting to it, and nothing a person could sign in to.

The control plane now serves the built console itself, from a directory the
package installs. Same-origin is the supported shape anyway -- the session
cookie depends on it -- so serving it from the process that sets the cookie
removes a reverse proxy nobody had documented rather than adding a moving part.
*/

// ConsoleConfig points the control plane at a built console.
type ConsoleConfig struct {
	// Dir holds index.html and assets/, as `vite build` writes them.
	Dir string
	// GatewayURL is where browsers reach argus-gateway's web listener. The
	// build bakes in a development default; this replaces it at run time.
	GatewayURL string
	// Version is this control plane's, so "Add a server" can offer the agent
	// package that matches it rather than whatever was current at build time.
	Version string
	// FleetURL is where servers reach this control plane, when that is not
	// where browsers do. Behind a Cloudflare Tunnel the console's address
	// carries no agent traffic, and "Add a server" would otherwise offer an
	// enrollment command pointed at it.
	FleetURL string
}

// consoleHandler serves the console's files, with the SPA's fallback to
// index.html for routes the router owns.
type consoleHandler struct {
	dir    string
	config []byte
	csp    string
}

// NewConsoleHandler checks the directory and builds the handler. A configured
// directory that holds no console is an error rather than a silent 404: an
// operator who set it expects a console there.
func NewConsoleHandler(cfg ConsoleConfig) (http.Handler, error) {
	if _, err := os.Stat(filepath.Join(cfg.Dir, "index.html")); err != nil {
		return nil, fmt.Errorf("console.dir %s holds no built console (index.html): %w", cfg.Dir, err)
	}
	connect := []string{"'self'"}
	if cfg.GatewayURL != "" {
		u, err := url.Parse(cfg.GatewayURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("console.gateway_url %q is not an http(s) URL", cfg.GatewayURL)
		}
		ws := "wss"
		if u.Scheme == "http" {
			ws = "ws"
		}
		// The terminal, the shadow stream and termination go straight to the
		// gateway, over fetch and WebSocket both.
		connect = append(connect, u.Scheme+"://"+u.Host, ws+"://"+u.Host)
	}
	if cfg.FleetURL != "" {
		if u, err := url.Parse(cfg.FleetURL); err != nil || u.Host == "" || u.Scheme != "https" {
			return nil, fmt.Errorf("console.fleet_url %q is not an https URL", cfg.FleetURL)
		}
	}
	cfgJSON, _ := json.Marshal(map[string]string{
		"gatewayUrl": cfg.GatewayURL, "version": cfg.Version, "fleetUrl": cfg.FleetURL,
	})
	return &consoleHandler{
		dir:    cfg.Dir,
		config: []byte("window.__ARGUS__ = " + string(cfgJSON) + ";\n"),
		// Tighter than "whatever the bundle happens to need" is not possible
		// without knowing the bundle; this is what it needs. Mantine sets
		// inline styles, vite inlines the smallest font subsets as data:
		// URLs, the replay decoder is WebAssembly, the recording
		// verifier runs in workers, and nothing loads from anywhere else --
		// the console has to render identically air-gapped.
		csp: "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; " +
			"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; " +
			"connect-src " + strings.Join(connect, " ") + "; worker-src 'self' blob:; " +
			"manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
	}, nil
}

func (h *consoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p := path.Clean("/" + r.URL.Path)
	// An API path that matched no route is an API 404, not the console's
	// index page: a client asking for JSON must not be handed HTML with a 200.
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/auth/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Security-Policy", h.csp)

	if p == "/console-config.js" {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(h.config)
		return
	}

	file := filepath.Join(h.dir, filepath.FromSlash(p))
	info, err := os.Stat(file)
	if err != nil || info.IsDir() {
		if errors.Is(err, os.ErrNotExist) && isBuiltAsset(p) {
			// A missing hashed asset is a stale tab after an upgrade. Serving
			// index.html as JavaScript would fail in a far stranger way.
			http.NotFound(w, r)
			return
		}
		// Everything else is a route the console's router owns.
		file = filepath.Join(h.dir, "index.html")
		p = "/index.html"
	}
	switch {
	case isBuiltAsset(p):
		// Content-hashed: a name never changes meaning.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		// index.html, sw.js, the manifest: an upgrade must reach the browser.
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, file)
}

// isBuiltAsset is a file vite wrote under assets/. Not every /assets/ path:
// the console's own Servers pages live at /assets, /assets/add and
// /assets/<id>, and those are routes, not files.
func isBuiltAsset(p string) bool {
	return strings.HasPrefix(p, "/assets/") && path.Ext(p) != ""
}
