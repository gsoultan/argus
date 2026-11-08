package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHome counts reads and serves whatever live count it is told.
type fakeHome struct {
	mu    sync.Mutex
	live  int
	fast  atomic.Int32
	slow  atomic.Int32
	audit []AuditEvent
}

func (f *fakeHome) homeFast(_ context.Context, h *HomeSnapshot) error {
	f.fast.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	h.Live.Count = f.live
	h.Servers = HomeServers{Total: 3, Answering: 3}
	h.Audit = f.audit
	return nil
}

func (f *fakeHome) homeSlow(_ context.Context, h *HomeSnapshot, _ time.Time) error {
	f.slow.Add(1)
	h.Sessions.Last24h = 7
	return nil
}

func (f *fakeHome) setLive(n int) {
	f.mu.Lock()
	f.live = n
	f.mu.Unlock()
}

func fastHome(t *testing.T) {
	t.Helper()
	pe, ps, pg := homeEvery, homeSlowEvery, homeNudgeGap
	homeEvery, homeSlowEvery, homeNudgeGap = 20*time.Millisecond, time.Hour, 0
	t.Cleanup(func() { homeEvery, homeSlowEvery, homeNudgeGap = pe, ps, pg })
}

func frame(t *testing.T, ch <-chan homeFrame) homeFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no frame within 3s")
	}
	return homeFrame{}
}

// The first frame is the whole dashboard; after it, only what changed, one
// version on; and nothing at all while nothing changes.
func TestHomeSendsTheWholeDashboardThenOnlyChanges(t *testing.T) {
	fastHome(t)
	src := &fakeHome{live: 2}
	h := &homeHub{src: src}
	defer h.closeAll()
	sub, stop, err := h.subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	first := frame(t, sub.ch)
	if !first.Full || first.Fields["live"] == nil || first.Fields["sessions"] == nil {
		t.Fatalf("first frame is not the whole dashboard: %+v", first)
	}

	src.setLive(5)
	f := frame(t, sub.ch)
	if f.Full || f.V != first.V+1 {
		t.Fatalf("second frame v%d full=%v after v%d", f.V, f.Full, first.V)
	}
	if _, ok := f.Fields["live"]; !ok || len(f.Fields) != 1 {
		t.Fatalf("an update carried more than what changed: %v", keys(f.Fields))
	}
	var live HomeLive
	_ = json.Unmarshal(f.Fields["live"], &live)
	if live.Count != 5 {
		t.Fatalf("live %+v", live)
	}

	// Several more builds with nothing new send nothing.
	before := src.fast.Load()
	for src.fast.Load() < before+3 {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case f := <-sub.ch:
		t.Fatalf("an unchanged dashboard was sent again: %v", keys(f.Fields))
	default:
	}
}

// Fifty viewers are one read, not fifty.
func TestHomeCostIsPerOrganizationNotPerViewer(t *testing.T) {
	fastHome(t)
	homeEvery = 50 * time.Millisecond
	src := &fakeHome{}
	h := &homeHub{src: src}
	defer h.closeAll()
	for range 50 {
		sub, stop, err := h.subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		go func() {
			for range sub.ch {
			}
		}()
	}
	time.Sleep(520 * time.Millisecond)
	// About ten ticks, plus the builds the subscriptions asked for.
	if n := src.fast.Load(); n > 16 {
		t.Fatalf("%d reads for 50 viewers in half a second", n)
	}
}

// With nobody watching the hub reads once a minute at most, for its lines,
// and forgets the organization after homeLinger.
func TestHomeStopsReadingWhenNobodyWatches(t *testing.T) {
	fastHome(t)
	src := &fakeHome{}
	var now atomic.Pointer[time.Time]
	t0 := time.Date(2026, 11, 7, 9, 0, 10, 0, time.UTC)
	now.Store(&t0)
	h := &homeHub{src: src, now: func() time.Time { return *now.Load() }}
	defer h.closeAll()
	sub, stop, _ := h.subscribe(context.Background())
	frame(t, sub.ch)
	stop()

	n := src.fast.Load()
	time.Sleep(200 * time.Millisecond) // ten ticks in the same minute
	if got := src.fast.Load(); got != n {
		t.Fatalf("read %d times with nobody watching", got-n)
	}
	t1 := t0.Add(time.Minute)
	now.Store(&t1)
	time.Sleep(100 * time.Millisecond)
	if got := src.fast.Load(); got != n+1 {
		t.Fatalf("a new minute read %d times, want once", got-n)
	}
	t2 := t0.Add(homeLinger + 2*time.Minute)
	now.Store(&t2)
	time.Sleep(100 * time.Millisecond)
	h.mu.Lock()
	left := len(h.groups)
	h.mu.Unlock()
	if left != 0 {
		t.Fatal("the organization was kept after an hour with nobody watching")
	}
}

