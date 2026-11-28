package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/caduceus"
)

/*
Why a subscription stopped.

Postgres keeps only counts of apply errors (pg_stat_subscription_stats); the
message, the table and the transaction it failed in are written to the
server log and nowhere else. With disable_on_error on, the worker logs the
error, logs "subscription … has been disabled because of an error" and turns
the subscription off, so Enable looks like it did nothing. subscription.why
reads the tail of the server log as the postgres account (the helper runs as
it), keeps only what this subscription's workers wrote, and returns the last
error with what a fix needs: the table, the change, the key, and the LSN
ALTER SUBSCRIPTION … SKIP takes. The console turns that into a sentence and
one fix; the agent states facts only.

Nothing else from the log leaves the helper: entries are attributed to the
subscription by its name, its replication origin (pg_<oid>) or the process
that said it was its worker, and every field is cut to a bounded length.
*/

// PGSubscriptionWhy answers subscription.why.
type PGSubscriptionWhy struct {
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	DisableOnError string `json:"disableOnError,omitempty"`
	Binary         string `json:"binary,omitempty"`
	Owner          string `json:"owner"`
	Slot           string `json:"slot,omitempty"`
	// Origin is the subscription's replication origin, pg_<oid>; AppliedTo
	// is the last publisher LSN it applied (0/0 before any).
	Origin      string `json:"origin"`
	AppliedTo   string `json:"appliedTo,omitempty"`
	ApplyErrors *int64 `json:"applyErrors,omitempty"`
	SyncErrors  *int64 `json:"syncErrors,omitempty"`
	// Conflicts are Postgres 18's per-kind conflict counts that are not
	// zero (insert_exists, update_missing, …).
	Conflicts map[string]int64 `json:"conflicts,omitempty"`
	// Log is the file read; LogNote says why none could be, or what was not.
	Log     string `json:"log,omitempty"`
	LogNote string `json:"logNote,omitempty"`
	// Error is the last error this subscription's workers logged in what
	// was read; Errors counts them and FirstAt is the earliest.
	Error   *PGApplyError `json:"error,omitempty"`
	Errors  int           `json:"errors"`
	FirstAt string        `json:"firstAt,omitempty"`
	// TurnedOff says the log has "has been disabled because of an error"
	// after the last error, at TurnedOffAt.
	TurnedOff   bool   `json:"turnedOff,omitempty"`
	TurnedOffAt string `json:"turnedOffAt,omitempty"`
}

