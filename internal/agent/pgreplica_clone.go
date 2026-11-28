package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The replica's half of a streaming replica: a fresh server's Postgres becomes
a byte-for-byte copy of a primary that keeps streaming from it.

	control ──replica-clone job──▶ this agent
	   1. reads the local Postgres: same major version as the primary, not a
	      replica already, and no databases or tables of its own
	   2. makes the replication password here, and sends only its SCRAM
	      verifier up (stage "verifier"); control has the primary make the
	      role and slot with it, then sends go
	   3. stops the Postgres service
	   4. as the postgres account: moves the empty data directory aside and
	      runs pg_basebackup into a new one, with -R so it starts as a
	      standby of the primary
	   5. starts the service and checks it is in recovery

The password never leaves this server. pg_basebackup -R writes it into the
replica's postgresql.auto.conf (mode 0600, the postgres account's), where
Postgres needs it to reconnect after a restart. A copy that fails puts the
old directory back and starts the server as it was. Argus never does this to
a server with databases of its own.
*/

// keptSuffix ends the name the server's own data directory is moved to.
const keptSuffix = ".argus-before-replica"

// verifierWait is how long a clone waits for the primary to be prepared.
const verifierWait = 30 * time.Minute

// pgLocal is what replica-local reads from this server's Postgres.
type pgLocal struct {
	DataDir    string   `json:"dataDir"`
	VersionNum int      `json:"versionNum"`
	InRecovery bool     `json:"inRecovery"`
	Databases  []string `json:"databases,omitempty"`
	UserTables int      `json:"userTables"`
	// Checkpointer is a postmaster child's pid, to find the service by.
	Checkpointer int `json:"checkpointer"`
}

func pgReplicaLocal(ctx context.Context, conn *caduceus.Conn) PGAdminResult {
	version := conn.Wire().ParameterStatus("server_version")
	v, err := row(ctx, conn, `SELECT current_setting('data_directory'), current_setting('server_version_num'), pg_is_in_recovery(),
		coalesce((SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres'), ''),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relkind IN ('r', 'p', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'),
		coalesce((SELECT pid FROM pg_stat_activity WHERE backend_type = 'checkpointer' LIMIT 1), 0)`)
	if err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
	}
	l := pgLocal{DataDir: string(v[0]), InRecovery: string(v[2]) == "t"}
	l.VersionNum, _ = strconv.Atoi(string(v[1]))
	if d := string(v[3]); d != "" {
		l.Databases = strings.Split(d, ",")
	}
	l.UserTables, _ = strconv.Atoi(string(v[4]))
	l.Checkpointer, _ = strconv.Atoi(string(v[5]))
	b, _ := json.Marshal(l)
	return PGAdminResult{OK: true, Version: version, Data: b}
}

// fresh says why l is not a server Argus may turn into a replica, or "".
func (l pgLocal) refuse(primaryMajor int) string {
	switch {
	case l.VersionNum/10000 != primaryMajor:
		return fmt.Sprintf("this server runs Postgres %d and the primary runs %d; install %d here first", l.VersionNum/10000, primaryMajor, primaryMajor)
	case l.InRecovery:
		return "this server's Postgres is a replica already"
	case len(l.Databases) > 0:
		return "this server's Postgres has databases of its own (" + strings.Join(l.Databases, ", ") + "); Argus only turns a fresh one into a replica"
	case l.UserTables > 0:
		return fmt.Sprintf("this server's postgres database has %d tables of its own; Argus only turns a fresh server into a replica", l.UserTables)
	case !filepath.IsAbs(l.DataDir) || l.DataDir == "/":
		return "the data directory " + l.DataDir + " is not one Argus will replace"
	}
	return ""
}

