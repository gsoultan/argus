package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
Cleaning log files, from the console.

The agent says what logs use its server's disk, and the console asks it to
free that space: by hand, after an admin saw the exact list, or by a rule
when a disk fills. Two halves, like package updates (packages.go):

  - The daemon decides. It scans every six hours while the console asks it
    to, and when the console asks for a cleanup it writes a plan and starts
    the helper. It reports both on the heartbeat.

  - The helper does the work: `argus-agent logs-scan` and
    `argus-agent logs-clean`, as root, in a transient systemd unit of their
    own (logclean_linux.go). Outside the agent's sandbox, which cannot write
    /var/log and should not; and outside its cgroup, so a cleanup goes on
    when the agent restarts. On Windows the agent already runs as SYSTEM
    with no sandbox, and the helper's work runs in the agent itself.

Argus's claim is that it can prove later who did what on a server, and
wiping logs is the first thing an intruder does to break that claim. So:

  - Login and audit records, the Windows Security log and Argus's own files
    are never touched. The list is compiled in here (protectedLog); the
    console can add to it, never take from it.
  - The console never names a path to delete that the scan did not report.
    The plan is the scan's own list, and the helper checks every file again
    just before it goes: still a regular file, not through a symlink, still
    under the root it was found in, not protected, the same size and age.
    One that changed is skipped and reported, so a stale list never deletes
    the wrong thing.
  - The journal holds sshd and sudo records too, so a vacuum always keeps at
    least a week of it.
  - The helper reports every file it removed or emptied, with its size and
    modified time; the console keeps that list with the run.

The scan reads names, sizes and dates. It never reads what is in a log.
*/

// LogFile is one file the scan found, as the plan names it back.
type LogFile struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// Log item kinds.
const (
	// LogJournal is the systemd journal; cleaning vacuums it.
	LogJournal = "journal"
	// LogRotated is logrotate's leftovers under /var/log; cleaning deletes
	// the old ones.
	LogRotated = "rotated"
	// LogActive is a log still being written; cleaning empties it.
	LogActive = "active"
	// LogContainer is a container's log; cleaning empties it.
	LogContainer = "container"
	// LogFiles is a set of files, deleted when old: a rule's extra path,
	// and the Windows folders.
	LogFiles = "files"
)

// LogItem is one thing that can be cleaned.
type LogItem struct {
	// Key names the item across scans: "journal", "rotated",
	// "active:<path>", "container:<id>", "path:<glob>", "iis", "winlogs",
	// "temp", "dumps".
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Root is the folder or file the item is.
	Root  string `json:"root"`
	Mount string `json:"mount,omitempty"`
	Bytes int64  `json:"bytes"`
	Count int    `json:"count"`
	// Oldest and Newest are the files' modified times.
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
	// Files are the item's files, oldest first, for the kinds cleaned by
	// deleting files. More is how many were left off the list.
	Files []LogFile `json:"files,omitempty"`
	More  int       `json:"more,omitempty"`
	// MinAgeDays is the youngest a file may be to go, whatever a rule says:
	// Windows Temp keeps a week, as programs leave files there they still use.
	MinAgeDays int `json:"min_age_days,omitempty"`
}

