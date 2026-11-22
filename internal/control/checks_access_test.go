package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Someone outside the fleet roles sees only the checks they, or a team of
// theirs, were given, one at a time or through a group; sees them limited;
// and checks one now or pauses it only when given that too.
func TestChecksAreSeenOnlyByWhoeverWasGivenThem(t *testing.T) {
	h := newTenantHarness(t)
	tn, owner, pw := newTenant(t, h.store)
	ctx := WithTenant(context.Background(), tn)
	admin := h.signIn(t, tn.Slug, owner, pw)

	add := func(name, url string) WebsiteCheck {
		t.Helper()
		code, body := h.call(t, admin, "", http.MethodPost, "/api/v1/websites",
			`{"name":"`+name+`","url":"`+url+`","method":"POST","body":"secret=1"}`)
		if code != http.StatusCreated {
			t.Fatalf("add %s = %d %s", name, code, body)
		}
		var c WebsiteCheck
		_ = json.Unmarshal([]byte(body), &c)
		return c
	}
	shop, blog, api := add("Shop", "https://shop.example.com"), add("Blog", "https://blog.example.com"), add("API", "https://api.example.com")

	person := func(role string) (string, *http.Client) {
		t.Helper()
		email := unique(role) + "@northwind.id"
		if _, err := h.store.CreateAccount(ctx, email, role, role, "correct horse battery staple"); err != nil {
			t.Fatal(err)
		}
		return email, h.signIn(t, tn.Slug, email, "correct horse battery staple")
	}
	devEmail, dev := person("operator")
	teamEmail, teammate := person("operator")
	_, stranger := person("operator")

	names := func(c *http.Client) []string {
		t.Helper()
		code, body := h.call(t, c, "", http.MethodGet, "/api/v1/websites", "")
		if code != http.StatusOK {
			t.Fatalf("list = %d %s", code, body)
		}
		var list []WebsiteCheck
		_ = json.Unmarshal([]byte(body), &list)
		var out []string
		for _, c := range list {
			out = append(out, c.Name)
			if !c.Limited || c.Body != "" {
				t.Errorf("%s shown to someone outside the fleet roles unlimited: %+v", c.Name, c)
			}
		}
		return out
	}

	// The shop to the developer, by name; the API to a team, through a group.
	if code, body := h.call(t, admin, "", http.MethodPut, "/api/v1/checks/"+shop.ID+"/viewers",
		`[{"email":"`+strings.ToUpper(devEmail)+`","canOperate":true}]`); code != http.StatusOK {
		t.Fatalf("give shop = %d %s", code, body)
	}
	teamID, err := h.store.CreateTeam(ctx, "Platform", []string{teamEmail}, owner)
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.call(t, admin, "", http.MethodPost, "/api/v1/check-groups", `{"name":"Public APIs"}`)
	if code != http.StatusCreated {
		t.Fatalf("add group = %d %s", code, body)
	}
	var g CheckGroup
	_ = json.Unmarshal([]byte(body), &g)
	if code, _ := h.call(t, admin, "", http.MethodPost, "/api/v1/check-groups", `{"name":"public apis"}`); code != http.StatusBadRequest {
		t.Errorf("a second group differing only in case = %d, want 400", code)
	}
	if code, body := h.call(t, admin, "", http.MethodPut, "/api/v1/checks/"+api.ID+"/group", `{"groupId":"`+g.ID+`"}`); code != http.StatusNoContent {
		t.Fatalf("move api = %d %s", code, body)
	}
	if code, body := h.call(t, admin, "", http.MethodPut, "/api/v1/check-groups/"+g.ID+"/viewers",
		`[{"teamId":"`+teamID+`"}]`); code != http.StatusOK {
		t.Fatalf("give group = %d %s", code, body)
	}

	if got := names(dev); strings.Join(got, ",") != "Shop" {
		t.Errorf("developer sees %v, want [Shop]", got)
	}
	if got := names(teammate); strings.Join(got, ",") != "API" {
		t.Errorf("teammate sees %v, want [API]", got)
	}
	if got := names(stranger); len(got) != 0 {
		t.Errorf("someone given nothing sees %v", got)
	}

	// One check: theirs opens, another is not there at all.
	if code, _ := h.call(t, dev, "", http.MethodGet, "/api/v1/websites/"+shop.ID, ""); code != http.StatusOK {
		t.Errorf("developer opening the shop = %d", code)
	}
	if code, _ := h.call(t, dev, "", http.MethodGet, "/api/v1/websites/"+blog.ID, ""); code != http.StatusNotFound {
		t.Errorf("developer opening the blog = %d, want 404", code)
	}

	// Operating: the developer was given it; the teammate only to see.
	if code, _ := h.call(t, dev, "", http.MethodPost, "/api/v1/websites/"+shop.ID+"/check", ""); code != http.StatusAccepted {
		t.Errorf("developer checking the shop now = %d", code)
	}
	if code, _ := h.call(t, dev, "", http.MethodPost, "/api/v1/websites/"+shop.ID+"/pause", `{"paused":true}`); code != http.StatusNoContent {
		t.Errorf("developer pausing the shop = %d", code)
	}
	if c, _ := h.store.WebsiteCheck(ctx, shop.ID); !c.Paused {
		t.Error("the shop is not paused")
	}
	if code, _ := h.call(t, teammate, "", http.MethodPost, "/api/v1/websites/"+api.ID+"/check", ""); code != http.StatusForbidden {
		t.Errorf("teammate checking the API now = %d, want 403", code)
	}
	if code, _ := h.call(t, teammate, "", http.MethodPost, "/api/v1/websites/"+blog.ID+"/check", ""); code != http.StatusNotFound {
		t.Errorf("teammate checking the blog now = %d, want 404", code)
	}
	// Giving is for admins and owners.
	if code, _ := h.call(t, dev, "", http.MethodPut, "/api/v1/checks/"+blog.ID+"/viewers", `[{"email":"`+devEmail+`"}]`); code != http.StatusForbidden {
		t.Errorf("developer giving themselves the blog = %d, want 403", code)
	}

	// Their Monitoring menu entry, and their own access list.
	for _, tc := range []struct {
		c    *http.Client
		want bool
	}{{dev, true}, {teammate, true}, {stranger, false}, {admin, true}} {
		_, body := h.call(t, tc.c, "", http.MethodGet, "/auth/me", "")
		if got := strings.Contains(body, `"seesChecks":true`); got != tc.want {
			t.Errorf("seesChecks = %v, want %v: %s", got, tc.want, body)
		}
	}
	sections, err := h.store.PersonAccess(ctx, teamEmail)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range sections {
		if sec.Kind == "checks" {
			if len(sec.Items) != 1 || sec.Items[0].Name != "API" || sec.Items[0].Where != "Public APIs" ||
				strings.Join(sec.Items[0].Teams, ",") != "Platform" || sec.Items[0].Own {
				t.Errorf("teammate's Monitoring access = %+v", sec.Items)
			}
		}
	}

	// Removing the group takes the API away from the team; the check stays.
	if code, _ := h.call(t, admin, "", http.MethodDelete, "/api/v1/check-groups/"+g.ID, ""); code != http.StatusNoContent {
		t.Fatalf("remove group = %d", code)
	}
	if got := names(teammate); len(got) != 0 {
		t.Errorf("after the group went, teammate sees %v", got)
	}
	if c, err := h.store.WebsiteCheck(ctx, api.ID); err != nil || c.GroupID != "" {
		t.Errorf("the API after its group went: %+v %v", c, err)
	}

	evs, err := h.store.AuditEvents(ctx, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Action] = true
	}
	for _, a := range []string{"check.viewers", "check.group.add", "check.group.move", "check.group.remove", "website.pause"} {
		if !seen[a] {
			t.Errorf("no %s in the audit log", a)
		}
	}
}