// pgUnits stops and starts the Postgres service.
type pgUnits struct {
	// unitOf names the systemd service a process is in.
	unitOf func(pid int) (string, error)
	run    func(ctx context.Context, verb, unit string) error
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+\.service$`)

func systemUnits() *pgUnits {
	return &pgUnits{
		unitOf: func(pid int) (string, error) {
			b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
			if err != nil {
				return "", err
			}
			return unitFromCgroup(string(b))
		},
		run: func(ctx context.Context, verb, unit string) error {
			if !unitName.MatchString(unit) || (verb != "stop" && verb != "start") {
				return errors.New("refusing to " + verb + " " + unit)
			}
			out, err := exec.CommandContext(ctx, "systemctl", verb, unit).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl %s %s: %v: %s", verb, unit, err, tail(out, 300))
			}
			return nil
		},
	}
}

// unitFromCgroup is the .service a /proc/<pid>/cgroup puts the process in.
func unitFromCgroup(cg string) (string, error) {
	for _, line := range strings.Split(cg, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		segs := strings.Split(parts[2], "/")
		for i := len(segs) - 1; i >= 0; i-- {
			if unitName.MatchString(segs[i]) {
				return segs[i], nil
			}
		}
	}
	return "", errors.New("this server's Postgres is not run by a systemd service, so Argus cannot stop and start it")
}

// replicaPrimary runs one replica operation on this primary.
func (j *backupJob) replicaPrimary(ctx context.Context) tunnel.BackupResult {
	r := *j.b.Replica
	rep := r.PGReplica
	r2 := j.cfg.Admin(ctx, PGAdminRequest{Op: "replica-" + r.Op, Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser, Replica: &rep})
	if !r2.OK {
		return j.fail(r.Op, errors.New(r2.Error))
	}
	return tunnel.BackupResult{OK: true, ServerVersion: r2.Version, Replica: r2.Data}
}

func (j *backupJob) local(ctx context.Context) (pgLocal, error) {
	r := j.cfg.Admin(ctx, PGAdminRequest{Op: "replica-local", Database: "postgres", Socket: j.socket(),
		Port: j.b.Port, AdminUser: j.b.AdminUser})
	if !r.OK {
		return pgLocal{}, errors.New(r.Error)
	}
	var l pgLocal
	if err := json.Unmarshal(r.Data, &l); err != nil {
		return pgLocal{}, err
	}
	return l, nil
}

// replicaClone turns this server's fresh Postgres into a replica.
func (j *backupJob) replicaClone(ctx context.Context) tunnel.BackupResult {
	r := *j.b.Replica
	j.progress("reading", "this server's Postgres", -1)
	l, err := j.local(ctx)
	if err != nil {
		return j.fail("reading", err)
	}
	if why := l.refuse(r.PrimaryVersion); why != "" {
		return j.fail("reading", errors.New(why))
	}
	unit, err := j.cfg.Units.unitOf(l.Checkpointer)
	if err != nil {
		return j.fail("reading", err)
	}

	password, err := sessionPassword()
	if err != nil {
		return j.fail("password", err)
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return j.fail("password", err)
	}
	j.progress(tunnel.ReplicaStageVerifier, verifier, -1)
	j.flushProgress()
	select {
	case <-j.goOn:
	case <-ctx.Done():
		return j.fail(tunnel.ReplicaStageVerifier, errors.New("cancelled before the primary was ready"))
	case <-time.After(verifierWait):
		return j.fail(tunnel.ReplicaStageVerifier, errors.New("the primary was not ready in time"))
	}

	// From here on, put the server back as it was on any failure.
	j.progress("stopping", unit, -1)
	if err := j.cfg.Units.run(ctx, "stop", unit); err != nil {
		return j.fail("stopping", err)
	}
	restart := func() {
		if err := j.cfg.Units.run(context.WithoutCancel(ctx), "start", unit); err != nil {
			j.log.Warn("replica clone: starting Postgres again failed", "unit", unit, "error", err)
		}
	}
	clone := &PGCloneRequest{DataDir: l.DataDir, Host: r.PrimaryHost, Port: r.PrimaryPort, Role: r.Role(),
		Slot: r.Slot(), Major: r.PrimaryVersion}
	j.progress("copying", "from "+r.PrimaryHost, -1)
	began := time.Now()
	var out, errb bytes.Buffer
	err = j.cfg.Tool(ctx, PGBackupRequest{Op: "basebackup", Socket: j.socket(), Port: j.b.Port, User: j.b.AdminUser, Clone: clone},
		strings.NewReader(password), &out, &lineSink{buf: &errb, each: func(s string) { j.progress("", s, -1) }})
	copyMS := time.Since(began).Milliseconds()
	if err != nil {
		restart()
		return j.fail("copying", fmt.Errorf("%s", strings.TrimSpace(lastLine(errb.String())+" "+err.Error())))
	}

	j.progress("starting", unit, -1)
	if err := j.cfg.Units.run(ctx, "start", unit); err != nil {
		var ub bytes.Buffer
		if uerr := j.cfg.Tool(context.WithoutCancel(ctx), PGBackupRequest{Op: "basebackup-undo", Socket: j.socket(), Port: j.b.Port,
			User: j.b.AdminUser, Clone: clone}, nil, &ub, &ub); uerr != nil {
			return j.fail("starting", fmt.Errorf("%v; putting the old data directory back failed too: %v %s", err, uerr, lastLine(ub.String())))
		}
		restart()
		return j.fail("starting", fmt.Errorf("%v; the server is back as it was", err))
	}

	// Postgres takes a moment to accept connections as a standby.
	j.progress("checking", "", -1)
	var now pgLocal
	for i := 0; ; i++ {
		if now, err = j.local(ctx); err == nil || i == 30 || ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return j.fail("checking", fmt.Errorf("Postgres did not answer after the copy: %w", err))
	}
	if !now.InRecovery {
		return j.fail("checking", errors.New("Postgres started, but not as a replica"))
	}
	b, _ := json.Marshal(tunnel.PGReplicaCloned{DataDir: l.DataDir, Kept: l.DataDir + keptSuffix, Unit: unit,
		Version: now.VersionNum, InRecovery: true, CopyMS: copyMS})
	return tunnel.BackupResult{OK: true, Replica: b}
}

// lineSink keeps what is written and calls each with every complete line;
// pg_basebackup's progress ends lines with \r.
type lineSink struct {
	buf  *bytes.Buffer
	part []byte
	each func(string)
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for _, c := range p {
		if c == '\n' || c == '\r' {
			if t := strings.TrimSpace(string(s.part)); t != "" {
				s.each(t)
			}
			s.part = s.part[:0]
			continue
		}
		if len(s.part) < 512 {
			s.part = append(s.part, c)
		}
	}
	return len(p), nil
}

// PGCloneRequest is the helper's half of a clone.
type PGCloneRequest struct {
	DataDir string `json:"dataDir"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Role    string `json:"role"`
	Slot    string `json:"slot"`
	Major   int    `json:"major"`
}

