package control

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// fakeAgents opens a pipe per stream and lets the test play the agent.
type fakeAgents struct {
	mu     sync.Mutex
	opened atomic.Int32
	agents []net.Conn
	refuse error
	header tunnel.Header
}

func (f *fakeAgents) open(_ context.Context, _ string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error) {
	f.opened.Add(1)
	if f.refuse != nil {
		return nil, tunnel.Answer{}, f.refuse
	}
	ctrl, ag := net.Pipe()
	f.mu.Lock()
	f.agents = append(f.agents, ag)
	f.header = hd
	f.mu.Unlock()
	return ctrl, tunnel.Answer{OK: true}, nil
}

func (f *fakeAgents) agent(t *testing.T, i int) net.Conn {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		if len(f.agents) > i {
			c := f.agents[i]
			f.mu.Unlock()
			return c
		}
		f.mu.Unlock()
	}
	t.Fatal("no stream was opened")
	return nil
}

func sendSample(t *testing.T, c net.Conn, busy float64) {
	t.Helper()
	b, _ := json.Marshal(HostUsage{At: time.Now(), CPUBusy: &busy, MemTotal: 100, MemAvailable: 3})
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if err := tunnel.WriteFrame(c, tunnel.FrameUsage, b); err != nil {
		t.Fatal(err)
	}
}

func nextUsage(t *testing.T, ch <-chan usageEvent) liveUsage {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok || ev.end != "" {
			t.Fatalf("stream ended: %q", ev.end)
		}
		var u liveUsage
		if err := json.Unmarshal(ev.data, &u); err != nil {
			t.Fatal(err)
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("no sample")
	}
	return liveUsage{}
}

// TestUsageLiveSharedStream: two viewers of one server share one stream to
// its agent, a late viewer is sent the latest sample at once, the figures
// are bounded and warned on like a heartbeat's, and the stream closes once
// the last viewer has been gone for the linger.
func TestUsageLiveSharedStream(t *testing.T) {
	old := usageLiveLinger
	usageLiveLinger = 50 * time.Millisecond
	defer func() { usageLiveLinger = old }()
	var h usageLiveHub
	t.Cleanup(h.closeAll)
	fa := &fakeAgents{}
	ctx := context.Background()

	a, leaveA, err := h.watch(ctx, fa.open, "db-01")
	if err != nil {
		t.Fatal(err)
	}
	ag := fa.agent(t, 0)
	if fa.header.Service != tunnel.UsageLiveService || fa.header.UsageEveryMS != 2000 {
		t.Fatalf("header %+v", fa.header)
	}
	sendSample(t, ag, 250) // not a percentage: bounded away
	u := nextUsage(t, a)
	if u.Usage == nil || u.Usage.CPUBusy != nil {
		t.Fatalf("an impossible figure was passed on: %+v", u.Usage)
	}
	if len(u.Warnings) == 0 || u.Warnings[0].Resource != "memory" {
		t.Fatalf("3%% memory left raised no warning: %+v", u.Warnings)
	}
	sendSample(t, ag, 99)
	for _, w := range nextUsage(t, a).Warnings {
		if w.Resource == "cpu" {
			t.Fatal("two busy seconds were warned as a processor running low")
		}
	}

	b, leaveB, err := h.watch(ctx, fa.open, "DB-01")
	if err != nil {
		t.Fatal(err)
	}
	if late := nextUsage(t, b); late.Usage == nil {
		t.Fatal("a late viewer waited for the next sample")
	}
	sendSample(t, ag, 12)
	if nextUsage(t, a).Usage.CPUBusy == nil || nextUsage(t, b).Usage.CPUBusy == nil {
		t.Fatal("a viewer missed the sample")
	}
	if n := fa.opened.Load(); n != 1 {
		t.Fatalf("%d streams opened for one server, want 1", n)
	}

	leaveA()
	leaveB()
	_ = ag.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := ag.Read(make([]byte, 1)); err == nil {
		t.Fatal("the agent's stream stayed open with nobody watching")
	}
	h.mu.Lock()
	n := len(h.feeds)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d feeds kept after the last viewer left", n)
	}
}