// PGApplyError is one error a subscription's worker logged, with what the
// message, its detail and its context name.
type PGApplyError struct {
	At       string `json:"at,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Context  string `json:"context,omitempty"`
	// Worker is "apply" or "sync" (a table's first copy).
	Worker string `json:"worker,omitempty"`
	// Table is schema.table as Postgres names it; Change the message type
	// being applied (INSERT, UPDATE, DELETE, …); Xid the publisher's
	// transaction; LSN where it finished, which is what SKIP takes.
	Table  string `json:"table,omitempty"`
	Change string `json:"change,omitempty"`
	Xid    string `json:"xid,omitempty"`
	LSN    string `json:"lsn,omitempty"`
	// Constraint, KeyColumns and KeyValues are a unique or foreign key's;
	// Columns the columns named as missing; Conflict Postgres 18's kind.
	Constraint string `json:"constraint,omitempty"`
	KeyColumns string `json:"keyColumns,omitempty"`
	KeyValues  string `json:"keyValues,omitempty"`
	Columns    string `json:"columns,omitempty"`
	Conflict   string `json:"conflict,omitempty"`
}

const (
	// whyTail is how much of each log file is read, from its end.
	whyTail = 8 << 20
	// whyField bounds every string taken from the log.
	whyField = 2000
)

func subscriptionWhy(ctx context.Context, conn *caduceus.Conn, name string) (*PGSubscriptionWhy, error) {
	if name == "" {
		return nil, errors.New("which subscription?")
	}
	major := pgMajor(conn.Wire().ParameterStatus("server_version"))
	errs, join := subscriptionErrorCols(major)
	opts := "'', ''"
	if major >= 15 {
		opts = "s.subdisableonerr::text, s.subbinary::text"
	} else if major == 14 {
		opts = "'', s.subbinary::text"
	}
	conflicts := "'{}'"
	if major >= 15 {
		conflicts = `coalesce((SELECT jsonb_object_agg(substr(e.key, 7), e.value::bigint) FROM jsonb_each_text(to_jsonb(ss)) e
    WHERE e.key LIKE 'confl\_%' AND e.value ~ '^[0-9]+$' AND e.value::bigint > 0), '{}')::text`
	}
	sql := `SELECT s.oid::text, s.subenabled, pg_get_userbyid(s.subowner), coalesce(s.subslotname, ''), ` + opts + `,
  coalesce((SELECT o.remote_lsn::text FROM pg_replication_origin_status o WHERE o.external_id = 'pg_' || s.oid), ''),
  ` + errs + `, ` + conflicts + `
FROM pg_subscription s
` + join + `
WHERE s.subname = $1 AND s.subdbid = (SELECT oid FROM pg_database WHERE datname = current_database())`
	res := pgRead(ctx, conn, sql, [][]byte{[]byte(name)})
	if res.Err != nil {
		return nil, res.Err
	}
	if len(res.Rows) == 0 {
		return nil, errors.New("this database has no subscription named " + name)
	}
	r := res.Rows[0]
	num := func(b []byte) *int64 {
		if len(b) == 0 {
			return nil
		}
		n, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	w := &PGSubscriptionWhy{Name: name, Enabled: string(r[1]) == "t", Owner: string(r[2]), Slot: string(r[3]),
		DisableOnError: onOff(string(r[4])), Binary: onOff(string(r[5])), Origin: "pg_" + string(r[0]),
		AppliedTo: string(r[6]), ApplyErrors: num(r[7]), SyncErrors: num(r[8])}
	if c := string(r[9]); c != "" && c != "{}" {
		_ = json.Unmarshal([]byte(c), &w.Conflicts)
	}

	files, note := serverLogFiles(ctx, conn, major)
	w.LogNote = note
	var entries []logEntry
	for _, f := range files {
		got, err := readLogTail(f.path, f.format)
		if err != nil {
			if w.LogNote == "" {
				w.LogNote = "could not read " + f.path + ": " + err.Error()
			}
			continue
		}
		if w.Log == "" {
			w.Log = f.path
		}
		// Older files come later in files; their entries go first.
		entries = append(got, entries...)
	}
	attributeErrors(w, entries)
	return w, nil
}

type logFile struct{ path, format string }

var debianData = regexp.MustCompile(`^/var/lib/postgresql/([0-9.]+)/([^/]+)/?$`)

// serverLogFiles finds where this server logs, newest file first, with the
// one before it when the log rotates into a directory. The structured
// formats are preferred to stderr's free text. With the collector off,
// Debian and Ubuntu's pg_ctlcluster still write a file, beside the data in a
// fixed place; anything else (journald, syslog, a container's stdout) is out
// of reach and the note says so.
func serverLogFiles(ctx context.Context, conn *caduceus.Conn, major int) ([]logFile, string) {
	formats := []string{"jsonlog", "csvlog", "stderr"}
	if major < 15 {
		formats = formats[1:]
	}
	cols := make([]string, 0, len(formats))
	for _, f := range formats {
		cols = append(cols, "coalesce(pg_current_logfile('"+f+"'), '')")
	}
	res := pgRead(ctx, conn, "SELECT current_setting('data_directory'), current_setting('logging_collector'), current_setting('log_destination'), "+
		strings.Join(cols, ", "), nil)
	if res.Err != nil {
		return nil, "could not ask the server where it logs: " + res.Err.Error()
	}
	row := res.Rows[0]
	data := string(row[0])
	for i, f := range formats {
		p := string(row[3+i])
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(data, p)
		}
		out := []logFile{{p, f}}
		if prev := previousLogFile(p); prev != "" {
			out = append(out, logFile{prev, f})
		}
		return out, ""
	}
	if m := debianData.FindStringSubmatch(data); m != nil {
		p := "/var/log/postgresql/postgresql-" + m[1] + "-" + m[2] + ".log"
		if _, err := os.Stat(p); err == nil {
			out := []logFile{{p, "stderr"}}
			if _, err := os.Stat(p + ".1"); err == nil {
				out = append(out, logFile{p + ".1", "stderr"})
			}
			return out, ""
		}
	}
	return nil, "the server does not log to a file Argus can read (logging_collector is " + string(row[1]) +
		", log_destination is " + string(row[2]) + "). Turn logging_collector on to see the message here."
}

// previousLogFile is the newest file in p's directory, with p's extension,
// older than p: where the error is when the log rotated after it.
func previousLogFile(p string) string {
	dir, ext := filepath.Dir(p), filepath.Ext(p)
	cur, err := os.Stat(p)
	if err != nil {
		return ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type cand struct {
		path string
		at   int64
	}
	var cs []cand
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ext || filepath.Join(dir, e.Name()) == p {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cur.ModTime()) {
			continue
		}
		cs = append(cs, cand{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(cs) == 0 {
		return ""
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].at > cs[j].at })
	return cs[0].path
}

// logEntry is one report from the log, whatever format it was in.
type logEntry struct {
	at, pid, severity, code, message, detail, hint, context string
	// prefix is stderr's log_line_prefix, to join a report's lines.
	prefix string
}

func readLogTail(path, format string) ([]logEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - whyTail
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, whyTail))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		// The first line is cut; a csv record may span lines, so skip to
		// the next line that starts like a timestamp.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return parseLog(b, format), nil
}

func parseLog(b []byte, format string) []logEntry {
	switch format {
	case "jsonlog":
		return parseJSONLog(b)
	case "csvlog":
		return parseCSVLog(b)
	}
	return parseStderrLog(b)
}

func parseJSONLog(b []byte) []logEntry {
	var out []logEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var j struct {
			Timestamp string `json:"timestamp"`
			PID       int    `json:"pid"`
			Severity  string `json:"error_severity"`
			Code      string `json:"state_code"`
			Message   string `json:"message"`
			Detail    string `json:"detail"`
			Hint      string `json:"hint"`
			Context   string `json:"context"`
		}
		if json.Unmarshal(sc.Bytes(), &j) != nil {
			continue
		}
		out = append(out, logEntry{at: j.Timestamp, pid: strconv.Itoa(j.PID), severity: j.Severity, code: j.Code,
			message: j.Message, detail: j.Detail, hint: j.Hint, context: j.Context})
	}
	return out
}

// csvlog's columns, as the documentation lists them.
const (
	csvTime     = 0
	csvPID      = 3
	csvSeverity = 11
	csvCode     = 12
	csvMessage  = 13
	csvDetail   = 14
	csvHint     = 15
	csvContext  = 18
)

func parseCSVLog(b []byte) []logEntry {
	var out []logEntry
	rd := csv.NewReader(bytes.NewReader(b))
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	for {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) <= csvContext {
			continue
		}
		out = append(out, logEntry{at: rec[csvTime], pid: rec[csvPID], severity: rec[csvSeverity], code: rec[csvCode],
			message: rec[csvMessage], detail: rec[csvDetail], hint: rec[csvHint], context: rec[csvContext]})
	}
	return out
}

var (
	stderrLine = regexp.MustCompile(`^(.*?)\b(LOG|ERROR|FATAL|PANIC|WARNING|NOTICE|INFO|DEBUG[1-5]?|DETAIL|HINT|CONTEXT|STATEMENT|QUERY|LOCATION):  (.*)$`)
	stderrPID  = regexp.MustCompile(`\[(\d+)\]`)
	stderrTime = regexp.MustCompile(`^\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d(?:\.\d+)?(?: [A-Za-z+\-0-9:]+)?`)
)

// parseStderrLog reads the plain format: each line of a report carries
// log_line_prefix, then SEVERITY, two spaces and the text; a message's own
// later lines start with a tab and carry no prefix.
func parseStderrLog(b []byte) []logEntry {
	var out []logEntry
	last := -1  // the entry the previous line belonged to
	field := "" // and which of its fields
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "\t") {
			if last >= 0 {
				appendField(&out[last], field, strings.TrimLeft(line, "\t"))
			}
			continue
		}
		m := stderrLine.FindStringSubmatch(line)
		if m == nil {
			last = -1
			continue
		}
		prefix, sev, text := m[1], m[2], m[3]
		switch sev {
		case "DETAIL", "HINT", "CONTEXT", "STATEMENT", "QUERY", "LOCATION":
			// Belongs to the latest report with the same prefix.
			last = -1
			for i := len(out) - 1; i >= 0 && i >= len(out)-8; i-- {
				if out[i].prefix == prefix {
					last = i
					break
				}
			}
			if last >= 0 {
				field = sev
				appendField(&out[last], field, text)
			}
			continue
		}
		e := logEntry{prefix: prefix, severity: sev, message: text}
		if p := stderrPID.FindStringSubmatch(prefix); p != nil {
			e.pid = p[1]
		}
		e.at = stderrTime.FindString(strings.TrimSpace(prefix))
		out = append(out, e)
		last, field = len(out)-1, "MESSAGE"
	}
	return out
}

func appendField(e *logEntry, field, text string) {
	add := func(s *string) {
		if *s == "" {
			*s = text
		} else {
			*s += "\n" + text
		}
	}
	switch field {
	case "MESSAGE":
		add(&e.message)
	case "DETAIL":
		add(&e.detail)
	case "HINT":
		add(&e.hint)
	case "CONTEXT":
		add(&e.context)
	}
}

var (
	forSubscription = regexp.MustCompile(`for subscription "((?:[^"]|"")+)"`)
	disabledBecause = regexp.MustCompile(`^subscription "((?:[^"]|"")+)" has been disabled because of an error`)
	targetRelation  = regexp.MustCompile(`replication target relation "([^"]+)"`)
	conflictOn      = regexp.MustCompile(`conflict detected on relation "([^"]+)": conflict=(\w+)`)
	duringChange    = regexp.MustCompile(`during (?:message type )?"(\w+)"`)
	inTransaction   = regexp.MustCompile(`in transaction (\d+)`)
	finishedAt      = regexp.MustCompile(`finished at ([0-9A-Fa-f]{1,8}/[0-9A-Fa-f]{1,8})`)
	missingColumns  = regexp.MustCompile(`missing replicated columns?: (.+)$`)
	constraintName  = regexp.MustCompile(`constraint "([^"]+)"`)
	keyEquals       = regexp.MustCompile(`(?i)key \(([^)]*)\)=\((.*?)\)(?: already exists| is not present|;|\.\s|\.?$|,)`)
	notNullColumn   = regexp.MustCompile(`column "([^"]+)" of relation "([^"]+)"`)
	relationNamed   = regexp.MustCompile(`(?:table|relation) "([^"]+)"`)
	deniedFor       = regexp.MustCompile(`permission denied for (?:table|relation) (\S+)`)
	workerExited    = regexp.MustCompile(`\(PID (\d+)\) exited`)
)

