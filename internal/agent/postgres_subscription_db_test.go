//go:build !windows

package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Each shape the stats row has had reads as the counts it holds, and a
// subscription without a stats row reads as unknown.
func TestSubscriptionErrorColsOnServer(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	ctx := context.Background()
	conn, err := pgConnect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	cols, _ := subscriptionErrorCols(15)
	for _, c := range []struct {
		name, row, apply, sync string
	}{
		{"15 to 18", "SELECT 2::bigint AS apply_error_count, 3::bigint AS sync_error_count", "2", "3"},
		{"split", "SELECT 2::bigint AS apply_error_count, 3::bigint AS sync_table_error_count, 4::bigint AS sync_seq_error_count", "2", "7"},
		{"tables only", "SELECT 0::bigint AS apply_error_count, 5::bigint AS sync_table_error_count", "0", "5"},
		{"no stats row", "SELECT 1::bigint AS apply_error_count, 1::bigint AS sync_error_count WHERE false", "", ""},
	} {
		sql := "SELECT " + cols + " FROM (SELECT 1) s LEFT JOIN (" + c.row + ") ss ON true"
		res := pgRead(ctx, conn, sql, nil)
		if res.Err != nil {
			t.Fatalf("%s: %v", c.name, res.Err)
		}
		if got := [2]string{string(res.Rows[0][0]), string(res.Rows[0][1])}; got != [2]string{c.apply, c.sync} {
			t.Fatalf("%s: got %q, want %q %q", c.name, got, c.apply, c.sync)
		}
	}
	// And the real query runs against whatever this server is.
	subs, err := listSubscriptions(ctx, conn)
	if err != nil {
		t.Fatalf("listSubscriptions on %s: %v", conn.Wire().ParameterStatus("server_version"), err)
	}
	if pgMajor(conn.Wire().ParameterStatus("server_version")) >= 15 {
		for _, s := range subs {
			if s.ApplyErrors == nil || s.SyncErrors == nil {
				t.Fatalf("%s: error counts missing: %v %v", s.Name, s.ApplyErrors, s.SyncErrors)
			}
		}
	}
}

// A subscription with disable_on_error that meets a row it already has turns
// itself off; subscription.why finds the reason in the server's own log, and
// skipping the transaction it names turns it back on for good. Needs a
// server with wal_level = logical whose log this process can read.
func TestSubscriptionWhyOnServer(t *testing.T) {
	dsn, host, port, _ := pgTest(t)
	ctx := context.Background()
	conn, err := pgConnect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if pgMajor(conn.Wire().ParameterStatus("server_version")) < 15 {
		t.Skip("disable_on_error needs Postgres 15")
	}
	if r := pgRead(ctx, conn, "SELECT current_setting('wal_level')", nil); r.Err != nil || string(r.Rows[0][0]) != "logical" {
		t.Skip("wal_level is not logical")
	}
	exec := func(c *caduceus.Conn, sql string) {
		t.Helper()
		if _, err := pgExec(ctx, c, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	tryExec := func(c *caduceus.Conn, sql string) { _, _ = pgExec(ctx, c, sql) }
	tryExec(conn, "DROP SUBSCRIPTION IF EXISTS why_sub")
	tryExec(conn, "SELECT pg_drop_replication_slot('why_slot')")
	tryExec(conn, "DROP DATABASE IF EXISTS argus_why_pub")
	exec(conn, "CREATE DATABASE argus_why_pub")
	defer func() {
		tryExec(conn, "ALTER SUBSCRIPTION why_sub DISABLE")
		tryExec(conn, "DROP SUBSCRIPTION IF EXISTS why_sub")
		tryExec(conn, "SELECT pg_drop_replication_slot('why_slot')")
		tryExec(conn, "DROP TABLE IF EXISTS why_orders")
		tryExec(conn, "DROP DATABASE IF EXISTS argus_why_pub")
	}()
	cfg, _ := wire.ParseConfig(dsn)
	pubDSN := strings.Replace(dsn, "/"+cfg.Database, "/argus_why_pub", 1)
	pub, err := pgConnect(ctx, pubDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close(ctx)
	exec(pub, "CREATE TABLE why_orders (id int PRIMARY KEY, note text)")
	exec(pub, "CREATE PUBLICATION why_pub FOR TABLE why_orders")
	exec(pub, "SELECT pg_create_logical_replication_slot('why_slot', 'pgoutput')")
	exec(conn, "CREATE TABLE why_orders (id int PRIMARY KEY, note text)")
	exec(conn, "INSERT INTO why_orders VALUES (5, 'here already')")
	ci := fmt.Sprintf("host=%s port=%d dbname=argus_why_pub user=%s", host, port, cfg.User)
	if cfg.Password != "" {
		ci += " password=" + cfg.Password
	}
	exec(conn, "CREATE SUBSCRIPTION why_sub CONNECTION "+quoteLiteral(ci)+
		" PUBLICATION why_pub WITH (create_slot = false, slot_name = 'why_slot', copy_data = false, disable_on_error = true)")
	exec(pub, "INSERT INTO why_orders VALUES (5, 'from the publisher')")
	enabled := func() bool {
		r := pgRead(ctx, conn, "SELECT subenabled FROM pg_subscription WHERE subname = 'why_sub'", nil)
		return r.Err == nil && len(r.Rows) == 1 && string(r.Rows[0][0]) == "t"
	}
	for i := 0; i < 100 && enabled(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if enabled() {
		t.Fatal("the subscription did not turn itself off")
	}
	var w *PGSubscriptionWhy
	for i := 0; i < 30; i++ {
		// The log is written by another process; give it a moment.
		if w, err = subscriptionWhy(ctx, conn, "why_sub"); err != nil {
			t.Fatal(err)
		}
		if w.Log == "" {
			t.Skipf("the server's log is not readable here: %s", w.LogNote)
		}
		if w.Error != nil && w.TurnedOff {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if w.Error == nil || w.Error.Table != "public.why_orders" || w.Error.KeyValues != "5" || w.Error.LSN == "" || !w.TurnedOff {
		t.Fatalf("why: %+v error %+v", w, w.Error)
	}
	if w.DisableOnError != "on" || w.Enabled || w.ApplyErrors == nil || *w.ApplyErrors < 1 {
		t.Fatalf("why: %+v", w)
	}
	built, err := buildAdminSQL(&tunnel.PGAdminOp{Kind: tunnel.AdminSubscriptionSkip, Name: "why_sub", LSN: w.Error.LSN}, "t@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range built.run {
		exec(conn, s)
	}
	time.Sleep(3 * time.Second)
	if !enabled() {
		t.Fatal("skipping the transaction did not keep the subscription on")
	}
	r := pgRead(ctx, conn, "SELECT note FROM why_orders WHERE id = 5", nil)
	if r.Err != nil || string(r.Rows[0][0]) != "here already" {
		t.Fatalf("the local row changed: %v %q", r.Err, r.Rows)
	}
}
