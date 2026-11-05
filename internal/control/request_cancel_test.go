package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

/*
A request someone takes back, and a request someone denies.

Root cause of the 502 on Deny: postDecision built the approval's audit text,
which dereferences the grant's expiry, before it knew the decision was a
denial; a denial has no expiry, so every Deny panicked after the denial had
committed. The proxy in front answered 502, and a second click was told the
request had already been denied.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable go test ./internal/control/ -run Request
*/

// statementFixture is a production database, an asker with write on it, and
// a statement ask waiting for a decision.
func statementFixture(t *testing.T) (*API, *Store, http.Handler, Database, string, StatementAsk) {
	t.Helper()
	a, s, h, db, _ := dbFixture(t)
	ana, _ := newAccount(t, s, "operator")
	a.UserTokens["ana"] = ana
	if w := do(t, h, http.MethodPut, "/api/v1/databases/"+db.ID+"/assignments", "admin-token",
		fmt.Sprintf(`{"userEmail":%q,"level":"write"}`, ana)); w.Code >= 300 {
		t.Fatalf("assign: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodPost, "/api/v1/databases/"+db.ID+"/statements", "ana",
		`{"items":[{"sql":"DELETE FROM orders WHERE id = 3","rows":1}],"summary":{"rows":1},"justification":"order 3 was entered twice by the import"}`)
	var ask StatementAsk
	_ = json.Unmarshal(w.Body.Bytes(), &ask)
	if w.Code != http.StatusCreated || ask.RequestID == "" {
		t.Fatalf("ask: %d %s", w.Code, w.Body)
	}
	return a, s, h, db, ana, ask
}

func deciderAlerts(t *testing.T, s *Store, admin string) []ConsoleAlert {
	t.Helper()
	page, err := s.ConsoleAlerts(context.Background(), alertReader{email: admin, role: "admin", prefs: DefaultAlertPrefs}, false, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return page.Alerts
}

func alertFor(als []ConsoleAlert, requestID string) *ConsoleAlert {
	for i := range als {
		if strings.Contains(als[i].Link, requestID) {
			return &als[i]
		}
	}
	return nil
}

func TestDenyingARequestAnswersAndIsAudited(t *testing.T) {
	_, s, h, _, ana, ask := statementFixture(t)

	w := do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/decision", "admin-token",
		`{"decision":"denied","note":"not during the sale"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", w.Code, w.Body)
	}
	var out AccessRequest
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.State != "denied" || out.ExpiresAt != nil {
		t.Fatalf("denied: %+v", out)
	}
	if !auditHas(t, s, "request.deny") {
		t.Fatal("the denial is not in the audit log")
	}
	// The asker is told.
	page, _ := s.ConsoleAlerts(context.Background(), alertReader{email: ana, role: "operator", prefs: DefaultAlertPrefs}, false, 0, 50)
	told := false
	for _, al := range page.Alerts {
		told = told || strings.Contains(al.Title, "was denied")
	}
	if !told {
		t.Fatal("the asker was not told of the denial")
	}

	// A second click says who decided it, as a conflict.
	w = do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/decision", "admin-token",
		`{"decision":"denied","note":"not during the sale"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already denied") {
		t.Fatalf("second deny: %d %s", w.Code, w.Body)
	}
}

func TestTheAskerCancelsAndDecidersAreTold(t *testing.T) {
	a, s, h, db, ana, ask := statementFixture(t)
	admin := a.UserTokens["admin-token"]
	ctx := context.Background()

	al := alertFor(deciderAlerts(t, s, admin), ask.RequestID)
	if al == nil || al.Closed != "" {
		t.Fatalf("the decider's alert before: %+v", al)
	}
	unread := func() *ConsoleAlert {
		page, err := s.ConsoleAlerts(ctx, alertReader{email: admin, role: "admin", prefs: DefaultAlertPrefs}, true, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return alertFor(page.Alerts, ask.RequestID)
	}
	if unread() == nil {
		t.Fatal("the decider's alert is not unread before")
	}

	// Only the asker may take it back.
	if w := do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/cancel", "admin-token", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("someone else cancelled it: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/requests/not-a-uuid/cancel", "ana", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("a malformed id: %d %s", w.Code, w.Body)
	}

	sub, stop, err := a.alertHub.subscribe(PlatformContext(ctx), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	w := do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/cancel", "ana", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	var out AccessRequest
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.State != "cancelled" || out.DecidedAt == nil {
		t.Fatalf("cancelled: %+v", out)
	}
	if !auditHas(t, s, "request.cancel") {
		t.Fatal("the cancellation is not in the audit log")
	}

	// Open consoles are told at once, with the alert that went stale.
	told := false
	for len(sub.ch) > 0 && !told {
		ev := <-sub.ch
		for _, c := range ev.closed {
			told = told || strings.Contains(c.Link, ask.RequestID)
		}
	}
	if !told {
		t.Fatal("the stream was not told the request was cancelled")
	}

	// The decider's alert says so, and no longer counts as waiting on them.
	al = alertFor(deciderAlerts(t, s, admin), ask.RequestID)
	if al == nil || !strings.Contains(al.Closed, "Cancelled") {
		t.Fatalf("the decider's alert after: %+v", al)
	}
	if unread() != nil {
		t.Fatal("a cancelled request's alert still counts as unread")
	}

	// Deciding it now says why there is nothing to decide, and is no 500.
	for _, body := range []string{`{"decision":"denied","note":"not during the sale"}`, `{"decision":"approved"}`} {
		w = do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/decision", "admin-token", body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cancelled") {
			t.Fatalf("deciding a cancelled request: %d %s", w.Code, w.Body)
		}
	}
	if w := do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/cancel", "ana", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("cancel twice: %d %s", w.Code, w.Body)
	}

	// The asker's wait sees it, and the statement cannot run under it.
	w = do(t, h, http.MethodGet, "/api/v1/databases/"+db.ID+"/statements/"+ask.RequestID, "ana", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("the wait: %d %s", w.Code, w.Body)
	}
	if _, err := s.UseStatementApproval(ctx, ask.RequestID, ana, db.ID, "DELETE FROM orders WHERE id = 3", nil); err == nil ||
		!strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("ran under a cancelled ask: %v", err)
	}
}

