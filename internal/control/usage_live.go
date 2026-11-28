package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/caduceus"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Live usage: a server's figures every couple of seconds while someone has its
page open, straight from its agent, never through the database.

	console ──SSE──▶ argus-control ──"usage.live" stream──▶ agent

  - Nothing while nobody watches. The first viewer of a server opens one
    stream to its agent; every other viewer on this replica shares it. The
    last one leaving closes it after usageLiveLinger, so switching between
    the Overview and Metrics tabs, or a reload, does not restart sampling.
  - One read of the database per viewer, to find the server's agent. Each
    sample is decoded, bounded and encoded once per server, then the same
    bytes go to every viewer. Nothing is written: the heartbeat goes on
    reporting usage about once a minute, and history is built from that.
  - A slow viewer is sent the newest sample, not a queue of old ones.
  - When the agent cannot be reached from this replica -- not connected,
    connected to another replica, or older than the service -- the stream
    says so and ends, and the console keeps showing the heartbeat's figures.

Who may watch is decided where the System card's is (getSystem), checked
again every alertStreamRecheck for as long as the stream stays open.
*/

var (
	// usageLiveLinger is how long a stream to an agent stays open after its
	// last viewer left.
	usageLiveLinger = 15 * time.Second
	// usageLiveEvery is the pace asked of agents.
	usageLiveEvery = tunnel.UsageLiveEvery
)

const (
	// maxUsageFeeds bounds the servers watched live through this replica.
	maxUsageFeeds = 2048
	// maxUsageViewers bounds the viewers of one server on this replica.
	maxUsageViewers = 256
)

var errUsageLiveFull = errors.New("too many live views are open on this control plane")

// usageEvent is one message to a viewer: a sample, or why there will be none.
type usageEvent struct {
	end  string // set on the last event: why the stream ended
	data []byte // a liveUsage, JSON
}

// liveUsage is one sample as the console reads it, in the System card's
// shape so the card can show it as it shows the heartbeat's.
type liveUsage struct {
	Usage    *SystemUsage      `json:"usage"`
	UsageAt  time.Time         `json:"usageAt"`
	Warnings []ResourceWarning `json:"warnings"`
	// EverySeconds is how often a sample comes.
	EverySeconds float64 `json:"everySeconds"`
}

type usageViewer struct{ ch chan usageEvent }

type usageFeed struct {
	key     string
	viewers map[*usageViewer]struct{}
	last    []byte
	stop    context.CancelFunc
	linger  *time.Timer
	ended   bool
}

// usageOpener opens the agent's stream; the TunnelHub, or a fake in tests.
type usageOpener func(ctx context.Context, host string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error)

type usageLiveHub struct {
	mu     sync.Mutex
	feeds  map[string]*usageFeed
	closed bool
	wg     sync.WaitGroup
	// now is the clock; tests replace it.
	now func() time.Time
}

func (h *usageLiveHub) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// watch adds a viewer of host's usage in ctx's organization. The returned
// channel is sent the latest sample at once when there is one, then each new
// one; it is closed after an event that carries end.
func (h *usageLiveHub) watch(ctx context.Context, open usageOpener, host string) (<-chan usageEvent, func(), error) {
	key := tunnelKey(tenantIDOf(ctx), host)
	v := &usageViewer{ch: make(chan usageEvent, 2)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, errUsageLiveFull
	}
	if h.feeds == nil {
		h.feeds = map[string]*usageFeed{}
	}
	f := h.feeds[key]
	if f == nil {
		if len(h.feeds) >= maxUsageFeeds {
			return nil, nil, errUsageLiveFull
		}
		fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &usageFeed{key: key, viewers: map[*usageViewer]struct{}{}, stop: cancel}
		h.feeds[key] = f
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.run(fctx, f, open, host)
		}()
	}
	if len(f.viewers) >= maxUsageViewers {
		return nil, nil, errUsageLiveFull
	}
	if f.linger != nil {
		f.linger.Stop()
		f.linger = nil
	}
	f.viewers[v] = struct{}{}
	if f.last != nil {
		v.ch <- usageEvent{data: f.last}
	}
	var once sync.Once
	return v.ch, func() { once.Do(func() { h.leave(f, v) }) }, nil
}

// closeAll ends every stream to an agent, for shutdown, and waits for them.
func (h *usageLiveHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	for _, f := range h.feeds {
		f.stop()
	}
	h.mu.Unlock()
	h.wg.Wait()
}

func (h *usageLiveHub) leave(f *usageFeed, v *usageViewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := f.viewers[v]; !ok {
		return
	}
	delete(f.viewers, v)
	if len(f.viewers) > 0 || f.ended {
		return
	}
	f.linger = time.AfterFunc(usageLiveLinger, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(f.viewers) == 0 && h.feeds[f.key] == f {
			delete(h.feeds, f.key)
			f.ended = true
			f.stop()
		}
	})
}

