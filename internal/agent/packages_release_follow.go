package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/* ── The helper: showing a release upgrade as it goes ────────────────────── */

// The upgraders write most of what they do to logs of their own, not to the
// output Argus reads: do-release-upgrade's non-interactive view prints little,
// and leapp prints less without --verbose. These are followed into the run's
// log while the upgrade runs, so the console shows it moving.
var (
	ubuntuUpgradeLogs = []string{"/var/log/dist-upgrade/main.log", "/var/log/dist-upgrade/apt-term.log"}
	leappUpgradeLogs  = []string{"/var/log/leapp/leapp-preupgrade.log", "/var/log/leapp/leapp-upgrade.log"}
)

// followEvery is how often the followed logs are read and the run's state
// rewritten while an upgrade runs. A var for tests.
var followEvery = 2 * time.Second

// maxStatusLine bounds the newest log line shown as the step's detail.
const maxStatusLine = 160

// follow copies what is appended to files into the run's log while an upgrade
// runs, hands the log over even when nothing new is printed, and keeps the
// step's detail on the newest line, prefixed by what the step is. The
// returned stop reads once more and waits for the follower to end.
func (a *PackageApplier) follow(ctx context.Context, what string, files []string) (stop func()) {
	t := &tailer{files: files, offsets: map[string]int64{}}
	t.start() // only what is written from now on
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	tick := func() {
		t.read(a.lw)
		a.lw.Flush()
		if line := a.lw.lastLine(); line != "" {
			a.update(func(s *PackageRunStatus) { s.Message = what + ": " + line })
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(followEvery)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				tick()
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
		t.read(a.lw)
		a.lw.Flush()
	}
}

// tailer reads what has been appended to a set of files since it last
// looked. A file that shrinks, or was replaced by a new one, was started
// again and is read from its start: do-release-upgrade moves the last run's
// logs aside and opens a fresh main.log on every run, which may well be
// longer than the old one by the time it is read.
type tailer struct {
	files   []string
	offsets map[string]int64
	seen    map[string]os.FileInfo
}

func (t *tailer) start() {
	for _, f := range t.files {
		if st, err := os.Stat(f); err == nil {
			t.offsets[f] = st.Size()
			t.remember(f, st)
		}
	}
}

func (t *tailer) remember(f string, st os.FileInfo) {
	if t.seen == nil {
		t.seen = map[string]os.FileInfo{}
	}
	t.seen[f] = st
}

func (t *tailer) read(w io.Writer) {
	for _, f := range t.files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		off := t.offsets[f]
		if st, err := fh.Stat(); err == nil {
			if prev, ok := t.seen[f]; (ok && !os.SameFile(prev, st)) || st.Size() < off {
				off = 0
			}
			t.remember(f, st)
		}
		if _, err := fh.Seek(off, io.SeekStart); err == nil {
			// Bounded per look; the run log keeps only its end anyway.
			n, _ := io.Copy(w, io.LimitReader(fh, 4*maxRunLog))
			t.offsets[f] = off + n
		}
		fh.Close()
	}
}

// lastLine is the newest complete, non-blank line held, shortened for a
// status line.
func (l *runLog) lastLine() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	s := string(l.buf)
	l.mu.Unlock()
	lines := strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > maxStatusLine {
			line = line[:maxStatusLine] + "…"
		}
		return line
	}
	return ""
}

// releaseUpgraderAlive is the process id of a release upgrader still
// running on the server, or "": Ubuntu's upgrader runs from
// /tmp/ubuntu-release-upgrader-*/<new codename>, started by
// do-release-upgrade. Tests replace it.
var releaseUpgraderAlive = func() string {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "cmdline"))
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(b), "\x00", " ")
		if strings.Contains(cmd, "ubuntu-release-upgrader") || strings.Contains(cmd, "do-release-upgrade") {
			return filepath.Base(d)
		}
	}
	return ""
}