// attributeErrors keeps the errors that are this subscription's and fills
// w with the last of them.
func attributeErrors(w *PGSubscriptionWhy, entries []logEntry) {
	origin := `replication origin "` + w.Origin
	// pid → subscription, from the lines a worker writes as it starts.
	// Bounded by the entries read; a pid reused by another worker is
	// overwritten when that one starts.
	workers := map[string]string{}
	sync := map[string]bool{}
	var last *PGApplyError
	for _, e := range entries {
		msg := e.message
		named := ""
		if m := forSubscription.FindStringSubmatch(msg); m != nil {
			named = strings.ReplaceAll(m[1], `""`, `"`)
			if e.pid != "" && strings.HasSuffix(msg, "has started") {
				workers[e.pid] = named
				sync[e.pid] = strings.Contains(msg, "synchronization worker")
			}
		}
		if m := workerExited.FindStringSubmatch(msg); m != nil {
			// The pid is free again; what reuses it is not this worker.
			delete(workers, m[1])
			continue
		}
		if m := disabledBecause.FindStringSubmatch(msg); m != nil {
			if strings.ReplaceAll(m[1], `""`, `"`) == w.Name {
				w.TurnedOff, w.TurnedOffAt = true, e.at
			}
			continue
		}
		if e.severity != "ERROR" && e.severity != "FATAL" && e.severity != "PANIC" {
			continue
		}
		ours := named == w.Name ||
			strings.Contains(e.context, origin+`"`) || strings.Contains(e.context, origin+`_`) ||
			(e.pid != "" && workers[e.pid] == w.Name && named == "")
		if !ours {
			continue
		}
		a := applyError(e)
		if sync[e.pid] || strings.Contains(e.context, origin+`_`) {
			a.Worker = "sync"
		}
		last = a
		w.Errors++
		if w.FirstAt == "" {
			w.FirstAt = e.at
		}
		// A later error is a new cause; whether it turned the
		// subscription off is said by a line after it.
		w.TurnedOff, w.TurnedOffAt = false, ""
	}
	w.Error = last
}

