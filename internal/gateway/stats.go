package gateway

import (
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"time"
)

/*
What this process is doing, in numbers.

A gateway that leaks a goroutine per session looks perfectly healthy for a week
and then does not. The failure is invisible from outside and unmeasurable from
inside, because until now nothing here reported a single number about itself --
"low memory, no leaks" was a claim with no way for an operator to check it.

Deliberately counts only. This process holds session plaintext: every keystroke
and every byte of output for every live session. net/http/pprof would expose
`/debug/pprof/heap`, and a heap dump of this program is a transcript of
everyone's privileged session, including anything they typed that was a
password. That is not a debugging endpoint, it is an exfiltration endpoint, and
it is why this file implements a handful of integers by hand rather than
mounting the standard one.
*/

// Stats is the gateway's self-report.
type Stats struct {
	// UptimeSeconds rather than a start timestamp: the question is almost
	// always "how long has this been up", and a clock skew between hosts makes
	// the timestamp form quietly wrong.
	UptimeSeconds int64 `json:"uptimeSeconds"`

	// Goroutines is the number that matters most. A relay that does not tear
	// down leaves two per session, so this rising while Sessions does not is
	// the leak, stated directly.
	Goroutines int `json:"goroutines"`
	Sessions   int `json:"sessions"`

	// HeapInUseBytes is live heap, not the process RSS. Go returns memory to
	// the OS lazily, so RSS lags and reads as a leak that is not there.
	HeapInUseBytes uint64 `json:"heapInUseBytes"`
	// HeapObjects moves with allocation shape rather than volume, which
	// separates "busy" from "accumulating".
	HeapObjects uint64 `json:"heapObjects"`
	StackInUse  uint64 `json:"stackInUseBytes"`
	NumGC       uint32 `json:"numGC"`
}

// stats gathers the current numbers.
func (s *Server) stats() Stats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	s.mu.Lock()
	sessions := len(s.sessions)
	s.mu.Unlock()

	return Stats{
		UptimeSeconds:  int64(time.Since(s.startedAt).Seconds()),
		Goroutines:     runtime.NumGoroutine(),
		Sessions:       sessions,
		HeapInUseBytes: m.HeapInuse,
		HeapObjects:    m.HeapObjects,
		StackInUse:     m.StackInuse,
		NumGC:          m.NumGC,
	}
}

// handleStats serves the numbers to a loopback caller.
//
// Loopback only, and not configurable. These numbers are harmless on their own,
// but session and goroutine counts tell an attacker when the gateway is busy
// and when it is not, which is exactly the reconnaissance worth denying. An
// operator who wants them off-host has a monitoring agent on the host already.
func (s *Server) handleStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.stats())
	}
}

// isLoopback reports whether addr is a local caller.
//
// A parse failure is not loopback. Guessing in the permissive direction here
// would turn an unfamiliar address format into an open endpoint.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
