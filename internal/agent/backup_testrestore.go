package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A test restore: prove a backup restores, without touching anything that
exists.

	room ─▶ hash ─▶ read the table of contents ─▶ CREATE DATABASE argus_test_… ─▶
	pg_restore (low priority, timed) ─▶ count and check the copy ─▶ DROP it

The copy is a database nobody can connect to but the admin account, under a
name only tests use (tunnel.ValidTestDB). It is dropped before the result is
sent, whatever happened; if this process dies first, the control plane asks
for the drop again (drop-test) until it is gone.

The agent reports numbers: what it found and how long it took. Whether that
is a pass, a warning or a failure is decided by the control plane, which has
the previous passing test to compare with.
*/

// testRoomFactor and testRoomSlack size the room check: a restored copy is
// about the size of the live database, and indexes rebuilding want more.
const (
	testRoomFactor = 1.3
	testRoomSlack  = 1 << 30
)

// serverJobs is the jobs running on this server: at most one per database,
// and a test restore alone.
type serverJobs struct {
	mu      sync.Mutex
	running map[string]int
	alone   bool
}

func (s *serverJobs) take(db string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alone || s.running[db] > 0 {
		return false
	}
	if s.running == nil {
		s.running = map[string]int{}
	}
	s.running[db]++
	return true
}

func (s *serverJobs) takeAlone(db string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alone || len(s.running) > 0 {
		return false
	}
	if s.running == nil {
		s.running = map[string]int{}
	}
	s.alone = true
	s.running[db]++
	return true
}

func (s *serverJobs) give(db string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[db]--; s.running[db] <= 0 {
		delete(s.running, db)
	}
	if len(s.running) == 0 {
		s.alone = false
	}
}

// testRestore runs a test restore end to end.
func (j *backupJob) testRestore(ctx context.Context, data <-chan []byte) tunnel.BackupResult {
	key, err := base64.StdEncoding.DecodeString(j.b.DataKey)
	if err != nil || len(key) != 32 {
		return j.fail("start", errors.New("the grant's key is not readable"))
	}
	defer zero(key)
	t := j.b.Test
	out := &tunnel.BackupTestResult{Scratch: t.Scratch}
	created := false
	// finish drops the copy if one was made, and attaches the numbers.
	finish := func(res tunnel.BackupResult) tunnel.BackupResult {
		if created {
			st := time.Now()
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			d := j.dropTest(dctx, t.Scratch)
			cancel()
			out.Dropped = d.OK
			detail := t.Scratch + " removed; no copy left on the disk"
			if !d.OK {
				detail = "could not drop " + t.Scratch + ": " + d.Error + "; Argus will try again"
			}
			out.Steps = append(out.Steps, tunnel.TestStep{Name: "Dropped", OK: d.OK, Detail: detail, MS: time.Since(st).Milliseconds()})
		} else {
			out.Dropped = true
		}
		res.Test = out
		return res
	}
	step := func(name string, st time.Time, ok bool, detail string) {
		out.Steps = append(out.Steps, tunnel.TestStep{Name: name, OK: ok, Detail: detail, MS: time.Since(st).Milliseconds()})
	}

	// Room: the copy goes where Postgres keeps its data.
	st := time.Now()
	j.progress("room", "", -1)
	sz := j.cfg.Admin(ctx, PGAdminRequest{Op: "database-size", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Target: j.b.Name})
	if !sz.OK {
		step("Room", st, false, sz.Error)
		return finish(j.fail("room", errors.New("could not read the database's size: "+sz.Error)))
	}
	out.SourceBytes = sz.Bytes
	out.NeedBytes = int64(float64(sz.Bytes)*testRoomFactor) + testRoomSlack
	out.FreeBytes = freeBytes(sz.DataDir)
	if out.FreeBytes > 0 && out.FreeBytes < out.NeedBytes {
		why := fmt.Sprintf("not enough room for a copy: %s needed, %s free under %s", humanBytes(out.NeedBytes),
			humanBytes(out.FreeBytes), sz.DataDir)
		step("Room", st, false, why)
		return finish(j.fail("room", errors.New(why)))
	}
	room := humanBytes(out.NeedBytes) + " needed"
	if out.FreeBytes > 0 {
		room += ", " + humanBytes(out.FreeBytes) + " free under " + sz.DataDir
	}
	step("Room", st, true, room)

	// Hash.
	st = time.Now()
	path, cleanup, bad := j.archive(ctx, t.Source, data)
	defer cleanup()
	if bad != nil {
		step("Hash", st, false, bad.Error)
		return finish(*bad)
	}
	step("Hash", st, true, "SHA-256 matches what the control plane recorded")

	// What the archive says it holds.
	st = time.Now()
	expected, err := j.tocTables(ctx, path, key)
	if err != nil {
		step("Contents", st, false, err.Error())
		return finish(j.fail("reading", err))
	}
	out.Expected = expected
	step("Contents", st, true, fmt.Sprintf("%d tables with data in the backup", len(expected)))

	// The throwaway database.
	st = time.Now()
	j.progress("preparing", "creating "+t.Scratch, -1)
	if r := j.cfg.Admin(ctx, PGAdminRequest{Op: "create-test", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Target: t.Scratch}); !r.OK {
		step("Test database", st, false, r.Error)
		return finish(j.fail("preparing", errors.New("could not create the test copy: "+r.Error)))
	}
	created = true
	step("Test database", st, true, t.Scratch+" · connection limit 0 · nobody can open it")

	// Restore, timed: this is how long a real restore would take.
	st = time.Now()
	f, err := os.Open(path)
	if err != nil {
		step("Restore", st, false, err.Error())
		return finish(j.fail("restoring", err))
	}
	defer f.Close()
	dr, err := tunnel.NewDumpReader(f, key, j.b.BackupID)
	if err != nil {
		step("Restore", st, false, err.Error())
		return finish(j.fail("restoring", err))
	}
	j.progress("restoring", "into "+t.Scratch, 0)
	errLines := &progressLines{job: j, restore: true}
	counting := &countReader{r: dr, job: j}
	err = j.cfg.Tool(ctx, PGBackupRequest{Op: "restore", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser,
		Database: t.Scratch, Existing: true, Low: true}, counting, io.Discard, errLines)
	out.RestoreMS = time.Since(st).Milliseconds()
	out.Warnings = errLines.warnings
	if ctx.Err() != nil {
		step("Restore", st, false, "cancelled")
		return finish(j.fail("restoring", errors.New("cancelled")))
	}
	if err != nil && !(errLines.warnings > 0 && errLines.ignored) {
		why := fmt.Sprintf("pg_restore failed: %s", lastLine(strings.TrimSpace(errLines.last)))
		step("Restore", st, false, why)
		return finish(j.fail("restoring", errors.New(why)))
	}
	restored := "pg_restore --no-owner, low priority · 0 errors"
	if out.Warnings > 0 {
		restored = fmt.Sprintf("pg_restore --no-owner, low priority · %d errors ignored", out.Warnings)
	}
	step("Restore", st, true, restored)

	// Count and check the copy.
	st = time.Now()
	j.progress("checking", "", -1)
	chk := j.cfg.Admin(ctx, PGAdminRequest{Op: "test-checks", Database: t.Scratch, Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Checks: j.b.Checks})
	if !chk.OK || chk.Test == nil {
		step("Checks", st, false, chk.Error)
		return finish(j.fail("checking", errors.New("could not read the test copy: "+chk.Error)))
	}
	out.Data = chk.Test
	step("Checks", st, true, fmt.Sprintf("%d tables counted, %d indexes read, %d of your checks run",
		len(chk.Test.Tables), chk.Test.Indexes, len(chk.Test.Checks)))
	return finish(tunnel.BackupResult{OK: true, Bytes: counting.n})
}

