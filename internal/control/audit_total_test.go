package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	events, err := s.AuditEvents(ctx, window, 0)
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

// The total rides on the first page, the only one the console reads it from,
// and a later page does not count the log again.
func TestTheAuditTotalIsCountedOnTheFirstPageOnly(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.AppendAudit(ctx, AuditEvent{Action: "test.page", Severity: "info",
			ActorEmail: "pager@example.com", Target: "audit", Detail: "page"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	first := do(t, h, http.MethodGet, "/api/v1/audit?limit=2", "admin-token", "")
	if first.Code != http.StatusOK || first.Header().Get("X-Argus-Audit-Total") == "" {
		t.Fatalf("first page: %d, total %q", first.Code, first.Header().Get("X-Argus-Audit-Total"))
	}
	var page []AuditEvent
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || len(page) != 2 {
		t.Fatalf("first page: %v, %d events", err, len(page))
	}
	next := do(t, h, http.MethodGet, fmt.Sprintf("/api/v1/audit?limit=2&before=%d", page[1].Seq), "admin-token", "")
	if next.Code != http.StatusOK {
		t.Fatalf("next page: %d %s", next.Code, next.Body)
	}
	if got := next.Header().Get("X-Argus-Audit-Total"); got != "" {
		t.Fatalf("a later page counted the log again: %q", got)
	}
	if err := json.Unmarshal(next.Body.Bytes(), &page); err != nil || len(page) == 0 {
		t.Fatalf("next page: %v, %d events", err, len(page))
	}
}
