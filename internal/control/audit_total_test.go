package control

import (
	"context"
	"testing"
)

// The log's size has to be knowable separately from what a request returned.
//
// AuditEvents caps at 500 by default, and the console verifies what it receives
// and reports "chain intact" -- so without a total it describes a window while
// sounding like it describes the log. The dev log holds 4,546 entries and the
// console was checking the most recent 500 of them.
func TestTheAuditTotalIsNotTheNumberOfEventsReturned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	total, err := s.CountAuditEvents(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	// Seeded by other tests in this package and by any dev data; the property
	// under test is the relationship, not a particular figure.
	if total <= 0 {
		t.Skip("no audit events in this database; nothing to compare a window against")
	}

	const window = 5
	events, err := s.AuditEvents(ctx, window)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if total <= window {
		t.Skipf("log holds %d events, too few to be truncated by a window of %d", total, window)
	}

	if len(events) != window {
		t.Fatalf("asked for %d events, got %d", window, len(events))
	}
	if len(events) == total {
		t.Fatalf("a window of %d returned the whole log of %d; this test proves nothing",
			window, total)
	}
	if total < len(events) {
		t.Errorf("count %d is smaller than a single page of %d", total, len(events))
	}
}
