package control

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The bell's routes. Every one is a signed-in person's, about their own
// alerts: the reader is always the session, never a parameter.

func (a *API) alertReaderFor(r *http.Request) (alertReader, bool, error) {
	sess, ok := a.authenticate(r)
	if !ok {
		return alertReader{}, false, nil
	}
	prefs, err := a.store.AlertPrefs(r.Context(), sess.Email)
	return alertReader{email: sess.Email, role: sess.Role, prefs: prefs}, true, err
}

// getConsoleAlerts lists the caller's alerts, newest first.
func (a *API) getConsoleAlerts(w http.ResponseWriter, r *http.Request) {
	rd, ok, err := a.alertReaderFor(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		a.fail(w, "alert preferences", err)
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := a.store.ConsoleAlerts(r.Context(), rd, q.Get("unread") == "1", before, limit)
	if err != nil {
		a.fail(w, "alerts", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// postConsoleAlertsRead marks alerts read: some by seq, or all up to one.
func (a *API) postConsoleAlertsRead(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Seqs []int64 `json:"seqs"`
		UpTo int64   `json:"upTo"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(in.Seqs) > 200 {
		writeErr(w, http.StatusBadRequest, "mark at most 200 alerts at a time, or all of them with upTo")
		return
	}
	// Marking an alert the caller cannot see read is harmless: it changes
	// nothing anyone reads but their own count, which never included it.
	if err := a.store.MarkConsoleAlertsRead(r.Context(), sess.Email, in.Seqs, in.UpTo); err != nil {
		a.fail(w, "mark alerts read", err)
		return
	}
	a.alertHub.touched(tenantIDOf(r.Context()), sess.Email)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) getAlertPrefs(w http.ResponseWriter, r *http.Request) {
	rd, ok, err := a.alertReaderFor(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		a.fail(w, "alert preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, rd.prefs)
}

func (a *API) putAlertPrefs(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in AlertPrefs
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.store.SetAlertPrefs(r.Context(), sess.Email, in)
	if errors.Is(err, ErrUnknownAlertKind) {
		writeErr(w, http.StatusBadRequest, "muted names a kind of alert this console does not have; the kinds are "+
			strings.Join(AlertKinds, ", "))
		return
	}
	if err != nil {
		a.fail(w, "save alert preferences", err)
		return
	}
	a.alertHub.touched(tenantIDOf(r.Context()), sess.Email)
	writeJSON(w, http.StatusOK, out)
}

// How often a stream is checked against the account, and kept warm for
// proxies that close a quiet connection.
var (
	alertStreamRecheck   = 30 * time.Second
	alertStreamHeartbeat = 25 * time.Second
)

const alertStreamWrite = 10 * time.Second

// getConsoleAlertStream is the server-sent events stream behind the bell.
//
// It says that something changed, and what, for the alerts the caller may
// see; the console then refetches the list, which is where the count and the
// read state come from. So a stream that dropped an event costs a toast, not
// a wrong badge.
//
// The cookie is checked again every alertStreamRecheck. A stream outlives the
// request that opened it, and a person signed out, disabled or demoted must
// stop hearing about what they no longer may see.
func (a *API) getConsoleAlertStream(w http.ResponseWriter, r *http.Request) {
	rd, ok, err := a.alertReaderFor(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		a.fail(w, "alert preferences", err)
		return
	}
	sub, cancel, err := a.alertHub.subscribe(r.Context(), rd.email)
	if errors.Is(err, ErrTooManyAlertStreams) {
		w.Header().Set("Retry-After", "30")
		writeErr(w, http.StatusTooManyRequests, "too many consoles are open on this account; close some tabs")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "the control plane is restarting")
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// nginx and some proxies buffer a response unless told not to.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, v any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(alertStreamWrite))
		if event != "" {
			b, _ := json.Marshal(v)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
				return false
			}
		} else if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// retry tells EventSource how long to wait before reconnecting.
	if _, err := io.WriteString(w, "retry: 5000\n\n"); err != nil {
		return
	}
	// The stream's id lets this console switch Home's live dashboard on and
	// off over it (postStreamHome) without opening a second connection.
	hc := &homeControl{id: newStreamID(), email: rd.email, tenant: tenantIDOf(r.Context()), ch: make(chan bool, 1)}
	a.homeStreams.Store(hc.id, hc)
	defer a.homeStreams.Delete(hc.id)
	var (
		homeCh   <-chan homeFrame
		homeStop func()
	)
	stopHome := func() {
		if homeStop != nil {
			homeStop()
		}
		homeCh, homeStop = nil, nil
	}
	defer stopHome()

	if !send("ready", map[string]any{"popup": rd.prefs.Popup, "sound": rd.prefs.Sound, "stream": hc.id}) {
		return
	}

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
			if !rs.ok || !strings.EqualFold(rs.sess.Email, rd.email) {
				send("end", map[string]string{"reason": "signed out"})
				return
			}
			rd.role = rs.sess.Role
			// Demoted while watching: Home's fleet numbers stop here.
			if homeCh != nil && !seesWholeFleet(rd.role) {
				stopHome()
				if !send("home", map[string]any{"off": true}) {
					return
				}
			}
		case on := <-hc.ch:
			stopHome()
			if !on {
				continue
			}
			if !seesWholeFleet(rd.role) {
				if !send("home", map[string]any{"off": true}) {
					return
				}
				continue
			}
			// Subscribing again is also how a console that missed an update
			// asks for the whole dashboard: a new subscription starts with it.
			hs, stop, err := a.homeHub.subscribe(r.Context())
			if err != nil {
				continue
			}
			homeCh, homeStop = hs.ch, stop
		case f, open := <-homeCh:
			if !open {
				homeCh, homeStop = nil, nil
				continue
			}
			if !send("home", f) {
				return
			}
		case ev, open := <-sub.ch:
			if !open {
				// Shut down, or too far behind: the console reconnects.
				return
			}
			if ev.read {
				if p, err := a.store.AlertPrefs(r.Context(), rd.email); err == nil {
					rd.prefs = p
				}
				if !send("read", map[string]any{"popup": rd.prefs.Popup, "sound": rd.prefs.Sound}) {
					return
				}
				continue
			}
			if len(ev.closed) > 0 {
				seqs := []int64{}
				for _, al := range ev.closed {
					if alertVisible(rd, al) {
						seqs = append(seqs, al.Seq)
					}
				}
				if len(seqs) > 0 && !send("closed", map[string]any{"seqs": seqs}) {
					return
				}
				continue
			}
			if ev.alert == nil || !alertVisible(rd, *ev.alert) {
				continue
			}
			if !send("alert", ev.alert) {
				return
			}
		}
	}
}

// homeControl is one open stream that can carry Home's live dashboard.
type homeControl struct {
	id     string
	email  string
	tenant string
	ch     chan bool
}

func newStreamID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// postStreamHome switches Home's live dashboard on or off on one of the
// caller's own alert streams. Switching it on again sends the whole
// dashboard, which is how a console that missed an update catches up.
//
// The id is not a credential: the stream must be the caller's, in the
// organization the caller is signed in to, and the caller must be one of the
// roles that see the whole fleet. Someone else's id answers 404, as if it did
// not exist.
func (a *API) postStreamHome(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		On bool `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, found := a.homeStreams.Load(r.PathValue("id"))
	hc, _ := v.(*homeControl)
	if !found || hc == nil || !strings.EqualFold(hc.email, sess.Email) || hc.tenant != tenantIDOf(r.Context()) {
		writeErr(w, http.StatusNotFound, "no such stream; it may have closed, and the console opens a new one")
		return
	}
	if in.On && !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "Home's live dashboard is for owners, admins and auditors")
		return
	}
	// The newest wish wins: a pending one is replaced, never queued.
	select {
	case <-hc.ch:
	default:
	}
	select {
	case hc.ch <- in.On:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// getHome serves Home's live dashboard whole, for the first paint and for a
// console that cannot hold a stream open. The fleet roles only, like the
// stream.
//
// While anyone in the organization watches, it is the hub's last build,
// lines and all, and costs nothing; otherwise one read, with whatever lines
// the hub still holds.
func (a *API) getHome(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "Home's live dashboard is for owners, admins and auditors")
		return
	}
	f, lines := a.homeHub.current(tenantIDOf(r.Context()))
	if f != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(f)
		return
	}
	var h HomeSnapshot
	if err := a.store.homeFast(r.Context(), &h); err != nil {
		a.fail(w, "home", err)
		return
	}
	if err := a.store.homeSlow(r.Context(), &h, time.Now()); err != nil {
		a.fail(w, "home", err)
		return
	}
	h.At = time.Now().UTC()
	h.Trends = HomeTrends{Answering: []int{}, Live: []int{}, Waiting: []int{}, Processor: []*float64{}}
	if lines != nil {
		_ = json.Unmarshal(lines, &h.Trends)
	}
	writeJSON(w, http.StatusOK, h)
}
