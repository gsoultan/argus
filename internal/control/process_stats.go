package control

import (
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"time"
)

/*
What this process is doing, in numbers.

Distinct from /api/v1/stats, which reports the fleet: assets, sessions, pending
requests -- the things an operator looks at. This reports the program itself, so
"no leaks" is something an operator can check rather than something a README
asserts.

Counts only, and loopback only, for the same reason as the gateway's: this
process holds audit data and session metadata, and net/http/pprof would hand a
heap dump to anyone who could reach it. See internal/gateway/stats.go.
*/

// ProcessStats is the control plane's self-report.
type ProcessStats struct {
	UptimeSeconds  int64  `json:"uptimeSeconds"`
	Goroutines     int    `json:"goroutines"`
	HeapInUseBytes uint64 `json:"heapInUseBytes"`
	HeapObjects    uint64 `json:"heapObjects"`
	StackInUse     uint64 `json:"stackInUseBytes"`
	NumGC          uint32 `json:"numGC"`

	// DBConns is the pool's current size. A handler that returns without
	// releasing a connection shows up here long before it shows up as an
	// outage, and it is invisible in every other number on this struct.
	DBConns int32 `json:"dbConns"`
	DBIdle  int32 `json:"dbIdleConns"`
}

func (a *API) processStats() ProcessStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	st := ProcessStats{
		UptimeSeconds:  int64(time.Since(a.startedAt).Seconds()),
		Goroutines:     runtime.NumGoroutine(),
		HeapInUseBytes: m.HeapInuse,
		HeapObjects:    m.HeapObjects,
		StackInUse:     m.StackInuse,
		NumGC:          m.NumGC,
	}
	if a.store != nil && a.store.pool != nil {
		s := a.store.pool.Stat()
		st.DBConns = s.TotalConns()
		st.DBIdle = s.IdleConns()
	}
	return st
}

// handleProcessStats serves the numbers to a loopback caller.
func (a *API) handleProcessStats(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackAddr(r.RemoteAddr) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(a.processStats())
}

// isLoopbackAddr reports whether addr is a local caller. A parse failure is not
// loopback: guessing permissively would turn an unfamiliar format into an open
// endpoint.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