// A subscriber too far behind is not cut off: its next frame is the whole
// dashboard.
func TestHomeSubscriberBehindGetsTheWholeDashboard(t *testing.T) {
	fastHome(t)
	src := &fakeHome{}
	h := &homeHub{src: src}
	defer h.closeAll()
	sub, stop, _ := h.subscribe(context.Background())
	defer stop()
	for i := range 20 { // more changes than the buffer holds, unread
		src.setLive(i + 1)
		time.Sleep(25 * time.Millisecond)
	}
	var last homeFrame
	for len(sub.ch) > 0 {
		last = <-sub.ch
	}
	src.setLive(100)
	f := frame(t, sub.ch)
	if !f.Full && f.V != last.V+1 {
		t.Fatalf("after falling behind got a partial frame v%d after v%d", f.V, last.V)
	}
}

// The lines are one point a minute, at most an hour of them.
func TestHomeTrendsAreAnHourAtMost(t *testing.T) {
	src := &fakeHome{live: 1}
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	h := &homeHub{src: src}
	g := &homeGroup{}
	ctx := context.Background()
	for i := range 90 {
		src.setLive(i)
		if err := h.build(ctx, g, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		// A second build in the same minute adds no point.
		if err := h.build(ctx, g, t0.Add(time.Duration(i)*time.Minute+30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	tr := g.snap.Trends
	if len(tr.Live) != homeRing || tr.Live[0] != 30 || tr.Live[homeRing-1] != 89 {
		t.Fatalf("live line %v", tr.Live)
	}
	if want := t0.Add(30 * time.Minute); !tr.Since.Equal(want) {
		t.Fatalf("since %v, want %v", tr.Since, want)
	}
}

func TestShutdownEndsHomeSubscriptions(t *testing.T) {
	fastHome(t)
	h := &homeHub{src: &fakeHome{}}
	sub, stop, _ := h.subscribe(context.Background())
	defer stop()
	frame(t, sub.ch)
	h.closeAll()
	for range sub.ch {
	}
	if _, _, err := h.subscribe(context.Background()); err == nil {
		t.Fatal("subscribed after shutdown")
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

/* ── Over the alert stream ───────────────────────────────────────────────── */

func streamID(t *testing.T, ev sseEvent) string {
	t.Helper()
	var ready struct{ Stream string }
	if ev.name != "ready" || json.Unmarshal([]byte(ev.data), &ready) != nil || ready.Stream == "" {
		t.Fatalf("ready event %q %s names no stream", ev.name, ev.data)
	}
	return ready.Stream
}

func postHome(t *testing.T, h *loginHarness, id string, on bool) int {
	t.Helper()
	body := `{"on":false}`
	if on {
		body = `{"on":true}`
	}
	res, err := h.client.Post(h.srv.URL+"/api/v1/alerts/stream/"+id+"/home", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func nextNamed(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	for {
		ev, ok := next(t, ch)
		if !ok {
			t.Fatalf("stream closed waiting for %q", name)
		}
		if ev.name == name {
			return ev
		}
	}
}

// An admin switches Home on over their own stream and gets the dashboard.
func TestHomeRidesTheAlertStream(t *testing.T) {
	h, _ := alertHarness(t, "admin")
	// The hub keeps an organization's lines for an hour after the last
	// viewer; a test ends sooner.
	defer h.api.StopAlertStreams()
	events, stop := openStream(t, h)
	defer stop()
	id := streamID(t, nextNamed(t, events, "ready"))
	if c := postHome(t, h, id, true); c != http.StatusNoContent {
		t.Fatalf("switch on = %d", c)
	}
	ev := nextNamed(t, events, "home")
	var f homeFrame
	if err := json.Unmarshal([]byte(ev.data), &f); err != nil || !f.Full || f.Fields["servers"] == nil {
		t.Fatalf("home frame %s", ev.data)
	}
	if strings.Contains(ev.data, `"hash"`) && !strings.Contains(ev.data, `"hash":""`) {
		t.Fatalf("Home carries the audit chain's hashes: %s", ev.data)
	}
}

// Home's fleet numbers are for the fleet roles: an operator is refused, and
// nobody can switch on another person's stream.
func TestHomeIsOnlyForTheFleetRolesAndOnlyOnYourOwnStream(t *testing.T) {
	op, _ := alertHarness(t, "operator")
	defer op.api.StopAlertStreams()
	events, stop := openStream(t, op)
	defer stop()
	opID := streamID(t, nextNamed(t, events, "ready"))
	if c := postHome(t, op, opID, true); c != http.StatusForbidden {
		t.Fatalf("operator switching Home on = %d, want 403", c)
	}

	// An admin on the same control plane, in a browser of their own.
	jar, _ := cookiejar.New(nil)
	ua := new(string)
	admin := &loginHarness{api: op.api, srv: op.srv, store: op.store, ua: ua,
		client: &http.Client{Jar: jar, Transport: uaTransport{ua: ua}}}
	email, password := newAccount(t, admin.store, "admin")
	signIn(t, admin, email, password, chromeMac)
	// The admin's own cookie, the operator's stream id.
	if c := postHome(t, admin, opID, true); c != http.StatusNotFound {
		t.Fatalf("someone else's stream = %d, want 404", c)
	}
	if c := postHome(t, admin, "no-such-stream", true); c != http.StatusNotFound {
		t.Fatalf("unknown stream = %d, want 404", c)
	}
}

// busyHome is a fleet that keeps Home busy: a full Live now, a full audit
// page, five busy servers and five filling disks, with a session starting or
// ending every 30 s.
type busyHome struct{ n int }

func (b *busyHome) homeFast(_ context.Context, h *HomeSnapshot) error {
	b.n++
	h.Servers = HomeServers{Total: 400, Answering: 396, Quiet: 3, Down: 1}
	h.Live = HomeLive{Count: 20 + (b.n/6)%5, Root: 4}
	h.Waiting = HomeWaiting{Count: 3}
	h.LiveList = make([]HomeSession, homeLiveMax)
	for i := range h.LiveList {
		h.LiveList[i] = HomeSession{ID: "0b6c1d1e-2f3a-4b5c-8d9e-" + itoa(100000000000+b.n/6+i), User: "someone.long@northwind.id",
			Principal: "deploy", Host: "web-" + itoa(i) + ".prod.northwind.internal", Kind: "terminal", StartedAt: time.Unix(int64(b.n/6+i), 0).UTC()}
	}
	h.Audit = make([]AuditEvent, homeAuditMax)
	for i := range h.Audit {
		h.Audit[i] = AuditEvent{Seq: int64(b.n/6 + i), ID: "x", Action: "session.start", Severity: "info",
			ActorEmail: "someone.long@northwind.id", Target: "web-07.prod", Detail: "Opened a terminal on web-07.prod as deploy, approved by an approver."}
	}
	return nil
}

func (b *busyHome) homeSlow(_ context.Context, h *HomeSnapshot, now time.Time) error {
	avg := 31.4
	h.Processor = HomeProcessor{Average: &avg, Reporting: 396}
	h.Disks = nil
	for i := range homeBusiest {
		h.Processor.Busiest = append(h.Processor.Busiest, HomeBusy{AssetID: "0b6c1d1e-2f3a-4b5c-8d9e-00000000000" + itoa(i), Name: "build-" + itoa(i), Busy: 90 - float64(i)})
	}
	for i := range homeDisksMax {
		h.Disks = append(h.Disks, HomeDisk{AssetID: "0b6c1d1e-2f3a-4b5c-8d9e-00000000001" + itoa(i), Name: "db-" + itoa(i), Mount: "/var/lib/postgresql", Used: 91})
	}
	h.Sessions.Last24h = 212 + now.Minute()
	h.SignIns.Last24h = 48
	return nil
}

// The network budget: an hour of a busy fleet costs under 0.3 KB/s, and the
// first message, the whole dashboard, under 8 KB.
func TestHomeNetworkBudget(t *testing.T) {
	src := &busyHome{}
	h := &homeHub{src: src}
	g := &homeGroup{subs: map[*homeSub]struct{}{}}
	h.groups = map[string]*homeGroup{"t": g}
	s := &homeSub{ch: make(chan homeFrame, 1), needFull: true}
	g.subs[s] = struct{}{}

	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	var first, rest, frames int
	for i := range 720 { // an hour, one build every 5 s
		if err := h.build(context.Background(), g, t0.Add(time.Duration(i)*5*time.Second)); err != nil {
			t.Fatal(err)
		}
		g.slowAt = time.Time{}.Add(time.Duration(i/6) * homeSlowEvery) // slow half every 30 s
		h.publish("t", g)
		select {
		case f := <-s.ch:
			b, _ := json.Marshal(f)
			// What goes on the wire: "event: home\ndata: ...\n\n".
			n := len(b) + len("event: home\ndata: \n\n")
			if f.Full {
				first += n
			} else {
				rest += n
				frames++
			}
		default:
		}
	}
	perSecond := float64(first+rest) / 3600
	t.Logf("whole dashboard %d B; %d updates, %d B; %.0f B/s over the hour", first, frames, rest, perSecond)
	if first > 8<<10 {
		t.Errorf("the whole dashboard is %d B, over 8 KB", first)
	}
	if perSecond > 300 {
		t.Errorf("an hour of a busy fleet is %.0f B/s, over the 300 B/s budget", perSecond)
	}
}
