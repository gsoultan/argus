package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

/*
A person's layout choices (cards or table, sidebar open or folded) follow them
to their next sign-in, and only the settings and values the console has can
be stored.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run UIPreference
*/

func TestUIPreferenceValuesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{"assets.view", "cards", true},
		{"assets.view", "table", true},
		{"sidebar", "collapsed", true},
		{"assets.view", "grid", false},
		{"assets.view", "", false},
		{"anything", "cards", false},
		{"", "", false},
	} {
		if err := validUIPreference(tc.key, tc.value); (err == nil) != tc.ok {
			t.Errorf("%q=%q: err %v, want ok=%v", tc.key, tc.value, err, tc.ok)
		}
	}
}

func TestAUIPreferenceIsKeptForTheNextSignIn(t *testing.T) {
	s := testStore(t)
	a := NewAPI(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	alice, _ := newAccount(t, s, "operator")
	bob, _ := newAccount(t, s, "operator")
	a.UserTokens = map[string]string{"alice": alice, "bob": bob}
	h := a.Handler()

	read := func(token string) map[string]string {
		t.Helper()
		w := do(t, h, http.MethodGet, "/api/v1/me/preferences", token, "")
		if w.Code != http.StatusOK {
			t.Fatalf("get: %d %s", w.Code, w.Body)
		}
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got
	}

	// Never chosen: the default.
	if got := read("alice"); got["assets.view"] != "cards" || got["sidebar"] != "expanded" {
		t.Fatalf("defaults = %v", got)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/me/preferences", "alice", `{"key":"assets.view","value":"table"}`); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	// A new request (the next sign-in reads the same way) sees the choice;
	// someone else does not.
	if got := read("alice"); got["assets.view"] != "table" {
		t.Fatalf("after choosing table, alice reads %v", got)
	}
	if got := read("bob"); got["assets.view"] != "cards" {
		t.Fatalf("alice's choice reached bob: %v", got)
	}
	// The latest choice wins.
	do(t, h, http.MethodPut, "/api/v1/me/preferences", "alice", `{"key":"assets.view","value":"cards"}`)
	if got := read("alice"); got["assets.view"] != "cards" {
		t.Fatalf("changing back did not stick: %v", got)
	}

	// Refused: a setting or value the console does not have, and no sign-in.
	if w := do(t, h, http.MethodPut, "/api/v1/me/preferences", "alice", `{"key":"assets.view","value":"<script>"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown value was stored: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/me/preferences", "alice", `{"key":"theme","value":"dark"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown setting was stored: %d", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/api/v1/me/preferences", "", `{"key":"assets.view","value":"table"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("a preference was stored with no sign-in: %d", w.Code)
	}
}