var cloneIdent = regexp.MustCompile(`^argus_(repl|replica)_[a-z][a-z0-9_]{0,39}$`)

func (c *PGCloneRequest) check() error {
	if c == nil {
		return errors.New("no clone named")
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) != c.DataDir || c.DataDir == "/" {
		return errors.New("not a data directory Argus will replace: " + c.DataDir)
	}
	g := tunnel.GrantReplica{PGReplica: tunnel.PGReplica{Name: "x"}, PrimaryHost: c.Host, PrimaryPort: c.Port, PrimaryVersion: c.Major}
	if err := g.Validate(tunnel.BackupJobReplicaClone); err != nil {
		return err
	}
	if !cloneIdent.MatchString(c.Role) || !cloneIdent.MatchString(c.Slot) {
		return errors.New("not a replication role or slot Argus made")
	}
	return nil
}

// runBaseBackup is the helper's half, as the postgres account.
func runBaseBackup(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	c := req.Clone
	if err := c.check(); err != nil {
		return err
	}
	kept := c.DataDir + keptSuffix
	if req.Op == "basebackup-undo" {
		if _, err := os.Stat(kept); err != nil {
			return errors.New("there is no earlier data directory to put back")
		}
		if err := os.RemoveAll(c.DataDir); err != nil {
			return err
		}
		return os.Rename(kept, c.DataDir)
	}
	if _, err := os.Stat(kept); err == nil {
		return errors.New(kept + " is left from an earlier try; look at it and remove it, then try again")
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "postmaster.pid")); err == nil {
		return errors.New("Postgres is still running from " + c.DataDir)
	}
	pw, err := io.ReadAll(io.LimitReader(stdin, 256))
	if err != nil || len(bytes.TrimSpace(pw)) == 0 {
		return errors.New("no replication password")
	}
	bin, err := pgToolMajor("pg_basebackup", c.Major)
	if err != nil {
		return err
	}
	st, err := os.Stat(c.DataDir)
	if err != nil {
		return err
	}
	if err := os.Rename(c.DataDir, kept); err != nil {
		return fmt.Errorf("moving the empty data directory aside: %w", err)
	}
	back := func() {
		_ = os.RemoveAll(c.DataDir)
		_ = os.Rename(kept, c.DataDir)
	}
	if err := os.Mkdir(c.DataDir, st.Mode().Perm()); err != nil {
		_ = os.Rename(kept, c.DataDir)
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "--host="+c.Host, "--port="+strconv.Itoa(c.Port), "--username="+c.Role,
		"--pgdata="+c.DataDir, "--slot="+c.Slot, "--wal-method=stream", "--write-recovery-conf",
		"--checkpoint=fast", "--no-password", "--progress")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(os.Environ(), "PGPASSWORD="+string(bytes.TrimSpace(pw)), "PGSSLMODE=prefer",
		"PGAPPNAME="+strings.TrimPrefix(c.Role, "argus_repl_"), "PGCONNECT_TIMEOUT=15")
	if err := cmd.Run(); err != nil {
		back()
		return fmt.Errorf("pg_basebackup: %w", err)
	}
	return nil
}

// pgToolMajor finds a Postgres tool of one major version, as pg_basebackup
// must match the server it copies into.
func pgToolMajor(name string, major int) (string, error) {
	v := strconv.Itoa(major)
	for _, p := range []string{"/usr/lib/postgresql/" + v + "/bin/" + name, "/usr/pgsql-" + v + "/bin/" + name,
		"/opt/homebrew/opt/postgresql@" + v + "/bin/" + name} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	bin, err := pgTool(name)
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil || !strings.HasPrefix(toolVersion(string(out)), v) {
		return "", fmt.Errorf("%s %d is not installed on this server", name, major)
	}
	return bin, nil
}
