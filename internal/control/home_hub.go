package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

/*
The home hub: Home's live dashboard, built once per organization and sent to
every console watching it as only what changed.

  - Cost per organization, not per viewer. One goroutine per organization
    reads the store every homeEvery while anyone watches; fifty people
    watching is still one read.
  - The fast half (servers, live sessions, waiting, the audit log) every
    homeEvery, or sooner when the store says something changed. The slow
    half (sessions and sign-ins by hour, the fleet's usage, which agents
    report about once a minute) every homeSlowEvery.
  - The last hour of four numbers is a ring of one point a minute, held here.
    With nobody watching the hub keeps sampling once a minute for
    homeLinger, so a person who comes back finds their lines; after that it
    stops and forgets them.
  - Each update carries a version. A console that sees a gap asks for the
    whole dashboard again; a subscriber too far behind is sent the whole
    dashboard on the next build instead of being cut off.

Only the roles that see the whole fleet subscribe. The hub never decides who
may see it: getConsoleAlertStream does, on every subscribe and on every
re-check of the cookie.
*/

var (
	homeEvery     = 5 * time.Second
	homeSlowEvery = 30 * time.Second
	homeLinger    = time.Hour
	// homeNudgeGap is the least time between two builds a change asked for.
	homeNudgeGap = 2 * time.Second
)

const homeRing = 60

// homeSource is what the hub reads; the Store, or a fake in tests.
type homeSource interface {
	homeFast(ctx context.Context, h *HomeSnapshot) error
	homeSlow(ctx context.Context, h *HomeSnapshot, now time.Time) error
}

// homeFrame is one message to a console: the whole dashboard (full) or the
// fields that changed since version V-1.
type homeFrame struct {
	V      int64                      `json:"v"`
	Full   bool                       `json:"full,omitempty"`
	Fields map[string]json.RawMessage `json:"set"`
}

type homeSub struct {
	ch chan homeFrame
	// needFull is set when the subscriber missed a frame or has had none:
	// the next build sends it everything.
	needFull bool
}

type homeGroup struct {
	tenant *Tenant
	subs   map[*homeSub]struct{}
	stop   chan struct{}
	wake   chan struct{}

	// Only the group's own goroutine touches these, except fields and v,
	// which subscribe reads under the hub's lock.
	snap      HomeSnapshot
	slowAt    time.Time
	fields    map[string]json.RawMessage
	v         int64
	idleSince time.Time

	since     *time.Time
	minute    time.Time
	answering []int
	live      []int
	waiting   []int
	processor []*float64
}

type homeHub struct {
	mu     sync.Mutex
	groups map[string]*homeGroup
	closed bool
	src    homeSource
	logErr func(msg string, args ...any)
	// now is the clock; tests replace it.
	now func() time.Time
}

