package agent

import (
	"strings"
	"testing"
)

// Lines as Postgres 16 wrote them when athena_sub met a row it already had,
// with disable_on_error on, and two of another subscription's around them.
const whyStderr = "2026-11-28 09:56:20.212 UTC [2189] LOG:  logical replication apply worker for subscription \"athena_sub\" has started\n" +
	"2026-11-28 09:56:20.215 UTC [2190] postgres@pub LOG:  starting logical decoding for slot \"athena_slot\"\n" +
	"2026-11-28 09:56:21.001 UTC [2201] LOG:  logical replication apply worker for subscription \"autocount_sub\" has started\n" +
	"2026-11-28 09:56:21.100 UTC [2201] ERROR:  logical replication target relation \"public.items\" is missing replicated column: \"qty\"\n" +
	"2026-11-28 09:56:21.100 UTC [2201] CONTEXT:  processing remote data for replication origin \"pg_16500\" during message type \"INSERT\" in transaction 745, finished at 0/1D5C060\n" +
	"2026-11-28 09:56:21.100 UTC [2201] LOG:  subscription \"autocount_sub\" has been disabled because of an error\n" +
	"2026-11-28 09:56:22.434 UTC [2189] ERROR:  duplicate key value violates unique constraint \"orders_pkey\"\n" +
	"2026-11-28 09:56:22.434 UTC [2189] DETAIL:  Key (id)=(5) already exists.\n" +
	"2026-11-28 09:56:22.434 UTC [2189] CONTEXT:  processing remote data for replication origin \"pg_16429\" during message type \"INSERT\" for replication target relation \"public.orders\" in transaction 739, finished at 0/1D5B8F0\n" +
	"2026-11-28 09:56:22.434 UTC [2189] LOG:  subscription \"athena_sub\" has been disabled because of an error\n" +
	"2026-11-28 09:56:22.500 UTC [2300] postgres@sub ERROR:  relation \"nope\" does not exist at character 15\n"

const whyJSON = `{"timestamp":"2026-11-28 09:56:20.212 UTC","pid":2189,"error_severity":"LOG","message":"logical replication apply worker for subscription \"athena_sub\" has started","backend_type":"logical replication worker"}
{"timestamp":"2026-11-28 09:56:22.434 UTC","pid":2189,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"orders_pkey\"","detail":"Key (id)=(5) already exists.","context":"processing remote data for replication origin \"pg_16429\" during message type \"INSERT\" for replication target relation \"public.orders\" in transaction 739, finished at 0/1D5B8F0","backend_type":"logical replication worker"}
{"timestamp":"2026-11-28 09:56:22.434 UTC","pid":2189,"error_severity":"LOG","message":"subscription \"athena_sub\" has been disabled because of an error","backend_type":"logical replication worker"}
`

const whyCSV = `2026-11-28 09:56:20.212 UTC,,,2189,,6b0aa544.88d,1,,2026-11-28 09:56:20 UTC,3/24,0,LOG,00000,"logical replication apply worker for subscription ""athena_sub"" has started",,,,,,,,,"","logical replication worker",,0
2026-11-28 09:56:22.434 UTC,,,2189,,6b0aa544.88d,2,,2026-11-28 09:56:20 UTC,3/27,740,ERROR,23505,"duplicate key value violates unique constraint ""orders_pkey""","Key (id)=(5) already exists.",,,,"processing remote data for replication origin ""pg_16429"" during message type ""INSERT"" for replication target relation ""public.orders"" in transaction 739, finished at 0/1D5B8F0",,,,"","logical replication worker",,0
2026-11-28 09:56:22.434 UTC,,,2189,,6b0aa544.88d,3,,2026-11-28 09:56:20 UTC,3/0,0,LOG,00000,"subscription ""athena_sub"" has been disabled because of an error",,,,,,,,,"","logical replication worker",,0
`

func whyOf(t *testing.T, name, origin, log, format string) *PGSubscriptionWhy {
	t.Helper()
	w := &PGSubscriptionWhy{Name: name, Origin: origin}
	attributeErrors(w, parseLog([]byte(log), format))
	return w
}

// Every log format gives the same answer: the duplicate key, its table, the
// transaction and the LSN that SKIP takes, and that it turned itself off.
func TestSubscriptionWhyEachFormat(t *testing.T) {
	for _, c := range []struct{ format, log string }{{"stderr", whyStderr}, {"jsonlog", whyJSON}, {"csvlog", whyCSV}} {
		w := whyOf(t, "athena_sub", "pg_16429", c.log, c.format)
		e := w.Error
		if e == nil {
			t.Fatalf("%s: no error found", c.format)
		}
		if !strings.Contains(e.Message, "duplicate key") || e.Table != "public.orders" || e.Change != "INSERT" ||
			e.Xid != "739" || e.LSN != "0/1D5B8F0" || e.Constraint != "orders_pkey" ||
			e.KeyColumns != "id" || e.KeyValues != "5" || e.Worker != "apply" {
			t.Fatalf("%s: %+v", c.format, e)
		}
		if !w.TurnedOff || w.TurnedOffAt != "2026-11-28 09:56:22.434 UTC" || w.Errors != 1 {
			t.Fatalf("%s: turned off %v at %q, %d errors", c.format, w.TurnedOff, w.TurnedOffAt, w.Errors)
		}
		if c.format != "stderr" && e.Code != "23505" {
			t.Fatalf("%s: code %q", c.format, e.Code)
		}
	}
}

