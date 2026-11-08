package control

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// cleanAlerts empties the bell's tables, which these tests share with every
// other run against the same database.
func cleanAlerts(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{`DELETE FROM console_alert_reads`, `DELETE FROM console_alerts`, `DELETE FROM console_alert_prefs`} {
		if _, err := s.db(ctx).Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// What the list query returns and what the stream pushes are decided by two
// copies of one rule. Every combination of audience, actor, role and
// preference is put through both; a difference is a leak or a lost alert.
func TestAlertVisibilityIsTheSameInSQLAndInMemory(t *testing.T) {
	s := testStore(t)
	cleanAlerts(t, s)
	ctx := context.Background()

	var all []ConsoleAlert
	for _, aud := range []string{AudienceDeciders, AudienceAdmins, AudiencePerson} {
		for _, actor := range []string{"", "ann@x.id", "bob@x.id"} {
			for _, kind := range AlertKinds {
				for _, sev := range []string{"info", "warning"} {
					al := ConsoleAlert{Kind: kind, Severity: sev, Audience: aud, Actor: actor, Title: "t"}
					if aud == AudiencePerson {
						al.Recipient = "ann@x.id"
					}
					out, err := s.InsertConsoleAlert(ctx, al)
					if err != nil {
						t.Fatal(err)
					}
					all = append(all, out)
				}
			}
		}
	}

	prefs := []AlertPrefs{
		DefaultAlertPrefs,
		{Muted: []string{AlertSession, AlertSecurity}},
		{Muted: []string{}, NewDevicesOnly: true},
		{Muted: []string{AlertApproval, AlertSignIn, AlertServer}, NewDevicesOnly: true},
	}
	for _, role := range []string{"viewer", "operator", "approver", "auditor", "admin", "owner"} {
		for _, email := range []string{"ann@x.id", "Bob@X.id", "cy@x.id"} {
			for _, p := range prefs {
				rd := alertReader{email: email, role: role, prefs: p}
				page, err := s.ConsoleAlerts(ctx, rd, false, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				var got, want []int64
				for _, al := range page.Alerts {
					got = append(got, al.Seq)
				}
				for i := len(all) - 1; i >= 0; i-- {
					if alertVisible(rd, all[i]) {
						want = append(want, all[i].Seq)
					}
				}
				// One page is at most 100: with every kind, an admin sees more,
				// and the page is the newest 100 of them.
				if len(want) > 100 {
					want = want[:100]
				}
				if !slices.Equal(got, want) {
					t.Fatalf("%s as %s with %+v: SQL shows %d alerts, memory %d", email, role, p, len(got), len(want))
				}
			}
		}
	}
}

// The rule itself, spelled out for the cases that matter.
func TestWhoSeesWhichAlert(t *testing.T) {
	ask := ConsoleAlert{Kind: AlertApproval, Severity: "warning", Audience: AudienceDeciders, Actor: "op@x.id"}
	signin := ConsoleAlert{Kind: AlertSignIn, Severity: "info", Audience: AudienceAdmins, Actor: "op@x.id"}
	sec := ConsoleAlert{Kind: AlertSecurity, Severity: "critical", Audience: AudienceAdmins}
	mine := ConsoleAlert{Kind: AlertApproval, Audience: AudiencePerson, Recipient: "op@x.id"}
	r := func(email, role string, p AlertPrefs) alertReader { return alertReader{email, role, p} }
	d := DefaultAlertPrefs
	cases := []struct {
		name string
		rd   alertReader
		al   ConsoleAlert
		want bool
	}{
		{"an approver is asked", r("ap@x.id", "approver", d), ask, true},
		{"an auditor cannot decide, so is not asked", r("au@x.id", "auditor", d), ask, false},
		{"nobody is asked to decide their own request", r("op@x.id", "admin", d), ask, false},
		{"an operator does not hear of sign-ins", r("o2@x.id", "operator", d), signin, false},
		{"an approver does not hear of sign-ins", r("ap@x.id", "approver", d), signin, false},
		{"an admin hears of sign-ins", r("ad@x.id", "admin", d), signin, true},
		{"an admin who wants only new devices skips a known one", r("ad@x.id", "admin", AlertPrefs{NewDevicesOnly: true}), signin, false},
		{"security cannot be muted", r("ad@x.id", "admin", AlertPrefs{Muted: []string{AlertSecurity}}), sec, true},
		{"the requester hears the decision", r("OP@x.id", "operator", d), mine, true},
		{"nobody else does", r("ad@x.id", "owner", d), mine, false},
		{"a muted kind is not shown", r("op@x.id", "operator", AlertPrefs{Muted: []string{AlertApproval}}), mine, false},
	}
	for _, c := range cases {
		if got := alertVisible(c.rd, c.al); got != c.want {
			t.Errorf("%s: visible = %v", c.name, got)
		}
	}
}

func TestReadStateAndUnreadCount(t *testing.T) {
	s := testStore(t)
	cleanAlerts(t, s)
	ctx := context.Background()
	rd := alertReader{email: "ad@x.id", role: "admin", prefs: DefaultAlertPrefs}
	var seqs []int64
	for range 4 {
		al, err := s.InsertConsoleAlert(ctx, ConsoleAlert{Kind: AlertServer, Audience: AudienceAdmins, Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, al.Seq)
	}
	unread := func() int {
		n, err := s.UnreadConsoleAlerts(ctx, rd)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := unread(); n != 4 {
		t.Fatalf("unread = %d, want 4", n)
	}
	if err := s.MarkConsoleAlertsRead(ctx, "AD@x.id", []int64{seqs[3]}, 0); err != nil {
		t.Fatal(err)
	}
	if n := unread(); n != 3 {
		t.Fatalf("after reading one, unread = %d, want 3", n)
	}
	page, _ := s.ConsoleAlerts(ctx, rd, true, 0, 50)
	if len(page.Alerts) != 3 || page.Alerts[0].Seq != seqs[2] {
		t.Fatalf("unread list = %+v", page.Alerts)
	}
	if err := s.MarkConsoleAlertsRead(ctx, "ad@x.id", nil, seqs[2]); err != nil {
		t.Fatal(err)
	}
	if n := unread(); n != 0 {
		t.Fatalf("after mark all read, unread = %d", n)
	}
	// Someone else's state is their own.
	if n, _ := s.UnreadConsoleAlerts(ctx, alertReader{email: "ad2@x.id", role: "admin", prefs: DefaultAlertPrefs}); n != 4 {
		t.Fatalf("another admin's unread = %d, want 4", n)
	}
	// Paging.
	page, _ = s.ConsoleAlerts(ctx, rd, false, 0, 2)
	if len(page.Alerts) != 2 || !page.More || !page.Alerts[0].Read {
		t.Fatalf("first page = %+v more=%v", page.Alerts, page.More)
	}
	page, _ = s.ConsoleAlerts(ctx, rd, false, page.Alerts[1].Seq, 2)
	if len(page.Alerts) != 2 || page.More {
		t.Fatalf("second page = %+v more=%v", page.Alerts, page.More)
	}
}

func TestPreferencesRefuseUnknownKindsAndKeepSecurityOn(t *testing.T) {
	s := testStore(t)
	cleanAlerts(t, s)
	ctx := context.Background()
	if _, err := s.SetAlertPrefs(ctx, "p@x.id", AlertPrefs{Muted: []string{"everything"}}); err != ErrUnknownAlertKind {
		t.Fatalf("err = %v", err)
	}
	got, err := s.SetAlertPrefs(ctx, "p@x.id", AlertPrefs{Muted: []string{AlertSecurity, AlertSession, AlertSession}, Sound: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Muted, []string{AlertSession}) || !got.Sound {
		t.Fatalf("saved %+v", got)
	}
	back, _ := s.AlertPrefs(ctx, "P@x.id")
	if !slices.Equal(back.Muted, []string{AlertSession}) || back.Popup || !back.Sound {
		t.Fatalf("read back %+v", back)
	}
}

func TestSweepDropsOldAlertsAndTheirReads(t *testing.T) {
	s := testStore(t)
	cleanAlerts(t, s)
	ctx := context.Background()
	old, _ := s.InsertConsoleAlert(ctx, ConsoleAlert{Kind: AlertServer, Audience: AudienceAdmins, Title: "old"})
	fresh, _ := s.InsertConsoleAlert(ctx, ConsoleAlert{Kind: AlertServer, Audience: AudienceAdmins, Title: "new"})
	if _, err := s.db(ctx).Exec(ctx, `UPDATE console_alerts SET at = now() - interval '31 days' WHERE seq = $1`, old.Seq); err != nil {
		t.Fatal(err)
	}
	_ = s.MarkConsoleAlertsRead(ctx, "ad@x.id", []int64{old.Seq}, 0)
	if n, err := s.SweepConsoleAlerts(ctx); err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	page, _ := s.ConsoleAlerts(ctx, alertReader{email: "ad@x.id", role: "admin", prefs: DefaultAlertPrefs}, false, 0, 50)
	if len(page.Alerts) != 1 || page.Alerts[0].Seq != fresh.Seq {
		t.Fatalf("left %+v", page.Alerts)
	}
}

/* ── The stream ──────────────────────────────────────────────────────────── */

type sseEvent struct{ name, data string }

// openStream signs in and opens the bell's stream, returning its events.
func openStream(t *testing.T, h *loginHarness) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/api/v1/alerts/stream", nil)
	res, err := h.client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	ch := make(chan sseEvent, 16)
	go func() {
		defer close(ch)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[len("data: "):]
			case line == "" && ev.name != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch, cancel
}

func next(t *testing.T, ch <-chan sseEvent) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}, false
}

func alertHarness(t *testing.T, role string) (*loginHarness, string) {
	t.Helper()
	h := newLoginHarness(t)
	h.api.RequireMFA = MFAOff
	cleanAlerts(t, h.store)
	email, password := newAccount(t, h.store, role)
	signIn(t, h, email, password, chromeMac)
	return h, email
}

// A stream carries what the reader may see, and nothing else.
func TestTheStreamCarriesOnlyWhatTheReaderMaySee(t *testing.T) {
	h, me := alertHarness(t, "approver")
	events, stop := openStream(t, h)
	defer stop()
	if ev, _ := next(t, events); ev.name != "ready" {
		t.Fatalf("first event %q", ev.name)
	}
	ctx := WithTenant(context.Background(), &Tenant{ID: DefaultTenantID})
	// An approver hears of a request, not of a sign-in, nor of another
	// person's decision, nor of their own request.
	h.api.raiseAlert(ctx, ConsoleAlert{Kind: AlertSignIn, Audience: AudienceAdmins, Title: "signin"})
	h.api.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Audience: AudiencePerson, Recipient: "else@x.id", Title: "theirs"})
	h.api.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Audience: AudienceDeciders, Actor: me, Title: "own ask"})
	h.api.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Audience: AudienceDeciders, Actor: "op@x.id", Title: "ask"})
	ev, _ := next(t, events)
	var got ConsoleAlert
	if err := json.Unmarshal([]byte(ev.data), &got); err != nil || ev.name != "alert" || got.Title != "ask" {
		t.Fatalf("got %q %s", ev.name, ev.data)
	}
	if strings.Contains(ev.data, "op@x.id") || strings.Contains(ev.data, "audience") {
		t.Fatalf("the stream leaks who may see an alert: %s", ev.data)
	}

	// Marking read in another tab tells this one.
	res, err := h.client.Post(h.srv.URL+"/api/v1/alerts/read", "application/json", strings.NewReader(`{"upTo":`+itoa(int(got.Seq))+`}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mark read = %d", res.StatusCode)
	}
	if ev, _ := next(t, events); ev.name != "read" {
		t.Fatalf("after marking read: %q", ev.name)
	}
}

// A signed-out session stops hearing anything at the next check.
func TestTheStreamEndsWhenTheSessionDoes(t *testing.T) {
	prev := alertStreamRecheck
	alertStreamRecheck = 100 * time.Millisecond
	defer func() { alertStreamRecheck = prev }()

	h, me := alertHarness(t, "admin")
	events, stop := openStream(t, h)
	defer stop()
	next(t, events) // ready
	if err := h.store.RevokeAllSessions(context.Background(), me); err != nil {
		t.Fatal(err)
	}
	if ev, _ := next(t, events); ev.name != "end" {
		t.Fatalf("got %q", ev.name)
	}
	if _, open := next(t, events); open {
		t.Fatal("the stream stayed open after the session ended")
	}
}

// Shutdown ends every stream, or it would wait its whole deadline on them.
func TestStopAlertStreamsEndsThem(t *testing.T) {
	h, _ := alertHarness(t, "admin")
	events, stop := openStream(t, h)
	defer stop()
	next(t, events)
	h.api.StopAlertStreams()
	if _, open := next(t, events); open {
		t.Fatal("stream still open after StopAlertStreams")
	}
	res, err := h.client.Get(h.srv.URL + "/api/v1/alerts/stream")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a stream opened during shutdown = %d", res.StatusCode)
	}
}

// One account cannot open streams without limit.
func TestStreamsPerAccountAreBounded(t *testing.T) {
	var h alertHub
	ctx := WithTenant(context.Background(), &Tenant{ID: DefaultTenantID})
	var cancels []func()
	for range maxAlertStreamsPerAccount {
		_, c, err := h.subscribe(ctx, "a@x.id")
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	if _, _, err := h.subscribe(ctx, "A@x.id"); err != ErrTooManyAlertStreams {
		t.Fatalf("one too many: %v", err)
	}
	if _, c, err := h.subscribe(ctx, "b@x.id"); err != nil {
		t.Fatalf("another account: %v", err)
	} else {
		c()
	}
	for _, c := range cancels {
		c()
	}
	if h.total != 0 || len(h.tenants) != 0 {
		t.Fatalf("left %d streams in %d organizations", h.total, len(h.tenants))
	}
}

// A stream that stops reading is dropped, not waited for.
func TestASlowStreamIsDroppedNotWaitedFor(t *testing.T) {
	var h alertHub
	ctx := WithTenant(context.Background(), &Tenant{ID: DefaultTenantID})
	sub, cancel, err := h.subscribe(ctx, "a@x.id")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			h.publish(DefaultTenantID, ConsoleAlert{Seq: int64(i + 1)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a stream nobody reads")
	}
	n := 0
	for range sub.ch {
		n++
	}
	if n != cap(sub.ch) {
		t.Fatalf("read %d before the close, want %d", n, cap(sub.ch))
	}
}