// OpenDeleted is a file someone deleted that a process still holds open,
// so its space is not free yet.
type OpenDeleted struct {
	PID     int    `json:"pid"`
	FD      int    `json:"fd"`
	Process string `json:"process"`
	// Unit is the systemd service the process belongs to, when it does.
	Unit  string `json:"unit,omitempty"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ProtectedLog is a group of logs never cleaned, shown so the operator
// knows their size.
type ProtectedLog struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Bytes  int64  `json:"bytes"`
}

// EventLog is a Windows event log.
type EventLog struct {
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
	MaxBytes int64  `json:"max_bytes"`
	// Retain is whether Windows keeps a full log instead of overwriting
	// its oldest events.
	Retain    bool  `json:"retain,omitempty"`
	Protected bool  `json:"protected,omitempty"`
	Records   int64 `json:"records,omitempty"`
}

// LogReport is what the scan found.
type LogReport struct {
	ScannedAt time.Time `json:"scanned_at"`
	// OS is linux or windows.
	OS        string `json:"os"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Error is why the last scan failed; the rest is what the scan before
	// it found.
	Error     string         `json:"error,omitempty"`
	Disks     []Disk         `json:"disks"`
	Items     []LogItem      `json:"items"`
	Open      []OpenDeleted  `json:"open"`
	Protected []ProtectedLog `json:"protected"`
	EventLogs []EventLog     `json:"event_logs,omitempty"`
	// ActiveOver is the size above which a log still being written is
	// reported.
	ActiveOver       int64 `json:"active_over"`
	UTCOffsetMinutes int   `json:"utc_offset_minutes"`
}

// Bounds on a report, which rides a heartbeat.
const (
	maxLogFilesPerItem = 1000
	maxLogFilesTotal   = 4000
	maxLogItems        = 60
	maxOpenDeleted     = 50
	// minOpenDeleted and minContainerLog leave out what is not worth a line.
	minOpenDeleted  = 10 << 20
	minContainerLog = 10 << 20
	// defaultActiveOver is when a log still being written is worth a line.
	defaultActiveOver = 1 << 30
	// minJournalDays is the least of the journal a vacuum keeps: it holds
	// sshd and sudo records too.
	minJournalDays = 7
	minJournalSize = 256 << 20
	// maxKeepTail bounds what an emptied log keeps, held in memory while
	// the file is emptied.
	maxKeepTail = 256 << 20
	// maxManifest bounds what a run reports it removed.
	maxManifest = 5000
)

// LogOrders is what the control plane says about logs on a heartbeat
// reply. Absent from a control plane older than log cleanup: the agent then
// does not scan.
type LogOrders struct {
	// Scan is whether to scan every six hours.
	Scan bool `json:"scan"`
	// ScanID asks for a scan now; the agent reports it back once done.
	ScanID string `json:"scan_id,omitempty"`
	// Paths are extra places to look, as globs: an application's logs
	// outside /var/log.
	Paths []string `json:"paths,omitempty"`
	// Protect are extra globs never cleaned, on top of the built-in list.
	Protect []string `json:"protect,omitempty"`
	// ActiveOverMB is the size above which a log being written is
	// reported; 0 is a gigabyte.
	ActiveOverMB int `json:"active_over_mb,omitempty"`
	// Run is a cleanup the console asked for and the agent has not
	// finished.
	Run *LogCleanRequest `json:"run,omitempty"`
}

// Log cleaning actions.
const (
	// LogActDelete deletes the files named, those still as scanned.
	LogActDelete = "delete"
	// LogActTruncate empties a log still being written, keeping its end.
	LogActTruncate = "truncate"
	// LogActJournal vacuums the journal.
	LogActJournal = "journal"
	// LogActOpenTruncate empties a deleted file a process still holds.
	LogActOpenTruncate = "open_truncate"
	// LogActRestart restarts a service holding deleted files.
	LogActRestart = "restart_unit"
	// LogActEventLogMax raises a Windows event log's maximum size.
	LogActEventLogMax = "eventlog_max"
)

