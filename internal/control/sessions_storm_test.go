package control

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/rgen/session"
)

// sessionsHandWritten is the query Sessions used before storm, kept verbatim so
// the migration can be checked against it rather than against a description of
// it. A rewrite that is only argued about is a rewrite nobody verified.
//
// Transcribing it by hand for this test put &v.Origin in the wrong position
// and the suite failed with "cannot scan NULL into *string" — a twenty-argument
// positional Scan against a twenty-column SELECT, with nothing on either side
// checking that the orders agree. That is the defect class the migration
// removes, demonstrated by accident while writing the test for it.
func (s *Store) sessionsHandWritten(ctx context.Context, f SessionFilter) ([]Session, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, user_email, asset_id::text, asset_hostname, principal,
		       protocol, origin, origin_reason, state, started_at, ended_at,
		       client_ip, fidelity, recording_bytes, command_count, exit_code,
		       chain_head, risk_flags, reported_by, recording_key,
		       terminated_by, termination_reason
		FROM sessions
		WHERE ($1 = '' OR state = $1)
		  AND ($2 = '' OR origin = $2)
		ORDER BY started_at DESC
		LIMIT $3`, f.State, f.Origin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.UserEmail, &v.AssetID, &v.AssetHostname,
			&v.Principal, &v.Protocol, &v.Origin, &v.OriginReason, &v.State,
			&v.StartedAt, &v.EndedAt, &v.ClientIP, &v.Fidelity, &v.RecordingBytes,
			&v.CommandCount, &v.ExitCode, &v.ChainHead, &v.RiskFlags, &v.ReportedBy,
			&v.RecordingKey, &v.TerminatedBy, &v.TerminationReason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Every filter combination, both implementations, same rows in the same order.
//
// The filters are the point: the hand-written form is one statement serving
// all four combinations, and the storm form is four statements. Equality has
// to hold for each, including the empty result and the unfiltered read.
// seedSessions writes rows this test can actually compare, and removes them
// again.
//
// Without them the test was green and empty: every filter returned nothing,
// len(got) != len(want) compared 0 to 0, and the loop calling diffSession never
// ran once. Measured on a fresh database with the whole package running --
// `SELECT count(*) FROM sessions` afterwards was 0 -- so nothing in CI ever put
// a row in front of it. A rewrite this size cannot be verified by a test that
// never sees a row.
//
// Distinct started_at values on purpose. The order is `started_at DESC` and the
// two implementations are different statements with different plans, so rows
// sharing a timestamp could legitimately come back in different orders and fail
// this for a reason that is not a defect.
func seedSessions(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	by, why := "lin@northwind.id", "credential harvesting observed"
	head := "a1b2c3d4e5f60718293a4b5c6d7e8f900112233445566778899aabbccddeeff0"

	rows := []Session{
		{State: "active", Origin: "brokered", Principal: "ops"},
		{State: "active", Origin: "direct", Principal: "root"},
		{State: "closed", Origin: "brokered", Principal: "ops", ChainHead: &head},
		{State: "closed", Origin: "direct", Principal: "dbadmin"},
		// The one that exercises the columns the schema gained while this
		// branch was open. A list that cannot say who ended a session is the
		// regression this file exists to catch.
		{State: "terminated", Origin: "brokered", Principal: "root",
			TerminatedBy: &by, TerminationReason: &why},
	}

	var ids []string
	for i, r := range rows {
		r.ID = newUUID(t)
		r.UserEmail = unique("seed") + "@northwind.id"
		r.AssetHostname = "pay-0" + strconv.Itoa(i+1)
		r.Protocol = "ssh"
		r.Fidelity = "pty"
		r.ReportedBy = "gateway"
		r.RiskFlags = []string{}
		r.StartedAt = base.Add(time.Duration(i) * time.Second)
		if r.State != "active" {
			ended := r.StartedAt.Add(30 * time.Second)
			r.EndedAt = &ended
		}
		if err := s.UpsertSession(ctx, r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}

	// The database is shared between tests and between runs; leaving these
	// behind would quietly change what every later test sees.
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(),
			`DELETE FROM sessions WHERE id = ANY($1::uuid[])`, ids); err != nil {
			t.Errorf("cleaning up seeded sessions: %v", err)
		}
	})
}

func TestSessionsMatchesTheHandWrittenQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedSessions(t, s)

	// atLeast is what each case has to return before its comparison means
	// anything. Without it a filter matching nothing passes by comparing two
	// empty slices, which is how this test read before there was any data.
	for _, tc := range []struct {
		f       SessionFilter
		atLeast int
	}{
		{SessionFilter{}, 5},
		{SessionFilter{State: "active"}, 2},
		{SessionFilter{State: "closed"}, 2},
		{SessionFilter{Origin: "direct"}, 2},
		{SessionFilter{Origin: "brokered"}, 3},
		{SessionFilter{State: "closed", Origin: "brokered"}, 1},
		{SessionFilter{State: "terminated"}, 1},
		{SessionFilter{Limit: 3}, 3},
		{SessionFilter{State: "closed", Limit: 1}, 1},
	} {
		f := tc.f
		want, err := s.sessionsHandWritten(ctx, f)
		if err != nil {
			t.Fatalf("%+v: hand-written: %v", f, err)
		}
		if len(want) < tc.atLeast {
			t.Fatalf("%+v matched %d rows, needs at least %d -- this case is "+
				"comparing nothing and would pass against any implementation",
				f, len(want), tc.atLeast)
		}
		got, err := s.Sessions(ctx, f)
		if err != nil {
			t.Fatalf("%+v: storm: %v", f, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%+v: storm returned %d rows, hand-written %d", f, len(got), len(want))
		}
		for i := range want {
			if d := diffSession(got[i], want[i]); d != "" {
				t.Fatalf("%+v: row %d differs in %s:\n storm: %+v\n  hand: %+v",
					f, i, d, got[i], want[i])
			}
		}
	}

	// The empty result, kept separate because it is the one case that must
	// return nothing and the only one the guard above cannot express.
	empty, err := s.Sessions(ctx, SessionFilter{State: "no-such-state"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("a filter matching no state returned %d rows", len(empty))
	}
}

// The shape cache is the thesis: a bounded set of filter combinations must
// mint a bounded set of statements, however many times they are called. If
// this grows with calls, storm is caching request data.
func TestSessionFiltersMintABoundedNumberOfShapes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	filters := []SessionFilter{
		{}, {State: "active"}, {Origin: "direct"}, {State: "closed", Origin: "brokered"},
	}
	for _, f := range filters {
		if _, err := s.Sessions(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	before := sessionShapes()

	// The same four shapes, with values that vary every call.
	for i := 0; i < 200; i++ {
		for _, f := range filters {
			g := f
			if g.State != "" {
				g.State = unique("state")
			}
			if g.Origin != "" {
				g.Origin = unique("origin")
			}
			if _, err := s.Sessions(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
	}
	if after := sessionShapes(); after != before {
		t.Errorf("shapes grew from %d to %d over 800 calls with varying values — "+
			"the shape key is reading the values, not the query", before, after)
	}
	if flushes := sessionFlushes(); flushes != 0 {
		t.Errorf("shape cache flushed %d times; a bounded workload should never flush", flushes)
	}
}

func sessionShapes() int  { return session.Shapes() }
func sessionFlushes() int { return session.ShapeFlushes() }

// diffSession compares two rows by VALUE, which for a timestamptz means the
// instant and not its rendering.
//
// pgx decodes timestamptz into the connection's time zone; storm decodes into
// UTC. Both are the same instant — a timestamptz stores no zone, and the
// client picks one to render it in — so reflect.DeepEqual reports a difference
// that does not exist, because time.Time carries its Location.
//
// The change IS visible at the API boundary: the console used to receive
// "2026-09-07T15:59:28+07:00" and now receives "2026-09-07T08:59:28Z" for the
// same moment. Any ISO-8601 parser reads them identically, and UTC is the
// better default because it does not depend on the server's TimeZone setting —
// but it is a change, so it is asserted here deliberately rather than papered
// over with a looser comparison.
func diffSession(got, want Session) string {
	if !got.StartedAt.Equal(want.StartedAt) {
		return "StartedAt"
	}
	if (got.EndedAt == nil) != (want.EndedAt == nil) {
		return "EndedAt nullness"
	}
	if got.EndedAt != nil && !got.EndedAt.Equal(*want.EndedAt) {
		return "EndedAt"
	}
	if got.StartedAt.Location() != time.UTC {
		return "StartedAt is not UTC — storm's stated convention"
	}
	g, w := got, want
	g.StartedAt, w.StartedAt = time.Time{}, time.Time{}
	g.EndedAt, w.EndedAt = nil, nil
	if !equalSessionRest(g, w) {
		return "a non-time field"
	}
	return ""
}

func equalSessionRest(a, b Session) bool {
	eqs := func(x, y *string) bool {
		if (x == nil) != (y == nil) {
			return false
		}
		return x == nil || *x == *y
	}
	eqi := func(x, y *int) bool {
		if (x == nil) != (y == nil) {
			return false
		}
		return x == nil || *x == *y
	}
	if a.ID != b.ID || a.UserEmail != b.UserEmail || a.AssetHostname != b.AssetHostname ||
		a.Principal != b.Principal || a.Protocol != b.Protocol || a.Origin != b.Origin ||
		a.State != b.State || a.ClientIP != b.ClientIP || a.Fidelity != b.Fidelity ||
		a.RecordingBytes != b.RecordingBytes || a.ReportedBy != b.ReportedBy {
		return false
	}
	if !eqs(a.AssetID, b.AssetID) || !eqs(a.OriginReason, b.OriginReason) ||
		!eqs(a.ChainHead, b.ChainHead) || !eqs(a.RecordingKey, b.RecordingKey) ||
		!eqs(a.TerminatedBy, b.TerminatedBy) || !eqs(a.TerminationReason, b.TerminationReason) {
		return false
	}
	if !eqi(a.CommandCount, b.CommandCount) || !eqi(a.ExitCode, b.ExitCode) {
		return false
	}
	if len(a.RiskFlags) != len(b.RiskFlags) {
		return false
	}
	for i := range a.RiskFlags {
		if a.RiskFlags[i] != b.RiskFlags[i] {
			return false
		}
	}
	return true
}
