package control

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

/*
Audit appends that arrive together are committed together.

Each test holds the audit lock from outside, so the first append is stuck
inside its transaction while the rest queue behind it, then lets go. What is
asserted is what a verifier and an auditor rely on: every caller gets its own
committed event, the stored rows are the ones handed back, each links to the
row before it, and one bad event costs nobody else theirs.
*/

// holdAuditLock takes the audit chain's lock until the returned func runs.
func holdAuditLock(t *testing.T, s *Store) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('argus_audit'))`); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

// waitQueued waits until n appends are queued behind the one being written.
func waitQueued(t *testing.T, s *Store, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.auditMu.Lock()
		q := len(s.auditQueue)
		s.auditMu.Unlock()
		if q >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d appends queued", n)
}

type appended struct {
	e   AuditEvent
	err error
}

func appendAll(s *Store, events []AuditEvent) (chan appended, *sync.WaitGroup) {
	out := make(chan appended, len(events))
	var wg sync.WaitGroup
	for _, e := range events {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.AppendAudit(context.Background(), e)
			out <- appended{got, err}
		}()
	}
	return out, &wg
}

func TestABurstOfAuditAppendsIsCommittedTogetherAndStaysChained(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const n = 200
	target := unique("burst")

	release := holdAuditLock(t, s)
	first, firstDone := appendAll(s, []AuditEvent{{Action: "burst.test", Target: target, Detail: "first"}})
	waitWriting(t, s)
	var rest []AuditEvent
	for i := range n - 1 {
		rest = append(rest, AuditEvent{Action: "burst.test", Target: target,
			ActorEmail: "u@x.id", Detail: fmt.Sprintf("event %d", i)})
	}
	out, wg := appendAll(s, rest)
	waitQueued(t, s, n-1)
	release()
	firstDone.Wait()
	wg.Wait()
	close(first)
	close(out)

	seqs := map[int64]AuditEvent{}
	for _, ch := range []chan appended{first, out} {
		for a := range ch {
			if a.err != nil {
				t.Fatalf("an append failed: %v", a.err)
			}
			if _, dup := seqs[a.e.Seq]; dup {
				t.Fatalf("two callers were handed seq %d", a.e.Seq)
			}
			seqs[a.e.Seq] = a.e
		}
	}
	if len(seqs) != n {
		t.Fatalf("%d events handed back, want %d", len(seqs), n)
	}

	// What each caller was told is what is stored, and every stored row
	// links to the one before it and hashes to what it says.
	for seq, e := range seqs {
		var stored, before AuditEvent
		err := s.pool.QueryRow(ctx, `
			SELECT seq, at, action, severity, actor_email, target, detail, prev_hash, hash
			  FROM audit_events WHERE seq = $1`, seq).Scan(&stored.Seq, &stored.At,
			&stored.Action, &stored.Severity, &stored.ActorEmail, &stored.Target,
			&stored.Detail, &stored.PrevHash, &stored.Hash)
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		if stored.Hash != e.Hash || stored.PrevHash != e.PrevHash || stored.Detail != e.Detail {
			t.Fatalf("seq %d: the caller was handed %+v but %+v is stored", seq, e, stored)
		}
		if chainHash(stored.PrevHash, stored) != stored.Hash {
			t.Fatalf("seq %d does not hash to what it stores", seq)
		}
		err = s.pool.QueryRow(ctx, `
			SELECT hash FROM audit_events WHERE seq < $1 ORDER BY seq DESC LIMIT 1`, seq).
			Scan(&before.Hash)
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		if stored.PrevHash != before.Hash {
			t.Fatalf("seq %d links to %s, not to the event before it", seq, short(stored.PrevHash))
		}
	}

	// The point of it: the 199 that queued went in one transaction, not 199.
	var txs int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT xmin::text) FROM audit_events WHERE target = $1`, target).
		Scan(&txs); err != nil {
		t.Fatal(err)
	}
	if txs > 2 {
		t.Errorf("%d appends that arrived together took %d transactions", n, txs)
	}
}

// A NUL byte is text Postgres refuses. The event carrying it fails; the
// events committed alongside it do not.
func TestOneBadAuditEventCostsNobodyElseTheirs(t *testing.T) {
	s := testStore(t)
	target := unique("bad")

	release := holdAuditLock(t, s)
	first, firstDone := appendAll(s, []AuditEvent{{Action: "bad.test", Target: target}})
	waitWriting(t, s)
	events := []AuditEvent{{Action: "bad.test", Target: target, Detail: "bad\x00byte"}}
	for i := range 5 {
		events = append(events, AuditEvent{Action: "bad.test", Target: target,
			Detail: fmt.Sprintf("good %d", i)})
	}
	out, wg := appendAll(s, events)
	waitQueued(t, s, len(events))
	release()
	firstDone.Wait()
	wg.Wait()
	close(first)
	close(out)

	if a := <-first; a.err != nil {
		t.Fatalf("the first append failed: %v", a.err)
	}
	var good, bad int
	for a := range out {
		switch {
		case a.err == nil && a.e.Seq > 0:
			good++
		case a.err != nil && a.e.Detail == "bad\x00byte" && a.e.Seq == 0:
			bad++
		default:
			t.Errorf("unexpected outcome: %+v, %v", a.e, a.err)
		}
	}
	if good != 5 || bad != 1 {
		t.Errorf("good %d, bad %d; want 5 committed and the bad one refused", good, bad)
	}
}

// waitWriting waits until an append holds the writer's role.
func waitWriting(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.auditMu.Lock()
		w, q := s.auditWriting, len(s.auditQueue)
		s.auditMu.Unlock()
		if w && q == 0 {
			// Give it a moment to reach the lock it will wait on.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no append started writing")
}
