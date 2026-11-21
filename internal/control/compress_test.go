package control

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bigJSON() string { return `{"rows":"` + strings.Repeat("server-01.prod.example.com ", 400) + `"}` }

func serve(t *testing.T, h http.Handler, req *http.Request) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func gzipGet(path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func jsonHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
}

func TestALargeJSONAnswerIsGzipped(t *testing.T) {
	body := bigJSON()
	res := serve(t, compress(jsonHandler(body)), gzipGet("/api/v1/assets", map[string]string{"Sec-Fetch-Site": "same-origin"}))
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", res.Header.Get("Content-Encoding"))
	}
	if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", res.Header.Get("Vary"))
	}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) >= len(body)/4 {
		t.Fatalf("compressed to %d of %d bytes", len(raw), len(body))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatal("decompressed body differs from what the handler wrote")
	}
}

// BREACH needs the attacker's requests to be compressed; a request the
// browser says another site made never is.
func TestACrossSiteRequestIsNotCompressed(t *testing.T) {
	for _, site := range []string{"cross-site", "same-site"} {
		res := serve(t, compress(jsonHandler(bigJSON())), gzipGet("/api/v1/assets", map[string]string{"Sec-Fetch-Site": site}))
		if res.Header.Get("Content-Encoding") != "" {
			t.Fatalf("Sec-Fetch-Site %s: compressed", site)
		}
		if b, _ := io.ReadAll(res.Body); string(b) != bigJSON() {
			t.Fatalf("Sec-Fetch-Site %s: body changed", site)
		}
	}
}

func TestWhatShouldNotBeCompressedIsNot(t *testing.T) {
	cases := map[string]struct {
		h   http.Handler
		req *http.Request
	}{
		"small answer": {jsonHandler(`{"ok":true}`), gzipGet("/x", nil)},
		"no gzip accepted": {jsonHandler(bigJSON()), func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.Header.Set("Accept-Encoding", "br, gzip;q=0")
			return r
		}()},
		"range request": {jsonHandler(bigJSON()), gzipGet("/x", map[string]string{"Range": "bytes=0-10"})},
		"websocket":     {jsonHandler(bigJSON()), gzipGet("/x", map[string]string{"Upgrade": "websocket"})},
		"post": {jsonHandler(bigJSON()), func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/x", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			return r
		}()},
		"recording": {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, bigJSON())
		}), gzipGet("/x", nil)},
		"already encoded": {http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
			_, _ = io.WriteString(w, bigJSON())
		}), gzipGet("/x", nil)},
	}
	for name, c := range cases {
		res := serve(t, compress(c.h), c.req)
		if ce := res.Header.Get("Content-Encoding"); ce == "gzip" {
			t.Errorf("%s: compressed", name)
		}
	}
}

// An event stream has to reach the browser as each event is written, so it
// is never held in a compressor.
func TestAnEventStreamFlushesUncompressed(t *testing.T) {
	srv := httptest.NewServer(compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done() // held open; the client reads the first event
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := srv.Client().Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.CloseClientConnections()
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("event stream was compressed")
	}
	buf := make([]byte, len("data: one\n\n"))
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "data: one\n\n" {
		t.Fatalf("first event did not arrive: %q %v", buf, err)
	}
}

// A JSON answer that is flushed part way still decompresses whole.
func TestAFlushedJSONAnswerIsComplete(t *testing.T) {
	body := bigJSON()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body[:100])
		http.NewResponseController(w).Flush()
		_, _ = io.WriteString(w, body[100:])
	})
	res := serve(t, compress(h), gzipGet("/x", nil))
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("not compressed")
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatal("body differs")
	}
}

// The console's built files, through the real handler: compressed, without
// the uncompressed length, and still served whole to a client without gzip.
func TestTheConsoleBundleIsCompressed(t *testing.T) {
	dir := t.TempDir()
	js := "export const x = '" + strings.Repeat("console ", 2000) + "'\n"
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>Argus</title>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "index-abc.js"), []byte(js), 0o644)
	console, err := NewConsoleHandler(ConsoleConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h := compress(console)

	res := serve(t, h, gzipGet("/assets/index-abc.js", nil))
	if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Content-Length") != "" {
		t.Fatalf("encoding %q, length %q", res.Header.Get("Content-Encoding"), res.Header.Get("Content-Length"))
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != js {
		t.Fatal("bundle differs after decompression")
	}

	plain := httptest.NewRequest(http.MethodGet, "/assets/index-abc.js", nil)
	res = serve(t, h, plain)
	if got, _ := io.ReadAll(res.Body); res.Header.Get("Content-Encoding") != "" || string(got) != js {
		t.Fatal("a client without gzip did not get the file as it is")
	}
}

func TestAcceptsGzip(t *testing.T) {
	for in, want := range map[string]bool{
		"gzip": true, "gzip, br": true, "br;q=1, GZIP;q=0.5": true,
		"gzip;q=0": false, "br": false, "": false, "identity": false,
	} {
		if got := acceptsGzip([]string{in}); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", in, got, want)
		}
	}
}
