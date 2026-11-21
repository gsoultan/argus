package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Website checks: a site is never called down when it is not. Each test runs
in an organization of its own.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Website
*/

func TestWebsiteInputNormalise(t *testing.T) {
	in := WebsiteInput{URL: "shop.example.com/health"}
	if err := in.normalise(); err != nil {
		t.Fatal(err)
	}
	if in.URL != "https://shop.example.com/health" || in.Name != "shop.example.com" ||
		in.EverySeconds != 60 || in.TimeoutSeconds != tunnel.SiteTimeoutDefault {
		t.Errorf("defaults: %+v", in)
	}
	for _, bad := range []WebsiteInput{
		{URL: "https://example.com", EverySeconds: 30},
		{URL: "https://example.com", TimeoutSeconds: 2},
		{URL: "https://example.com", ConfirmFrom: []string{"a", "b", "c", "d"}},
		{URL: "http://169.254.169.254/"},
		{URL: "ftp://example.com/"},
	} {
		if err := bad.normalise(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// websiteEnv is an organization with one check and a probe the test
// decides the answers of.
type websiteEnv struct {
	api   *API
	ctx   context.Context
	check WebsiteCheck
	// answers are what the probe returns, in turn; the last repeats.
	answers []tunnel.SiteResult
	calls   atomic.Int32
}

func newWebsiteEnv(t *testing.T, in WebsiteInput) *websiteEnv {
	t.Helper()
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	e := &websiteEnv{ctx: WithTenant(context.Background(), tn)}
	e.api = NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := websiteRetryGap
	websiteRetryGap = time.Millisecond
	t.Cleanup(func() { websiteRetryGap = old })
	e.api.websiteProbe = func(context.Context, tunnel.GrantSite) tunnel.SiteResult {
		n := int(e.calls.Add(1)) - 1
		if n >= len(e.answers) {
			n = len(e.answers) - 1
		}
		return e.answers[n]
	}
	// Argus's own network works unless a test says otherwise.
	e.api.websites.canary.lookupNow = func(context.Context) (bool, bool) { return true, true }
	if in.URL == "" {
		in.URL = "https://shop.example.com/"
	}
	c, err := s.CreateWebsiteCheck(e.ctx, in, owner)
	if err != nil {
		t.Fatalf("CreateWebsiteCheck: %v", err)
	}
	e.check = c
	return e
}

var (
	siteUp   = tunnel.SiteResult{OK: true, Status: 200, Detail: "answered 200 OK", MS: 120}
	site502  = tunnel.SiteResult{Status: 502, Fail: tunnel.SiteFailStatus, Detail: "answered 502 Bad Gateway", MS: 80}
	siteGone = tunnel.SiteResult{Fail: tunnel.SiteFailConnect, Detail: "connection refused", MS: 3}
)

// round runs one round as the loop would, at a time of the test's choice.
func (e *websiteEnv) round(t *testing.T, at time.Time) WebsiteCheck {
	t.Helper()
	r := e.api.websiteRound(e.ctx, e.check)
	r.At = at
	tr, err := e.api.store.RecordWebsiteRound(e.ctx, e.check.ID, r)
	if err != nil {
		t.Fatalf("RecordWebsiteRound: %v", err)
	}
	if tr != nil {
		e.api.announceWebsite(e.ctx, *tr)
	}
	c, err := e.api.store.WebsiteCheck(e.ctx, e.check.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *websiteEnv) rounds(t *testing.T) []WebsiteRound {
	t.Helper()
	rs, err := e.api.store.WebsiteRounds(e.ctx, e.check.ID, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func (e *websiteEnv) alerts(t *testing.T) []ConsoleAlert {
	t.Helper()
	rows, err := e.api.store.db(e.ctx).Query(e.ctx, `SELECT `+alertColumns+`
		FROM console_alerts a WHERE a.kind = 'website' ORDER BY a.seq`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := collectAlerts(rows)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWebsiteBlipIsNotAFailure(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{})
	e.answers = []tunnel.SiteResult{site502, siteUp}
	c := e.round(t, time.Now().Add(-time.Minute))
	if c.State != "up" || c.FailingRounds != 0 {
		t.Errorf("a failure that a retry cleared is up: %s, %d failing", c.State, c.FailingRounds)
	}
	if got := e.calls.Load(); got != 2 {
		t.Errorf("tried %d times, want 2", got)
	}
	if rs := e.rounds(t); len(rs) != 1 || rs[0].Outcome != RoundUp {
		t.Errorf("rounds = %+v", rs)
	}
}

func TestWebsiteDownNeedsTwoConfirmedRoundsAndComesBack(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{Name: "Shop"})
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	e.answers = []tunnel.SiteResult{siteUp}
	e.round(t, base)

	e.answers, e.calls = []tunnel.SiteResult{site502}, atomic.Int32{}
	c := e.round(t, base.Add(time.Minute))
	if c.State != "up" || c.FailingRounds != 1 {
		t.Fatalf("one failed round is not down yet: %s, %d", c.State, c.FailingRounds)
	}
	if got := e.calls.Load(); got != websiteRetries+1 {
		t.Errorf("a failing site is tried %d times in a round, want %d", got, websiteRetries+1)
	}
	if len(e.alerts(t)) != 0 {
		t.Fatal("nobody is told after one failed round")
	}
	c = e.round(t, base.Add(2*time.Minute))
	if c.State != "down" || c.StateSince == nil || !c.StateSince.Equal(base.Add(time.Minute)) {
		t.Fatalf("two failed rounds are down since the first: %s since %v", c.State, c.StateSince)
	}
	als := e.alerts(t)
	if len(als) != 1 || als[0].Severity != "critical" || !strings.HasPrefix(als[0].Title, "Shop (") || !strings.HasSuffix(als[0].Title, ") is down") ||
		!strings.Contains(als[0].Link, e.check.ID) {
		t.Fatalf("one alert that it is down: %+v", als)
	}
	// Still down: no second alert.
	e.round(t, base.Add(3*time.Minute))
	if len(e.alerts(t)) != 1 {
		t.Error("an outage is announced once")
	}

	e.answers = []tunnel.SiteResult{siteUp}
	c = e.round(t, base.Add(4*time.Minute))
	if c.State != "up" {
		t.Fatalf("a good round ends it: %s", c.State)
	}
	als = e.alerts(t)
	if len(als) != 2 || !strings.Contains(als[1].Title, "back up after 3 minutes") || als[0].Closed == "" {
		t.Fatalf("back up is announced and closes the down alert: %+v", als)
	}
	outages, _ := e.api.store.WebsiteOutages(e.ctx, e.check.ID, 10)
	if len(outages) != 1 || outages[0].EndedAt == nil || !outages[0].StartedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("one closed outage from the first failed round: %+v", outages)
	}
	// 2 rounds up, 3 down: the first failed round counts as down now.
	var outcomes []string
	for _, r := range e.rounds(t) {
		outcomes = append(outcomes, r.Outcome)
	}
	if strings.Join(outcomes, ",") != "up,down,down,down,up" {
		t.Errorf("rounds = %v", outcomes)
	}
	up, _ := e.api.store.WebsiteUptimes(e.ctx, time.Now())
	if d := up[e.check.ID].Day; d == nil || *d != 40 {
		t.Errorf("uptime over the day = %v, want 40", d)
	}
}

// Argus's own network failing is never the site being down.
func TestWebsiteArgusNetworkDownIsUnknownNotDown(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{})
	e.answers = []tunnel.SiteResult{siteUp}
	base := time.Now().UTC().Add(-10 * time.Minute)
	e.round(t, base)
	e.api.refreshCanaries(e.ctx) // the canaries answered here before

	e.answers = []tunnel.SiteResult{siteGone}
	e.api.websites.canary.lookupNow = func(context.Context) (bool, bool) { return false, false }
	e.api.websites.canary.at = time.Time{}
	for i := 1; i <= 4; i++ {
		c := e.round(t, base.Add(time.Duration(i)*time.Minute))
		if c.State != "unknown" {
			t.Fatalf("round %d: %s, want unknown", i, c.State)
		}
	}
	if len(e.alerts(t)) != 0 {
		t.Error("nobody is told a site is down when Argus cannot check")
	}
	up, _ := e.api.store.WebsiteUptimes(e.ctx, time.Now())
	if d := up[e.check.ID].Day; d == nil || *d != 100 {
		t.Errorf("unknown rounds count neither way: %v", d)
	}
	// A status failure is the site's, whatever the canaries say.
	e.answers = []tunnel.SiteResult{site502}
	e.round(t, base.Add(5*time.Minute))
	if c := e.round(t, base.Add(6*time.Minute)); c.State != "down" {
		t.Errorf("a site answering 502 is down even when the canaries are quiet: %s", c.State)
	}
}

// On a network that never reached the canaries (no internet), their
// silence proves nothing, so failures still count.
func TestWebsiteCanaryThatNeverAnsweredIsIgnored(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{URL: "http://intranet.local/"})
	e.api.websites.canary.lookupNow = func(context.Context) (bool, bool) { return false, false }
	e.answers = []tunnel.SiteResult{siteGone}
	base := time.Now().UTC().Add(-5 * time.Minute)
	e.round(t, base)
	if c := e.round(t, base.Add(time.Minute)); c.State != "down" {
		t.Errorf("an intranet site that refuses connections is down: %s", c.State)
	}
}

// A chosen server that reaches the site means it is not down from
// everywhere: Argus's location is the problem.
func TestWebsiteServerThatReachesItIsPartialNotDown(t *testing.T) {
	s := testStore(t)
	_ = s
	e := newWebsiteEnv(t, WebsiteInput{})
	asset, err := e.api.store.CreateAsset(e.ctx, AssetInput{Hostname: unique("web") + ".example.com"})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	in := WebsiteInput{Name: e.check.Name, URL: e.check.URL, ConfirmFrom: []string{asset.ID}}
	if _, e.check, err = e.api.store.UpdateWebsiteCheck(e.ctx, e.check.ID, in); err != nil {
		t.Fatalf("UpdateWebsiteCheck: %v", err)
	}
	asked := atomic.Int32{}
	serverSees := siteUp
	e.api.websiteAsk = func(_ context.Context, host string, _ tunnel.GrantSite) serverOpinion {
		asked.Add(1)
		return serverOpinion{host: host, res: serverSees}
	}
	e.answers = []tunnel.SiteResult{siteGone}
	base := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < 3; i++ {
		c := e.round(t, base.Add(time.Duration(i)*time.Minute))
		if c.State != "up" || c.LastOutcome != RoundPartial || !strings.Contains(c.LastDetail, asset.Hostname+" could") {
			t.Fatalf("round %d: %s %q", i, c.State, c.LastDetail)
		}
	}
	if asked.Load() != 3 || len(e.alerts(t)) != 0 {
		t.Errorf("asked %d times, %d alerts", asked.Load(), len(e.alerts(t)))
	}
	// When the server agrees, it is down, and the outage says who agreed.
	serverSees = siteGone
	e.round(t, base.Add(3*time.Minute))
	e.round(t, base.Add(4*time.Minute))
	outages, _ := e.api.store.WebsiteOutages(e.ctx, e.check.ID, 10)
	if len(outages) != 1 || !strings.Contains(outages[0].Confirmed, asset.Hostname) {
		t.Fatalf("outage confirmed by Argus and the server: %+v", outages)
	}
	// A server that cannot be asked does not stop the outage being found,
	// and is named as not asked.
	e.api.websiteAsk = func(_ context.Context, host string, _ tunnel.GrantSite) serverOpinion {
		return serverOpinion{host: host, err: "its agent is not connected"}
	}
	c := e.round(t, base.Add(5*time.Minute))
	if c.State != "down" || !strings.Contains(c.LastDetail, "could not be asked") {
		t.Errorf("%s %q", c.State, c.LastDetail)
	}
}

func TestWebsiteLeaseRunsEachRoundOnce(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{})
	first, err := e.api.store.TakeDueWebsiteChecks(e.ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("take = %v, %v", first, err)
	}
	if again, _ := e.api.store.TakeDueWebsiteChecks(e.ctx, 10); len(again) != 0 {
		t.Fatal("a leased check is not taken twice")
	}
	e.answers = []tunnel.SiteResult{siteUp}
	e.api.runWebsiteRound(e.ctx, first[0])
	if again, _ := e.api.store.TakeDueWebsiteChecks(e.ctx, 10); len(again) != 0 {
		t.Fatal("a check just run is not due for a minute")
	}
	if err := e.api.store.CheckWebsiteNow(e.ctx, e.check.ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.api.store.TakeDueWebsiteChecks(e.ctx, 10); len(again) != 1 {
		t.Fatal("Check now makes it due")
	}
	// A round cut short by a shutdown records nothing and gives it back.
	cctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.api.runWebsiteRound(cctx, e.check)
	if rs := e.rounds(t); len(rs) != 1 {
		t.Errorf("a cancelled round recorded something: %+v", rs)
	}
}

// Adding, changing and removing a check is in the audit log with who did
// it; an operator may not.
func TestWebsiteRoutesAreAuditedAndGuarded(t *testing.T) {
	h := newTenantHarness(t)
	tn, owner, pw := newTenant(t, h.store)
	c := h.signIn(t, tn.Slug, owner, pw)
	code, body := h.call(t, c, "", http.MethodPost, "/api/v1/websites", `{"url":"https://shop.example.com","keyword":"Buy"}`)
	if code != http.StatusCreated {
		t.Fatalf("add = %d %s", code, body)
	}
	var got WebsiteCheck
	_ = json.Unmarshal([]byte(body), &got)
	code, body = h.call(t, c, "", http.MethodPut, "/api/v1/websites/"+got.ID,
		`{"name":"Shop","url":"https://shop.example.com","everySeconds":300}`)
	if code != http.StatusOK {
		t.Fatalf("change = %d %s", code, body)
	}
	code, body = h.call(t, c, "", http.MethodGet, "/api/v1/websites", "")
	if code != http.StatusOK || !strings.Contains(body, `"everySeconds":300`) {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, _ = h.call(t, c, "", http.MethodDelete, "/api/v1/websites/"+got.ID, ""); code != http.StatusNoContent {
		t.Fatalf("remove = %d", code)
	}
	ctx := WithTenant(context.Background(), tn)
	evs, err := h.store.AuditEvents(ctx, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if strings.HasPrefix(ev.Action, "website.") {
			if !strings.EqualFold(ev.ActorEmail, owner) {
				t.Errorf("%s by %s, want %s", ev.Action, ev.ActorEmail, owner)
			}
			seen[ev.Action] = true
		}
	}
	for _, a := range []string{"website.add", "website.change", "website.remove"} {
		if !seen[a] {
			t.Errorf("no %s in the audit log", a)
		}
	}

	opEmail := unique("op") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, opEmail, "Op", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	op := h.signIn(t, tn.Slug, opEmail, "correct horse battery staple")
	if code, _ := h.call(t, op, "", http.MethodPost, "/api/v1/websites", `{"url":"https://x.example.com"}`); code != http.StatusForbidden {
		t.Errorf("an operator adding a check = %d, want 403", code)
	}
	// Someone outside the fleet roles sees only the checks they were given:
	// here none (checks_access_test.go gives some).
	if code, body := h.call(t, op, "", http.MethodGet, "/api/v1/websites", ""); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("an operator reading checks = %d %s, want 200 []", code, body)
	}
}
