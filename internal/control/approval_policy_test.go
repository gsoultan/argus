package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

/*
The approval policy: who may act directly where Argus would wait for a
second person. Each test runs in an organization of its own.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run ApprovalPolicy
*/

func TestApprovalPolicyEnvironmentsFromTagsAndLabels(t *testing.T) {
	cases := map[string][]string{
		EnvProduction: {"production"},
		EnvStaging:    {"web", "env:staging"},
		EnvOther:      {"web", "linux"},
	}
	for want, tags := range cases {
		if got := envOfTags(tags); got != want {
			t.Errorf("envOfTags(%v) = %q, want %q", tags, got, want)
		}
	}
	if got := envOfTags([]string{"staging", "environment=prod"}); got != EnvProduction {
		t.Errorf("a server tagged as two is the stricter: %q", got)
	}
	if envOfLabel("production") != EnvProduction || envOfLabel("development") != EnvOther || envOfLabel("") != EnvOther {
		t.Error("labels map to the three environments")
	}
}

func TestApprovalPolicyRejectsWhatItDoesNotKnow(t *testing.T) {
	for _, p := range []ApprovalPolicy{
		{Rules: []ApprovalRule{{Kind: "nope", Env: EnvProduction, Choice: ChoiceAdmins}}},
		{Rules: []ApprovalRule{{Kind: KindCloudChange, Env: EnvAll, Choice: ChoiceAdmins}}},
		{Rules: []ApprovalRule{{Kind: KindCloudPurchase, Env: EnvProduction, Choice: ChoiceAdmins}}},
		{Rules: []ApprovalRule{{Kind: KindCloudChange, Env: EnvProduction, Choice: "everyone"}}},
		{Rules: []ApprovalRule{{Kind: KindCloudChange, Env: EnvProduction, Choice: ChoiceAdmins},
			{Kind: KindCloudChange, Env: EnvProduction, Choice: ChoiceOwners}}},
	} {
		if err := p.normalise(); err == nil {
			t.Errorf("accepted %+v", p.Rules)
		}
	}
	p := ApprovalPolicy{Rules: []ApprovalRule{{Kind: KindCloudChange, Env: EnvProduction, Choice: ChoiceSecond}}}
	if err := p.normalise(); err != nil || len(p.Rules) != 0 {
		t.Errorf("a cell set to the default is not stored: %+v %v", p.Rules, err)
	}
}

func TestApprovalPolicyLoosensIsEveryWayMorePeopleActDirectly(t *testing.T) {
	cur := ApprovalPolicy{RequireReason: true, NotifyProduction: true,
		Rules: []ApprovalRule{{Kind: KindDataChange, Env: EnvProduction, Choice: ChoiceAdmins}}}
	tighter := ApprovalPolicy{RequireReason: true, NotifyProduction: true,
		Rules: []ApprovalRule{{Kind: KindDataChange, Env: EnvProduction, Choice: ChoiceOwners}}}
	if l := cur.Loosens(tighter); len(l) != 0 {
		t.Errorf("admins → owners is tighter, not looser: %v", l)
	}
	looser := ApprovalPolicy{RequireReason: false, NotifyProduction: true,
		Rules: []ApprovalRule{{Kind: KindDataChange, Env: EnvProduction, Choice: ChoiceAdmins},
			{Kind: KindCloudChange, Env: EnvStaging, Choice: ChoiceAnyone}}}
	if l := cur.Loosens(looser); len(l) != 2 {
		t.Errorf("a new cell and the reason switch both loosen: %v", l)
	}
}

