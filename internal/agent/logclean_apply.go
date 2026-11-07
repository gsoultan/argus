package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

/* ── The helper: cleaning ────────────────────────────────────────────────── */

// logOps is what cleaning needs from the host.
type logOps interface {
	// protected says why p must not be touched, or "".
	protected(p string, extra []string) string
	// roots are where an item's files may be: a folder ends in the path
	// separator, anything else is one file. nil is an item this host does
	// not know.
	roots(item string) []string
	// real maps a reported path to the one on disk (a test's fixture tree).
	real(p string) string
	// journal vacuums the journal and says how big it was and is.
	journal(ctx context.Context, keepDays int, maxBytes int64) (before, after int64, err error)
	// openTruncate empties a deleted file pid holds on fd, if it is still
	// path; it returns the size it had.
	openTruncate(pid, fd int, path string) (int64, error)
	// restart restarts a service.
	restart(ctx context.Context, unit string) error
	// eventLogMax sets a Windows event log's maximum size.
	eventLogMax(ctx context.Context, name string, bytes int64) (before int64, err error)
	// rescan scans again once the cleanup is done.
	rescan(ctx context.Context, plan LogPlan) (LogReport, error)
}

// LogCleaner carries out a cleanup. It is what `argus-agent logs-clean`
// runs, as root, in a unit of its own (on Windows, in the agent).
type LogCleaner struct {
	Plan LogPlan
	Log  *slog.Logger
	Now  func() time.Time
	ops  logOps

	st LogRunStatus
}

// RunLogsClean is `argus-agent logs-clean --plan <path>`.
func RunLogsClean(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logs-clean:", err)
		return 2
	}
	var plan LogPlan
	if err := json.Unmarshal(b, &plan); err != nil || plan.StateDir == "" || plan.ID == "" {
		fmt.Fprintln(os.Stderr, "logs-clean: the plan cannot be read")
		return 2
	}
	c := &LogCleaner{Plan: plan, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), ops: hostLogOps(plan.StateDir)}
	ctx, cancel := context.WithTimeout(context.Background(), logCleanTimeout)
	defer cancel()
	st := c.Clean(ctx)
	_ = os.Remove(planPath)
	if st.State == RunFailed {
		return 1
	}
	return 0
}

// Clean runs the plan and returns how it ended, which it has also written
// to the state file the daemon follows.
func (c *LogCleaner) Clean(ctx context.Context) LogRunStatus {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	now := c.Now().UTC()
	c.st = LogRunStatus{ID: c.Plan.ID, State: RunRunning, Step: "cleaning", Unit: c.Plan.Unit,
		BootID: readBootID(), StartedAt: now, At: now}
	// The daemon's copy says when it started; keep that.
	if b, err := os.ReadFile(filepath.Join(c.Plan.StateDir, logsRunFile)); err == nil {
		var prev LogRunStatus
		if json.Unmarshal(b, &prev) == nil && prev.ID == c.Plan.ID && !prev.StartedAt.IsZero() {
			c.st.StartedAt, c.st.BootID = prev.StartedAt, prev.BootID
		}
	}
	c.save()
	if err := validLogRequest(c.Plan.LogCleanRequest); err != nil {
		return c.finish(RunFailed, err.Error())
	}
	for _, a := range c.Plan.Actions {
		if ctx.Err() != nil {
			return c.finish(RunFailed, "the cleanup ran out of time; what it removed is listed")
		}
		c.st.Step = a.Item
		c.save()
		c.do(ctx, a)
	}
	c.st.Step = "scanning"
	c.save()
	if r, err := c.ops.rescan(ctx, c.Plan); err == nil {
		if err := writeFileAtomic(filepath.Join(c.Plan.StateDir, logsReportFile), mustJSON(r), 0o600); err != nil {
			c.Log.Error("logs: cannot keep the report", "error", err)
		}
	} else {
		c.Log.Warn("logs: scan after the cleanup failed", "error", err)
	}
	msg := fmt.Sprintf("freed %s", humanBytes(c.st.Freed))
	if n := len(c.st.Skipped); n > 0 {
		msg += fmt.Sprintf("; %d left alone", n)
	}
	return c.finish(RunDone, msg)
}