// LogAction is one step of a cleanup.
type LogAction struct {
	Kind string `json:"kind"`
	// Item is the scan's item key the action is for.
	Item string `json:"item"`
	// Files are what to delete, as the scan reported them.
	Files []LogFile `json:"files,omitempty"`
	// KeepDays is the youngest a deleted file may be, and what of the
	// journal to keep.
	KeepDays int `json:"keep_days,omitempty"`
	// Path and Bytes are the file to empty and its size at the scan.
	Path  string `json:"path,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	// KeepBytes is how much of an emptied log's end to keep.
	KeepBytes int64 `json:"keep_bytes,omitempty"`
	// MaxBytes is the journal's size to vacuum to, or an event log's new
	// maximum.
	MaxBytes int64  `json:"max_bytes,omitempty"`
	PID      int    `json:"pid,omitempty"`
	FD       int    `json:"fd,omitempty"`
	Unit     string `json:"unit,omitempty"`
	// Log is a Windows event log's name.
	Log string `json:"log,omitempty"`
}

// LogCleanRequest is a cleanup the console asked for.
type LogCleanRequest struct {
	ID      string      `json:"id"`
	Actions []LogAction `json:"actions"`
	// Paths and Protect are the rule's extra globs, so the helper knows
	// where a file of a path item may be, and what else not to touch.
	Paths   []string `json:"paths,omitempty"`
	Protect []string `json:"protect,omitempty"`
}

// LogRemoved is one line of what a cleanup did.
type LogRemoved struct {
	Path     string    `json:"path"`
	Action   string    `json:"action"` // deleted, truncated, vacuumed, emptied, restarted, resized
	Bytes    int64     `json:"bytes"`
	After    int64     `json:"after,omitempty"`
	Modified time.Time `json:"modified,omitempty"`
}

// LogSkipped is a file the cleanup left alone, and why.
type LogSkipped struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

// LogRunStatus is what the agent says about a cleanup, on every heartbeat
// until the console has it.
type LogRunStatus struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Step    string `json:"step,omitempty"`
	Message string `json:"message,omitempty"`
	// Freed is what the cleanup gave back, by the files' sizes.
	Freed      int64        `json:"freed"`
	Removed    []LogRemoved `json:"removed,omitempty"`
	More       int          `json:"more,omitempty"`
	Skipped    []LogSkipped `json:"skipped,omitempty"`
	Unit       string       `json:"unit,omitempty"`
	BootID     string       `json:"boot_id,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at,omitempty"`
	At         time.Time    `json:"at"`
}

// Finished reports whether the cleanup has reached an end.
func (s LogRunStatus) Finished() bool {
	switch s.State {
	case RunDone, RunFailed, RunInterrupted:
		return true
	}
	return false
}

// LogScanRequest is what the scan is handed.
type LogScanRequest struct {
	StateDir   string   `json:"state_dir"`
	Paths      []string `json:"paths,omitempty"`
	Protect    []string `json:"protect,omitempty"`
	ActiveOver int64    `json:"active_over,omitempty"`
}

// LogPlan is what the cleaning helper is handed.
type LogPlan struct {
	LogCleanRequest
	StateDir string `json:"state_dir"`
	Unit     string `json:"unit"`
}

const (
	logsDir        = "logs"
	logsReportFile = "logs.json"
	logsRunFile    = "logs-run.json"
	// logScanEvery is how often the agent scans.
	logScanEvery    = 6 * time.Hour
	logScanRetry    = 30 * time.Minute
	logScanTimeout  = 10 * time.Minute
	logCleanTimeout = time.Hour
)

// logPlatform is the part of log cleanup that depends on the host.
type logPlatform interface {
	// supported says why logs cannot be cleaned from the console here, or
	// "" when they can.
	supported() string
	// scan runs the scan and returns its report.
	scan(ctx context.Context, req LogScanRequest) (LogReport, error)
	// launch starts the helper for a cleanup and returns without waiting.
	launch(ctx context.Context, planPath, unit string) error
	// active reports whether the helper is still running.
	active(unit string) bool
	bootID() string
}

// Logs is the daemon's half of log cleanup.
type Logs struct {
	StateDir string
	Log      *slog.Logger
	// Changed is called when there is something new to report.
	Changed func()

	platform logPlatform
	now      func() time.Time

	mu         sync.Mutex
	enabled    bool
	orders     LogOrders
	report     *LogReport
	sent       time.Time
	scanning   bool
	lastTry    time.Time
	scanID     string
	scanDone   string
	answered   string
	run        LogRunStatus
	loaded     bool
	lastChange time.Time
}

// NewLogs builds the daemon's half for this host. executable is the agent
// binary the helper runs from.
func NewLogs(stateDir, executable string, log *slog.Logger) *Logs {
	if log == nil {
		log = slog.Default()
	}
	return &Logs{StateDir: stateDir, Log: log, platform: hostLogPlatform(executable, stateDir), now: time.Now}
}

// LogBeat is what rides the heartbeat.
type LogBeat struct {
	Supported bool          `json:"supported"`
	Reason    string        `json:"reason,omitempty"`
	Report    *LogReport    `json:"report,omitempty"`
	ScanID    string        `json:"scan_id,omitempty"`
	Scanning  bool          `json:"scanning,omitempty"`
	Run       *LogRunStatus `json:"run,omitempty"`
}

// Beat is what to put on the next heartbeat.
func (l *Logs) Beat() LogBeat {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	b := LogBeat{Scanning: l.scanning, ScanID: l.scanDone}
	if reason := l.platform.supported(); reason != "" {
		b.Reason = reason
	} else {
		b.Supported = true
	}
	if l.report != nil && l.report.ScannedAt.After(l.sent) {
		r := *l.report
		b.Report = &r
	}
	if l.run.ID != "" {
		r := l.run
		b.Run = &r
	}
	return b
}

// Delivered says a heartbeat carrying b reached the control plane.
func (l *Logs) Delivered(b LogBeat) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b.Report != nil && b.Report.ScannedAt.After(l.sent) {
		l.sent = b.Report.ScannedAt
	}
	if b.ScanID != "" && b.ScanID == l.scanDone {
		l.scanDone = ""
	}
}

// Handle acts on what the control plane said on a heartbeat reply. o is nil
// from a control plane older than log cleanup.
func (l *Logs) Handle(ctx context.Context, o *LogOrders) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if o == nil {
		l.enabled = false
		return
	}
	l.enabled = o.Scan
	l.orders = LogOrders{Paths: o.Paths, Protect: o.Protect, ActiveOverMB: o.ActiveOverMB}
	if o.ScanID != "" && o.ScanID != l.scanID && o.ScanID != l.scanDone && o.ScanID != l.answered {
		l.scanID = o.ScanID
	}
	if o.Run != nil {
		l.startLocked(ctx, *o.Run)
	}
}

var reLogUnit = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}\.service$`)

