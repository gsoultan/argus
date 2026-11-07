//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

/*
Windows has no sandbox to step out of: the agent runs as SYSTEM. The scan
and the cleanup run in the agent itself, the cleanup on a goroutine the
daemon follows like the Linux helper's unit. A restart of the agent half
way ends it; the daemon then calls it interrupted and lists what it did.

What is cleaned: IIS logs, C:\Windows\Logs, C:\Windows\Temp older than a
week, crash dumps, and a rule's extra paths. Event logs are shown, not
cleared: saving one before clearing it frees nothing, and the Security log
is evidence. When the Security log is full and overwriting its oldest
events, the console offers to raise its maximum instead.
*/

type windowsLogs struct {
	stateDir string
}

func hostLogPlatform(_ string, stateDir string) logPlatform { return &windowsLogs{stateDir: stateDir} }

func (p *windowsLogs) supported() string { return "" }

func (p *windowsLogs) scan(ctx context.Context, req LogScanRequest) (LogReport, error) {
	return scanHostLogs(ctx, req)
}

var windowsCleanups sync.Map // unit -> struct{}

func (p *windowsLogs) launch(_ context.Context, planPath, unit string) error {
	b, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var plan LogPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		return err
	}
	windowsCleanups.Store(unit, struct{}{})
	go func() {
		defer windowsCleanups.Delete(unit)
		defer os.Remove(planPath)
		ctx, cancel := context.WithTimeout(context.Background(), logCleanTimeout)
		defer cancel()
		c := &LogCleaner{Plan: plan, Log: slog.Default(), ops: hostLogOps(plan.StateDir)}
		c.Clean(ctx)
	}()
	return nil
}

func (p *windowsLogs) active(unit string) bool {
	_, ok := windowsCleanups.Load(unit)
	return ok
}

func (p *windowsLogs) bootID() string { return "" }

/* ── The scan ────────────────────────────────────────────────────────────── */

func winDirs() (systemRoot, systemDrive string) {
	systemRoot, systemDrive = os.Getenv("SystemRoot"), os.Getenv("SystemDrive")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	if systemDrive == "" {
		systemDrive = `C:`
	}
	return filepath.Clean(systemRoot), systemDrive
}

