//go:build !windows

package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real Postgres, as the postgres tests are (ARGUS_TEST_PG_SUPER_DSN),
and with pg_dump on PATH. The tools run in-process over the DSN's socket
rather than through the helper, which needs a second account.
*/

const testSecret = "ssssssssssssssssssssssssssssssssssss"

type backupHarness struct {
	t      *testing.T
	stream tunnel.Stream
	fr     *tunnel.FrameReader
	answer tunnel.Answer
	// what came back
	progress []tunnel.BackupProgress
	data     bytes.Buffer
	ended    bool
	result   tunnel.BackupResult
}

func backupTest(t *testing.T) (dsn, host string, port int, dbname string) {
	t.Helper()
	dsn, host, port, dbname = pgTest(t)
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump is not on PATH")
	}
	return
}

func newBackupService(t *testing.T, dsn, dir string) (TunnelService, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	key, _ := tunnel.GrantKeyFromSecret(testSecret)
	pub := key.Public().(ed25519.PublicKey)
	cfg, _ := pgconn.ParseConfig(dsn)
	svc := Backup(BackupConfig{
		Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		Dir:      dir,
		Admin: DSNAdmin(func(database string) string {
			c := *cfg
			c.Database = database
			return fmt.Sprintf("postgres://%s@/%s?host=%s&port=%d", c.User, database, c.Host, c.Port)
		}),
		Tool: func(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error {
			return RunPGBackup(ctx, req, stdin, stdout, stderr)
		},
	})
	return svc, pub, key
}

// runJob opens a backup stream with grant g and collects everything the
// agent sends until the result frame.
func runJob(t *testing.T, svc TunnelService, pub ed25519.PublicKey, key ed25519.PrivateKey, g tunnel.Grant,
	during func(h *backupHarness)) *backupHarness {
	t.Helper()
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = a.Close(); _ = b.Close(); <-done })
	go func() {
		defer close(done)
		svc(ctx, &Call{Header: tunnel.Header{Service: tunnel.BackupService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: pub})
	}()
	h := &backupHarness{t: t, stream: pipeStream{b}, fr: tunnel.NewFrameReader(b)}
	if err := tunnel.ReadJSON(b, &h.answer); err != nil {
		t.Fatal(err)
	}
	if !h.answer.OK {
		return h
	}
	var wg sync.WaitGroup
	if during != nil {
		wg.Add(1)
		go func() { defer wg.Done(); during(h) }()
	}
	for {
		typ, p, err := h.fr.Next()
		if err != nil {
			t.Fatalf("stream ended without a result: %v", err)
		}
		switch typ {
		case tunnel.FrameBackupProgress:
			var pr tunnel.BackupProgress
			_ = json.Unmarshal(p, &pr)
			h.progress = append(h.progress, pr)
		case tunnel.FrameBackupData:
			h.data.Write(p)
		case tunnel.FrameBackupEnd:
			h.ended = true
		case tunnel.FrameBackupResult:
			_ = json.Unmarshal(p, &h.result)
			wg.Wait()
			// The service frees the database's job slot after it sends the
			// result; wait for it so the next job is not refused as busy.
			<-done
			return h
		}
	}
}

func backupGrant(job string, host string, port int, dbname string) tunnel.Grant {
	g := tunnel.NewGrant("dewi@corp.example", "db-01", "backup", "")
	g.Service = tunnel.BackupService
	g.Backup = &tunnel.GrantBackup{Job: job, BackupID: "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", DatabaseID: "db-x",
		Organization: "default", Name: dbname, Host: host, Port: port, AdminUser: "postgres"}
	return g
}

