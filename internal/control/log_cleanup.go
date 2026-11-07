package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Cleaning log files on servers, from the console.

The agent reports what logs use its server's disk on its heartbeat; the
console shows it on the server's Logs tab and asks the agent to clean what
an admin picked, or what a rule says once a disk fills or a window opens.
The agent side is internal/agent/logclean*.go, which says why it is built
the way it is; the short of it for this side:

  - The console never sends a path the scan did not report. A cleanup is
    planned here from the agent's own report and the rule (planLogCleanup),
    and the agent checks every file again before it goes.
  - What a cleanup removed is kept with it (log_cleanup_runs.removed) and
    named in the audit log, because a deletion on a managed server is
    something a reconstruction needs to see.
  - Login and audit records, the Security event log and Argus's own files
    are never offered; the agent refuses them whatever it is sent.
*/

// Wire types, as the agent sends them (internal/agent/logclean.go).

// LogFile is one file a scan found.
type LogFile struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// LogItem is one thing that can be cleaned.
type LogItem struct {
	Key        string     `json:"key"`
	Kind       string     `json:"kind"`
	Title      string     `json:"title"`
	Detail     string     `json:"detail,omitempty"`
	Root       string     `json:"root"`
	Mount      string     `json:"mount,omitempty"`
	Bytes      int64      `json:"bytes"`
	Count      int        `json:"count"`
	Oldest     *time.Time `json:"oldest,omitempty"`
	Newest     *time.Time `json:"newest,omitempty"`
	Files      []LogFile  `json:"files,omitempty"`
	More       int        `json:"more,omitempty"`
	MinAgeDays int        `json:"min_age_days,omitempty"`
}

// OpenDeleted is a deleted file a process still holds.
type OpenDeleted struct {
	PID     int    `json:"pid"`
	FD      int    `json:"fd"`
	Process string `json:"process"`
	Unit    string `json:"unit,omitempty"`
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
}

// ProtectedLog is a group of logs never cleaned.
type ProtectedLog struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Bytes  int64  `json:"bytes"`
}

// EventLog is a Windows event log.
type EventLog struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	MaxBytes  int64  `json:"max_bytes"`
	Retain    bool   `json:"retain,omitempty"`
	Protected bool   `json:"protected,omitempty"`
	Records   int64  `json:"records,omitempty"`
}

// LogDisk is a filesystem as the agent reports it.
type LogDisk struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
}

// percent is how full d is, counted like df: used over used plus what can
// still be written, so the root reserve does not hide a full disk.
func (d LogDisk) percent() int {
	if d.Used+d.Avail == 0 {
		return 0
	}
	return int(d.Used * 100 / (d.Used + d.Avail))
}

// LogReport is what the agent's scan found.
type LogReport struct {
	ScannedAt        time.Time      `json:"scanned_at"`
	OS               string         `json:"os"`
	Supported        bool           `json:"supported"`
	Reason           string         `json:"reason,omitempty"`
	Error            string         `json:"error,omitempty"`
	Disks            []LogDisk      `json:"disks"`
	Items            []LogItem      `json:"items"`
	Open             []OpenDeleted  `json:"open"`
	Protected        []ProtectedLog `json:"protected"`
	EventLogs        []EventLog     `json:"event_logs,omitempty"`
	ActiveOver       int64          `json:"active_over"`
	UTCOffsetMinutes int            `json:"utc_offset_minutes"`
}

// bounded keeps what a misbehaving agent sends to the size a real one does.
func (r *LogReport) bounded() {
	if len(r.Items) > 60 {
		r.Items = r.Items[:60]
	}
	left := 4000
	for i := range r.Items {
		it := &r.Items[i]
		if len(it.Files) > left {
			it.More += len(it.Files) - left
			it.Files = it.Files[:left]
		}
		left -= len(it.Files)
		it.Title, it.Detail = clip(it.Title, 300), clip(it.Detail, 300)
	}
	if len(r.Open) > 50 {
		r.Open = r.Open[:50]
	}
	if len(r.Disks) > 32 {
		r.Disks = r.Disks[:32]
	}
	if len(r.Protected) > 10 {
		r.Protected = r.Protected[:10]
	}
	if len(r.EventLogs) > 10 {
		r.EventLogs = r.EventLogs[:10]
	}
	r.Reason, r.Error = clip(r.Reason, 500), clip(r.Error, 1000)
	if r.Disks == nil {
		r.Disks = []LogDisk{}
	}
	if r.Items == nil {
		r.Items = []LogItem{}
	}
	if r.Open == nil {
		r.Open = []OpenDeleted{}
	}
	if r.Protected == nil {
		r.Protected = []ProtectedLog{}
	}
}

func (r LogReport) item(key string) *LogItem {
	for i := range r.Items {
		if r.Items[i].Key == key {
			return &r.Items[i]
		}
	}
	return nil
}

func (r LogReport) openBytes() int64 {
	var n int64
	for _, o := range r.Open {
		n += o.Bytes
	}
	return n
}

