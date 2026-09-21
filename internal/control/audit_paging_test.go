package control

import (
	"context"
	"testing"
)

// Paging backwards has to reach every entry exactly once.
//
// The console verified the newest 500 and called the verdict the log's. It now
// pages until it has everything, so the two properties that matter are that the
// pages join up with no gap and no repeat -- a hash chain with a link skipped
// verifies as broken, and one with a link doubled verifies as broken too.
func TestPagingBackwardsReachesEveryEntryOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	total, err := s.CountAuditEvents(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total < 3 {
		t.Skip("too few audit events to page through")
	}

	// Deliberately small and not a divisor of any plausible total, so the last
	// page is short and the boundary is exercised rather than landing flush.
	const page = 7
	seen := map[int64]bool{}
	before := 0
	order := []int64{}

	for i := 0; ; i++ {
		if i > total/page+2 {
			t.Fatalf("paging did not terminate after %d requests", i)
		}
		batch, err := s.AuditEvents(ctx, page, before)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if seen[e.Seq] {
				t.Fatalf("seq %d returned twice; a doubled link breaks the chain", e.Seq)
			}
			seen[e.Seq] = true
			order = append(order, e.Seq)
		}
		before = int(batch[len(batch)-1].Seq)
	}

	if len(seen) != total {
		t.Errorf("paging reached %d of %d entries", len(seen), total)
	}
	// Strictly descending across page boundaries, which is what makes `before`
	// safe to derive from the last row of the previous page.
	for i := 1; i < len(order); i++ {
		if order[i] >= order[i-1] {
			t.Fatalf("seq went %d -> %d at index %d; paging is not monotonic",
				order[i-1], order[i], i)
		}
	}
}

// The first page is the newest, whether or not `before` is given.
func TestAuditPagingStartsAtTheNewest(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	head, err := s.AuditEvents(ctx, 1, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(head) == 0 {
		t.Skip("no audit events")
	}
	all, err := s.AuditEvents(ctx, 5, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if all[0].Seq != head[0].Seq {
		t.Errorf("a limit of 1 gave seq %d, a limit of 5 gave %d; the first page moved",
			head[0].Seq, all[0].Seq)
	}
}