func mustExec(t *testing.T, dsn, sql string) {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.Exec(context.Background(), sql).Close(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func queryOne(t *testing.T, dsn, sql string) string {
	t.Helper()
	conn, err := pgconn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	res := conn.ExecParams(context.Background(), sql, nil, nil, nil, nil).Read()
	if res.Err != nil {
		t.Fatalf("%s: %v", sql, res.Err)
	}
	if len(res.Rows) == 0 {
		return ""
	}
	return string(res.Rows[0][0])
}

func withDB(dsn, db string) string {
	cfg, _ := pgconn.ParseConfig(dsn)
	return fmt.Sprintf("postgres://%s@/%s?host=%s&port=%d", cfg.User, db, cfg.Host, cfg.Port)
}

func TestBackupDumpsEncryptsVerifiesAndStreams(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	dir := t.TempDir()
	svc, pub, key := newBackupService(t, dsn, dir)
	mustExec(t, dsn, `CREATE TABLE IF NOT EXISTS backup_probe (id int PRIMARY KEY, note text)`)
	mustExec(t, dsn, `INSERT INTO backup_probe VALUES (1, 'kept') ON CONFLICT DO NOTHING`)

	priv, _ := tunnel.DeriveBackupKey(testSecret, "default")
	g := backupGrant(tunnel.BackupJobDump, host, port, dbname)
	g.Backup.Destination = tunnel.BackupDestBoth
	g.Backup.PublicKey = tunnel.EncodeBackupPublicKey(priv.PublicKey())
	g.Backup.Reason = "test"
	h := runJob(t, svc, pub, key, g, nil)
	if !h.answer.OK {
		t.Fatalf("refused: %s", h.answer.Error)
	}
	if !h.result.OK {
		t.Fatalf("dump failed at %s: %s", h.result.Stage, h.result.Error)
	}
	if !h.result.Verified || h.result.SHA256 == "" || h.result.WrappedKey == "" {
		t.Fatalf("result lacks verification or the wrap: %+v", h.result)
	}
	// Streamed up: the bytes are the file, the hash is the file's.
	if !h.ended || int64(h.data.Len()) != h.result.Bytes {
		t.Fatalf("streamed %d bytes of %d, ended=%v", h.data.Len(), h.result.Bytes, h.ended)
	}
	if tunnel.SHA256Hex(h.data.Bytes()) != h.result.SHA256 {
		t.Fatal("the streamed bytes do not hash to what the result says")
	}
	if _, err := os.Stat(h.result.LocalPath); err != nil {
		t.Fatalf("no local file: %v", err)
	}
	// Not readable without the organization's key, and readable with it.
	hdr, _, err := tunnel.ReadDumpHeader(bufio.NewReader(bytes.NewReader(h.data.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if hdr.DBName != dbname || hdr.BackupID != g.Backup.BackupID || hdr.Reason != "test" {
		t.Fatalf("header: %+v", hdr)
	}
	dk, err := tunnel.UnwrapBackupKey(priv, hdr)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := tunnel.NewDumpReader(bytes.NewReader(h.data.Bytes()), dk, g.Backup.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(dr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(plain, []byte("PGDMP")) {
		t.Fatal("the decrypted archive is not a pg_dump custom archive")
	}
	if !bytes.Contains(plain, []byte("backup_probe")) {
		t.Fatal("the archive does not mention the table")
	}
	// And the ciphertext does not.
	if bytes.Contains(h.data.Bytes(), []byte("backup_probe")) {
		t.Fatal("the table name is visible in the encrypted file")
	}
	// Progress was reported with a stage.
	var sawData bool
	for _, p := range h.progress {
		if p.Stage == "data" || p.Stage == "uploading" || p.Stage == "verifying" {
			sawData = true
		}
	}
	if !sawData {
		t.Logf("progress frames: %+v", h.progress)
	}
}

// dumpFor makes a backup and returns the grant used, the file bytes, and
// the data key, for the restore tests.
func dumpFor(t *testing.T, svc TunnelService, pub ed25519.PublicKey, key ed25519.PrivateKey,
	host string, port int, dbname string) (tunnel.Grant, []byte, []byte) {
	t.Helper()
	priv, _ := tunnel.DeriveBackupKey(testSecret, "default")
	g := backupGrant(tunnel.BackupJobDump, host, port, dbname)
	g.Backup.Destination = tunnel.BackupDestBoth
	g.Backup.PublicKey = tunnel.EncodeBackupPublicKey(priv.PublicKey())
	h := runJob(t, svc, pub, key, g, nil)
	if !h.answer.OK || !h.result.OK {
		t.Fatalf("dump: %s %s", h.answer.Error, h.result.Error)
	}
	hdr, _, _ := tunnel.ReadDumpHeader(bufio.NewReader(bytes.NewReader(h.data.Bytes())))
	dk, err := tunnel.UnwrapBackupKey(priv, hdr)
	if err != nil {
		t.Fatal(err)
	}
	return g, h.data.Bytes(), dk
}

func restoreGrant(from tunnel.Grant, file, dk []byte, r tunnel.GrantRestore) tunnel.Grant {
	g := backupGrant(tunnel.BackupJobRestore, from.Backup.Host, from.Backup.Port, from.Backup.Name)
	g.Backup.DataKey = base64.StdEncoding.EncodeToString(dk)
	g.Backup.SHA256 = tunnel.SHA256Hex(file)
	g.Backup.Restore = &r
	return g
}

func TestRestoreIntoANewDatabaseTouchesNothingElse(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	mustExec(t, dsn, `CREATE TABLE IF NOT EXISTS backup_probe (id int PRIMARY KEY, note text)`)
	mustExec(t, dsn, `INSERT INTO backup_probe VALUES (2, 'restored') ON CONFLICT DO NOTHING`)
	g, file, dk := dumpFor(t, svc, pub, key, host, port, dbname)

	newName := fmt.Sprintf("%s_restored_%d", dbname, time.Now().Unix()%100000)
	t.Cleanup(func() { mustExec(t, withDB(dsn, "postgres"), "DROP DATABASE IF EXISTS "+newName) })
	// From the local file: the dump above left one.
	rg := restoreGrant(g, file, dk, tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: newName, Source: "local"})
	h := runJob(t, svc, pub, key, rg, nil)
	if !h.answer.OK {
		t.Fatalf("refused: %s", h.answer.Error)
	}
	if !h.result.OK {
		t.Fatalf("restore failed at %s: %s", h.result.Stage, h.result.Error)
	}
	if got := queryOne(t, withDB(dsn, newName), `SELECT note FROM backup_probe WHERE id = 2`); got != "restored" {
		t.Fatalf("the new database does not have the row: %q", got)
	}
	// The source is as it was.
	if got := queryOne(t, dsn, `SELECT count(*) FROM backup_probe WHERE id = 2`); got != "1" {
		t.Fatalf("the source changed: %s", got)
	}
}

func TestRestoreFromAStreamChecksTheHashFirst(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	g, file, dk := dumpFor(t, svc, pub, key, host, port, dbname)
	// Remove the local file so the restore has to take the stream.
	_ = os.RemoveAll(t.TempDir())

	tampered := append([]byte{}, file...)
	tampered[len(tampered)-1] ^= 1
	rg := restoreGrant(g, file, dk, tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: dbname + "_never", Source: "stream"})
	h := runJob(t, svc, pub, key, rg, func(h *backupHarness) {
		for i := 0; i < len(tampered); i += tunnel.BackupDataChunk {
			end := min(i+tunnel.BackupDataChunk, len(tampered))
			_ = tunnel.WriteFrame(h.stream, tunnel.FrameBackupData, tampered[i:end])
		}
		_ = tunnel.WriteFrame(h.stream, tunnel.FrameBackupEnd, nil)
	})
	if !h.answer.OK {
		t.Fatalf("refused: %s", h.answer.Error)
	}
	if h.result.OK || !strings.Contains(h.result.Error, "hash") {
		t.Fatalf("a tampered stream was restored: %+v", h.result)
	}
	if got := queryOne(t, withDB(dsn, "postgres"), `SELECT count(*) FROM pg_database WHERE datname = '`+dbname+`_never'`); got != "0" {
		t.Fatal("a database was created from a tampered archive")
	}
}

func TestReplaceNeedsAnApprovalAndKeepsTheOldDatabase(t *testing.T) {
	dsn, host, port, _ := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	// A scratch database to replace, so the shared test database is not.
	victim := fmt.Sprintf("replace_me_%d", time.Now().UnixNano()%1000000)
	maint := withDB(dsn, "postgres")
	mustExec(t, maint, "CREATE DATABASE "+victim)
	renameTo := victim + "_before_restore_t"
	t.Cleanup(func() {
		mustExec(t, maint, "DROP DATABASE IF EXISTS "+victim)
		mustExec(t, maint, "DROP DATABASE IF EXISTS "+renameTo)
	})
	vdsn := withDB(dsn, victim)
	mustExec(t, vdsn, `CREATE TABLE marker (v text)`)
	mustExec(t, vdsn, `INSERT INTO marker VALUES ('before')`)
	g, file, dk := dumpFor(t, svc, pub, key, host, port, victim)
	mustExec(t, vdsn, `UPDATE marker SET v = 'after'`)

	// Without an approval: refused before anything runs.
	rg := restoreGrant(g, file, dk, tunnel.GrantRestore{Mode: tunnel.RestoreReplace, Source: "local", RenameTo: renameTo})
	h := runJob(t, svc, pub, key, rg, nil)
	if h.answer.OK || !strings.Contains(h.answer.Error, "approval") {
		t.Fatalf("a replace without an approval was accepted: %+v", h.answer)
	}
	if got := queryOne(t, vdsn, `SELECT v FROM marker`); got != "after" {
		t.Fatal("the database changed without an approval")
	}

	// With one: the old database is renamed and kept, the new one restored.
	rg = restoreGrant(g, file, dk, tunnel.GrantRestore{Mode: tunnel.RestoreReplace, Source: "local",
		RenameTo: renameTo, ApprovalID: "req-1"})
	h = runJob(t, svc, pub, key, rg, nil)
	if !h.answer.OK {
		t.Fatalf("refused: %s", h.answer.Error)
	}
	if !h.result.OK {
		t.Fatalf("replace failed at %s: %s", h.result.Stage, h.result.Error)
	}
	if h.result.RenamedTo != renameTo {
		t.Fatalf("result does not say where the old database went: %+v", h.result)
	}
	if got := queryOne(t, vdsn, `SELECT v FROM marker`); got != "before" {
		t.Fatalf("the replaced database has %q, want the backup's row", got)
	}
	// The kept copy takes no connections until someone asks for it.
	mustExec(t, maint, "ALTER DATABASE "+renameTo+" WITH ALLOW_CONNECTIONS true")
	if got := queryOne(t, withDB(dsn, renameTo), `SELECT v FROM marker`); got != "after" {
		t.Fatalf("the kept copy has %q, want what was there before the restore", got)
	}

	// A failed replace puts the old database back: a key for another
	// backup fails after the rename.
	wrong := restoreGrant(g, file, make([]byte, 32), tunnel.GrantRestore{Mode: tunnel.RestoreReplace, Source: "local",
		RenameTo: renameTo + "2", ApprovalID: "req-2"})
	t.Cleanup(func() { mustExec(t, maint, "DROP DATABASE IF EXISTS "+renameTo+"2") })
	h = runJob(t, svc, pub, key, wrong, nil)
	if h.result.OK {
		t.Fatal("a restore with the wrong key succeeded")
	}
	if !strings.Contains(h.result.Error, "put back") {
		t.Fatalf("the failure does not say the database was put back: %s", h.result.Error)
	}
	if got := queryOne(t, vdsn, `SELECT v FROM marker`); got != "before" {
		t.Fatalf("after the rollback the database has %q", got)
	}
}

func TestBackupGrantCannotOpenAWorkspaceAndTheReverse(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	// A postgres grant on the backup service.
	g := tunnel.NewGrant("ana@corp.example", "db-01", "ana", "")
	g.Service = tunnel.PostgresService
	g.DB = &tunnel.GrantDB{ID: "db-x", Name: dbname, Host: host, Port: port, Level: "dba"}
	h := runJob(t, svc, pub, key, g, nil)
	if h.answer.OK || !strings.Contains(h.answer.Error, "not for a backup") {
		t.Fatalf("a workspace grant started a backup job: %+v", h.answer)
	}
	// A backup grant on the postgres service.
	pgsvc := Postgres(PostgresConfig{Verifier: &tunnel.GrantVerifier{Host: "db-01"}, RecordDir: t.TempDir(),
		Admin: DSNAdmin(func(string) string { return dsn })})
	bg := backupGrant(tunnel.BackupJobCatalog, host, port, dbname)
	tok, _ := tunnel.SignGrant(key, bg)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pgsvc(context.Background(), &Call{Header: tunnel.Header{Service: tunnel.PostgresService, Grant: tok},
			Stream: pipeStream{a}, GrantKey: pub})
	}()
	var ans tunnel.Answer
	_ = tunnel.ReadJSON(b, &ans)
	<-done
	if ans.OK || !strings.Contains(ans.Error, "not for a database") {
		t.Fatalf("a backup grant opened a workspace: %+v", ans)
	}
}

func TestCatalogAndPrune(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	dir := t.TempDir()
	svc, pub, key := newBackupService(t, dsn, dir)
	g, _, _ := dumpFor(t, svc, pub, key, host, port, dbname)

	c := backupGrant(tunnel.BackupJobCatalog, host, port, dbname)
	h := runJob(t, svc, pub, key, c, nil)
	if !h.result.OK || h.result.Catalog == nil {
		t.Fatalf("catalog: %+v", h.result)
	}
	if h.result.Catalog.DumpVersion == "" || h.result.Catalog.Local[g.Backup.BackupID] == 0 {
		t.Fatalf("catalog does not know the tools or the file: %+v", h.result.Catalog)
	}
	p := backupGrant(tunnel.BackupJobPrune, host, port, dbname)
	p.Backup.Prune = []string{g.Backup.BackupID}
	h = runJob(t, svc, pub, key, p, nil)
	if !h.result.OK || len(h.result.Pruned) != 1 {
		t.Fatalf("prune: %+v", h.result)
	}
	h = runJob(t, svc, pub, key, backupGrant(tunnel.BackupJobCatalog, host, port, dbname), nil)
	if h.result.Catalog.Local[g.Backup.BackupID] != 0 {
		t.Fatal("the pruned file is still listed")
	}
}

func testGrant(from tunnel.Grant, file, dk []byte, scratch, source string, checks []tunnel.TestCheck) tunnel.Grant {
	g := backupGrant(tunnel.BackupJobTest, from.Backup.Host, from.Backup.Port, from.Backup.Name)
	g.Backup.DataKey = base64.StdEncoding.EncodeToString(dk)
	g.Backup.SHA256 = tunnel.SHA256Hex(file)
	g.Backup.Test = &tunnel.GrantTest{Scratch: scratch, Source: source}
	g.Backup.Checks = checks
	return g
}

func TestTestRestoreRestoresCountsChecksAndDrops(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	mustExec(t, dsn, `CREATE TABLE IF NOT EXISTS backup_probe (id int PRIMARY KEY, note text)`)
	mustExec(t, dsn, `INSERT INTO backup_probe SELECT g, 'row' FROM generate_series(1, 25) g ON CONFLICT DO NOTHING`)
	g, file, dk := dumpFor(t, svc, pub, key, host, port, dbname)
	want := queryOne(t, dsn, `SELECT count(*) FROM backup_probe`)

	scratch := fmt.Sprintf("argus_test_%08x", time.Now().UnixNano()&0xffffffff)
	maint := withDB(dsn, "postgres")
	t.Cleanup(func() { mustExec(t, maint, "DROP DATABASE IF EXISTS "+scratch) })
	h := runJob(t, svc, pub, key, testGrant(g, file, dk, scratch, "local", []tunnel.TestCheck{
		{Name: "Probe rows", SQL: "SELECT count(*) FROM backup_probe"},
		{Name: "Nothing", SQL: "SELECT count(*) FROM backup_probe WHERE id < 0"},
		// A read-only transaction stops a check that writes, whatever it
		// looks like.
		{Name: "Sneaky", SQL: "SELECT pg_catalog.set_config('x.y', 'z', false) IS NOT NULL AND nextval('nope') > 0"},
	}), nil)
	if !h.answer.OK {
		t.Fatalf("refused: %s", h.answer.Error)
	}
	if !h.result.OK {
		t.Fatalf("test restore failed at %s: %s (%+v)", h.result.Stage, h.result.Error, h.result.Test)
	}
	tr := h.result.Test
	if tr == nil || tr.Data == nil {
		t.Fatalf("no numbers in the result: %+v", h.result)
	}
	if !tr.Dropped {
		t.Fatal("the test copy was not dropped")
	}
	if got := queryOne(t, maint, `SELECT count(*) FROM pg_database WHERE datname = '`+scratch+`'`); got != "0" {
		t.Fatal("the test copy is still there")
	}
	var probe int64 = -2
	for _, tb := range tr.Data.Tables {
		if tb.Name == "public.backup_probe" {
			probe = tb.Rows
		}
	}
	if fmt.Sprint(probe) != want {
		t.Fatalf("counted %d rows in the copy, the source has %s", probe, want)
	}
	found := false
	for _, e := range tr.Expected {
		found = found || e == "public.backup_probe"
	}
	if !found {
		t.Fatalf("the table of contents does not list the table: %v", tr.Expected)
	}
	if tr.RestoreMS <= 0 || len(tr.Steps) < 5 {
		t.Fatalf("steps or time missing: %+v", tr)
	}
	byName := map[string]tunnel.CheckResult{}
	for _, c := range tr.Data.Checks {
		byName[c.Name] = c
	}
	if c := byName["Probe rows"]; !c.OK || c.Value != want {
		t.Fatalf("Probe rows: %+v", c)
	}
	if c := byName["Nothing"]; c.OK || c.Value != "0" {
		t.Fatalf("a zero count passed: %+v", c)
	}
	if c := byName["Sneaky"]; c.OK || c.Error == "" {
		t.Fatalf("a check that writes passed: %+v", c)
	}
	// The live database was never touched.
	if got := queryOne(t, dsn, `SELECT count(*) FROM backup_probe`); got != want {
		t.Fatalf("the source changed: %s", got)
	}
}

func TestTestRestoreRefusesNamesThatAreNotTestCopies(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	g, file, dk := dumpFor(t, svc, pub, key, host, port, dbname)
	for _, name := range []string{dbname, "postgres", "argus_test_", "argus_test_zzzzzzzz", "orders_before_restore_x"} {
		h := runJob(t, svc, pub, key, testGrant(g, file, dk, name, "local", nil), nil)
		if h.answer.OK {
			t.Fatalf("a test restore into %q was accepted", name)
		}
		d := backupGrant(tunnel.BackupJobDropTest, host, port, dbname)
		d.Backup.Kept = name
		if h := runJob(t, svc, pub, key, d, nil); h.answer.OK {
			t.Fatalf("a drop of %q as a test copy was accepted", name)
		}
	}
	// And a restore into a new database cannot take a test copy's name.
	rg := restoreGrant(g, file, dk, tunnel.GrantRestore{Mode: tunnel.RestoreNew, NewName: "argus_test_0000abcd", Source: "local"})
	if h := runJob(t, svc, pub, key, rg, nil); h.answer.OK {
		t.Fatal("a restore took a test copy's name")
	}
}

func TestChecksJobRunsReadOnlyAgainstTheLiveDatabase(t *testing.T) {
	dsn, host, port, dbname := backupTest(t)
	svc, pub, key := newBackupService(t, dsn, t.TempDir())
	mustExec(t, dsn, `CREATE TABLE IF NOT EXISTS backup_probe (id int PRIMARY KEY, note text)`)
	g := backupGrant(tunnel.BackupJobChecks, host, port, dbname)
	g.Backup.Checks = []tunnel.TestCheck{{Name: "exists", SQL: "SELECT true"}, {Name: "typo", SQL: "SELECT count(*) FROM no_such_table"}}
	h := runJob(t, svc, pub, key, g, nil)
	if !h.answer.OK || !h.result.OK || len(h.result.Checks) != 2 {
		t.Fatalf("checks job: %+v %+v", h.answer, h.result)
	}
	if !h.result.Checks[0].OK || h.result.Checks[1].OK || !strings.Contains(h.result.Checks[1].Error, "no_such_table") {
		t.Fatalf("checks: %+v", h.result.Checks)
	}
	// A write is refused before it reaches Postgres.
	g.Backup.Checks = []tunnel.TestCheck{{Name: "w", SQL: "DELETE FROM backup_probe"}}
	if h := runJob(t, svc, pub, key, g, nil); h.answer.OK {
		t.Fatal("a DELETE was accepted as a check")
	}
}

func TestServerJobsLetATestRunAlone(t *testing.T) {
	var s serverJobs
	if !s.take("a") || s.take("a") {
		t.Fatal("one job per database")
	}
	if s.takeAlone("b") {
		t.Fatal("a test started beside another job")
	}
	s.give("a")
	if !s.takeAlone("b") {
		t.Fatal("a test could not start on an idle server")
	}
	if s.take("c") || s.takeAlone("c") {
		t.Fatal("a job started beside a test")
	}
	s.give("b")
	if !s.take("c") {
		t.Fatal("the server stayed busy after the test")
	}
}

func TestTOCDataTables(t *testing.T) {
	list := `;
; Archive created at 2026-11-04 04:00:00 UTC
3360; 0 16390 TABLE DATA public orders postgres
3361; 0 16391 TABLE DATA billing Order Items postgres
215; 1259 16386 TABLE public orders postgres
`
	got := tocDataTables(list)
	if len(got) != 2 || got[0] != "public.orders" || got[1] != "billing.Order Items" {
		t.Fatalf("got %q", got)
	}
}