// Nothing goes straight through until the organization says so, and then
// only for the roles, kinds and environments it named.
func TestApprovalPolicyDecidesWhoActsDirectly(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admin := unique("ad") + "@northwind.id"
	operator := unique("op") + "@northwind.id"
	for email, role := range map[string]string{admin: "admin", operator: "operator"} {
		if _, err := s.CreateAccount(ctx, email, "Someone", role, "correct horse battery staple"); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}

	// The default, as every organization starts after an upgrade.
	for _, who := range []string{owner, admin, operator} {
		if d := a.actsDirectly(ctx, KindCloudChange, EnvProduction, who); d != nil {
			t.Fatalf("%s acts directly under the default policy", who)
		}
	}

	_, err := s.SetApprovalPolicy(ctx, ApprovalPolicy{RequireReason: true, NotifyProduction: true,
		Rules: []ApprovalRule{{Kind: KindCloudChange, Env: EnvProduction, Choice: ChoiceAdmins},
			{Kind: KindCloudPurchase, Env: EnvAll, Choice: ChoiceOwners}}}, owner, "")
	if err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	if a.actsDirectly(ctx, KindCloudChange, EnvProduction, admin) == nil ||
		a.actsDirectly(ctx, KindCloudChange, EnvProduction, owner) == nil {
		t.Error("admins and owners act directly on Production cloud changes")
	}
	if a.actsDirectly(ctx, KindCloudChange, EnvProduction, operator) != nil {
		t.Error("an operator is not an admin")
	}
	if a.actsDirectly(ctx, KindCloudChange, EnvStaging, admin) != nil {
		t.Error("Staging was not loosened")
	}
	if a.actsDirectly(ctx, KindDataChange, EnvProduction, admin) != nil {
		t.Error("another kind was not loosened")
	}
	if a.actsDirectly(ctx, KindCloudPurchase, EnvProduction, owner) == nil {
		t.Error("a kind with no environments is read from its one column")
	}
	if a.actsDirectly(ctx, KindCloudPurchase, EnvAll, admin) != nil {
		t.Error("purchases were loosened for owners only")
	}
	if a.actsDirectly(ctx, "made.up", EnvProduction, owner) != nil {
		t.Error("a kind the policy does not know waits")
	}
	if a.actsDirectlyWithReason(ctx, KindCloudChange, EnvProduction, admin, "  ") != nil {
		t.Error("the policy asks for a reason, and none was given")
	}

	// A disabled admin acts on nothing.
	if err := s.SetAccountDisabled(ctx, admin, true); err != nil {
		t.Fatalf("SetAccountDisabled: %v", err)
	}
	if a.actsDirectly(ctx, KindCloudChange, EnvProduction, admin) != nil {
		t.Error("a disabled account acted directly")
	}

	// A platform lock wins over the organization's choice.
	pctx := PlatformContext(context.Background())
	if err := s.SetApprovalLocks(pctx, []ApprovalLock{{Kind: KindCloudPurchase, Env: EnvAll, Reason: "contract"}},
		"platform@northwind.id"); err != nil {
		t.Fatalf("SetApprovalLocks: %v", err)
	}
	t.Cleanup(func() { _ = s.SetApprovalLocks(pctx, nil, "test") })
	if a.actsDirectly(ctx, KindCloudPurchase, EnvAll, owner) != nil {
		t.Error("a locked kind went straight through")
	}

	// Recording names the rule that allowed it.
	d := a.actsDirectly(ctx, KindCloudChange, EnvProduction, owner)
	a.recordDirect(ctx, d, owner, "aws prod pay-api-01", "Resized pay-api-01", "checkout is slow")
	events, err := s.AuditEvents(ctx, 20, 0)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	var got *AuditEvent
	for i := range events {
		if events[i].Action == "approval.direct" {
			got = &events[i]
		}
	}
	if got == nil || got.ActorEmail != owner || !strings.Contains(got.Detail, "No second person") ||
		!strings.Contains(got.Detail, "Production · Changing resources · admins and owners act directly") ||
		!strings.Contains(got.Detail, "checkout is slow") {
		t.Fatalf("direct action audit = %+v", got)
	}
}

// A request filed where the policy lets its asker act directly is approved
// at once, as them; anywhere else it waits as before.
func TestApprovalPolicyApprovesALoginRequestAtOnce(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prod, dev := unique("prod"), unique("dev")
	for host, tags := range map[string][]string{prod: {"production"}, dev: {"dev"}} {
		if _, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "10.0.0.1", Port: 22, Protocol: "ssh",
			OS: "linux", Tags: tags, Principals: []string{"root"}}); err != nil {
			t.Fatalf("CreateAsset: %v", err)
		}
	}
	ask := func(host string) AccessRequest {
		t.Helper()
		req, err := s.CreateRequest(ctx, AccessRequest{RequesterEmail: owner, AssetHostnames: []string{host},
			Principal: "root", Justification: "restoring payroll after the failed migration", DurationMinutes: 30})
		if err != nil {
			t.Fatalf("CreateRequest: %v", err)
		}
		return req
	}
	if _, ok := a.approveByPolicy(ctx, ask(prod), "", "root"); ok {
		t.Fatal("approved under the default policy")
	}
	if _, err := s.SetApprovalPolicy(ctx, ApprovalPolicy{RequireReason: true, NotifyProduction: true,
		Rules: []ApprovalRule{{Kind: KindElevatedLogin, Env: EnvOther, Choice: ChoiceOwners}}}, owner, ""); err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	if _, ok := a.approveByPolicy(ctx, ask(prod), "", "root"); ok {
		t.Error("a Production server was approved by a rule for Development")
	}
	out, ok := a.approveByPolicy(ctx, ask(dev), "", "root")
	if !ok || out.State != "approved" || out.DecidedByEmail == nil || *out.DecidedByEmail != owner ||
		out.ExpiresAt == nil || out.DecisionNote == nil || !strings.Contains(*out.DecisionNote, "approval policy") {
		t.Fatalf("approved by the policy: %+v", out)
	}
	if n := auditActions(t, s, ctx, "approval.direct"); n != 1 {
		t.Errorf("approval.direct entries = %d, want 1", n)
	}
	if _, ok := a.approveByPolicy(ctx, ask(unique("nowhere")), "", "root"); ok {
		t.Error("a server Argus does not know has no environment, so it waits")
	}
}