// startLocked starts a cleanup the console asked for, once.
func (l *Logs) startLocked(ctx context.Context, req LogCleanRequest) {
	if req.ID == l.run.ID {
		return
	}
	if !reRunID.MatchString(req.ID) {
		l.Log.Error("logs: refusing a cleanup with a malformed id")
		return
	}
	if l.run.ID != "" && !l.run.Finished() {
		l.Log.Warn("logs: a cleanup was asked for while another is going; it waits",
			"going", l.run.ID, "asked", req.ID)
		return
	}
	now := l.now().UTC()
	st := LogRunStatus{ID: req.ID, StartedAt: now, At: now}
	fail := func(msg string) {
		st.State, st.Message, st.FinishedAt = RunFailed, msg, now
		l.setRunLocked(st)
	}
	if reason := l.platform.supported(); reason != "" {
		fail(reason)
		return
	}
	if err := validLogRequest(req); err != nil {
		fail(err.Error())
		return
	}
	dir := filepath.Join(l.StateDir, logsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("cannot prepare the cleanup: " + err.Error())
		return
	}
	short := strings.ReplaceAll(req.ID, "-", "")
	if len(short) > 12 {
		short = short[:12]
	}
	unit := "argus-logclean-" + short
	planPath := filepath.Join(dir, "plan-"+req.ID+".json")
	if err := writeFileAtomic(planPath, mustJSON(LogPlan{LogCleanRequest: req, StateDir: l.StateDir, Unit: unit}),
		0o600); err != nil {
		fail("cannot prepare the cleanup: " + err.Error())
		return
	}
	st.State, st.Step, st.Message, st.Unit, st.BootID = RunStarting, "starting", "starting", unit, l.platform.bootID()
	l.setRunLocked(st)
	if err := l.platform.launch(ctx, planPath, unit); err != nil {
		st.Unit = ""
		fail("cannot start the cleanup: " + err.Error())
		return
	}
	l.Log.Info("logs: cleanup started", "id", req.ID, "actions", len(req.Actions), "unit", unit)
}

