//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

/* ── The daemon's platform ───────────────────────────────────────────────── */

type linuxLogs struct {
	executable string
	stateDir   string
}

func hostLogPlatform(executable, stateDir string) logPlatform {
	return &linuxLogs{executable: executable, stateDir: stateDir}
}

func (p *linuxLogs) supported() string {
	switch {
	case os.Geteuid() != 0:
		return "the agent is not running as root, so it cannot clean logs"
	case !systemdRunning():
		return "this server does not run systemd, which cleaning logs from the console needs"
	}
	return ""
}

// scan runs `argus-agent logs-scan` outside the agent's sandbox, which
// cannot see other processes' open files, and reads the report it prints.
func (p *linuxLogs) scan(ctx context.Context, req LogScanRequest) (LogReport, error) {
	dir := filepath.Join(p.stateDir, logsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return LogReport{}, err
	}
	reqPath := filepath.Join(dir, "scan.json")
	if err := writeFileAtomic(reqPath, mustJSON(req), 0o600); err != nil {
		return LogReport{}, err
	}
	cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
		"--service-type=exec", "--description=Argus log scan", p.executable, "logs-scan", "--req", reqPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return LogReport{}, fmt.Errorf("scanning the logs failed: %s", tail([]byte(msg), 400))
	}
	var r LogReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return LogReport{}, fmt.Errorf("cannot read the scan's report: %w", err)
	}
	return r, nil
}

func (p *linuxLogs) launch(ctx context.Context, planPath, unit string) error {
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit="+unit, "--service-type=exec", "--description=Argus log cleanup",
		p.executable, "logs-clean", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *linuxLogs) active(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run() == nil
}

func (p *linuxLogs) bootID() string { return readBootID() }

/* ── The scan ────────────────────────────────────────────────────────────── */

// scanHostLogs is the helper's scan of this server.
func scanHostLogs(ctx context.Context, req LogScanRequest) (LogReport, error) {
	s := linuxLogScan{req: req, now: time.Now, disks: func() []Disk { return readDisks(liveRoots) }}
	return s.scan(ctx), nil
}

// linuxLogScan finds what logs use the disk. root is "" on a server and a
// fixture tree in tests; every path it reports is without it.
type linuxLogScan struct {
	req   LogScanRequest
	root  string
	now   func() time.Time
	disks func() []Disk
}

func (s linuxLogScan) real(p string) string { return s.root + p }

func (s linuxLogScan) scan(ctx context.Context) LogReport {
	now := s.now()
	_, off := now.Zone()
	active := s.req.ActiveOver
	if active <= 0 {
		active = defaultActiveOver
	}
	r := LogReport{ScannedAt: now.UTC(), OS: "linux", Supported: true, Disks: s.disks(), Items: []LogItem{},
		Open: []OpenDeleted{}, Protected: []ProtectedLog{}, ActiveOver: active, UTCOffsetMinutes: off / 60}
	if r.Disks == nil {
		r.Disks = []Disk{}
	}
	prot := map[string]*ProtectedLog{
		"login": {Title: "Login records", Detail: "auth.log or secure, wtmp, btmp, lastlog, sudo"},
		"audit": {Title: "Audit log", Detail: "/var/log/audit/"},
		"argus": {Title: "Argus", Detail: "the agent's state and recordings"},
	}

	// The journal, on its own: cleaning it is a vacuum, not a delete.
	if j := s.journal(); j.Bytes > 0 {
		r.Items = append(r.Items, j)
	}

	// /var/log: rotated copies together, big live logs one by one.
	rotated := newFileSet("rotated", LogRotated, "Rotated logs", "/var/log")
	var actives []LogItem
	s.walk(ctx, "/var/log", func(p string, fi fs.FileInfo) {
		if strings.HasPrefix(p, "/var/log/journal/") {
			return
		}
		if why := protectedLinux(p, s.req.StateDir, nil); why != "" {
			prot[groupOf(p)].Bytes += fi.Size()
			return
		}
		if matchesGlobOrParent(p, s.req.Protect) {
			return
		}
		if isRotated(filepath.Base(p)) {
			rotated.add(p, fi)
			return
		}
		if fi.Size() >= active {
			m := fi.ModTime().UTC()
			actives = append(actives, LogItem{Key: "active:" + p, Kind: LogActive, Title: strings.TrimPrefix(p, "/var/log/"),
				Detail: "still being written", Root: p, Bytes: fi.Size(), Count: 1, Oldest: &m, Newest: &m})
		}
	})
	if rotated.item.Count > 0 {
		r.Items = append(r.Items, rotated.done())
	}
	sort.Slice(actives, func(i, j int) bool { return actives[i].Bytes > actives[j].Bytes })
	r.Items = append(r.Items, actives...)

	// Containers' logs, which Docker keeps outside /var/log.
	r.Items = append(r.Items, s.containers()...)

	// A rule's extra paths.
	for _, g := range s.req.Paths {
		if ValidLogGlob(g) != nil {
			continue
		}
		set := newFileSet("path:"+g, LogFiles, g, filepath.Dir(g))
		matches, _ := filepath.Glob(s.real(g))
		for _, m := range matches {
			p := strings.TrimPrefix(m, s.root)
			fi, err := os.Lstat(m)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if protectedLinux(p, s.req.StateDir, s.req.Protect) != "" {
				continue
			}
			set.add(p, fi)
		}
		set.item.Detail = "from the rule's Also clean"
		if set.item.Count > 0 {
			r.Items = append(r.Items, set.done())
		}
	}

	// Argus's own.
	if s.req.StateDir != "" {
		s.walk(ctx, s.req.StateDir, func(_ string, fi fs.FileInfo) { prot["argus"].Bytes += fi.Size() })
	}
	for _, k := range []string{"login", "audit", "argus"} {
		r.Protected = append(r.Protected, *prot[k])
	}
	r.Open = s.openDeleted()
	for i := range r.Items {
		r.Items[i].Mount = mountOf(r.Items[i].Root, r.Disks)
	}
	boundReport(&r)
	return r
}