// Owners change the policy. Asking for more approval applies at once;
// letting more people act directly waits for another owner, or the phrase
// when there is none.
func TestApprovalPolicyChangesNeedAnotherOwnerToLoosen(t *testing.T) {
	h := newTenantHarness(t)
	tn, owner, pw := newTenant(t, h.store)
	ctx := WithTenant(context.Background(), tn)
	in := h.signIn(t, tn.Slug, owner, pw)

	looser := `{"rules":[{"kind":"cloud.change","env":"production","choice":"admins"}],
		"requireReason":true,"notifyProduction":true,"reason":"four people; nights block fixes"}`

	// The only owner: the phrase, and a record that nobody else agreed.
	code, body := h.call(t, in, "", http.MethodPut, "/api/v1/approval-policy", looser)
	if code != http.StatusConflict || !strings.Contains(body, PolicySelfPhrase) {
		t.Fatalf("sole owner without the phrase = %d %s", code, body)
	}
	code, body = h.call(t, in, "", http.MethodPut, "/api/v1/approval-policy",
		strings.Replace(looser, `"reason"`, `"confirmSelf":"`+PolicySelfPhrase+`","reason"`, 1))
	if code != http.StatusOK || !strings.Contains(body, `"applied":true`) {
		t.Fatalf("sole owner with the phrase = %d %s", code, body)
	}

	// An admin may not change it, and a second owner now decides loosening.
	admin, apw := unique("ad")+"@northwind.id", "correct horse battery staple"
	second := unique("ow") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, admin, "Admin", "admin", apw); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateAccount(ctx, second, "Second", "owner", apw); err != nil {
		t.Fatal(err)
	}
	asAdmin := h.signIn(t, tn.Slug, admin, apw)
	if code, _ := h.call(t, asAdmin, "", http.MethodPut, "/api/v1/approval-policy", `{"rules":[]}`); code != http.StatusForbidden {
		t.Errorf("an admin changed the policy: %d", code)
	}

	// Tighter applies at once, with nobody else.
	code, body = h.call(t, in, "", http.MethodPut, "/api/v1/approval-policy",
		`{"rules":[{"kind":"cloud.change","env":"production","choice":"owners"}],"requireReason":true,"notifyProduction":true}`)
	if code != http.StatusOK || !strings.Contains(body, `"applied":true`) {
		t.Fatalf("tightening = %d %s", code, body)
	}

	// Looser waits.
	code, body = h.call(t, in, "", http.MethodPut, "/api/v1/approval-policy", looser)
	if code != http.StatusAccepted {
		t.Fatalf("loosening with another owner = %d %s", code, body)
	}
	var res struct {
		Pending ApprovalPolicyChange `json:"pending"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if res.Pending.ID == "" {
		t.Fatalf("no pending change in %s", body)
	}
	p, _ := h.store.ApprovalPolicy(ctx)
	if p.choice(KindCloudChange, EnvProduction) != ChoiceOwners {
		t.Fatal("a loosening took effect before another owner agreed")
	}
	path := "/api/v1/approval-policy/changes/" + res.Pending.ID
	if code, _ := h.call(t, in, "", http.MethodPost, path+"/approve", ""); code != http.StatusForbidden {
		t.Errorf("the owner who asked approved their own change: %d", code)
	}
	asSecond := h.signIn(t, tn.Slug, second, apw)
	if code, body := h.call(t, asSecond, "", http.MethodPost, path+"/approve", ""); code != http.StatusOK {
		t.Fatalf("another owner approves = %d %s", code, body)
	}
	p, _ = h.store.ApprovalPolicy(ctx)
	if p.choice(KindCloudChange, EnvProduction) != ChoiceAdmins {
		t.Errorf("approved change not in effect: %+v", p.Rules)
	}
	if n := auditActions(t, h.store, ctx, "approval_policy.changed"); n != 3 {
		t.Errorf("approval_policy.changed entries = %d, want 3", n)
	}

	// Everyone signed in reads it.
	if code, body := h.call(t, asAdmin, "", http.MethodGet, "/api/v1/approval-policy", ""); code != http.StatusOK ||
		!strings.Contains(body, `"canChange":false`) || !strings.Contains(body, `"logins.elevated"`) {
		t.Errorf("an admin reads the policy = %d %s", code, body)
	}
}
