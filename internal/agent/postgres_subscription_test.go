package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The error counts must never name a stats column directly: the server
// refused "ss.sync_error_count does not exist" once the column was split,
// and that took the whole Replication view down with it.
func TestSubscriptionErrorColsByVersion(t *testing.T) {
	for _, major := range []int{10, 13, 14} {
		cols, join := subscriptionErrorCols(major)
		if cols != "NULL::bigint, NULL::bigint" || join != "" {
			t.Fatalf("%d: want no stats before 15, got %q %q", major, cols, join)
		}
	}
	for _, major := range []int{15, 16, 17, 18, 19} {
		cols, join := subscriptionErrorCols(major)
		if !strings.Contains(join, "pg_stat_subscription_stats ss") {
			t.Fatalf("%d: join %q", major, join)
		}
		for _, bare := range []string{"ss.sync_error_count", "ss.apply_error_count", "ss.sync_table_error_count", "ss.sync_seq_error_count"} {
			if strings.Contains(cols, bare) {
				t.Fatalf("%d: %q names %s directly", major, cols, bare)
			}
		}
	}
}

// Each shape the stats row has had reads as the counts it holds, and a
// subscription without a stats row reads as unknown.
func TestSubscriptionErrorColsOnServer(t *testing.T) {
	dsn, _, _, _ := pgTest(t)
	ctx := context.Background()
	conn, err := pgconn.Connect(ctx, dsn)
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
		res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
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
		t.Fatalf("listSubscriptions on %s: %v", conn.ParameterStatus("server_version"), err)
	}
	if pgMajor(conn.ParameterStatus("server_version")) >= 15 {
		for _, s := range subs {
			if s.ApplyErrors == nil || s.SyncErrors == nil {
				t.Fatalf("%s: error counts missing: %v %v", s.Name, s.ApplyErrors, s.SyncErrors)
			}
		}
	}
}
