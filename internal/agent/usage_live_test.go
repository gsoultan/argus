package agent

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// TestUsageLiveSamplesUntilClosed: samples arrive at the pace asked for, on
// a sampler of the stream's own, and sampling stops when the control plane
// closes the stream.
func TestUsageLiveSamplesUntilClosed(t *testing.T) {
	var taken atomic.Int32
	var seen = map[*UsageSampler]bool{}
	svc := UsageLive(UsageLiveConfig{Sample: func(s *UsageSampler) (Usage, bool) {
		seen[s] = true
		n := taken.Add(1)
		v := float64(n)
		return Usage{At: time.Now(), CPUBusy: &v}, true
	}})
	ctrl, agentEnd := net.Pipe()
	call := &Call{Header: tunnel.Header{Service: UsageLiveService, UsageEveryMS: 1000}, Stream: agentEnd}
	done := make(chan struct{})
	go func() { svc(context.Background(), call); close(done) }()

	var a tunnel.Answer
	if err := tunnel.ReadJSON(ctrl, &a); err != nil || !a.OK {
		t.Fatalf("answer %+v, %v", a, err)
	}
	fr := tunnel.NewFrameReader(ctrl)
	start := time.Now()
	for i := 0; i < 2; i++ {
		typ, b, err := fr.Next()
		if err != nil || typ != tunnel.FrameUsage {
			t.Fatalf("frame %q, %v", typ, err)
		}
		var u Usage
		if json.Unmarshal(b, &u) != nil || u.CPUBusy == nil {
			t.Fatalf("bad sample %s", b)
		}
	}
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Fatalf("two samples in %v: faster than the pace asked for", el)
	}
	_ = ctrl.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the service went on after the stream closed")
	}
	after := taken.Load()
	time.Sleep(1200 * time.Millisecond)
	if taken.Load() != after {
		t.Fatal("still sampling after the stream closed")
	}
	if len(seen) != 1 {
		t.Fatalf("%d samplers, want the stream's own one", len(seen))
	}
}

// TestUsageLiveBounded: past maxUsageLive streams at once, a further one is
// refused rather than sampled.
func TestUsageLiveBounded(t *testing.T) {
	svc := UsageLive(UsageLiveConfig{Sample: func(*UsageSampler) (Usage, bool) { return Usage{}, true }})
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxUsageLive; i++ {
		ctrl, ag := net.Pipe()
		conns = append(conns, ctrl)
		go svc(context.Background(), &Call{Header: tunnel.Header{Service: UsageLiveService}, Stream: ag})
		var a tunnel.Answer
		if err := tunnel.ReadJSON(ctrl, &a); err != nil || !a.OK {
			t.Fatalf("stream %d: %+v %v", i, a, err)
		}
	}
	ctrl, ag := net.Pipe()
	conns = append(conns, ctrl)
	go svc(context.Background(), &Call{Header: tunnel.Header{Service: UsageLiveService}, Stream: ag})
	var a tunnel.Answer
	if err := tunnel.ReadJSON(ctrl, &a); err != nil || a.OK {
		t.Fatalf("one past the bound was accepted: %+v %v", a, err)
	}
}

func TestUsagePaceBounded(t *testing.T) {
	for ms, want := range map[int]time.Duration{0: 2 * time.Second, 10: time.Second, 2000: 2 * time.Second, 600000: 10 * time.Second} {
		if got := tunnel.UsagePace(ms); got != want {
			t.Errorf("UsagePace(%d) = %v, want %v", ms, got, want)
		}
	}
}