// run holds one agent's stream and hands each sample to every viewer.
func (h *usageLiveHub) run(ctx context.Context, f *usageFeed, open usageOpener, host string) {
	defer f.stop()
	reason := h.relay(ctx, f, open, host)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds[f.key] == f {
		delete(h.feeds, f.key)
	}
	f.ended = true
	if f.linger != nil {
		f.linger.Stop()
	}
	for v := range f.viewers {
		// Room is made for the last word: it is what tells the console to
		// stop waiting and say why.
		select {
		case <-v.ch:
		default:
		}
		v.ch <- usageEvent{end: reason}
		close(v.ch)
	}
	f.viewers = nil
}

// relay reads samples until the stream ends, and says why it did.
func (h *usageLiveHub) relay(ctx context.Context, f *usageFeed, open usageOpener, host string) string {
	octx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, _, err := open(octx, host, tunnel.Header{Service: tunnel.UsageLiveService,
		UsageEveryMS: int(usageLiveEvery / time.Millisecond)})
	cancel()
	if err != nil {
		return usageLiveRefusal(err)
	}
	defer st.Close()
	go func() {
		<-ctx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		_ = st.SetDeadline(time.Now().Add(usageLiveEvery + 20*time.Second))
		typ, b, err := fr.Next()
		if err != nil {
			if ctx.Err() != nil {
				return "nobody is watching"
			}
			return "the agent stopped sending live figures"
		}
		if typ != tunnel.FrameUsage {
			continue
		}
		out, ok := h.decode(b)
		if !ok {
			continue
		}
		h.mu.Lock()
		f.last = out
		for v := range f.viewers {
			select {
			case v.ch <- usageEvent{data: out}:
			default:
				// Behind: drop the older sample for this one.
				select {
				case <-v.ch:
				default:
				}
				select {
				case v.ch <- usageEvent{data: out}:
				default:
				}
			}
		}
		h.mu.Unlock()
	}
}

// decode bounds one sample the way a heartbeat's is bounded and encodes it
// for the console. The sample time is the agent's, capped at now.
func (h *usageLiveHub) decode(b []byte) ([]byte, bool) {
	var hu HostUsage
	if json.Unmarshal(b, &hu) != nil {
		return nil, false
	}
	hu = *hu.bounded()
	at := hu.At
	if now := h.clock(); at.IsZero() || at.After(now) {
		at = now
	}
	// A processor busy for two seconds is not running low: that warning is
	// the heartbeat's, over a minute, and the console keeps it from there.
	warn := LowResources(hu)
	kept := warn[:0]
	for _, w := range warn {
		if w.Resource != "cpu" {
			kept = append(kept, w)
		}
	}
	out, err := json.Marshal(liveUsage{Usage: systemUsageOf(hu), UsageAt: at.UTC(),
		Warnings: kept, EverySeconds: usageLiveEvery.Seconds()})
	return out, err == nil
}

// usageLiveRefusal is why a stream could not be opened, in the console's words.
func usageLiveRefusal(err error) string {
	var refused tunnel.ErrRefused
	switch {
	case errors.Is(err, ErrNoTunnel):
		return "not connected"
	case errors.As(err, &refused) && strings.Contains(refused.Reason, "unknown service"):
		return "agent too old"
	case errors.As(err, &refused):
		return "refused: " + refused.Reason
	}
	return "unreachable"
}

// AgentNameForAsset is the name the agent reporting for an asset goes by,
// which its tunnel is found under.
func (s *Store) AgentNameForAsset(ctx context.Context, id string) (string, error) {
	var name *string
	err := s.db(ctx).QueryRow(ctx, `
		SELECT `+agentNameSQL+`
		  FROM assets a
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&name)
	if errors.Is(err, caduceus.ErrNoRows) {
		return "", ErrAssetNotFound
	}
	if err != nil || name == nil {
		return "", err
	}
	return *name, nil
}

// getUsageLive serves GET /api/v1/assets/{id}/usage/live, a server-sent
// events stream of "usage" samples ending in one "end" saying why.
func (a *API) getUsageLive(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing a server's system details requires the admin, owner or auditor role")
		return
	}
	host, err := a.store.AgentNameForAsset(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "live usage", err)
		return
	}

	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, data []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(alertStreamWrite))
		var err error
		if event == "" {
			_, err = io.WriteString(w, ": ping\n\n")
		} else {
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		}
		return err == nil && rc.Flush() == nil
	}
	end := func(reason string) {
		b, _ := json.Marshal(map[string]string{"reason": reason})
		send("end", b)
	}
	if host == "" {
		end("no agent")
		return
	}
	if a.Tunnels == nil {
		end("not connected")
		return
	}
	ch, leave, err := a.usageLive.watch(r.Context(), a.Tunnels.OpenWith, host)
	if err != nil {
		end("busy")
		return
	}
	defer leave()

	recheck := time.NewTicker(alertStreamRecheck)
	defer recheck.Stop()
	beat := time.NewTicker(alertStreamHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			if !send("", nil) {
				return
			}
		case <-recheck.C:
			rs := a.resolveSession(r)
			if !rs.ok || !strings.EqualFold(rs.sess.Email, sess.Email) || !seesWholeFleet(rs.sess.Role) {
				end("signed out")
				return
			}
		case ev, open := <-ch:
			if !open {
				return
			}
			if ev.end != "" {
				end(ev.end)
				return
			}
			if !send("usage", ev.data) {
				return
			}
		}
	}
}