func (c *LogCleaner) do(ctx context.Context, a LogAction) {
	switch a.Kind {
	case LogActDelete:
		roots := c.ops.roots(a.Item)
		glob := ""
		if strings.HasPrefix(a.Item, "path:") {
			glob = strings.TrimPrefix(a.Item, "path:")
			if !hasString(c.Plan.Paths, glob) {
				c.skip(a.Item, "this path is no longer in the rule")
				return
			}
		} else if roots == nil {
			c.skip(a.Item, "not something this server cleans")
			return
		}
		cutoff := c.Now().Add(-time.Duration(a.KeepDays) * 24 * time.Hour)
		for _, f := range a.Files {
			if why := c.deleteOne(f, roots, glob, cutoff); why != "" {
				c.skip(f.Path, why)
			}
		}
	case LogActTruncate:
		if why := c.truncate(a); why != "" {
			c.skip(a.Path, why)
		}
	case LogActJournal:
		days, max := a.KeepDays, a.MaxBytes
		if days < minJournalDays {
			days = minJournalDays
		}
		if max < minJournalSize {
			max = minJournalSize
		}
		before, after, err := c.ops.journal(ctx, days, max)
		if err != nil {
			c.skip("journal", "vacuuming the journal failed: "+err.Error())
			return
		}
		c.removed(LogRemoved{Path: "journal", Action: "vacuumed", Bytes: before, After: after})
	case LogActOpenTruncate:
		if why := c.ops.protected(a.Path, c.Plan.Protect); why != "" {
			c.skip(a.Path, why)
			return
		}
		n, err := c.ops.openTruncate(a.PID, a.FD, a.Path)
		if err != nil {
			c.skip(a.Path, err.Error())
			return
		}
		c.removed(LogRemoved{Path: a.Path + " (deleted)", Action: "emptied", Bytes: n})
	case LogActRestart:
		if err := c.ops.restart(ctx, a.Unit); err != nil {
			c.skip(a.Unit, err.Error())
			return
		}
		c.removed(LogRemoved{Path: a.Unit, Action: "restarted", Bytes: a.Bytes})
	case LogActEventLogMax:
		before, err := c.ops.eventLogMax(ctx, a.Log, a.MaxBytes)
		if err != nil {
			c.skip(a.Log, err.Error())
			return
		}
		c.st.Removed = append(c.st.Removed, LogRemoved{Path: a.Log + " event log", Action: "resized",
			Bytes: before, After: a.MaxBytes})
	}
}

// deleteOne deletes f if it is still the file the scan saw and may go. It
// says why not otherwise.
func (c *LogCleaner) deleteOne(f LogFile, roots []string, glob string, cutoff time.Time) string {
	p := filepath.Clean(f.Path)
	if p != f.Path || !filepath.IsAbs(p) {
		return "not a plain path"
	}
	if why := c.ops.protected(p, c.Plan.Protect); why != "" {
		return why
	}
	if glob != "" {
		if ok, _ := filepath.Match(glob, p); !ok {
			return "not under the rule's path"
		}
	} else if !underRoots(p, roots) {
		return "not where this kind of log is kept"
	}
	if why := c.plainFile(p); why != "" {
		return why
	}
	fi, err := os.Lstat(c.ops.real(p))
	if err != nil {
		if os.IsNotExist(err) {
			return "already gone"
		}
		return err.Error()
	}
	if why := stillAsScanned(f, fi.Size(), fi.ModTime()); why != "" {
		return why
	}
	if !fi.ModTime().Before(cutoff) {
		return "newer than the rule keeps"
	}
	if err := os.Remove(c.ops.real(p)); err != nil {
		return "cannot delete it: " + err.Error()
	}
	c.removed(LogRemoved{Path: p, Action: "deleted", Bytes: fi.Size(), Modified: fi.ModTime().UTC()})
	return ""
}

// plainFile says why p is not a regular file reached without a symlink.
func (c *LogCleaner) plainFile(p string) string {
	real := c.ops.real(p)
	fi, err := os.Lstat(real)
	if err != nil {
		if os.IsNotExist(err) {
			return "already gone"
		}
		return err.Error()
	}
	if !fi.Mode().IsRegular() {
		return "not a regular file"
	}
	dir := filepath.Dir(real)
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || !samePath(resolved, dir) {
		return "reached through a symlink"
	}
	return ""
}