// LogAction is one step of a cleanup, as the agent reads it.
type LogAction struct {
	Kind      string    `json:"kind"`
	Item      string    `json:"item"`
	Files     []LogFile `json:"files,omitempty"`
	KeepDays  int       `json:"keep_days,omitempty"`
	Path      string    `json:"path,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	KeepBytes int64     `json:"keep_bytes,omitempty"`
	MaxBytes  int64     `json:"max_bytes,omitempty"`
	PID       int       `json:"pid,omitempty"`
	FD        int       `json:"fd,omitempty"`
	Unit      string    `json:"unit,omitempty"`
	Log       string    `json:"log,omitempty"`
}

// LogRemoved is one line of what a cleanup did.
type LogRemoved struct {
	Path     string    `json:"path"`
	Action   string    `json:"action"`
	Bytes    int64     `json:"bytes"`
	After    int64     `json:"after,omitempty"`
	Modified time.Time `json:"modified,omitempty"`
}

// LogSkipped is a file a cleanup left alone, and why.
type LogSkipped struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// LogRunStatus is what the agent says about a cleanup.
type LogRunStatus struct {
	ID      string       `json:"id"`
	State   string       `json:"state"`
	Step    string       `json:"step,omitempty"`
	Message string       `json:"message,omitempty"`
	Freed   int64        `json:"freed"`
	Removed []LogRemoved `json:"removed,omitempty"`
	More    int          `json:"more,omitempty"`
	Skipped []LogSkipped `json:"skipped,omitempty"`
}

// LogBeat is the logs part of a heartbeat. Absent from an agent older than
// log cleanup.
type LogBeat struct {
	Supported bool          `json:"supported"`
	Reason    string        `json:"reason,omitempty"`
	Report    *LogReport    `json:"report,omitempty"`
	ScanID    string        `json:"scan_id,omitempty"`
	Scanning  bool          `json:"scanning,omitempty"`
	Run       *LogRunStatus `json:"run,omitempty"`
}

type logsState struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Scanning  bool   `json:"scanning,omitempty"`
}

// LogCleanRequest is a cleanup as the agent reads it.
type LogCleanRequest struct {
	ID      string      `json:"id"`
	Actions []LogAction `json:"actions"`
	Paths   []string    `json:"paths,omitempty"`
	Protect []string    `json:"protect,omitempty"`
}

// LogOrders is what goes on a heartbeat reply.
type LogOrders struct {
	Scan         bool             `json:"scan"`
	ScanID       string           `json:"scan_id,omitempty"`
	Paths        []string         `json:"paths,omitempty"`
	Protect      []string         `json:"protect,omitempty"`
	ActiveOverMB int              `json:"active_over_mb,omitempty"`
	Run          *LogCleanRequest `json:"run,omitempty"`
}

/* ── Rules ───────────────────────────────────────────────────────────────── */

// Rule modes.
const (
	LogCleanOff   = "off"
	LogCleanDisk  = "disk"
	LogCleanDaily = "daily"
)

// LogCleanupRule says what a cleanup keeps on servers with a tag, and when
// one starts on its own.
type LogCleanupRule struct {
	ID                string `json:"id,omitempty"`
	Position          int    `json:"position"`
	Tag               string `json:"tag"`
	Mode              string `json:"mode"`
	DiskPercent       int    `json:"diskPercent"`
	WindowStartMinute int    `json:"windowStartMinute"`
	WindowMinutes     int    `json:"windowMinutes"`
	RotatedKeepDays   int    `json:"rotatedKeepDays"`
	JournalKeepDays   int    `json:"journalKeepDays"`
	JournalMaxMB      int    `json:"journalMaxMb"`
	// ActiveOverMB empties a log still being written above this size; 0
	// leaves live logs alone.
	ActiveOverMB  int  `json:"activeOverMb"`
	ActiveKeepMB  int  `json:"activeKeepMb"`
	NeedsApproval bool `json:"needsApproval"`
}

// LogCleanupPolicy is every rule and the extra paths.
type LogCleanupPolicy struct {
	Rules   []LogCleanupRule `json:"rules"`
	Paths   []string         `json:"paths"`
	Protect []string         `json:"protect"`
}

// defaultLogRule is what a server gets when no rule can be read: show, and
// clean nothing on its own.
func defaultLogRule() LogCleanupRule {
	return LogCleanupRule{Mode: LogCleanOff, DiskPercent: 85, WindowStartMinute: 180, WindowMinutes: 120,
		RotatedKeepDays: 14, JournalKeepDays: 28, JournalMaxMB: 2048, ActiveKeepMB: 100}
}

// Validate checks a policy before it is saved.
func (p *LogCleanupPolicy) Validate() error {
	if len(p.Rules) == 0 || len(p.Rules) > 50 {
		return errors.New("give between one and fifty rules, including the one for every other server")
	}
	seen := map[string]bool{}
	defaults := 0
	for i := range p.Rules {
		r := &p.Rules[i]
		r.Tag = strings.TrimSpace(r.Tag)
		if r.Tag == "" {
			defaults++
		} else if !reTag.MatchString(r.Tag) {
			return fmt.Errorf("%q is not a server tag", r.Tag)
		}
		if seen[strings.ToLower(r.Tag)] {
			return fmt.Errorf("two rules are for the tag %q; keep one", r.Tag)
		}
		seen[strings.ToLower(r.Tag)] = true
		switch r.Mode {
		case LogCleanOff, LogCleanDisk, LogCleanDaily:
		default:
			return fmt.Errorf("%q is not a way to clean; use off, disk or daily", r.Mode)
		}
		switch {
		case r.DiskPercent < 50 || r.DiskPercent > 99:
			return errors.New("clean when a disk passes between 50% and 99%")
		case r.WindowStartMinute < 0 || r.WindowStartMinute > 1439:
			return errors.New("a window starts between 00:00 and 23:59")
		case r.WindowMinutes < 30 || r.WindowMinutes > 1440:
			return errors.New("a window lasts between 30 minutes and 24 hours")
		case r.RotatedKeepDays < 0 || r.RotatedKeepDays > 3650:
			return errors.New("keep rotated logs between 0 and 3650 days")
		case r.JournalKeepDays < 7 || r.JournalKeepDays > 3650:
			return errors.New("keep at least a week of the journal: it holds sshd and sudo records too")
		case r.JournalMaxMB < 256 || r.JournalMaxMB > 1048576:
			return errors.New("let the journal keep between 256 MB and 1 TB")
		case r.ActiveOverMB != 0 && (r.ActiveOverMB < 100 || r.ActiveOverMB > 1048576):
			return errors.New("empty live logs above at least 100 MB, or never")
		case r.ActiveKeepMB < 0 || r.ActiveKeepMB > 256:
			return errors.New("keep between 0 and 256 MB of an emptied log")
		}
		r.Position = i
	}
	if defaults != 1 {
		return errors.New("keep exactly one rule for every other server (the one with no tag)")
	}
	clean := func(list []string, what string) ([]string, error) {
		if len(list) > 50 {
			return nil, fmt.Errorf("give at most fifty %s", what)
		}
		out := []string{}
		for _, g := range list {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			if err := validLogGlob(g); err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return dedupe(out), nil
	}
	var err error
	if p.Paths, err = clean(p.Paths, "paths to clean"); err != nil {
		return err
	}
	if p.Protect, err = clean(p.Protect, "paths to keep"); err != nil {
		return err
	}
	return nil
}

var (
	reLogGlobBad  = regexp.MustCompile(`[\x00-\x1f]`)
	logGlobDenied = []string{"/etc/", "/usr/", "/bin/", "/sbin/", "/lib", "/boot/", "/dev/", "/proc/",
		"/sys/", "/run/", "/root/", "/var/lib/", "/home/"}
	winLogGlobDenied = []string{`\windows\system32`, `\windows\syswow64`, `\programdata\argus`,
		`\windows\winsxs`, `\users\`}
)

// validLogGlob checks a rule's extra path. The agent applies the same rule
// (agent.ValidLogGlob) and refuses a path that breaks it.
func validLogGlob(g string) error {
	if len(g) > 300 || reLogGlobBad.MatchString(g) {
		return fmt.Errorf("%q is not a path", g)
	}
	if strings.Contains(g, "..") {
		return fmt.Errorf("%q: a path cannot go up with ..", g)
	}
	if _, err := filepath.Match(g, ""); err != nil {
		return fmt.Errorf("%q is not a valid pattern", g)
	}
	if !strings.Contains(strings.ToLower(g), "log") {
		return fmt.Errorf("%q: a log path says log somewhere in it", g)
	}
	switch {
	case strings.HasPrefix(g, "/"):
		if strings.Count(strings.TrimSuffix(g, "/"), "/") < 2 {
			return fmt.Errorf("%q is too near the top of the disk", g)
		}
		for _, d := range logGlobDenied {
			if strings.HasPrefix(g, d) {
				return fmt.Errorf("%q is in a system folder Argus never cleans", g)
			}
		}
	case len(g) > 3 && g[1] == ':' && (g[2] == '\\' || g[2] == '/'):
		low := strings.ToLower(strings.ReplaceAll(g, "/", `\`))
		if strings.Count(strings.TrimSuffix(low, `\`), `\`) < 2 {
			return fmt.Errorf("%q is too near the top of the disk", g)
		}
		for _, d := range winLogGlobDenied {
			if strings.Contains(low, d) {
				return fmt.Errorf("%q is in a system folder Argus never cleans", g)
			}
		}
	default:
		return fmt.Errorf("%q is not an absolute path", g)
	}
	return nil
}

// LogCleanupPolicy reads every rule and the extra paths.
func (s *Store) LogCleanupPolicy(ctx context.Context) (LogCleanupPolicy, error) {
	p := LogCleanupPolicy{Rules: []LogCleanupRule{}, Paths: []string{}, Protect: []string{}}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, position, tag, mode, disk_percent, window_start_minute, window_minutes,
		       rotated_keep_days, journal_keep_days, journal_max_mb, active_over_mb, active_keep_mb,
		       needs_approval
		  FROM log_cleanup_rules ORDER BY (tag = '') ASC, position, tag`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var r LogCleanupRule
		if err := rows.Scan(&r.ID, &r.Position, &r.Tag, &r.Mode, &r.DiskPercent, &r.WindowStartMinute,
			&r.WindowMinutes, &r.RotatedKeepDays, &r.JournalKeepDays, &r.JournalMaxMB, &r.ActiveOverMB,
			&r.ActiveKeepMB, &r.NeedsApproval); err != nil {
			return p, err
		}
		p.Rules = append(p.Rules, r)
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT log_clean_paths, log_protect_paths FROM gateway_policy LIMIT 1`).
		Scan(&p.Paths, &p.Protect)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if p.Paths == nil {
		p.Paths = []string{}
	}
	if p.Protect == nil {
		p.Protect = []string{}
	}
	return p, err
}

// SaveLogCleanupPolicy replaces every rule and the extra paths.
func (s *Store) SaveLogCleanupPolicy(ctx context.Context, p LogCleanupPolicy) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM log_cleanup_rules`); err != nil {
		return err
	}
	for _, r := range p.Rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO log_cleanup_rules (position, tag, mode, disk_percent, window_start_minute,
			       window_minutes, rotated_keep_days, journal_keep_days, journal_max_mb, active_over_mb,
			       active_keep_mb, needs_approval)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			r.Position, r.Tag, r.Mode, r.DiskPercent, r.WindowStartMinute, r.WindowMinutes, r.RotatedKeepDays,
			r.JournalKeepDays, r.JournalMaxMB, r.ActiveOverMB, r.ActiveKeepMB, r.NeedsApproval); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE gateway_policy SET log_clean_paths = $1, log_protect_paths = $2`,
		p.Paths, p.Protect); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ruleFor is the rule for a server with tags: the first rule naming one of
// them, else the one for every other server.
func (p LogCleanupPolicy) ruleFor(tags []string) LogCleanupRule {
	has := map[string]bool{}
	for _, t := range tags {
		has[strings.ToLower(t)] = true
	}
	var def *LogCleanupRule
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Tag == "" {
			def = r
			continue
		}
		if has[strings.ToLower(r.Tag)] {
			return *r
		}
	}
	if def != nil {
		return *def
	}
	return defaultLogRule()
}

// inWindow reports whether now is inside r's daily window, by a clock
// offset minutes from UTC, and the server's date the window opened on.
func (r LogCleanupRule) inWindow(now time.Time, offsetMinutes int) (string, bool) {
	local := now.UTC().Add(time.Duration(offsetMinutes) * time.Minute)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	for back := 0; back <= 1; back++ {
		d := day.AddDate(0, 0, -back)
		start := d.Add(time.Duration(r.WindowStartMinute) * time.Minute)
		end := start.Add(time.Duration(r.WindowMinutes) * time.Minute)
		if !local.Before(start) && local.Before(end) {
			return d.Format("2006-01-02"), true
		}
	}
	return "", false
}

// serverDay is the server's own date at now.
func serverDay(now time.Time, offsetMinutes int) string {
	return now.UTC().Add(time.Duration(offsetMinutes) * time.Minute).Format("2006-01-02")
}

/* ── Planning ────────────────────────────────────────────────────────────── */

// LogPreview is what cleaning an item would do under a rule.
type LogPreview struct {
	Key   string `json:"key"`
	Does  string `json:"does"`
	Frees int64  `json:"frees"`
	// Files is how many files would go, for the kinds that delete.
	Files int `json:"files"`
	// Automatic is whether a rule's own cleanup takes it.
	Automatic bool `json:"automatic"`
}

// previewItem says what cleaning it would do under rule.
func previewItem(it LogItem, rule LogCleanupRule, now time.Time) (LogPreview, []LogAction) {
	p := LogPreview{Key: it.Key}
	switch it.Kind {
	case "journal":
		keep := int64(rule.JournalMaxMB) << 20
		p.Does = fmt.Sprintf("Keep %s, at most %s", days(rule.JournalKeepDays), mb(rule.JournalMaxMB))
		if it.Bytes > keep {
			p.Frees = it.Bytes - keep
		}
		// Older than the days kept goes too, but the scan does not say how
		// much of the journal that is: only the size is promised.
		p.Automatic = true
		return p, []LogAction{{Kind: "journal", Item: it.Key, KeepDays: rule.JournalKeepDays, MaxBytes: keep}}
	case "rotated", "files":
		keepDays := rule.RotatedKeepDays
		if it.MinAgeDays > keepDays {
			keepDays = it.MinAgeDays
		}
		cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
		var files []LogFile
		for _, f := range it.Files {
			if f.Modified.Before(cutoff) {
				files = append(files, f)
				p.Frees += f.Bytes
			}
		}
		p.Files = len(files)
		if keepDays == 0 {
			p.Does = "Delete"
		} else {
			p.Does = "Delete older than " + days(keepDays)
		}
		p.Automatic = it.Key != "dumps"
		if len(files) == 0 {
			return p, nil
		}
		return p, []LogAction{{Kind: "delete", Item: it.Key, Files: files, KeepDays: keepDays}}
	case "active", "container":
		keep := int64(rule.ActiveKeepMB) << 20
		p.Does = "Empty it, keep the last " + mb(rule.ActiveKeepMB)
		if rule.ActiveKeepMB == 0 {
			p.Does = "Empty it"
		}
		if it.Bytes > keep {
			p.Frees = it.Bytes - keep
		}
		p.Automatic = rule.ActiveOverMB > 0 && it.Bytes >= int64(rule.ActiveOverMB)<<20
		if p.Frees == 0 {
			return p, nil
		}
		return p, []LogAction{{Kind: "truncate", Item: it.Key, Path: it.Root, Bytes: it.Bytes, KeepBytes: keep}}
	}
	return p, nil
}

func days(n int) string {
	switch {
	case n == 1:
		return "1 day"
	case n%7 == 0 && n >= 14 && n <= 70:
		return fmt.Sprintf("%d weeks", n/7)
	}
	return fmt.Sprintf("%d days", n)
}

func mb(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%d GB", n/1024)
	}
	return fmt.Sprintf("%d MB", n)
}

func gbytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// LogPick is what an admin picked to clean.
type LogPick struct {
	Items []string `json:"items"`
	// Open are deleted files still held, by process and descriptor.
	Open []struct {
		PID int `json:"pid"`
		FD  int `json:"fd"`
	} `json:"open"`
	// Restart are services holding deleted files.
	Restart []string `json:"restart"`
	// EventLogs raises event logs' maximum sizes.
	EventLogs []struct {
		Name  string `json:"name"`
		MaxMB int    `json:"maxMb"`
	} `json:"eventLogs"`
}

// logPlan is a cleanup planned from a report.
type logPlan struct {
	Actions []LogAction
	Items   []string
	Bytes   int64
	Summary string
}

// planLogCleanup turns what was picked into actions on the scan's own
// files. Nothing picked that the report does not name is planned: the
// console cannot send the agent a path of its own.
func planLogCleanup(r LogReport, rule LogCleanupRule, pick LogPick, now time.Time) (logPlan, error) {
	var (
		out   logPlan
		parts []string
	)
	seen := map[string]bool{}
	for _, key := range pick.Items {
		if seen[key] {
			continue
		}
		seen[key] = true
		it := r.item(key)
		if it == nil {
			return out, fmt.Errorf("%q is not in the server's last scan; scan again and pick from that", key)
		}
		p, acts := previewItem(*it, rule, now)
		if len(acts) == 0 {
			continue
		}
		out.Actions = append(out.Actions, acts...)
		out.Items = append(out.Items, key)
		out.Bytes += p.Frees
		what := it.Title
		if p.Files > 0 {
			what += fmt.Sprintf(" (%d file%s)", p.Files, plural(p.Files))
		}
		parts = append(parts, what)
	}
	seenOpen := map[[2]int]bool{}
	for _, o := range pick.Open {
		if seenOpen[[2]int{o.PID, o.FD}] {
			continue
		}
		seenOpen[[2]int{o.PID, o.FD}] = true
		var hit *OpenDeleted
		for i := range r.Open {
			if r.Open[i].PID == o.PID && r.Open[i].FD == o.FD {
				hit = &r.Open[i]
			}
		}
		if hit == nil {
			return out, errors.New("that open file is not in the server's last scan; scan again")
		}
		out.Actions = append(out.Actions, LogAction{Kind: "open_truncate", Item: "open", PID: hit.PID, FD: hit.FD,
			Path: hit.Path, Bytes: hit.Bytes})
		out.Bytes += hit.Bytes
		parts = append(parts, "the deleted "+filepath.Base(hit.Path)+" held by "+hit.Process)
	}
	for _, unit := range dedupe(pick.Restart) {
		var held int64
		for _, o := range r.Open {
			if o.Unit == unit {
				held += o.Bytes
			}
		}
		if held == 0 || unit == "" {
			return out, fmt.Errorf("%q holds no deleted file in the server's last scan", unit)
		}
		out.Actions = append(out.Actions, LogAction{Kind: "restart_unit", Item: "open", Unit: unit, Bytes: held})
		out.Bytes += held
		parts = append(parts, "restart "+unit)
	}
	for _, e := range pick.EventLogs {
		var hit *EventLog
		for i := range r.EventLogs {
			if r.EventLogs[i].Name == e.Name {
				hit = &r.EventLogs[i]
			}
		}
		want := int64(e.MaxMB) << 20
		if hit == nil {
			return out, fmt.Errorf("%q is not an event log the server reported", e.Name)
		}
		if want <= hit.MaxBytes || e.MaxMB > 4096 {
			return out, fmt.Errorf("raise the %s log above its %s, to at most 4 GB", e.Name, gbytes(hit.MaxBytes))
		}
		out.Actions = append(out.Actions, LogAction{Kind: "eventlog_max", Item: "eventlog:" + e.Name, Log: e.Name,
			MaxBytes: want})
		parts = append(parts, fmt.Sprintf("raise the %s log to %s", e.Name, mb(e.MaxMB)))
	}
	if len(out.Actions) == 0 {
		return out, errors.New("nothing picked would free anything under this server's rule")
	}
	out.Summary = clip(strings.Join(parts, ", "), 1000)
	return out, nil
}

// automaticPick is what a rule's own cleanup takes: everything it keeps
// days or sizes of, never a crash dump, a restart or an open file.
func automaticPick(r LogReport, rule LogCleanupRule, now time.Time) LogPick {
	var pick LogPick
	for _, it := range r.Items {
		if p, acts := previewItem(it, rule, now); p.Automatic && len(acts) > 0 {
			pick.Items = append(pick.Items, it.Key)
		}
	}
	return pick
}

// fullDisk is the fullest disk at or over percent, if any.
func (r LogReport) fullDisk(percent int) (LogDisk, bool) {
	var (
		best LogDisk
		ok   bool
	)
	for _, d := range r.Disks {
		if d.percent() >= percent && (!ok || d.percent() > best.percent()) {
			best, ok = d, true
		}
	}
	return best, ok
}

/* ── What the agent reported ─────────────────────────────────────────────── */

// recordLogBeat keeps what the agent said about logs. It returns the
// report it replaced when b carries a new one.
func (s *Store) recordLogBeat(ctx context.Context, host string, b LogBeat) (*LogReport, error) {
	st, err := json.Marshal(logsState{Supported: b.Supported, Reason: clip(b.Reason, 500), Scanning: b.Scanning})
	if err != nil {
		return nil, err
	}
	if _, err := s.db(ctx).Exec(ctx, `
		UPDATE agents SET logs_state = $2::jsonb
		 WHERE lower(hostname) = lower($1) AND logs_state IS DISTINCT FROM $2::jsonb`, host, st); err != nil {
		return nil, err
	}
	if b.ScanID != "" {
		if _, err := s.db(ctx).Exec(ctx, `
			UPDATE agents SET logs_scan_id = NULL, logs_scan_at = NULL
			 WHERE lower(hostname) = lower($1) AND logs_scan_id = $2`, host, b.ScanID); err != nil {
			return nil, err
		}
	}
	if b.Report == nil {
		return nil, nil
	}
	r := *b.Report
	r.bounded()
	rb, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var old []byte
	err = s.db(ctx).QueryRow(ctx, `
		WITH old AS (SELECT hostname, logs FROM agents WHERE lower(hostname) = lower($1) FOR UPDATE)
		UPDATE agents a SET logs = $2::jsonb FROM old WHERE a.hostname = old.hostname
		RETURNING old.logs`, host, rb).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(old) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p LogReport
	if json.Unmarshal(old, &p) != nil {
		return nil, nil
	}
	return &p, nil
}

// logReportFor reads what host's agent last reported.
func (s *Store) logReportFor(ctx context.Context, host string) (*LogReport, *logsState, bool, error) {
	var (
		rb, sb []byte
		scan   *string
	)
	err := s.db(ctx).QueryRow(ctx, `SELECT logs, logs_state, logs_scan_id FROM agents WHERE lower(hostname) = lower($1)`,
		host).Scan(&rb, &sb, &scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var (
		r  *LogReport
		st *logsState
	)
	if len(rb) > 0 {
		r = &LogReport{}
		if json.Unmarshal(rb, r) != nil {
			r = nil
		}
	}
	if len(sb) > 0 {
		st = &logsState{}
		if json.Unmarshal(sb, st) != nil {
			st = nil
		}
	}
	return r, st, scan != nil, nil
}

func (s *Store) logScanAsked(ctx context.Context, host string) (string, error) {
	var id *string
	err := s.db(ctx).QueryRow(ctx, `SELECT logs_scan_id FROM agents WHERE lower(hostname) = lower($1)`, host).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return "", nil
	}
	return *id, err
}

/* ── Runs ────────────────────────────────────────────────────────────────── */

// A cleanup's states are the package runs' (RunQueued and the rest),
// without waiting and rebooting.

// LogCleanupRun is one cleanup, as the console shows it.
type LogCleanupRun struct {
	ID            string       `json:"id"`
	Hostname      string       `json:"hostname"`
	AssetHostname string       `json:"assetHostname"`
	AssetID       string       `json:"assetId,omitempty"`
	Trigger       string       `json:"trigger"`
	State         string       `json:"state"`
	Items         []string     `json:"items"`
	Summary       string       `json:"summary"`
	PlannedBytes  int64        `json:"plannedBytes"`
	Reason        string       `json:"reason"`
	RequestedBy   string       `json:"requestedBy"`
	RequestedAt   time.Time    `json:"requestedAt"`
	DecidedBy     *string      `json:"decidedBy"`
	DecidedAt     *time.Time   `json:"decidedAt"`
	SentAt        *time.Time   `json:"sentAt"`
	StartedAt     *time.Time   `json:"startedAt"`
	FinishedAt    *time.Time   `json:"finishedAt"`
	UpdatedAt     time.Time    `json:"updatedAt"`
	Step          string       `json:"step"`
	Message       string       `json:"message"`
	Freed         int64        `json:"freed"`
	RemovedCount  int          `json:"removedCount"`
	Removed       []LogRemoved `json:"removed,omitempty"`
	RemovedMore   int          `json:"removedMore"`
	Skipped       []LogSkipped `json:"skipped"`
	// Actions is the plan, kept to send; never shown.
	Actions []LogAction `json:"-"`
}

const logRunCols = `id::text, hostname, asset_hostname, trigger, state, items, summary, planned_bytes, reason,
	requested_by, requested_at, decided_by, decided_at, sent_at, started_at, finished_at, updated_at, step,
	message, freed, removed, removed_more, skipped, actions`

func scanLogRun(row pgx.Row, full bool) (LogCleanupRun, error) {
	var (
		r                      LogCleanupRun
		removed, skipped, acts []byte
		more                   int32
	)
	err := row.Scan(&r.ID, &r.Hostname, &r.AssetHostname, &r.Trigger, &r.State, &r.Items, &r.Summary,
		&r.PlannedBytes, &r.Reason, &r.RequestedBy, &r.RequestedAt, &r.DecidedBy, &r.DecidedAt, &r.SentAt,
		&r.StartedAt, &r.FinishedAt, &r.UpdatedAt, &r.Step, &r.Message, &r.Freed, &removed, &more, &skipped, &acts)
	if err != nil {
		return r, err
	}
	r.RemovedMore = int(more)
	var list []LogRemoved
	_ = json.Unmarshal(removed, &list)
	r.RemovedCount = len(list) + r.RemovedMore
	if full {
		r.Removed = list
		if r.Removed == nil {
			r.Removed = []LogRemoved{}
		}
	}
	_ = json.Unmarshal(skipped, &r.Skipped)
	if r.Skipped == nil {
		r.Skipped = []LogSkipped{}
	}
	if r.Items == nil {
		r.Items = []string{}
	}
	_ = json.Unmarshal(acts, &r.Actions)
	return r, nil
}

// ErrLogRunActive is a cleanup asked of a server that already has one.
var ErrLogRunActive = errors.New("this server already has a log cleanup going or waiting; " +
	"wait for it to finish or take it back")

// ErrLogRunNotFound is a cleanup that does not exist.
var ErrLogRunNotFound = errors.New("no such log cleanup")

type newLogRun struct {
	Hostname, AssetHostname string
	Trigger, State          string
	Plan                    logPlan
	Reason                  string
	RequestedBy             string
	AutoDay                 *string
}

func (s *Store) hasPendingLogRun(ctx context.Context, host string) (bool, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FROM log_cleanup_runs
		 WHERE lower(hostname) = lower($1) AND state IN ('awaiting_approval','queued','running')`, host).Scan(&n)
	return n > 0, err
}

