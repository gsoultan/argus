//go:build !windows

package agent

import (
	"context"
	"testing"
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
