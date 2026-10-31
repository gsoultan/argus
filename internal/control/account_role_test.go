package control

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// Roles are changed from the People page. Each refusal here is a way the page
// could otherwise be used to take over a deployment or lock everyone out.

func TestAnAdminChangesSomeonesRole(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	who, _ := newAccount(t, s, "operator")

	w := do(t, h, http.MethodPut, "/api/v1/users/"+who+"/role", "admin-token", `{"role":"approver"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	acct, err := s.Account(context.Background(), who)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Role != "approver" {
		t.Errorf("role = %q, want approver", acct.Role)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'account.role_changed' AND target = $1`,
		who).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit entries = %d, want 1", n)
	}
}

func TestNobodyChangesTheirOwnRole(t *testing.T) {
	_, _, h, admin := enrollAPI(t)
	w := do(t, h, http.MethodPut, "/api/v1/users/"+admin+"/role", "admin-token", `{"role":"owner"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an admin changed their own role: status %d", w.Code)
	}
}

func TestOnlyAnOwnerMakesAnOwner(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	who, _ := newAccount(t, s, "operator")
	w := do(t, h, http.MethodPut, "/api/v1/users/"+who+"/role", "admin-token", `{"role":"owner"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an admin made an owner: status %d", w.Code)
	}
}

func TestAnOperatorCannotChangeRoles(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	op, _ := newAccount(t, s, "operator")
	other, _ := newAccount(t, s, "operator")
	a.UserTokens = map[string]string{"operator-token": op}
	w := do(t, a.Handler(), http.MethodPut, "/api/v1/users/"+other+"/role", "operator-token", `{"role":"admin"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an operator changed a role: status %d", w.Code)
	}
}

func TestTheLastAdministratorCannotBeDemoted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	solo, _ := newAccount(t, s, "admin")
	rows, err := s.pool.Query(ctx, `SELECT email FROM users WHERE role IN ('owner','admin')
		AND disabled_at IS NULL AND lower(email) <> lower($1)`, solo)
	if err != nil {
		t.Fatal(err)
	}
	var parked []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		parked = append(parked, e)
	}
	rows.Close()
	for _, e := range parked {
		if err := s.SetAccountDisabled(ctx, e, true); err != nil {
			t.Fatalf("parking %s: %v", e, err)
		}
	}
	t.Cleanup(func() {
		for _, e := range parked {
			_ = s.SetAccountDisabled(context.Background(), e, false)
		}
	})
	if _, err := s.SetAccountRole(ctx, solo, "operator"); err != ErrLastAdmin {
		t.Fatalf("err = %v, want ErrLastAdmin", err)
	}
}