// CreateLogRun creates a cleanup, unless the server has one pending.
func (s *Store) CreateLogRun(ctx context.Context, n newLogRun) (LogCleanupRun, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return LogCleanupRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialise asks for one server: two admins pressing Clean at once
	// must not both get a cleanup, and approval waits are not in the
	// unique index.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('log-cleanup:' || lower($1)))`, n.Hostname); err != nil {
		return LogCleanupRun{}, err
	}
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM log_cleanup_runs
		 WHERE lower(hostname) = lower($1) AND state IN ('awaiting_approval','queued','running')`,
		n.Hostname).Scan(&pending); err != nil {
		return LogCleanupRun{}, err
	}
	if pending > 0 {
		return LogCleanupRun{}, ErrLogRunActive
	}
	acts, err := json.Marshal(n.Plan.Actions)
	if err != nil {
		return LogCleanupRun{}, err
	}
	items := n.Plan.Items
	if items == nil {
		items = []string{}
	}
	r, err := scanLogRun(tx.QueryRow(ctx, `
		INSERT INTO log_cleanup_runs (hostname, asset_hostname, trigger, state, actions, items, summary,
		                              planned_bytes, reason, requested_by, auto_day)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11)
		RETURNING `+logRunCols, n.Hostname, n.AssetHostname, n.Trigger, n.State, acts, items, n.Plan.Summary,
		n.Plan.Bytes, n.Reason, n.RequestedBy, n.AutoDay), false)
	if err != nil {
		if isUniqueViolation(err) {
			return r, ErrLogRunActive
		}
		return r, err
	}
	return r, tx.Commit(ctx)
}

// LogRunByID reads a cleanup with everything it removed.
func (s *Store) LogRunByID(ctx context.Context, id string) (LogCleanupRun, error) {
	r, err := scanLogRun(s.db(ctx).QueryRow(ctx, `SELECT `+logRunCols+` FROM log_cleanup_runs WHERE id::text = $1`, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrLogRunNotFound
	}
	return r, err
}

// LogRunsFor lists host's cleanups, newest first.
func (s *Store) LogRunsFor(ctx context.Context, host string, limit int) ([]LogCleanupRun, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+logRunCols+` FROM log_cleanup_runs
		WHERE lower(hostname) = lower($1) ORDER BY requested_at DESC LIMIT $2`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogCleanupRun{}
	for rows.Next() {
		r, err := scanLogRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// logRunToSend is the cleanup to put on host's heartbeat reply, marking it
// sent: from then on it cannot be taken back.
func (s *Store) logRunToSend(ctx context.Context, host string) (*LogCleanupRun, error) {
	r, err := scanLogRun(s.db(ctx).QueryRow(ctx, `
		UPDATE log_cleanup_runs SET sent_at = COALESCE(sent_at, now())
		 WHERE id = (SELECT id FROM log_cleanup_runs
		              WHERE lower(hostname) = lower($1) AND state IN ('queued','running')
		              ORDER BY requested_at LIMIT 1)
		RETURNING `+logRunCols, host), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// recordLogRunStatus keeps what the agent said about a cleanup it was
// sent. It returns the cleanup when the report ended it, so the end is
// audited once.
func (s *Store) recordLogRunStatus(ctx context.Context, host string, st LogRunStatus) (*LogCleanupRun, error) {
	state := st.State
	switch state {
	case "starting", RunRunning:
		state = RunRunning
	case RunDone, RunFailed, RunInterrupted:
	default:
		return nil, nil
	}
	if len(st.Removed) > 5000 {
		st.More += len(st.Removed) - 5000
		st.Removed = st.Removed[:5000]
	}
	if len(st.Skipped) > 500 {
		st.Skipped = st.Skipped[:500]
	}
	for i := range st.Removed {
		st.Removed[i].Path = clip(st.Removed[i].Path, 1024)
	}
	for i := range st.Skipped {
		st.Skipped[i].Path, st.Skipped[i].Why = clip(st.Skipped[i].Path, 1024), clip(st.Skipped[i].Why, 300)
	}
	removed, _ := json.Marshal(st.Removed)
	if st.Removed == nil {
		removed = []byte("[]")
	}
	skipped, _ := json.Marshal(st.Skipped)
	if st.Skipped == nil {
		skipped = []byte("[]")
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE log_cleanup_runs SET
		       state = $3, step = $4, message = $5, freed = $6, removed = $7::jsonb, removed_more = $8,
		       skipped = $9::jsonb,
		       started_at = COALESCE(started_at, now()),
		       finished_at = CASE WHEN $3 IN ('done','failed','interrupted') THEN now() ELSE NULL END,
		       updated_at = now()
		 WHERE id::text = $2 AND lower(hostname) = lower($1)
		   AND (state IN ('queued','running') OR (state = 'interrupted' AND step = 'stale'))
		   AND ((state, step, message, freed) IS DISTINCT FROM ($3, $4, $5, $6)
		        OR updated_at < now() - interval '5 minutes')
		RETURNING `+logRunCols, host, st.ID, state, clip(st.Step, 300), clip(st.Message, 2000), st.Freed,
		removed, st.More, skipped)
	r, err := scanLogRun(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !runFinished(r.State) {
		return nil, nil
	}
	return &r, nil
}

