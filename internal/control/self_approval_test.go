package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/*
The person who asked approves their own request only when nobody else could.
Each test runs in an organization of its own, so the accounts other tests
leave in the default one do not count as someone else.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run SelfApprov
*/

func TestSelfApprovalCountsEveryoneWhoCouldDecide(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)

	sole := func(email string, roles []string) bool {
		t.Helper()
		ok, err := s.SoleDecider(ctx, email, roles)
		if err != nil {
			t.Fatalf("SoleDecider: %v", err)
		}
		return ok
	}
	if !sole(owner, requestDeciders) || !sole(owner, runDeciders) {
		t.Fatal("the organization's only owner decides alone")
	}
	if !sole(strings.ToUpper(owner), requestDeciders) {
		t.Error("an address is the same person in any case")
	}

	// An operator decides nothing, so is never the one who decides alone.
	operator := unique("op") + "@northwind.id"
	if _, err := s.CreateAccount(ctx, operator, "Operator", "operator", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if sole(operator, requestDeciders) {
		t.Error("an operator was taken for the one who decides")
	}

	// An approver decides requests but not runs.
	approver := unique("ap") + "@northwind.id"
	if _, err := s.CreateAccount(ctx, approver, "Approver", "approver", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if sole(owner, requestDeciders) {
		t.Error("an approver could decide, so the owner does not decide requests alone")
	}
	if !sole(owner, runDeciders) {
		t.Error("an approver cannot decide runs, so the owner still decides those alone")
	}

	// A disabled account decides nothing.
	if err := s.SetAccountDisabled(ctx, approver, true); err != nil {
		t.Fatalf("SetAccountDisabled: %v", err)
	}
	if !sole(owner, requestDeciders) {
		t.Error("a disabled approver was counted")
	}

	// A second admin: the normal rule, for everything.
	admin := unique("ad") + "@northwind.id"
	if _, err := s.CreateAccount(ctx, admin, "Admin", "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if sole(owner, requestDeciders) || sole(owner, runDeciders) || sole(admin, runDeciders) {
		t.Error("with two admins nobody decides alone")
	}
}

func TestApprovingYourOwnNeedsNobodyElseAndThePhrase(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))

	try := func(approve, others bool, roles []string, confirm string) (bool, int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		ok := a.approveOwn(w, r, owner, approve, roles, others, confirm, "refused as usual", "cloud change: remove pay-api-01", "payments-prod")
		return ok, w.Code, w.Body.String()
	}

	if ok, code, _ := try(false, false, requestDeciders, SelfApprovalPhrase); ok || code != http.StatusForbidden {
		t.Errorf("denying your own: ok=%v %d; it is taken back, never denied", ok, code)
	}
	if ok, code, _ := try(true, true, requestDeciders, SelfApprovalPhrase); ok || code != http.StatusForbidden {
		t.Errorf("another editor could decide: ok=%v %d", ok, code)
	}
	ok, code, body := try(true, false, requestDeciders, "")
	if ok || code != http.StatusConflict || !strings.Contains(body, SelfApprovalPhrase) {
		t.Errorf("without the phrase: ok=%v %d %s", ok, code, body)
	}
	if n := auditActions(t, s, ctx, "approval.self_approved"); n != 0 {
		t.Fatalf("recorded a self-approval that did not happen: %d", n)
	}
	if ok, code, body := try(true, false, requestDeciders, "  "+SelfApprovalPhrase+" "); !ok {
		t.Fatalf("the only one who decides, with the phrase: %d %s", code, body)
	}
	events, err := s.AuditEvents(ctx, 20, 0)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	var got *AuditEvent
	for i := range events {
		if events[i].Action == "approval.self_approved" {
			got = &events[i]
		}
	}
	if got == nil || got.Severity != "warning" || got.ActorEmail != owner || got.Target != "payments-prod" ||
		!strings.Contains(got.Detail, "remove pay-api-01") || !strings.Contains(got.Detail, "nobody else") {
		t.Fatalf("self-approval audit = %+v", got)
	}

	// The moment someone else could decide, the normal rule is back, for
	// requests already waiting too.
	approver := unique("ap") + "@northwind.id"
	if _, err := s.CreateAccount(ctx, approver, "Approver", "approver", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if ok, code, body := try(true, false, requestDeciders, SelfApprovalPhrase); ok || code != http.StatusForbidden ||
		!strings.Contains(body, "refused as usual") {
		t.Errorf("with an approver: ok=%v %d %s", ok, code, body)
	}
	if ok, code, body := try(true, false, runDeciders, SelfApprovalPhrase); !ok {
		t.Errorf("an approver cannot decide a run, so the owner still may: %d %s", code, body)
	}
}

