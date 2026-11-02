package agent

import (
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

func bp(b bool) *bool { return &b }

// The statements the helper builds, from hostile input. The password is the
// one thing that must never appear in the redacted text.
func TestAdminSQLBuilders(t *testing.T) {
	secret := `it's a \ "secret" pass`
	op := &tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionCreate, Name: "orders_from_eu",
		Conn:         &tunnel.PGConnInfo{Host: "eu-db-01", Port: 5432, DBName: "orders", User: "replicator", Password: secret, SSLMode: "require"},
		Publications: []string{"orders_pub", "Odd Name"}, CopyData: bp(true), CreateSlot: bp(false), SlotName: "slot_1"}
	b, err := buildAdminSQL(op, "r.hakim@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.run) != 2 || !strings.HasPrefix(b.run[0], `CREATE SUBSCRIPTION "orders_from_eu" CONNECTION E'host=''eu-db-01'' port=5432 dbname=''orders'' user=''replicator'' password=''it\\''s a \\\\ "secret" pass'' sslmode=require' PUBLICATION "orders_pub", "Odd Name" WITH (`) {
		t.Fatalf("unexpected statement: %q", b.run)
	}
	if !strings.Contains(b.run[0], "create_slot = false, copy_data = true, slot_name = E'slot_1'") {
		t.Fatalf("options: %q", b.run[0])
	}
	if !strings.Contains(b.run[1], `COMMENT ON SUBSCRIPTION "orders_from_eu" IS E'argus-managed by=r.hakim@corp.example'`) {
		t.Fatalf("comment: %q", b.run[1])
	}
	red := b.text()
	if strings.Contains(red, "secret") || strings.Contains(red, "COMMENT") || !strings.Contains(red, "password=[redacted]") {
		t.Fatalf("the redacted text leaks or is wrong: %q", red)
	}

	// Identifiers Argus did not make are quoted, never trusted.
	b, err = buildAdminSQL(&tunnel.PGAdminOp{Kind: tunnel.AdminExtensionCreate, Name: `uuid-ossp"; DROP TABLE x; --`, Schema: "public", Version: "1.1", Cascade: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `CREATE EXTENSION IF NOT EXISTS "uuid-ossp""; DROP TABLE x; --" SCHEMA "public" VERSION E'1.1' CASCADE`; b.run[0] != want {
		t.Fatalf("got %q, want %q", b.run[0], want)
	}
	b, err = buildAdminSQL(&tunnel.PGAdminOp{Kind: tunnel.AdminPublicationCreate, Name: "p", Tables: []string{"public.orders", `sales."a.b"`}, Publish: []string{"INSERT", "update", "insert"}}, "x@y")
	if err != nil {
		t.Fatal(err)
	}
	if want := `CREATE PUBLICATION "p" FOR TABLE "public"."orders", "sales"."""a.b""" WITH (publish = E'insert, update')`; b.run[0] != want {
		t.Fatalf("got %q, want %q", b.run[0], want)
	}
	b, _ = buildAdminSQL(&tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionDrop, Name: "s", PublisherGone: true}, "")
	if len(b.run) != 3 || b.run[1] != `ALTER SUBSCRIPTION "s" SET (slot_name = NONE)` {
		t.Fatalf("publisher-gone drop: %q", b.run)
	}

	// Refusals.
	for _, bad := range []*tunnel.PGAdminOp{
		{Kind: "role.create", Name: "x"},
		{Kind: tunnel.AdminExtensionCreate},
		{Kind: tunnel.AdminExtensionCreate, Name: "x", Version: "1.0'; DROP"},
		{Kind: tunnel.AdminPublicationCreate, Name: "p"},
		{Kind: tunnel.AdminPublicationCreate, Name: "p", Tables: []string{"orders"}},
		{Kind: tunnel.AdminPublicationCreate, Name: "p", Tables: []string{"a.b"}, Publish: []string{"drop"}},
		{Kind: tunnel.AdminSubscriptionCreate, Name: "s", Publications: []string{"p"}},
		{Kind: tunnel.AdminSubscriptionCreate, Name: "s", Publications: []string{"p"}, Conn: &tunnel.PGConnInfo{Host: "h", DBName: "d", User: "u", SSLMode: "yes"}},
		{Kind: tunnel.AdminSubscriptionCreate, Name: "s", Conn: &tunnel.PGConnInfo{Host: "h", DBName: "d", User: "u"}},
		{Kind: tunnel.AdminExtensionDrop, Name: strings.Repeat("x", 64)},
	} {
		if _, err := buildAdminSQL(bad, ""); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	// Lists build nothing and are not mutations.
	if b, err := buildAdminSQL(&tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionList}, ""); err != nil || len(b.run) != 0 {
		t.Fatal("a list built a statement")
	}
	if (&tunnel.PGAdminOp{Kind: tunnel.AdminExtensionList}).Mutates() || !(&tunnel.PGAdminOp{Kind: tunnel.AdminExtensionDrop}).Mutates() {
		t.Fatal("Mutates is wrong")
	}
}

func TestRedactConninfo(t *testing.T) {
	cases := map[string]string{
		`host=eu-db-01 port=5432 dbname=orders user=replicator password=hunter2 sslmode=require`: `host=eu-db-01 port=5432 dbname=orders user=replicator password=[redacted] sslmode=require`,
		`host='eu-db-01' user='r' password='it\'s a \\ pass' dbname=x`:                           `host='eu-db-01' user='r' password=[redacted] dbname=x`,
		`PASSWORD = topsecret host=h`:                                          `PASSWORD = [redacted] host=h`,
		`postgresql://replicator:hunter2@eu-db-01:5432/orders?sslmode=require`: `postgresql://replicator:[redacted]@eu-db-01:5432/orders?sslmode=require`,
		`postgres://u@h/d`:       `postgres://u@h/d`,
		`host=h user=u dbname=d`: `host=h user=u dbname=d`,
	}
	for in, want := range cases {
		if got := redactConninfo(in); got != want {
			t.Errorf("redactConninfo(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(redactConninfo(in), "hunter2") || strings.Contains(redactConninfo(in), "topsecret") {
			t.Errorf("leaked: %q", in)
		}
	}
	if got := publisherOf(`host='eu-db-01' port=5432 dbname=orders user='replicator' password='x y' sslmode=require`); got != "replicator@eu-db-01:5432/orders" {
		t.Fatalf("publisherOf = %q", got)
	}
	if got := publisherOf(`user=u password=hunter2 host=h`); got != "u@h" || strings.Contains(got, "hunter2") {
		t.Fatalf("publisherOf = %q", got)
	}
}