// DecideLogRun approves or denies a cleanup waiting for approval. The one
// who asked cannot decide it.
func (s *Store) DecideLogRun(ctx context.Context, id, by string, approve bool) (LogCleanupRun, error) {
	r, err := s.LogRunByID(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != RunAwaitingApproval {
		return r, fmt.Errorf("%w: this cleanup is %s, not waiting for approval", ErrRunState,
			strings.ReplaceAll(r.State, "_", " "))
	}
	if strings.EqualFold(r.RequestedBy, by) {
		return r, fmt.Errorf("%w: a cleanup you asked for is approved by another admin", ErrRunState)
	}
	next := RunDenied
	if approve {
		next = RunQueued
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE log_cleanup_runs SET state = $2, decided_by = $3, decided_at = now(), updated_at = now(),
		       finished_at = CASE WHEN $2 = 'denied' THEN now() ELSE NULL END,
		       message = CASE WHEN $2 = 'denied' THEN 'denied by ' || $3 ELSE message END
		 WHERE id::text = $1 AND state = 'awaiting_approval'
		RETURNING `+logRunCols, id, next, by)
	r, err = scanLogRun(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: someone decided this cleanup already", ErrRunState)
	}
	if err != nil && isUniqueViolation(err) {
		return r, ErrLogRunActive
	}
	return r, err
}

// CancelLogRun takes back a cleanup that has not reached the server.
func (s *Store) CancelLogRun(ctx context.Context, id, by string) (LogCleanupRun, error) {
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE log_cleanup_runs SET state = 'cancelled', finished_at = now(), updated_at = now(),
		       message = 'taken back by ' || $2
		 WHERE id::text = $1
		   AND (state = 'awaiting_approval' OR (state = 'queued' AND sent_at IS NULL))
		RETURNING `+logRunCols, id, by)
	r, err := scanLogRun(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.LogRunByID(ctx, id)
		if gerr != nil {
			return cur, gerr
		}
		return cur, fmt.Errorf("%w: the server has already started this cleanup; it reports how it ended", ErrRunState)
	}
	return r, err
}

// expireStaleLogRuns ends cleanups the server stopped reporting on. A
// cleanup takes minutes; an hour of silence is a server that is down.
func (s *Store) expireStaleLogRuns(ctx context.Context, host string) ([]LogCleanupRun, error) {
	rows, err := s.db(ctx).Query(ctx, `
		UPDATE log_cleanup_runs SET state = 'interrupted', step = 'stale', finished_at = now(), updated_at = now(),
		       message = 'the server stopped reporting on this cleanup for an hour; scan it again to see ' ||
		                 'what is still there'
		 WHERE state = 'running' AND updated_at < now() - interval '1 hour'
		   AND ($1 = '' OR lower(hostname) = lower($1))
		RETURNING `+logRunCols, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogCleanupRun
	for rows.Next() {
		r, err := scanLogRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ── The view ────────────────────────────────────────────────────────────── */

// LogsView is a server's Logs tab.
type LogsView struct {
	Hostname  string          `json:"hostname"`
	Enrolled  bool            `json:"enrolled"`
	Reported  bool            `json:"reported"`
	Supported bool            `json:"supported"`
	Reason    string          `json:"reason"`
	Scanning  bool            `json:"scanning"`
	ScanAsked bool            `json:"scanAsked"`
	Report    *LogReport      `json:"report"`
	Previews  []LogPreview    `json:"previews"`
	Rule      LogCleanupRule  `json:"rule"`
	Runs      []LogCleanupRun `json:"runs"`
	// Confirm is what an admin types to clean: the server's short name.
	Confirm string `json:"confirm"`
}

// confirmName is what an admin types to confirm a cleanup on host.
func confirmName(host string) string {
	short, _, _ := strings.Cut(host, ".")
	return strings.ToLower(short)
}