// A check going down tells whoever was given it, in their own bell, and
// coming back up closes that alert.
func TestCheckAlertsReachWhoeverWasGivenThem(t *testing.T) {
	e := newWebsiteEnv(t, WebsiteInput{Name: "Shop"})
	email := unique("dev") + "@northwind.id"
	if _, err := e.api.store.CreateAccount(e.ctx, email, "Dev", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.api.store.SetCheckViewers(e.ctx, "check", e.check.ID, []CheckViewer{{Email: email}}, "test"); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	e.answers = []tunnel.SiteResult{siteUp}
	e.round(t, start)
	e.answers, e.calls = []tunnel.SiteResult{site502}, atomic.Int32{}
	e.round(t, start.Add(time.Minute))
	e.round(t, start.Add(2*time.Minute))
	var person []ConsoleAlert
	for _, al := range e.alerts(t) {
		if al.Audience == AudiencePerson {
			person = append(person, al)
		}
	}
	if len(person) != 1 || person[0].Recipient != email || person[0].Subject != websiteSubject(e.check.ID) {
		t.Fatalf("person alerts = %+v", person)
	}
	e.answers = []tunnel.SiteResult{siteUp}
	e.round(t, start.Add(3*time.Minute))
	for _, al := range e.alerts(t) {
		if al.Audience == AudiencePerson && al.Subject != "" && al.ClosedAt == nil {
			t.Errorf("the down alert stayed open after the check came back: %+v", al)
		}
	}
}
