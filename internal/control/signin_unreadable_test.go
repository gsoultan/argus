package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A control plane whose database cannot be read must not tell the sign-in
// page that nobody has an account. It used to: the error was dropped and
// "no" went out, so a working deployment with Postgres down showed the
// first-run page and its `users add` command, and looked wiped.
func TestSignInPageSaysTheDatabaseIsUnreadableNotThatItIsEmpty(t *testing.T) {
	// Nothing listens on port 1, so every query fails, as with Postgres
	// stopped. pgxpool connects lazily, so this opens without one.
	cfg, err := pgxpool.ParseConfig("postgres://argus:argus@127.0.0.1:1/argus?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.ConnectTimeout = time.Second
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	api := NewAPI(newStore(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	api.SetAuth(signer, "", false)
	// A setup offer is on the table, as on a host where the token file was
	// written: even then an unreadable database must not send anyone to it.
	api.OfferSetup("setup-token", "")
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me map[string]any
	if err := json.NewDecoder(res.Body).Decode(&me); err != nil {
		t.Fatalf("status %d, body not JSON: %v", res.StatusCode, err)
	}
	if me["databaseUnreadable"] != true {
		t.Errorf("databaseUnreadable = %v, want true; body %v", me["databaseUnreadable"], me)
	}
	if v, ok := me["accountsExist"]; ok {
		t.Errorf("accountsExist = %v; with the database unreadable it must be left out, not answered", v)
	}
	if me["setupAvailable"] == true {
		t.Error("an unreadable database offered first-run setup")
	}
}