// The store lets the person who asked decide only when the caller says so.
func TestDecideRequestTakesYourOwnOnlyWhenAllowed(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)
	req, err := s.CreateRequest(ctx, AccessRequest{RequesterEmail: owner, AssetHostnames: []string{"pay-01"},
		Principal: "root", Justification: "restoring payroll after the failed migration", DurationMinutes: 30})
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if _, err := s.DecideRequest(ctx, req.ID, "approved", "", strings.ToUpper(owner), false); err != ErrSelfApproval {
		t.Fatalf("own request in another case: %v", err)
	}
	out, err := s.DecideRequest(ctx, req.ID, "approved", "", owner, true)
	if err != nil || out.State != "approved" || out.DecidedByEmail == nil || *out.DecidedByEmail != owner {
		t.Fatalf("allowed self-approval: %+v %v", out, err)
	}
}

func auditActions(t *testing.T, s *Store, ctx context.Context, action string) int {
	t.Helper()
	events, err := s.AuditEvents(ctx, 50, 0)
	if err != nil {
		t.Fatalf("AuditEvents: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// A change on a database whose editors approve each other's is the asker's to
// approve only when no other editor could decide it, and the console is told
// exactly that: the list marks the request the way the decision rules on it.
func TestApproveOwnChangeCountsOnlyEditorsWhoCouldDecide(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := WithTenant(context.Background(), tn)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))

	host := unique("db-host")
	asset, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: host, Port: 22, Protocol: "ssh",
		Principals: []string{"ubuntu"}})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	db, err := s.CreateDatabase(ctx, Database{AssetID: asset.ID, Name: "athena_etl_db"}, owner)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db.Policy.WriteApprovers = ApproversEditors
	if db, err = s.UpdateDatabase(ctx, db); err != nil {
		t.Fatalf("UpdateDatabase: %v", err)
	}
	assign := func(email string) {
		t.Helper()
		if err := s.SetDBAssignment(ctx, DBAssignment{DatabaseID: db.ID, UserEmail: &email, Level: "write"}, owner); err != nil {
			t.Fatalf("SetDBAssignment: %v", err)
		}
	}
	assign(owner)
	req, err := s.CreateStatementAsk(ctx, owner, db, []StatementItem{{SQL: "GRANT USAGE ON SCHEMA etl TO hermod"}},
		nil, "assign schema etl to hermod, there is a permission denied")
	if err != nil {
		t.Fatalf("CreateStatementAsk: %v", err)
	}
	marked := func() bool {
		t.Helper()
		reqs := []AccessRequest{req}
		if err := a.markApproveOwn(ctx, owner, reqs); err != nil {
			t.Fatalf("markApproveOwn: %v", err)
		}
		return reqs[0].ApproveOwn
	}
	others := func() []string {
		t.Helper()
		o, err := a.otherEditorsDecide(ctx, owner, req.ID)
		if err != nil {
			t.Fatalf("otherEditorsDecide: %v", err)
		}
		return o
	}
	if !marked() || len(others()) != 0 {
		t.Fatalf("the only owner and only editor: marked=%v others=%v", marked(), others())
	}

	// An address given Write with no account behind it decides nothing.
	assign(unique("gone") + "@northwind.id")
	if !marked() || len(others()) != 0 {
		t.Fatalf("an editor with no account counted: marked=%v others=%v", marked(), others())
	}

	// An operator with Write decides it, so the owner may not.
	operator := unique("op") + "@northwind.id"
	if _, err := s.CreateAccount(ctx, operator, "Operator", "operator", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	assign(operator)
	if marked() || len(others()) != 1 {
		t.Fatalf("another editor could decide: marked=%v others=%v", marked(), others())
	}
	if msg := ownRefusal(ErrSelfApproval, others()); !strings.Contains(msg, strings.ToLower(operator)) {
		t.Errorf("the refusal does not say who decides: %s", msg)
	}

	// Disabled, they decide nothing again.
	if err := s.SetAccountDisabled(ctx, operator, true); err != nil {
		t.Fatalf("SetAccountDisabled: %v", err)
	}
	if !marked() || len(others()) != 0 {
		t.Fatalf("a disabled editor counted: marked=%v others=%v", marked(), others())
	}

	// Someone else's request is never marked, nor a decided one.
	reqs := []AccessRequest{req}
	if err := a.markApproveOwn(ctx, operator, reqs); err != nil || reqs[0].ApproveOwn {
		t.Fatalf("marked for someone who did not ask: %v", err)
	}
	reqs = []AccessRequest{req}
	reqs[0].State = "approved"
	if err := a.markApproveOwn(ctx, owner, reqs); err != nil || reqs[0].ApproveOwn {
		t.Fatalf("a decided request marked: %v", err)
	}
}