// truncate empties a log still being written, keeping its end.
func (c *LogCleaner) truncate(a LogAction) string {
	p := filepath.Clean(a.Path)
	if p != a.Path || !filepath.IsAbs(p) {
		return "not a plain path"
	}
	if why := c.ops.protected(p, c.Plan.Protect); why != "" {
		return why
	}
	roots := c.ops.roots(a.Item)
	if strings.HasPrefix(a.Item, "path:") {
		glob := strings.TrimPrefix(a.Item, "path:")
		if ok, _ := filepath.Match(glob, p); !ok || !hasString(c.Plan.Paths, glob) {
			return "not under the rule's path"
		}
	} else if !underRoots(p, roots) {
		return "not where this kind of log is kept"
	}
	if why := c.plainFile(p); why != "" {
		return why
	}
	f, err := os.OpenFile(c.ops.real(p), os.O_RDWR, 0)
	if err != nil {
		return "cannot open it: " + err.Error()
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	size := fi.Size()
	if size < a.Bytes/2 {
		return "it was rotated or emptied since the scan"
	}
	keep := a.KeepBytes
	if keep > maxKeepTail {
		keep = maxKeepTail
	}
	if keep > size {
		keep = size
	}
	if keep >= size {
		return "it is no bigger than what is kept"
	}
	tailBuf := make([]byte, keep)
	if keep > 0 {
		if _, err := f.ReadAt(tailBuf, size-keep); err != nil && err != io.EOF {
			return "cannot read its end: " + err.Error()
		}
		// Start at a line, not half way through one.
		if i := strings.IndexByte(string(tailBuf[:min(len(tailBuf), 64<<10)]), '\n'); i >= 0 {
			tailBuf = tailBuf[i+1:]
		}
	}
	if err := f.Truncate(0); err != nil {
		return "cannot empty it: " + err.Error()
	}
	if len(tailBuf) > 0 {
		if _, err := f.WriteAt(tailBuf, 0); err != nil {
			c.Log.Warn("logs: emptied a log but could not put back its end", "path", p, "error", err)
		}
	}
	c.removed(LogRemoved{Path: p, Action: "truncated", Bytes: size, After: int64(len(tailBuf)),
		Modified: fi.ModTime().UTC()})
	return ""
}

func (c *LogCleaner) removed(r LogRemoved) {
	c.st.Freed += r.Bytes - r.After
	if len(c.st.Removed) >= maxManifest {
		c.st.More++
		return
	}
	c.st.Removed = append(c.st.Removed, r)
}

func (c *LogCleaner) skip(p, why string) {
	if len(c.st.Skipped) >= 500 {
		return
	}
	c.st.Skipped = append(c.st.Skipped, LogSkipped{Path: p, Why: why})
}

func (c *LogCleaner) finish(state, msg string) LogRunStatus {
	now := c.Now().UTC()
	c.st.State, c.st.Step, c.st.Message, c.st.FinishedAt, c.st.At = state, "", msg, now, now
	c.save()
	return c.st
}

func (c *LogCleaner) save() {
	c.st.At = c.Now().UTC()
	if err := writeFileAtomic(filepath.Join(c.Plan.StateDir, logsRunFile), mustJSON(c.st), 0o600); err != nil {
		c.Log.Error("logs: cannot write the cleanup's state", "error", err)
	}
}

// underRoots reports whether p is inside one of roots: under a folder
// (ending in the separator), or one of the files named.
func underRoots(p string, roots []string) bool {
	for _, r := range roots {
		if strings.HasSuffix(r, string(filepath.Separator)) {
			if strings.HasPrefix(strings.ToLower(p), strings.ToLower(r)) && len(p) > len(r) {
				if filepath.Separator == '/' && !strings.HasPrefix(p, r) {
					continue // case matters on Linux
				}
				return true
			}
		} else if samePath(p, r) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// RunLogsScan is `argus-agent logs-scan --req <path>`: the report on
// stdout.
func RunLogsScan(reqPath string, stdout io.Writer) int {
	var req LogScanRequest
	if b, err := os.ReadFile(reqPath); err != nil || json.Unmarshal(b, &req) != nil {
		fmt.Fprintln(os.Stderr, "logs-scan: the request cannot be read")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), logScanTimeout)
	defer cancel()
	r, err := scanHostLogs(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(r); err != nil {
		return 1
	}
	return 0
}