// validLogRequest checks the shape of a request before anything starts.
// Where a file may be is checked again by the helper, file by file.
func validLogRequest(req LogCleanRequest) error {
	if len(req.Actions) == 0 {
		return fmt.Errorf("nothing to clean was named")
	}
	if len(req.Actions) > 200 {
		return fmt.Errorf("too many things to clean at once")
	}
	for _, g := range append(append([]string{}, req.Paths...), req.Protect...) {
		if err := ValidLogGlob(g); err != nil {
			return err
		}
	}
	files := 0
	for _, a := range req.Actions {
		switch a.Kind {
		case LogActDelete:
			files += len(a.Files)
			if a.KeepDays < 0 {
				return fmt.Errorf("keep days cannot be negative")
			}
		case LogActTruncate:
			if a.Path == "" || a.KeepBytes < 0 {
				return fmt.Errorf("a log to empty needs its path")
			}
		case LogActJournal:
		case LogActOpenTruncate:
			if a.PID <= 1 || a.FD < 0 {
				return fmt.Errorf("an open file to empty needs its process")
			}
		case LogActRestart:
			if !reLogUnit.MatchString(a.Unit) {
				return fmt.Errorf("%q is not a service", a.Unit)
			}
		case LogActEventLogMax:
			if !eventLogNames[a.Log] || a.MaxBytes <= 0 {
				return fmt.Errorf("%q is not an event log Argus resizes", a.Log)
			}
		default:
			return fmt.Errorf("%q is not a way to clean", a.Kind)
		}
	}
	if files > maxLogFilesTotal*2 {
		return fmt.Errorf("too many files at once")
	}
	return nil
}

// eventLogNames are the Windows event logs Argus reads and may resize.
var eventLogNames = map[string]bool{"Application": true, "System": true, "Security": true, "Setup": true}

func (l *Logs) setRunLocked(st LogRunStatus) {
	l.run = st
	if err := writeFileAtomic(filepath.Join(l.StateDir, logsRunFile), mustJSON(st), 0o600); err != nil {
		l.Log.Error("logs: cannot keep the cleanup's state", "error", err)
	}
	l.lastChange = l.now()
}

// loadLocked reads what an earlier agent left.
func (l *Logs) loadLocked() {
	if l.loaded {
		return
	}
	l.loaded = true
	if b, err := os.ReadFile(filepath.Join(l.StateDir, logsRunFile)); err == nil {
		var st LogRunStatus
		if json.Unmarshal(b, &st) == nil {
			l.run = st
		}
	}
	if b, err := os.ReadFile(filepath.Join(l.StateDir, logsReportFile)); err == nil {
		var r LogReport
		if json.Unmarshal(b, &r) == nil {
			l.report = &r
		}
	}
}

// Watch follows a cleanup and scans when it is time. It returns when ctx
// ends.
func (l *Logs) Watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		l.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (l *Logs) tick(ctx context.Context) {
	if l.followRun() {
		l.changed()
	}
	l.maybeScan(ctx)
}

func (l *Logs) changed() {
	if l.Changed != nil {
		l.Changed()
	}
}