// TestUsageLiveComingBackKeepsStream: a viewer who leaves and comes back
// within the linger (a reload, another tab of the page) does not restart
// sampling.
func TestUsageLiveComingBackKeepsStream(t *testing.T) {
	var h usageLiveHub
	t.Cleanup(h.closeAll)
	fa := &fakeAgents{}
	_, leave, _ := h.watch(context.Background(), fa.open, "web-1")
	fa.agent(t, 0)
	leave()
	ch, leave2, err := h.watch(context.Background(), fa.open, "web-1")
	if err != nil {
		t.Fatal(err)
	}
	defer leave2()
	sendSample(t, fa.agent(t, 0), 5)
	nextUsage(t, ch)
	if n := fa.opened.Load(); n != 1 {
		t.Fatalf("%d streams opened, want 1", n)
	}
}

// TestUsageLiveSays WhyNot: an agent this replica cannot reach, or one too
// old for the service, ends the stream with a reason the console can show.
func TestUsageLiveSaysWhyNot(t *testing.T) {
	for err, want := range map[error]string{
		ErrNoTunnel: "not connected",
		tunnel.ErrRefused{Reason: "unknown service"}: "agent too old",
	} {
		var h usageLiveHub
		defer h.closeAll()
		t.Cleanup(h.closeAll)
		ch, leave, werr := h.watch(context.Background(), (&fakeAgents{refuse: err}).open, "x")
		if werr != nil {
			t.Fatal(werr)
		}
		select {
		case ev := <-ch:
			if ev.end != want {
				t.Errorf("%v: end %q, want %q", err, ev.end, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%v: no end", err)
		}
		leave()
	}
}

// TestUsageLiveSlowViewerGetsNewest: a viewer that is behind is sent the
// newest sample rather than holding up the others or a backlog.
func TestUsageLiveSlowViewerGetsNewest(t *testing.T) {
	var h usageLiveHub
	t.Cleanup(h.closeAll)
	fa := &fakeAgents{}
	ch, leave, _ := h.watch(context.Background(), fa.open, "s")
	defer leave()
	ag := fa.agent(t, 0)
	for i := 1; i <= 6; i++ {
		sendSample(t, ag, float64(i))
	}
	time.Sleep(50 * time.Millisecond)
	var last float64
	for len(ch) > 0 {
		u := nextUsage(t, ch)
		last = *u.Usage.CPUBusy
	}
	if last != 6 {
		t.Fatalf("last sample read %v, want the newest (6)", last)
	}
}

// TestUsageLiveOrganizationsApart: the same host name in two organizations
// is two feeds, never one shared across them.
func TestUsageLiveOrganizationsApart(t *testing.T) {
	var h usageLiveHub
	t.Cleanup(h.closeAll)
	fa := &fakeAgents{}
	c1 := WithTenant(context.Background(), &Tenant{ID: "org-a"})
	c2 := WithTenant(context.Background(), &Tenant{ID: "org-b"})
	_, l1, _ := h.watch(c1, fa.open, "db")
	_, l2, _ := h.watch(c2, fa.open, "db")
	defer l1()
	defer l2()
	fa.agent(t, 1)
	if n := fa.opened.Load(); n != 2 {
		t.Fatalf("%d streams for two organizations' servers, want 2", n)
	}
}

// TestUsageLiveRoute: the live stream is refused to whoever may not see the
// System card, and says why when the agent cannot be reached from here.
func TestUsageLiveRoute(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, _ := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/usage/live"
	if w := do(t, h, http.MethodGet, path, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Errorf("an operator opened a server's live usage: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/assets/00000000-0000-0000-0000-000000000000/usage/live", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown asset: %d", w.Code)
	}
	w := do(t, h, http.MethodGet, path, "admin-token", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: end") ||
		!strings.Contains(w.Body.String(), `"reason":"not connected"`) {
		t.Errorf("no tunnel here: %d %s", w.Code, w.Body)
	}
}
