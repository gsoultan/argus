package control

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

/*
Compressing what the console downloads.

Nothing was compressed. The console's own JavaScript and CSS went out at full
size on every first load, and so did every JSON answer the console polls:
measured against a seeded control plane with 7,000 servers, GET /api/v1/assets
was 5.96 MB, fetched every 30 seconds by each open tab. Gzip at its fastest
level makes that 0.30 MB for 14 ms of CPU, and the console's entry bundle a
third of its size.

What is compressed, and what is not:

  - Only text formats that are worth it: JSON, HTML, CSS, JavaScript, SVG, the
    web manifest and WebAssembly. Never an event stream (it would sit in the
    compressor's buffer instead of reaching the browser), never a recording or
    a file download (already compressed, or large and served with ranges).
  - Only answers of minCompress bytes or more. A small answer is held until it
    is known to be small and then sent as it was.
  - Never a WebSocket handshake or a range request.
  - Never a request the browser says came from another site.

That last rule is BREACH. Compression leaks, through the length of an
encrypted answer, how much of it repeats -- so a page that can make the
browser ask many slightly different questions, and see how long each answer
is, can learn a secret in the answer a byte at a time. That needs the
attacker's requests to carry the victim's cookie. The session cookie is
SameSite=Lax, which already keeps it off cross-site subrequests, and this
keeps a cross-site top-level navigation from being compressed as well:
Sec-Fetch-Site is set by the browser and a page cannot forge it. A request
with no Sec-Fetch-Site is not from a modern browser and carries no ambient
cookie, so it may be compressed.
*/

// minCompress is the smallest answer worth compressing: below about one
// network packet gzip saves nothing a person could notice.
const minCompress = 1400

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

// compress gzips responses the browser can take, under the rules above.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mayCompress(r) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		// Not deferred: after a panic the answer must not be completed as if
		// the handler had finished it. recoverPanics writes the 500.
		cw.finish()
	})
}

// mayCompress is whether the request allows a compressed answer at all.
func mayCompress(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	return acceptsGzip(r.Header.Values("Accept-Encoding"))
}

// acceptsGzip reads Accept-Encoding for gzip with a quality above zero.
func acceptsGzip(values []string) bool {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, val, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
						q = f
					}
				}
			}
			return q > 0
		}
	}
	return false
}

// compressible is whether a Content-Type is one worth compressing.
func compressible(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch mt {
	case "application/json", "text/html", "text/css", "text/javascript",
		"application/javascript", "image/svg+xml", "application/manifest+json",
		"application/wasm", "text/plain":
		return true
	}
	return false
}

// compressWriter holds the start of an answer until it knows whether to
// compress it, then either compresses everything or passes everything on.
type compressWriter struct {
	http.ResponseWriter
	status    int
	headed    bool // the handler has set the status
	committed bool // the status has gone to the client
	eligible  bool
	buf       []byte
	gz        *gzip.Writer
}

func (c *compressWriter) WriteHeader(code int) {
	if code < 200 {
		// 1xx is informational and is not the answer.
		c.ResponseWriter.WriteHeader(code)
		return
	}
	if c.headed {
		return
	}
	c.headed, c.status = true, code
	h := c.Header()
	c.eligible = code != http.StatusNoContent && code != http.StatusNotModified &&
		h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type"))
	if !c.eligible {
		c.commit(false)
	}
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.headed {
		c.WriteHeader(http.StatusOK)
	}
	if c.committed {
		if c.gz != nil {
			return c.gz.Write(p)
		}
		return c.ResponseWriter.Write(p)
	}
	c.buf = append(c.buf, p...)
	if len(c.buf) >= minCompress {
		if err := c.commit(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush sends what is held. A handler that flushes wants the client to see
// it now, so a held answer is decided on what has arrived so far.
func (c *compressWriter) Flush() {
	if c.headed && !c.committed {
		_ = c.commit(c.eligible && len(c.buf) > 0)
	}
	if c.gz != nil {
		_ = c.gz.Flush()
	}
	_ = http.NewResponseController(c.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the connection underneath.
func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// commit sends the status, compressed or not, and whatever is held.
func (c *compressWriter) commit(gz bool) error {
	c.committed = true
	h := c.Header()
	if c.eligible {
		h.Add("Vary", "Accept-Encoding")
	}
	if gz {
		h.Del("Content-Length")
		h.Del("Accept-Ranges")
		h.Set("Content-Encoding", "gzip")
		c.gz = gzipPool.Get().(*gzip.Writer)
		c.gz.Reset(c.ResponseWriter)
	}
	c.ResponseWriter.WriteHeader(c.status)
	held := c.buf
	c.buf = nil
	if len(held) == 0 {
		return nil
	}
	var err error
	if c.gz != nil {
		_, err = c.gz.Write(held)
	} else {
		_, err = c.ResponseWriter.Write(held)
	}
	return err
}

// finish completes the answer once the handler has returned.
func (c *compressWriter) finish() {
	if !c.headed {
		return
	}
	if !c.committed {
		_ = c.commit(false)
	}
	if c.gz != nil {
		_ = c.gz.Close()
		c.gz.Reset(nil)
		gzipPool.Put(c.gz)
		c.gz = nil
	}
}
