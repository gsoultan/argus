package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A check can send a method, headers and a body. Header values are sealed,
// never returned, kept across an edit that does not retype them, and sent
// with every round; without them a round is "could not check", not down.
func TestWebsiteRequestPayload(t *testing.T) {
	h := newTenantHarness(t)
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	h.api.Tunnels = &TunnelHub{GrantKey: key}
	var sent []tunnel.GrantSite
	h.api.websiteProbe = func(_ context.Context, g tunnel.GrantSite) tunnel.SiteResult {
		sent = append(sent, g)
		return tunnel.SiteResult{OK: true, Status: 200, Detail: "answered 200 OK", Method: g.Method}
	}
	tn, owner, pw := newTenant(t, h.store)
	c := h.signIn(t, tn.Slug, owner, pw)
	ctx := WithTenant(context.Background(), tn)

	add := `{"url":"https://api.example.com/health","method":"post","body":"{\"ping\":1}",
		"headers":[{"name":"X-Api-Key","value":"s3cret-key"},{"name":"Accept","value":"application/json"}]}`
	code, body := h.call(t, c, "", http.MethodPost, "/api/v1/websites", add)
	if code != http.StatusCreated {
		t.Fatalf("add = %d %s", code, body)
	}
	if strings.Contains(body, "s3cret-key") {
		t.Fatalf("a header's value came back: %s", body)
	}
	var got WebsiteCheck
	_ = json.Unmarshal([]byte(body), &got)
	if got.Method != "POST" || got.Body != `{"ping":1}` || strings.Join(got.HeaderNames, ",") != "X-Api-Key,Accept" {
		t.Fatalf("saved as %+v", got)
	}
	var sealed []byte
	if err := h.store.db(ctx).QueryRow(ctx, `SELECT headers_sealed FROM website_checks WHERE id::text = $1`, got.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, []byte("s3cret")) {
		t.Fatal("the headers are not sealed")
	}

	// A round sends all of it.
	saved, _ := h.store.WebsiteCheck(ctx, got.ID)
	if r := h.api.websiteRound(ctx, saved); r.Outcome != RoundUp {
		t.Fatalf("round = %+v", r)
	}
	g := sent[len(sent)-1]
	if g.Method != "POST" || g.Body != `{"ping":1}` || len(g.Headers) != 2 || g.Headers[0].Value != "s3cret-key" {
		t.Fatalf("the round sent %+v", g)
	}

	// Editing without the headers keeps them; keeping one by name keeps its
	// value; a new one is added; a dropped one is gone.
	code, body = h.call(t, c, "", http.MethodPut, "/api/v1/websites/"+got.ID,
		`{"url":"https://api.example.com/health","method":"POST","body":"{\"ping\":2}"}`)
	if code != http.StatusOK || !strings.Contains(body, `"headerNames":["X-Api-Key","Accept"]`) {
		t.Fatalf("change without headers = %d %s", code, body)
	}
	code, body = h.call(t, c, "", http.MethodPut, "/api/v1/websites/"+got.ID,
		`{"url":"https://api.example.com/health","method":"POST","body":"{\"ping\":2}",
		  "headers":[{"name":"X-Api-Key","keep":true},{"name":"X-Trace","value":"argus"}]}`)
	if code != http.StatusOK || !strings.Contains(body, `"headerNames":["X-Api-Key","X-Trace"]`) {
		t.Fatalf("change with headers = %d %s", code, body)
	}
	saved, _ = h.store.WebsiteCheck(ctx, got.ID)
	g, err = h.api.websiteGrant(ctx, saved)
	if err != nil || len(g.Headers) != 2 || g.Headers[0].Value != "s3cret-key" || g.Headers[1].Value != "argus" || g.Body != `{"ping":2}` {
		t.Fatalf("after the edit it sends %+v (%v)", g, err)
	}

	// Testing an edit sends the kept value too.
	code, body = h.call(t, c, "", http.MethodPost, "/api/v1/websites/test?id="+got.ID,
		`{"url":"https://api.example.com/health","method":"POST","headers":[{"name":"X-Api-Key","keep":true}]}`)
	if code != http.StatusOK || sent[len(sent)-1].Headers[0].Value != "s3cret-key" {
		t.Fatalf("test = %d %s, sent %+v", code, body, sent[len(sent)-1])
	}

	for _, bad := range []string{
		`{"url":"https://a.example.com","method":"DELETE"}`,
		`{"url":"https://a.example.com","body":"x"}`,
		`{"url":"https://a.example.com","method":"HEAD","keyword":"ok"}`,
		`{"url":"https://a.example.com","method":"POST","headers":[{"name":"Host","value":"x"}]}`,
		`{"url":"https://a.example.com","method":"POST","headers":[{"name":"X-Key","value":""}]}`,
		`{"url":"https://a.example.com","method":"POST","headers":[{"name":"X-Key","keep":true}]}`,
	} {
		if code, body := h.call(t, c, "", http.MethodPost, "/api/v1/websites", bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s", bad, code, body)
		}
	}

	// Headers that cannot be opened any more are not sent without: the round
	// could not check, which is never down.
	other, _ := tunnel.GrantKeyFromSecret("another-signing-secret-that-is-long-enough-9876")
	h.api.Tunnels = &TunnelHub{GrantKey: other}
	before := len(sent)
	if r := h.api.websiteRound(ctx, saved); r.Outcome != RoundUnknown || !strings.Contains(r.Detail, "signing_secret") {
		t.Errorf("round with unreadable headers = %+v", r)
	}
	if len(sent) != before {
		t.Error("a round with unreadable headers fetched the site anyway")
	}

	evs, _ := h.store.AuditEvents(ctx, 20, 0)
	for _, e := range evs {
		if strings.Contains(e.Detail, "s3cret") {
			t.Errorf("a header's value is in the audit log: %s", e.Detail)
		}
	}
}
