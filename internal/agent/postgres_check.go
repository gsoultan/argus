package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Check: what a write would do, without doing it.

The console asks before it runs an INSERT, UPDATE, DELETE or MERGE. The agent
runs the statement as the person's own role inside a transaction it then
rolls back, or inside a savepoint when the person is in a transaction of
their own, and answers with the command tag's count and every table the
statement touched, cascades and triggers included, read from
pg_stat_xact_user_tables before and after.

Rolled back is not the same as never run. Sequences advance, and anything a
trigger sends outside the database (dblink, a NOTIFY listener after commit is
the exception, since NOTIFY is transactional) has happened. The console says
"counted by running it and rolling it back" for that reason. Locks are held
for as long as the statement takes, under the session's statement_timeout
like any statement.

The check is recorded: a "check" line with the statement, then a done or
error line, so the replay shows it ran and that it was rolled back.
*/

// xactTouchedSQL lists the tables this transaction has written to so far.
const xactTouchedSQL = `SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname),
  s.n_tup_ins, s.n_tup_upd, s.n_tup_del, greatest(c.reltuples, 0)::int8
FROM pg_stat_xact_user_tables s
JOIN pg_class c ON c.oid = s.relid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE s.n_tup_ins + s.n_tup_upd + s.n_tup_del > 0
ORDER BY 1 LIMIT 200`

func (s *pgSession) xactTouched(ctx context.Context) (map[string]tunnel.PGCheckTable, error) {
	res := s.conn.ExecParams(ctx, xactTouchedSQL, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make(map[string]tunnel.PGCheckTable, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) != 5 {
			continue
		}
		t := tunnel.PGCheckTable{Name: string(r[0])}
		fmt.Sscan(string(r[1]), &t.Inserted)
		fmt.Sscan(string(r[2]), &t.Updated)
		fmt.Sscan(string(r[3]), &t.Deleted)
		fmt.Sscan(string(r[4]), &t.Estimate)
		out[t.Name] = t
	}
	return out, nil
}

func (s *pgSession) check(ctx context.Context, req tunnel.PGRequest) error {
	sql := strings.TrimSpace(req.SQL)
	switch {
	case sql == "":
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "there is no statement to check"})
	case !tunnel.RecordedTextFits(sql):
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: tunnel.ErrRecordTooLong.Error()})
	case !utf8.ValidString(sql) || strings.IndexByte(sql, 0) >= 0:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "the statement is not valid text"})
	}
	class := tunnel.ClassifySQL(sql)
	switch class.Verb {
	case "insert", "update", "delete", "merge":
	default:
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: tunnel.GuardCheckOnly,
			Error: "only INSERT, UPDATE, DELETE and MERGE are checked before they run"})
	}
	if s.conn.TxStatus() == 'E' {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: "25P02",
			Error: "your transaction has already failed; ROLLBACK before checking anything"})
	}

	if err := s.record(tunnel.PGRecord{Kind: "check", Request: req.ID, SQL: sql}); err != nil {
		if errors.Is(err, tunnel.ErrRecordTooLong) {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
		}
		return fmt.Errorf("the recording failed: %w", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	s.cmu.Lock()
	s.cancel, s.curID = cancel, req.ID
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.cancel, s.curID = nil, 0
		s.cmu.Unlock()
		cancel()
	}()
	start := time.Now()
	cleanup := context.WithoutCancel(sctx)

	// Ours: a transaction we open and always roll back. Theirs: a savepoint
	// inside it, rolled back to and released, so their transaction goes on
	// exactly as it was.
	ours := s.conn.TxStatus() == 'I'
	open, undo := "SAVEPOINT argus_check", "ROLLBACK TO SAVEPOINT argus_check; RELEASE SAVEPOINT argus_check"
	if ours {
		open, undo = "BEGIN", "ROLLBACK"
		if s.pooled {
			open = s.prelude
		}
	}
	if _, err := s.conn.Exec(sctx, open).ReadAll(); err != nil {
		return s.statementFailed(ctx, req.ID, err, 0, time.Since(start).Milliseconds())
	}
	before, err := s.xactTouched(sctx)
	var (
		tag     pgconn.CommandTag
		touched map[string]tunnel.PGCheckTable
	)
	if err == nil {
		rr := s.conn.ExecParams(sctx, sql, nil, nil, nil, []int16{0})
		for rr.NextRow() {
			// RETURNING rows are counted by the tag; nobody sees them.
		}
		tag, err = rr.Close()
	}
	if err == nil {
		touched, err = s.xactTouched(sctx)
	}
	// Always undone, whatever happened, before anything else is said.
	if !s.conn.IsClosed() {
		if _, uerr := s.conn.Exec(cleanup, undo).ReadAll(); uerr != nil && err == nil {
			err = uerr
		}
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return s.statementFailed(ctx, req.ID, err, 0, elapsed)
	}

	c := &tunnel.PGCheck{Class: class, Rows: tag.RowsAffected(), Tag: tag.String(),
		Guard: tunnel.GuardSQL(class, s.db.Name, "")}
	for name, t := range touched {
		b := before[name]
		t.Inserted -= b.Inserted
		t.Updated -= b.Updated
		t.Deleted -= b.Deleted
		if t.Inserted+t.Updated+t.Deleted > 0 {
			c.Tables = append(c.Tables, t)
		}
	}
	sortCheckTables(c.Tables)
	if rerr := s.record(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: "ROLLED BACK " + c.Tag,
		Rows: c.Rows, MS: elapsed}); rerr != nil {
		return rerr
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "check", Tag: c.Tag, Rows: c.Rows, MS: elapsed, Check: c})
}

// sortCheckTables puts the busiest table first, then by name.
func sortCheckTables(ts []tunnel.PGCheckTable) {
	n := func(t tunnel.PGCheckTable) int64 { return t.Inserted + t.Updated + t.Deleted }
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && (n(ts[j]) > n(ts[j-1]) || n(ts[j]) == n(ts[j-1]) && ts[j].Name < ts[j-1].Name); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}
