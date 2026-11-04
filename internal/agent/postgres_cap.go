package agent

import (
	"context"
	"fmt"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5/pgconn"
)

/*
The cap on an approved change.

A statement that ran under an approval carries RowCap: how many rows the dry
run counted when the person asked, which is what the approver saw. It runs in
a transaction of its own (or a savepoint, inside the person's or the pooler's
transaction), and if it changes more rows than that, it is rolled back and
answered with tunnel.GuardOverCap: someone else changed the data since, and
the approver did not agree to the difference.
*/

const capSavepoint = "argus_approved_cap"

// capped is whether req's statement is held to its approval's row count.
func capped(req tunnel.PGRequest, c tunnel.SQLClass) bool {
	return req.RowCap != nil && tunnel.CountsRows(c)
}

// capOpen begins what lets a capped statement be taken back: "tx" for a
// transaction of its own, "sp" for a savepoint in one already open, "" when
// the statement is not capped.
func (s *pgSession) capOpen(ctx context.Context, req tunnel.PGRequest, c tunnel.SQLClass) (string, error) {
	if !capped(req, c) {
		return "", nil
	}
	switch s.conn.TxStatus() {
	case 'I':
		if _, err := s.conn.Exec(ctx, "BEGIN").ReadAll(); err != nil {
			return "", err
		}
		return "tx", nil
	case 'T':
		if _, err := s.conn.Exec(ctx, "SAVEPOINT "+capSavepoint).ReadAll(); err != nil {
			return "", err
		}
		return "sp", nil
	}
	// An aborted transaction runs nothing; Postgres says so.
	return "", nil
}

// capClose ends what capOpen began: it keeps the change when it is within
// the cap and takes it back when it is not, or when the statement failed.
func (s *pgSession) capClose(ctx context.Context, kind string, changed int64, limit *int64, err error) error {
	if kind == "" || s.conn.IsClosed() {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	over := err == nil && limit != nil && changed > *limit
	if over {
		err = &pgconn.PgError{Code: tunnel.GuardOverCap, Severity: "ERROR",
			Message: fmt.Sprintf("this change would affect %d rows, more than the %d that were approved; nothing was changed. "+
				"The data changed since you asked, so ask again", changed, *limit)}
	}
	switch {
	case kind == "tx" && err == nil:
		if _, cerr := s.conn.Exec(ctx, "COMMIT").ReadAll(); cerr != nil {
			return cerr
		}
	case kind == "tx":
		_, _ = s.conn.Exec(ctx, "ROLLBACK").ReadAll()
	case err == nil:
		if _, cerr := s.conn.Exec(ctx, "RELEASE SAVEPOINT "+capSavepoint).ReadAll(); cerr != nil {
			return cerr
		}
	case over:
		_, _ = s.conn.Exec(ctx, "ROLLBACK TO SAVEPOINT "+capSavepoint+"; RELEASE SAVEPOINT "+capSavepoint).ReadAll()
	}
	// A failed statement inside a savepoint leaves the transaction aborted,
	// as it would have been without the cap; the person rolls it back.
	return err
}

// changedRows is how many rows a statement's tag says it changed.
func changedRows(tag pgconn.CommandTag) int64 {
	if tag.Select() {
		return 0
	}
	return tag.RowsAffected()
}