func TestDecidingClosesTheDecidersAlert(t *testing.T) {
	a, s, h, _, _, ask := statementFixture(t)
	admin := a.UserTokens["admin-token"]
	if w := do(t, h, http.MethodPost, "/api/v1/requests/"+ask.RequestID+"/decision", "admin-token", `{"decision":"approved"}`); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	al := alertFor(deciderAlerts(t, s, admin), ask.RequestID)
	if al == nil || !strings.Contains(al.Closed, "Approved by") {
		t.Fatalf("the decider's alert: %+v", al)
	}
}

func TestOneRequestIsReadByWhoMaySeeIt(t *testing.T) {
	a, s, h, _, _, ask := statementFixture(t)
	cy, _ := newAccount(t, s, "operator")
	a.UserTokens["cy"] = cy
	for who, want := range map[string]int{"ana": http.StatusOK, "admin-token": http.StatusOK, "cy": http.StatusNotFound} {
		w := do(t, h, http.MethodGet, "/api/v1/requests/"+ask.RequestID, who, "")
		if w.Code != want {
			t.Fatalf("%s reads it: %d %s", who, w.Code, w.Body)
		}
		if want == http.StatusOK && !strings.Contains(w.Body.String(), `"statement"`) {
			t.Fatalf("%s reads it without its statements: %s", who, w.Body)
		}
	}
	if w := do(t, h, http.MethodGet, "/api/v1/requests/nope", "admin-token", ""); w.Code != http.StatusNotFound {
		t.Fatalf("a malformed id: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/requests/nope/decision", "admin-token", `{"decision":"approved"}`); w.Code != http.StatusNotFound {
		t.Fatalf("deciding a malformed id: %d %s", w.Code, w.Body)
	}
}

// A handler that panics answers 500 with a body, rather than dropping the
// connection for a proxy to call a bad gateway.
func TestAPanicAnswers500(t *testing.T) {
	a := &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := a.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		var p *int
		_ = *p
	}))
	w := do(t, h, http.MethodGet, "/x", "", "")
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "internal error") {
		t.Fatalf("panic: %d %s", w.Code, w.Body)
	}
}
