package agent

import "testing"

// BenchmarkUsageSample is the cost of one usage sample on this machine: the
// heartbeat takes one a minute, and a live view one every two seconds.
// On a 4-processor VM with ~100 processes (2026-11-16) it went from 1.1 ms
// and 392 KB to 0.85 ms and 124 KB when the process scan stopped allocating
// per process.
func BenchmarkUsageSample(b *testing.B) {
	var u UsageSampler
	u.Sample()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		u.Sample()
	}
}