// followRun reads the cleanup's state from the helper's file, and settles
// one whose helper is gone. It reports whether anything changed.
func (l *Logs) followRun() bool {
	l.mu.Lock()
	l.loadLocked()
	cur := l.run
	l.mu.Unlock()
	if cur.ID == "" || cur.Finished() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(l.StateDir, logsRunFile))
	if err != nil {
		return false
	}
	var disk LogRunStatus
	if json.Unmarshal(b, &disk) != nil || disk.ID != cur.ID {
		return false
	}
	if !disk.Finished() && disk.Unit != "" && !l.platform.active(disk.Unit) &&
		l.now().Sub(disk.StartedAt) > 10*time.Second {
		now := l.now().UTC()
		disk.State, disk.Step, disk.At, disk.FinishedAt = RunInterrupted, "", now, now
		disk.Message = "the cleanup stopped before it finished; what it removed is listed, the rest is still there"
		if disk.BootID != "" && disk.BootID != l.platform.bootID() {
			disk.Message = "the server restarted during the cleanup; what it removed is listed, the rest is still there"
		}
		if err := writeFileAtomic(filepath.Join(l.StateDir, logsRunFile), mustJSON(disk), 0o600); err != nil {
			l.Log.Error("logs: cannot keep the cleanup's state", "error", err)
		}
		l.mu.Lock()
		if l.scanID == "" {
			l.scanID = "after-" + disk.ID
		}
		l.mu.Unlock()
		l.Log.Warn("logs: cleanup settled without its helper", "id", disk.ID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if disk.At.Equal(l.run.At) && disk.State == l.run.State {
		return false
	}
	if !disk.Finished() && l.now().Sub(l.lastChange) < 3*time.Second {
		l.run = disk
		return false
	}
	l.run = disk
	l.lastChange = l.now()
	if disk.Finished() {
		// The helper scanned again once it was done.
		if b, err := os.ReadFile(filepath.Join(l.StateDir, logsReportFile)); err == nil {
			var r LogReport
			if json.Unmarshal(b, &r) == nil && (l.report == nil || r.ScannedAt.After(l.report.ScannedAt)) {
				l.report = &r
			}
		}
	}
	return true
}

// maybeScan scans when the console asked for it now, or when it is time.
func (l *Logs) maybeScan(ctx context.Context) {
	l.mu.Lock()
	running := l.run.ID != "" && !l.run.Finished()
	due := l.enabled && (l.report == nil || l.now().Sub(l.report.ScannedAt) >= logScanEvery)
	if l.report != nil && l.report.Error != "" && l.enabled {
		due = l.now().Sub(l.lastTry) >= logScanRetry
	} else if due && l.now().Sub(l.lastTry) < logScanRetry {
		due = false
	}
	asked := l.scanID
	unsupported := l.platform.supported() != ""
	if l.scanning || running || (!due && asked == "") || unsupported {
		if asked != "" && unsupported {
			l.scanDone, l.scanID, l.answered = asked, "", asked
		}
		l.mu.Unlock()
		return
	}
	l.scanning, l.lastTry = true, l.now()
	o := l.orders
	l.mu.Unlock()
	l.changed()

	req := LogScanRequest{StateDir: l.StateDir, Paths: o.Paths, Protect: o.Protect,
		ActiveOver: int64(o.ActiveOverMB) << 20}
	cctx, cancel := context.WithTimeout(ctx, logScanTimeout)
	r, err := l.platform.scan(cctx, req)
	cancel()

	l.mu.Lock()
	l.scanning = false
	if err != nil {
		prev := LogReport{Supported: true, Disks: []Disk{}, Items: []LogItem{}, Open: []OpenDeleted{},
			Protected: []ProtectedLog{}}
		if l.report != nil {
			prev = *l.report
		}
		prev.Error, prev.ScannedAt = err.Error(), l.now().UTC()
		r = prev
		l.Log.Warn("logs: scan failed", "error", err)
	}
	l.report = &r
	if err := writeFileAtomic(filepath.Join(l.StateDir, logsReportFile), mustJSON(r), 0o600); err != nil {
		l.Log.Error("logs: cannot keep the report", "error", err)
	}
	if asked != "" && l.scanID == asked {
		l.scanDone, l.scanID, l.answered = asked, "", asked
	}
	l.mu.Unlock()
	l.changed()
}
