package agent

import (
	"strings"
	"testing"
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