// Each subscription gets its own cause from the same log, and a client's
// error in between belongs to neither.
func TestSubscriptionWhyKeepsSubscriptionsApart(t *testing.T) {
	w := whyOf(t, "autocount_sub", "pg_16500", whyStderr, "stderr")
	if w.Error == nil || w.Error.Table != "public.items" || w.Error.Columns != `"qty"` || w.Error.LSN != "0/1D5C060" || !w.TurnedOff {
		t.Fatalf("%+v %+v", w, w.Error)
	}
	if w.Errors != 1 {
		t.Fatalf("%d errors; the other subscription's or a client's were counted", w.Errors)
	}
	none := whyOf(t, "other_sub", "pg_1", whyStderr, "stderr")
	if none.Error != nil || none.TurnedOff {
		t.Fatalf("another subscription's error was taken: %+v", none.Error)
	}
}

// A worker that errored again after being turned back on is still failing:
// the last error counts, and "turned off" only if a line says so after it.
func TestSubscriptionWhyLastErrorWins(t *testing.T) {
	log := whyStderr +
		"2026-11-28 10:00:00.000 UTC [2400] LOG:  logical replication apply worker for subscription \"athena_sub\" has started\n" +
		"2026-11-28 10:00:01.000 UTC [2400] ERROR:  could not receive data from WAL stream: ERROR:  replication slot \"athena_slot\" does not exist\n"
	w := whyOf(t, "athena_sub", "pg_16429", log, "stderr")
	if w.Errors != 2 || w.TurnedOff || !strings.Contains(w.Error.Message, "replication slot") || w.FirstAt != "2026-11-28 09:56:22.434 UTC" {
		t.Fatalf("%+v %+v", w, w.Error)
	}
}

// A pid freed by an exiting worker and taken by an ordinary session does
// not make that session's errors the subscription's.
func TestSubscriptionWhyPidReuse(t *testing.T) {
	log := "2026-11-28 10:00:00.000 UTC [77] LOG:  logical replication apply worker for subscription \"athena_sub\" has started\n" +
		"2026-11-28 10:00:01.000 UTC [1] LOG:  background worker \"logical replication apply worker\" (PID 77) exited with exit code 1\n" +
		"2026-11-28 10:00:02.000 UTC [77] app@sub ERROR:  division by zero\n"
	if w := whyOf(t, "athena_sub", "pg_16429", log, "stderr"); w.Error != nil {
		t.Fatalf("took a session's error: %+v", w.Error)
	}
}

// Postgres 18 reports a conflict by kind, over several lines; a message's
// own later lines start with a tab.
func TestSubscriptionWhyConflictPG18(t *testing.T) {
	log := "2026-11-28 10:00:00.000 UTC [90] ERROR:  conflict detected on relation \"public.test\": conflict=insert_exists\n" +
		"2026-11-28 10:00:00.000 UTC [90] DETAIL:  Key already exists in unique index \"t_pkey\", modified locally in transaction 740 at 2026-11-28 09:00:00+00.\n" +
		"\tKey (c)=(1); existing local row (1, 'local'); remote row (1, 'remote').\n" +
		"2026-11-28 10:00:00.000 UTC [90] CONTEXT:  processing remote data for replication origin \"pg_16395\" during \"INSERT\" for replication target relation \"public.test\" in transaction 725, finished at 0/14C0378\n"
	w := whyOf(t, "s", "pg_16395", log, "stderr")
	e := w.Error
	if e == nil || e.Conflict != "insert_exists" || e.Table != "public.test" || e.Change != "INSERT" || e.LSN != "0/14C0378" ||
		e.KeyColumns != "c" || e.KeyValues != "1" {
		t.Fatalf("%+v", e)
	}
}

// Table sync workers use pg_<oid>_<relid> origins and say they are the
// subscription's when they start.
func TestSubscriptionWhySyncWorker(t *testing.T) {
	log := "2026-11-28 10:00:00.000 UTC [91] LOG:  logical replication table synchronization worker for subscription \"s\", table \"orders\" has started\n" +
		"2026-11-28 10:00:01.000 UTC [91] ERROR:  permission denied for table orders\n"
	w := whyOf(t, "s", "pg_16395", log, "stderr")
	if w.Error == nil || w.Error.Worker != "sync" || w.Error.Table != "orders" {
		t.Fatalf("%+v", w.Error)
	}
}

// Whatever the log holds, a field is bounded.
func TestSubscriptionWhyBounded(t *testing.T) {
	long := strings.Repeat("x", 10000)
	log := "2026-11-28 10:00:00.000 UTC [92] ERROR:  " + long + "\n" +
		"2026-11-28 10:00:00.000 UTC [92] CONTEXT:  processing remote data for replication origin \"pg_5\" during message type \"UPDATE\" in transaction 1, finished at 0/1\n"
	w := whyOf(t, "s", "pg_5", log, "stderr")
	if w.Error == nil || len(w.Error.Message) > whyField+4 {
		t.Fatalf("message %d bytes", len(w.Error.Message))
	}
}