// tocTables lists the tables the archive has data for, as schema.name.
func (j *backupJob) tocTables(ctx context.Context, path string, key []byte) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dr, err := tunnel.NewDumpReader(f, key, j.b.BackupID)
	if err != nil {
		return nil, fmt.Errorf("the backup does not open: %w", err)
	}
	var toc, errb bytes.Buffer
	if err := j.cfg.Tool(ctx, PGBackupRequest{Op: "toc", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser}, dr, &toc, &errb); err != nil {
		return nil, fmt.Errorf("pg_restore cannot read the archive: %s", strings.TrimSpace(lastLine(errb.String())))
	}
	return tocDataTables(toc.String()), nil
}

// tocDataTables reads pg_restore --list for its TABLE DATA entries:
//
//	3360; 0 16390 TABLE DATA public orders postgres
func tocDataTables(list string) []string {
	var out []string
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ";") {
			continue
		}
		i := strings.Index(line, " TABLE DATA ")
		if i < 0 {
			continue
		}
		f := strings.Fields(line[i+len(" TABLE DATA "):])
		if len(f) < 3 {
			continue
		}
		out = append(out, f[0]+"."+strings.Join(f[1:len(f)-1], " "))
	}
	return out
}

// runChecks runs the custom checks against the live database, read only.
func (j *backupJob) runChecks(ctx context.Context) tunnel.BackupResult {
	r := j.cfg.Admin(ctx, PGAdminRequest{Op: "run-checks", Database: j.b.Name, Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Checks: j.b.Checks})
	if !r.OK {
		return j.fail("checking", errors.New(r.Error))
	}
	return tunnel.BackupResult{OK: true, Checks: r.Checks}
}

// dropTest drops a test copy.
func (j *backupJob) dropTest(ctx context.Context, name string) tunnel.BackupResult {
	if !tunnel.ValidTestDB.MatchString(name) {
		return j.fail("dropping", errors.New("not a test copy: "+name))
	}
	r := j.cfg.Admin(ctx, PGAdminRequest{Op: "drop-test", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Target: name})
	if !r.OK {
		return j.fail("dropping", errors.New(r.Error))
	}
	return tunnel.BackupResult{OK: true, RenamedTo: name}
}
