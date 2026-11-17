package control

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

/*
Removing a database someone has already used.

Sessions and access requests pointed at the database with a plain foreign
key, so once anyone had opened it, or asked to, the DELETE broke that key and
the console showed "internal error". Removing it keeps those records, as the
button says, and only lets go of the link.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run RemoveUsedDatabase
*/

func TestRemoveUsedDatabaseKeepsItsSessions(t *testing.T) {
	_, s, h, db, _ := dbFixture(t)
	ctx := context.Background()

	sessID := newUUID(t)
	dropSession(t, s, sessID)
	if err := s.UpsertSession(ctx, Session{
		ID: sessID, UserEmail: "dewi.p@northwind.id", AssetHostname: db.Hostname,
		Principal: "orders", Protocol: "postgres", Origin: "brokered", State: "closed",
		StartedAt: time.Now().UTC().Add(-time.Minute), Fidelity: "statements", RiskFlags: []string{},
		ReportedBy: "agent", Kind: "database", DatabaseID: &db.ID,
	}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	var reqID string
	if err := s.pool.QueryRow(ctx, `INSERT INTO access_requests
		(requester_email, principal, justification, duration_minutes, database_id)
		VALUES ('dewi.p@northwind.id', 'orders', 'month end', 60, $1::uuid) RETURNING id::text`,
		db.ID).Scan(&reqID); err != nil {
		t.Fatalf("insert access request: %v", err)
	}

	w := do(t, h, http.MethodDelete, "/api/v1/databases/"+db.ID, "admin-token", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("remove a database with a session and a request: %d %s", w.Code, w.Body)
	}
	if _, err := s.Database(ctx, db.ID); err != ErrDatabaseNotFound {
		t.Fatalf("the database is still there: %v", err)
	}

	got, err := s.Session(ctx, sessID)
	if err != nil || got == nil {
		t.Fatalf("the session went with the database: %v %v", got, err)
	}
	if got.DatabaseID != nil || got.AssetHostname != db.Hostname {
		t.Errorf("session after removal: database %v, host %q", got.DatabaseID, got.AssetHostname)
	}
	if _, dbID, err := s.requestDatabase(ctx, reqID); err != nil || dbID != "" {
		t.Errorf("access request after removal: database %q, err %v", dbID, err)
	}
	if e := lastAuditOn(t, s, db.Hostname); e.Action != "db.removed" {
		t.Errorf("last audit on %s: %s", db.Hostname, e.Action)
	}
}

// Anything that still blocks a removal says what it is, not "internal error".
func TestRemoveBlockedDatabaseSaysWhy(t *testing.T) {
	_, s, h, db, _ := dbFixture(t)
	ctx := context.Background()
	table := "remove_block_" + newUUID(t)[:8]
	if _, err := s.pool.Exec(ctx, `CREATE TABLE `+table+` (database_id UUID REFERENCES databases(id))`); err != nil {
		t.Fatalf("create blocking table: %v", err)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table) })
	if _, err := s.pool.Exec(ctx, `INSERT INTO `+table+` VALUES ($1::uuid)`, db.ID); err != nil {
		t.Fatalf("insert blocking row: %v", err)
	}

	w := do(t, h, http.MethodDelete, "/api/v1/databases/"+db.ID, "admin-token", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still has records that point at this database") {
		t.Fatalf("a blocked removal: %d %s", w.Code, w.Body)
	}
	if _, err := s.Database(ctx, db.ID); err != nil {
		t.Fatalf("the database should still be there: %v", err)
	}
}