func applyError(e logEntry) *PGApplyError {
	cut := func(s string) string {
		if len(s) > whyField {
			return s[:whyField] + "…"
		}
		return s
	}
	a := &PGApplyError{At: e.at, Severity: e.severity, Code: e.code, Message: cut(e.message), Detail: cut(e.detail),
		Hint: cut(e.hint), Context: cut(e.context), Worker: "apply"}
	if m := targetRelation.FindStringSubmatch(e.context); m != nil {
		a.Table = m[1]
	} else if m := targetRelation.FindStringSubmatch(e.message); m != nil {
		a.Table = m[1]
	}
	if m := conflictOn.FindStringSubmatch(e.message); m != nil {
		a.Table, a.Conflict = m[1], m[2]
	}
	if m := duringChange.FindStringSubmatch(e.context); m != nil {
		a.Change = m[1]
	}
	if m := inTransaction.FindStringSubmatch(e.context); m != nil {
		a.Xid = m[1]
	}
	if m := finishedAt.FindStringSubmatch(e.context); m != nil {
		a.LSN = strings.ToUpper(m[1])
	}
	if m := missingColumns.FindStringSubmatch(e.message); m != nil {
		a.Columns = m[1]
	}
	if m := constraintName.FindStringSubmatch(e.message); m != nil {
		a.Constraint = m[1]
	}
	if m := keyEquals.FindStringSubmatch(e.detail); m != nil {
		a.KeyColumns, a.KeyValues = m[1], m[2]
	}
	if m := notNullColumn.FindStringSubmatch(e.message); m != nil {
		a.Columns = m[1]
	}
	if a.Table == "" {
		// "permission denied for table orders", a foreign key's "on table".
		if m := relationNamed.FindStringSubmatch(e.message); m != nil {
			a.Table = m[1]
		} else if m := deniedFor.FindStringSubmatch(e.message); m != nil {
			a.Table = m[1]
		}
	}
	return a
}