func groupOf(p string) string {
	for _, x := range linuxProtected {
		if strings.HasPrefix(p, x.prefix) {
			return x.group
		}
	}
	return "argus"
}

// walk visits the regular files under dir, not following symlinks, to a
// bounded depth and count.
func (s linuxLogScan) walk(ctx context.Context, dir string, visit func(p string, fi fs.FileInfo)) {
	base := s.real(dir)
	seen := 0
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			if d != nil && d.IsDir() && path != base {
				return fs.SkipDir
			}
			return nil
		}
		rel := strings.TrimPrefix(path, base)
		if d.IsDir() {
			if strings.Count(rel, "/") > 8 {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if seen++; seen > 200000 {
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		visit(strings.TrimPrefix(path, s.root), fi)
		return nil
	})
}

// journal is the systemd journal's size and age.
func (s linuxLogScan) journal() LogItem {
	it := LogItem{Key: "journal", Kind: LogJournal, Title: "Journal", Root: "/var/log/journal"}
	for _, dir := range []string{"/var/log/journal", "/run/log/journal"} {
		s.walk(context.Background(), dir, func(p string, fi fs.FileInfo) {
			if !strings.Contains(filepath.Base(p), ".journal") {
				return
			}
			it.Bytes += fi.Size()
			it.Count++
			m := fi.ModTime().UTC()
			if it.Oldest == nil || m.Before(*it.Oldest) {
				it.Oldest = &m
			}
			if it.Newest == nil || m.After(*it.Newest) {
				it.Newest = &m
			}
		})
	}
	it.Detail = "systemd journal"
	return it
}

