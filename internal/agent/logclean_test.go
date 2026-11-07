package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidLogGlob(t *testing.T) {
	for _, g := range []string{"/srv/*/logs/*.log", "/opt/app/var/log/*", `D:\apps\shop\logs\*.log`,
		`C:\inetpub\custom-logs\*`} {
		if err := ValidLogGlob(g); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
	for _, g := range []string{"", "/*", "/var/*", "/etc/logrotate.d/*", "/srv/app/data/*", "relative/log/*",
		"/srv/../etc/log", "/var/lib/mysql/binlog.*", "/home/me/logs/*", `C:\Windows\System32\LogFiles\*`,
		`C:\log`, "/srv/log\n/x", "/srv/log/[", `C:\ProgramData\Argus\logs\*`} {
		if err := ValidLogGlob(g); err == nil {
			t.Errorf("%q: accepted", g)
		}
	}
}

func TestIsRotated(t *testing.T) {
	for name, want := range map[string]bool{
		"syslog.1": true, "syslog.2.gz": true, "messages-20261101": true, "messages-20261101.gz": true,
		"kern.log.old": true, "x.log.xz": true, "syslog": false, "access.log": false, "dpkg.log": false,
		"app.2026.log": false,
	} {
		if isRotated(name) != want {
			t.Errorf("%s: rotated=%v", name, !want)
		}
	}
}

func TestValidLogRequest(t *testing.T) {
	ok := LogCleanRequest{ID: "run-1", Actions: []LogAction{{Kind: LogActJournal, KeepDays: 28}}}
	if err := validLogRequest(ok); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]LogCleanRequest{
		"nothing":        {ID: "x"},
		"unknown kind":   {Actions: []LogAction{{Kind: "shred"}}},
		"bad unit":       {Actions: []LogAction{{Kind: LogActRestart, Unit: "sshd; reboot"}}},
		"init":           {Actions: []LogAction{{Kind: LogActOpenTruncate, PID: 1}}},
		"bad event log":  {Actions: []LogAction{{Kind: LogActEventLogMax, Log: "Microsoft-Windows-Foo", MaxBytes: 1}}},
		"bad path":       {Paths: []string{"/etc/*"}, Actions: []LogAction{{Kind: LogActJournal}}},
		"empty truncate": {Actions: []LogAction{{Kind: LogActTruncate}}},
	} {
		if err := validLogRequest(req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStillAsScanned(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := LogFile{Bytes: 1000, Modified: at}
	if why := stillAsScanned(f, 1000, at); why != "" {
		t.Error(why)
	}
	if stillAsScanned(f, 1000, at.Add(time.Second)) == "" {
		t.Error("a newer file is not the one scanned")
	}
	if stillAsScanned(f, 1200, at) == "" {
		t.Error("a file 20% bigger is not the one scanned")
	}
}

func TestBoundReport(t *testing.T) {
	r := LogReport{}
	for i := 0; i < 5; i++ {
		it := LogItem{Key: "k"}
		for j := 0; j < maxLogFilesPerItem; j++ {
			it.Files = append(it.Files, LogFile{})
		}
		r.Items = append(r.Items, it)
	}
	boundReport(&r)
	n := 0
	for _, it := range r.Items {
		n += len(it.Files)
	}
	if n != maxLogFilesTotal || r.Items[4].More != maxLogFilesPerItem {
		t.Errorf("listed %d, last item more=%d", n, r.Items[4].More)
	}
}

// fakeLogPlatform stands in for the host in the daemon's tests.
type fakeLogPlatform struct {
	unsupported string
	scans       int
	launched    []string
	running     bool
}

func (f *fakeLogPlatform) supported() string { return f.unsupported }
func (f *fakeLogPlatform) scan(context.Context, LogScanRequest) (LogReport, error) {
	f.scans++
	return LogReport{ScannedAt: time.Now().UTC(), Supported: true}, nil
}
func (f *fakeLogPlatform) launch(_ context.Context, plan, unit string) error {
	f.launched = append(f.launched, plan)
	f.running = true
	return nil
}
func (f *fakeLogPlatform) active(string) bool { return f.running }
func (f *fakeLogPlatform) bootID() string     { return "boot-1" }

func TestLogsDaemonStartsACleanupOnceAndSettlesALostOne(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeLogPlatform{}
	now := time.Now()
	l := &Logs{StateDir: dir, Log: slog.Default(), platform: fp, now: func() time.Time { return now }}
	req := &LogCleanRequest{ID: "run-00000009", Actions: []LogAction{{Kind: LogActJournal, KeepDays: 28}}}
	l.Handle(context.Background(), &LogOrders{Scan: true, Run: req})
	l.Handle(context.Background(), &LogOrders{Scan: true, Run: req})
	if len(fp.launched) != 1 {
		t.Fatalf("launched %d times", len(fp.launched))
	}
	if b := l.Beat(); b.Run == nil || b.Run.State != RunStarting || !b.Supported {
		t.Fatalf("beat = %+v", b)
	}
	if _, err := os.Stat(fp.launched[0]); err != nil {
		t.Errorf("no plan written: %v", err)
	}
	// The helper went away without saying how it ended.
	fp.running = false
	now = now.Add(time.Minute)
	if !l.followRun() {
		t.Fatal("nothing changed")
	}
	b := l.Beat()
	if b.Run.State != RunInterrupted || !strings.Contains(b.Run.Message, "stopped before it finished") {
		t.Errorf("run = %+v", b.Run)
	}
	// And a scan is asked for, to show what is still there.
	if l.scanID == "" {
		t.Error("no scan after an interrupted cleanup")
	}
	// A restart reads the same state back.
	l2 := &Logs{StateDir: dir, Log: slog.Default(), platform: fp, now: time.Now}
	if b := l2.Beat(); b.Run == nil || b.Run.ID != req.ID || b.Run.State != RunInterrupted {
		t.Errorf("after restart = %+v", b.Run)
	}
}

func TestLogsDaemonScansWhenAskedAndAnswersOnce(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeLogPlatform{}
	l := &Logs{StateDir: dir, Log: slog.Default(), platform: fp, now: time.Now}
	l.Handle(context.Background(), &LogOrders{Scan: true, ScanID: "scan-1"})
	l.maybeScan(context.Background())
	if fp.scans != 1 {
		t.Fatalf("scans = %d", fp.scans)
	}
	b := l.Beat()
	if b.ScanID != "scan-1" || b.Report == nil {
		t.Fatalf("beat = %+v", b)
	}
	l.Delivered(b)
	l.Handle(context.Background(), &LogOrders{Scan: true, ScanID: "scan-1"})
	l.maybeScan(context.Background())
	if fp.scans != 1 {
		t.Error("an answered scan ran again")
	}
	if b := l.Beat(); b.Report != nil || b.ScanID != "" {
		t.Errorf("a delivered report was sent again: %+v", b)
	}
	// An older control plane: stop scanning.
	l.Handle(context.Background(), nil)
	if l.enabled {
		t.Error("still scanning for a control plane that does not ask")
	}
}