// winItems are the folders Argus looks in, by item key.
func winItems() []struct {
	key, title, detail string
	roots              []string
	minAge             int
} {
	sr, sd := winDirs()
	return []struct {
		key, title, detail string
		roots              []string
		minAge             int
	}{
		{"iis", "IIS logs", sd + `\inetpub\logs\LogFiles`, []string{sd + `\inetpub\logs\LogFiles\`}, 0},
		{"winlogs", "Windows logs", sr + `\Logs (CBS, DISM, WindowsUpdate)`, []string{sr + `\Logs\`}, 0},
		{"temp", "Temp", sr + `\Temp, older than 7 days`, []string{sr + `\Temp\`}, 7},
		{"dumps", "Crash dumps", sr + `\Minidump, MEMORY.DMP`, []string{sr + `\Minidump\`, sr + `\MEMORY.DMP`}, 0},
	}
}

func scanHostLogs(ctx context.Context, req LogScanRequest) (LogReport, error) {
	now := time.Now()
	_, off := now.Zone()
	r := LogReport{ScannedAt: now.UTC(), OS: "windows", Supported: true, Items: []LogItem{},
		Open: []OpenDeleted{}, Protected: []ProtectedLog{}, UTCOffsetMinutes: off / 60}
	drives := map[string]bool{}
	for _, w := range winItems() {
		set := newFileSet(w.key, LogFiles, w.title, strings.TrimSuffix(w.roots[0], `\`))
		set.item.Detail, set.item.MinAgeDays = w.detail, w.minAge
		cutoff := now.Add(-time.Duration(w.minAge) * 24 * time.Hour)
		for _, root := range w.roots {
			winWalk(ctx, root, func(p string, fi fs.FileInfo) {
				if protectedWindows(p, req.StateDir, req.Protect) != "" {
					return
				}
				if w.minAge > 0 && !fi.ModTime().Before(cutoff) {
					return
				}
				set.add(p, fi)
			})
		}
		if set.item.Count > 0 {
			r.Items = append(r.Items, set.done())
			drives[volumeOf(w.roots[0])] = true
		}
	}
	for _, g := range req.Paths {
		if ValidLogGlob(g) != nil || !strings.Contains(g, `:`) {
			continue
		}
		set := newFileSet("path:"+g, LogFiles, g, filepath.Dir(g))
		set.item.Detail = "from the rule's Also clean"
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			fi, err := os.Lstat(m)
			if err != nil || !fi.Mode().IsRegular() || protectedWindows(m, req.StateDir, req.Protect) != "" {
				continue
			}
			set.add(m, fi)
		}
		if set.item.Count > 0 {
			r.Items = append(r.Items, set.done())
			drives[volumeOf(g)] = true
		}
	}
	sort.SliceStable(r.Items, func(i, j int) bool { return r.Items[i].Bytes > r.Items[j].Bytes })
	var argus int64
	if req.StateDir != "" {
		winWalk(ctx, req.StateDir, func(_ string, fi fs.FileInfo) { argus += fi.Size() })
	}
	r.EventLogs = readEventLogs(ctx)
	var security int64
	for _, e := range r.EventLogs {
		if e.Name == "Security" {
			security = e.Bytes
		}
	}
	r.Protected = append(r.Protected,
		ProtectedLog{Title: "Security event log", Detail: "logons and privilege use", Bytes: security},
		ProtectedLog{Title: "Argus", Detail: "the agent's state and recordings", Bytes: argus})
	_, sd := winDirs()
	drives[strings.ToUpper(sd)] = true
	for d := range drives {
		if d == "" {
			continue
		}
		var avail, total, free uint64
		root, err := windows.UTF16PtrFromString(d + `\`)
		if err != nil || windows.GetDiskFreeSpaceEx(root, &avail, &total, &free) != nil || total == 0 {
			continue
		}
		r.Disks = append(r.Disks, Disk{Mount: d, FS: "ntfs", Total: total, Used: total - free, Avail: avail})
	}
	sort.Slice(r.Disks, func(i, j int) bool { return r.Disks[i].Mount < r.Disks[j].Mount })
	if r.Disks == nil {
		r.Disks = []Disk{}
	}
	for i := range r.Items {
		r.Items[i].Mount = volumeOf(r.Items[i].Root)
	}
	boundReport(&r)
	return r, nil
}

func volumeOf(p string) string { return strings.ToUpper(filepath.VolumeName(p)) }

func winWalk(ctx context.Context, dir string, visit func(p string, fi fs.FileInfo)) {
	base := strings.TrimSuffix(dir, `\`)
	if fi, err := os.Lstat(base); err == nil && fi.Mode().IsRegular() {
		visit(base, fi)
		return
	}
	seen := 0
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			if d != nil && d.IsDir() && path != base {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if seen++; seen > 200000 {
			return fs.SkipAll
		}
		if fi, err := d.Info(); err == nil {
			visit(path, fi)
		}
		return nil
	})
}

// protectedWindows says why p must not be cleaned, or "".
func protectedWindows(p, stateDir string, extra []string) string {
	low := strings.ToLower(filepath.Clean(p))
	if stateDir != "" {
		sd := strings.ToLower(filepath.Clean(stateDir))
		if low == sd || strings.HasPrefix(low, sd+`\`) {
			return protectedWhy("argus")
		}
	}
	if strings.Contains(low, `\programdata\argus`) {
		return protectedWhy("argus")
	}
	if strings.Contains(low, `\system32\winevt\`) {
		return "an event log, which Argus does not delete"
	}
	if matchesGlobOrParent(filepath.Clean(p), extra) {
		return "kept by the console's Never clean list"
	}
	return ""
}

// readEventLogs reads the size of the main event logs.
func readEventLogs(ctx context.Context) []EventLog {
	out := []EventLog{}
	for _, name := range []string{"Application", "System", "Security", "Setup"} {
		e := EventLog{Name: name, Protected: name == "Security"}
		info, err := exec.CommandContext(ctx, "wevtutil", "gli", name).Output()
		if err != nil {
			continue
		}
		conf, _ := exec.CommandContext(ctx, "wevtutil", "gl", name).Output()
		kv := wevtutilFields(string(info) + "\n" + string(conf))
		e.Bytes, _ = strconv.ParseInt(kv["fileSize"], 10, 64)
		e.MaxBytes, _ = strconv.ParseInt(kv["maxSize"], 10, 64)
		e.Records, _ = strconv.ParseInt(kv["numberOfLogRecords"], 10, 64)
		e.Retain = kv["retention"] == "true"
		out = append(out, e)
	}
	return out
}

// wevtutilFields reads wevtutil's "name: value" lines.
func wevtutilFields(s string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

/* ── Cleaning on Windows ─────────────────────────────────────────────────── */

type windowsLogOps struct{ stateDir string }

func hostLogOps(stateDir string) logOps { return windowsLogOps{stateDir: stateDir} }

func (o windowsLogOps) protected(p string, extra []string) string {
	return protectedWindows(p, o.stateDir, extra)
}

func (o windowsLogOps) real(p string) string { return p }

func (o windowsLogOps) roots(item string) []string {
	for _, w := range winItems() {
		if w.key == item {
			return w.roots
		}
	}
	return nil
}

func (o windowsLogOps) journal(context.Context, int, int64) (int64, int64, error) {
	return 0, 0, fmt.Errorf("the journal is on Linux")
}

func (o windowsLogOps) openTruncate(int, int, string) (int64, error) {
	return 0, fmt.Errorf("open deleted files are found on Linux")
}

func (o windowsLogOps) restart(context.Context, string) error {
	return fmt.Errorf("services are restarted from the console on Linux only")
}

// maxEventLog bounds a raised event log maximum.
const maxEventLog = 4 << 30

func (o windowsLogOps) eventLogMax(ctx context.Context, name string, bytes int64) (int64, error) {
	if !eventLogNames[name] {
		return 0, fmt.Errorf("%q is not an event log Argus resizes", name)
	}
	conf, err := exec.CommandContext(ctx, "wevtutil", "gl", name).Output()
	if err != nil {
		return 0, fmt.Errorf("cannot read the %s log's settings: %v", name, err)
	}
	cur, _ := strconv.ParseInt(wevtutilFields(string(conf))["maxSize"], 10, 64)
	if bytes <= cur {
		return cur, fmt.Errorf("the %s log is already %s; Argus only raises a maximum", name, humanBytes(cur))
	}
	if bytes > maxEventLog {
		bytes = maxEventLog
	}
	if out, err := exec.CommandContext(ctx, "wevtutil", "sl", name, "/ms:"+strconv.FormatInt(bytes, 10)).CombinedOutput(); err != nil {
		return cur, fmt.Errorf("wevtutil: %v: %s", err, tail(out, 300))
	}
	return cur, nil
}

func (o windowsLogOps) rescan(ctx context.Context, plan LogPlan) (LogReport, error) {
	return scanHostLogs(ctx, LogScanRequest{StateDir: plan.StateDir, Paths: plan.Paths, Protect: plan.Protect})
}