// containers is each Docker container's log, by the name Docker gave it.
func (s linuxLogScan) containers() []LogItem {
	base := "/var/lib/docker/containers"
	ents, err := os.ReadDir(s.real(base))
	if err != nil {
		return nil
	}
	var out []LogItem
	for _, e := range ents {
		if !e.IsDir() || len(e.Name()) < 12 {
			continue
		}
		id := e.Name()
		p := base + "/" + id + "/" + id + "-json.log"
		fi, err := os.Lstat(s.real(p))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() < minContainerLog {
			continue
		}
		name := id[:12]
		if b, err := os.ReadFile(s.real(base + "/" + id + "/config.v2.json")); err == nil && len(b) < 4<<20 {
			var cfg struct{ Name string }
			if json.Unmarshal(b, &cfg) == nil && cfg.Name != "" {
				name = strings.TrimPrefix(cfg.Name, "/")
			}
		}
		m := fi.ModTime().UTC()
		out = append(out, LogItem{Key: "container:" + id, Kind: LogContainer, Title: "Container " + name,
			Detail: "Docker's log for " + name, Root: p, Bytes: fi.Size(), Count: 1, Oldest: &m, Newest: &m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// openDeleted is the files processes hold open after someone deleted them.
func (s linuxLogScan) openDeleted() []OpenDeleted {
	procs, err := os.ReadDir(s.real("/proc"))
	if err != nil {
		return []OpenDeleted{}
	}
	out := []OpenDeleted{}
	seen := map[[2]uint64]bool{}
	for _, pe := range procs {
		pid, err := strconv.Atoi(pe.Name())
		if err != nil || pid <= 1 {
			continue
		}
		fdDir := s.real(fmt.Sprintf("/proc/%d/fd", pid))
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fe := range fds {
			fd, err := strconv.Atoi(fe.Name())
			if err != nil {
				continue
			}
			link, err := os.Readlink(filepath.Join(fdDir, fe.Name()))
			if err != nil || !strings.HasSuffix(link, " (deleted)") || !strings.HasPrefix(link, "/") {
				continue
			}
			fi, err := os.Stat(filepath.Join(fdDir, fe.Name()))
			if err != nil || !fi.Mode().IsRegular() || fi.Size() < minOpenDeleted {
				continue
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				key := [2]uint64{uint64(st.Dev), st.Ino}
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			path := strings.TrimSuffix(link, " (deleted)")
			comm, _ := os.ReadFile(s.real(fmt.Sprintf("/proc/%d/comm", pid)))
			out = append(out, OpenDeleted{PID: pid, FD: fd, Process: strings.TrimSpace(string(comm)),
				Unit: unitOf(s.real(fmt.Sprintf("/proc/%d/cgroup", pid))), Path: path, Bytes: fi.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	if len(out) > maxOpenDeleted {
		out = out[:maxOpenDeleted]
	}
	return out
}

// unitOf is the systemd service a process's cgroup file names, or "".
func unitOf(cgroupFile string) string {
	b, err := os.ReadFile(cgroupFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if strings.HasSuffix(parts[i], ".service") && reLogUnit.MatchString(parts[i]) {
				return parts[i]
			}
		}
	}
	return ""
}

// mountOf is the disk p is on, by the longest mount point above it.
func mountOf(p string, disks []Disk) string {
	best := ""
	for _, d := range disks {
		m := d.Mount
		if (p == m || strings.HasPrefix(p, strings.TrimSuffix(m, "/")+"/") || m == "/") && len(m) > len(best) {
			best = m
		}
	}
	return best
}

/* ── Cleaning on Linux ───────────────────────────────────────────────────── */

type linuxLogOps struct {
	stateDir string
	root     string
}

func hostLogOps(stateDir string) logOps { return linuxLogOps{stateDir: stateDir} }

func (o linuxLogOps) protected(p string, extra []string) string {
	return protectedLinux(p, o.stateDir, extra)
}

func (o linuxLogOps) real(p string) string { return o.root + p }

func (o linuxLogOps) roots(item string) []string {
	switch {
	case item == "rotated", strings.HasPrefix(item, "active:"):
		return []string{"/var/log/"}
	case strings.HasPrefix(item, "container:"):
		id := strings.TrimPrefix(item, "container:")
		if id == "" || strings.ContainsAny(id, "/.") {
			return nil
		}
		return []string{"/var/lib/docker/containers/" + id + "/" + id + "-json.log"}
	}
	return nil
}

func (o linuxLogOps) journal(ctx context.Context, keepDays int, maxBytes int64) (int64, int64, error) {
	scan := linuxLogScan{root: o.root}
	before := scan.journal().Bytes
	out, err := exec.CommandContext(ctx, "journalctl", "--vacuum-time="+strconv.Itoa(keepDays)+"d",
		"--vacuum-size="+strconv.FormatInt(maxBytes>>20, 10)+"M").CombinedOutput()
	if err != nil {
		return before, before, fmt.Errorf("%v: %s", err, tail(out, 300))
	}
	return before, scan.journal().Bytes, nil
}

func (o linuxLogOps) openTruncate(pid, fd int, path string) (int64, error) {
	fdPath := o.real(fmt.Sprintf("/proc/%d/fd/%d", pid, fd))
	link, err := os.Readlink(fdPath)
	if err != nil {
		return 0, fmt.Errorf("the process no longer holds it")
	}
	if link != path+" (deleted)" {
		return 0, fmt.Errorf("the process now holds another file there")
	}
	fi, err := os.Stat(fdPath)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("not a regular file")
	}
	if err := os.Truncate(fdPath, 0); err != nil {
		return 0, fmt.Errorf("cannot empty it: %v", err)
	}
	return fi.Size(), nil
}

func (o linuxLogOps) restart(ctx context.Context, unit string) error {
	if !reLogUnit.MatchString(unit) {
		return fmt.Errorf("%q is not a service", unit)
	}
	// Only a service that holds a deleted file now: the console may not
	// restart anything else through here.
	held := false
	for _, od := range (linuxLogScan{root: o.root}).openDeleted() {
		if od.Unit == unit {
			held = true
			break
		}
	}
	if !held {
		return fmt.Errorf("%s no longer holds a deleted file", unit)
	}
	out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %v: %s", unit, err, tail(out, 300))
	}
	return nil
}

func (o linuxLogOps) eventLogMax(context.Context, string, int64) (int64, error) {
	return 0, fmt.Errorf("event logs are on Windows")
}

func (o linuxLogOps) rescan(ctx context.Context, plan LogPlan) (LogReport, error) {
	r := linuxLogScan{req: LogScanRequest{StateDir: plan.StateDir, Paths: plan.Paths, Protect: plan.Protect},
		root: o.root, now: time.Now, disks: func() []Disk { return readDisks(liveRoots) }}
	// The scan the helper ran with knew the console's threshold; keep it.
	if b, err := os.ReadFile(filepath.Join(plan.StateDir, logsDir, "scan.json")); err == nil {
		var req LogScanRequest
		if json.Unmarshal(b, &req) == nil {
			r.req.ActiveOver = req.ActiveOver
		}
	}
	return r.scan(ctx), nil
}