func (h *homeHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

var errHomeClosed = errors.New("shutting down")

// subscribe adds a viewer to the organization ctx names. The returned
// subscription is sent the whole dashboard as soon as there is one.
func (h *homeHub) subscribe(ctx context.Context) (*homeSub, func(), error) {
	tid := tenantIDOf(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.src == nil {
		return nil, nil, errHomeClosed
	}
	if h.groups == nil {
		h.groups = map[string]*homeGroup{}
	}
	g := h.groups[tid]
	if g == nil {
		t := TenantOf(ctx)
		if t == nil {
			t = defaultTenant()
		}
		g = &homeGroup{tenant: t, subs: map[*homeSub]struct{}{}, stop: make(chan struct{}), wake: make(chan struct{}, 1)}
		h.groups[tid] = g
		go h.run(tid, g)
	}
	s := &homeSub{ch: make(chan homeFrame, 8), needFull: true}
	g.subs[s] = struct{}{}
	if g.fields != nil {
		s.needFull = false
		s.ch <- homeFrame{V: g.v, Full: true, Fields: g.fields}
	} else {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
	return s, func() { h.unsubscribe(tid, g, s) }, nil
}

func (h *homeHub) unsubscribe(tid string, g *homeGroup, s *homeSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := g.subs[s]; !ok {
		return
	}
	delete(g.subs, s)
	close(s.ch)
	if len(g.subs) == 0 {
		g.idleSince = h.clock()
	}
}

// nudge says something Home shows may have changed in organization tid: a
// session, a request, an audit entry. The group builds again soon, rather
// than on its next tick. Never blocks.
func (h *homeHub) nudge(tid string) {
	h.mu.Lock()
	g := h.groups[tid]
	h.mu.Unlock()
	if g == nil {
		return
	}
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// closeAll ends every subscription and every group, for a shutdown.
func (h *homeHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for tid, g := range h.groups {
		for s := range g.subs {
			close(s.ch)
		}
		g.subs = nil
		close(g.stop)
		delete(h.groups, tid)
	}
}

// run is one organization's builder.
func (h *homeHub) run(tid string, g *homeGroup) {
	ctx := WithTenant(context.Background(), g.tenant)
	t := time.NewTicker(homeEvery)
	defer t.Stop()
	var lastBuild time.Time
	for {
		nudged := false
		select {
		case <-g.stop:
			return
		case <-t.C:
		case <-g.wake:
			nudged = true
		}
		now := h.clock()
		if nudged && now.Sub(lastBuild) < homeNudgeGap {
			// Several changes at once are one build, a moment later.
			time.Sleep(homeNudgeGap - now.Sub(lastBuild))
			now = h.clock()
		}

		h.mu.Lock()
		watched := len(g.subs) > 0
		if !watched && now.Sub(g.idleSince) > homeLinger {
			// Nobody has watched for an hour: forget this organization.
			if h.groups[tid] == g {
				delete(h.groups, tid)
			}
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()

		// Unwatched, the hub only keeps its lines: one read a minute.
		if !watched && now.Truncate(time.Minute).Equal(g.minute) {
			continue
		}
		if err := h.build(ctx, g, now); err != nil {
			if h.logErr != nil {
				h.logErr("could not build Home's dashboard", "error", err)
			}
			continue
		}
		lastBuild = now
		h.publish(tid, g)
	}
}

// build reads the store into g.snap and moves the lines on.
func (h *homeHub) build(ctx context.Context, g *homeGroup, now time.Time) error {
	qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	next := g.snap
	if err := h.src.homeFast(qctx, &next); err != nil {
		return err
	}
	if g.slowAt.IsZero() || now.Sub(g.slowAt) >= homeSlowEvery {
		if err := h.src.homeSlow(qctx, &next, now); err != nil {
			return err
		}
		g.slowAt = now
	}
	next.At = now.UTC()

	if m := now.Truncate(time.Minute); !m.Equal(g.minute) {
		g.minute = m
		if g.since == nil {
			s := m.UTC()
			g.since = &s
		}
		g.answering = pushRing(g.answering, next.Servers.Answering)
		g.live = pushRing(g.live, next.Live.Count)
		g.waiting = pushRing(g.waiting, next.Waiting.Count)
		g.processor = pushRing(g.processor, next.Processor.Average)
		if len(g.answering) == homeRing {
			// A full hour: the line begins an hour ago, not when we started.
			s := m.Add(-(homeRing - 1) * time.Minute).UTC()
			g.since = &s
		}
	}
	next.Trends = HomeTrends{
		Since:     g.since,
		Answering: append([]int{}, g.answering...),
		Live:      append([]int{}, g.live...),
		Waiting:   append([]int{}, g.waiting...),
		Processor: append([]*float64{}, g.processor...),
	}
	g.snap = next
	return nil
}

func pushRing[T any](r []T, v T) []T {
	if len(r) >= homeRing {
		r = append(r[:0], r[len(r)-homeRing+1:]...)
	}
	return append(r, v)
}

// homeFields splits a snapshot into its top-level fields, each marshalled,
// so two snapshots compare field by field.
func homeFields(s HomeSnapshot) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	return out, json.Unmarshal(b, &out)
}

// homeDiff is the fields of next that are not the same in prev. "at" is
// left out: it changes on every build and on its own says nothing.
func homeDiff(prev, next map[string]json.RawMessage) map[string]json.RawMessage {
	d := map[string]json.RawMessage{}
	for k, v := range next {
		if k == "at" {
			continue
		}
		if !bytes.Equal(prev[k], v) {
			d[k] = v
		}
	}
	return d
}

// publish sends what changed since the last build, and the whole dashboard
// to whoever needs it.
func (h *homeHub) publish(tid string, g *homeGroup) {
	fields, err := homeFields(g.snap)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.groups[tid] != g {
		return
	}
	diff := homeDiff(g.fields, fields)
	changed := len(diff) > 0 || g.fields == nil
	if changed {
		g.v++
		g.fields = fields
	}
	for s := range g.subs {
		var f homeFrame
		switch {
		case s.needFull:
			f = homeFrame{V: g.v, Full: true, Fields: g.fields}
		case changed:
			f = homeFrame{V: g.v, Fields: diff}
		default:
			continue
		}
		select {
		case s.ch <- f:
			s.needFull = false
		default:
			// Too far behind: skip ahead to the whole dashboard next time.
			s.needFull = true
		}
	}
}

// current is organization tid's last build as one JSON object while someone
// there watches (so it is at most homeEvery old), and otherwise nil with the
// lines the hub still holds, if any.
func (h *homeHub) current(tid string) (map[string]json.RawMessage, json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.groups[tid]
	if g == nil || g.fields == nil {
		return nil, nil
	}
	if len(g.subs) > 0 {
		return g.fields, nil
	}
	return nil, g.fields["trends"]
}
