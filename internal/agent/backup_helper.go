package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
`argus-agent pg-backup <request>`: the fixed-purpose helper the backup
service runs as the server's postgres account, over the local socket with
peer authentication. It takes one request (base64 JSON in its one argument:
no secrets, nothing a `ps` would mind) and does exactly one of:

	dump      pg_dump of the database to stdout, --verbose progress on stderr
	restore   createdb + pg_restore of the plain archive on stdin; with
	          Existing, pg_restore alone into a test copy already made
	catalog   the versions of the tools, on stdout as JSON
	toc       pg_restore --list of the archive on stdin: proves it is readable

It never sees a key: the daemon encrypts what comes out of the dump and
decrypts what goes into the restore. The database and the new name are
checked here again against tunnel.ValidDBName, so nothing reaches the
command line that needs quoting.
*/

// PGBackupRequest is the helper's one argument.
type PGBackupRequest struct {
	Op       string `json:"op"`
	Socket   string `json:"socket"`
	Port     int    `json:"port"`
	Database string `json:"database,omitempty"`
	// User is the Postgres role to sign in as over peer auth; empty is
	// postgres.
	User string `json:"user,omitempty"`
	// Compression is pg_dump's -Z: 0..9 with gzip.
	Compression int `json:"compression,omitempty"`
	// Existing restores into a database already made (a test copy, which
	// create-test made with no connections allowed) instead of creating it.
	Existing bool `json:"existing,omitempty"`
	// Low runs the tool at low CPU and I/O priority where nice and ionice
	// are installed: a test restore should not slow the server's real work.
	Low bool `json:"low,omitempty"`
}

// EncodePGBackupRequest and DecodePGBackupRequest carry it on the command line.
func EncodePGBackupRequest(r PGBackupRequest) string {
	b, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodePGBackupRequest(s string) (PGBackupRequest, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return PGBackupRequest{}, errors.New("bad request")
	}
	var r PGBackupRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return PGBackupRequest{}, errors.New("bad request")
	}
	return r, nil
}

// PGBackupMain is the helper's entry point.
func PGBackupMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: argus-agent pg-backup <request>")
		return 2
	}
	req, err := DecodePGBackupRequest(args[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := RunPGBackup(ctx, req, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// RunPGBackup does the request with the Postgres tools on PATH or in the
// usual package directories.
func RunPGBackup(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	if !strings.HasPrefix(req.Socket, "/") && !tunnel.LocalPGHost(req.Socket) {
		return errors.New("the database must be on this server")
	}
	if strings.Contains(req.Socket, "..") || req.Port <= 0 || req.Port > 65535 {
		return errors.New("bad socket or port")
	}
	user := req.User
	if user == "" {
		user = "postgres"
	}
	if !safeAccountName.MatchString(user) {
		return errors.New("bad account name")
	}
	conn := []string{"--host=" + req.Socket, "--port=" + strconv.Itoa(req.Port), "--no-password", "--username=" + user}
	switch req.Op {
	case "catalog":
		c, err := pgToolCatalog()
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(c)
	case "dump":
		if !tunnel.ValidDBName.MatchString(req.Database) {
			return errors.New("not a database name Argus will dump")
		}
		z := req.Compression
		if z <= 0 || z > 9 {
			z = 6
		}
		bin, err := pgTool("pg_dump")
		if err != nil {
			return err
		}
		args := append([]string{"--format=custom", "--compress=" + strconv.Itoa(z), "--verbose",
			"--dbname=" + req.Database}, conn...)
		return runTool(ctx, bin, args, nil, stdout, stderr)
	case "toc":
		bin, err := pgTool("pg_restore")
		if err != nil {
			return err
		}
		return runTool(ctx, bin, []string{"--list"}, stdin, stdout, stderr)
	case "restore":
		if !tunnel.ValidDBName.MatchString(req.Database) {
			return errors.New("not a database name Argus will create")
		}
		if req.Existing {
			// Only ever a test copy: anything else must not already exist.
			if !tunnel.ValidTestDB.MatchString(req.Database) {
				return errors.New("only a test copy is restored into an existing database")
			}
		} else {
			createdb, err := pgTool("createdb")
			if err != nil {
				return err
			}
			if err := runTool(ctx, createdb, append([]string{"--template=template0", req.Database}, conn...),
				nil, stdout, stderr); err != nil {
				return fmt.Errorf("createdb %s: %w", req.Database, err)
			}
		}
		bin, err := pgTool("pg_restore")
		if err != nil {
			return err
		}
		args := append([]string{"--format=custom", "--no-owner", "--verbose", "--dbname=" + req.Database}, conn...)
		if req.Low {
			bin, args = lowPriority(bin, args)
		}
		return runTool(ctx, bin, args, stdin, stdout, stderr)
	}
	return errors.New("unknown operation " + req.Op)
}

func runTool(ctx context.Context, bin string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=15", "PGOPTIONS=-c lock_timeout=10s -c statement_timeout=0")
	return cmd.Run()
}

// lowPriority wraps a command in nice and ionice when they are installed,
// and leaves it as it is when they are not.
func lowPriority(bin string, args []string) (string, []string) {
	if ionice, err := exec.LookPath("ionice"); err == nil {
		args = append([]string{"-c3", bin}, args...)
		bin = ionice
	}
	if nice, err := exec.LookPath("nice"); err == nil {
		args = append([]string{"-n", "10", bin}, args...)
		bin = nice
	}
	return bin, args
}

// pgTool finds a Postgres client tool: on PATH, else the newest of the
// package directories Debian and Red Hat install into.
func pgTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	var found []string
	for _, pat := range []string{"/usr/lib/postgresql/*/bin/" + name, "/usr/pgsql-*/bin/" + name,
		"/usr/local/pgsql/bin/" + name, "/opt/homebrew/opt/postgresql@*/bin/" + name} {
		m, _ := filepath.Glob(pat)
		found = append(found, m...)
	}
	if len(found) == 0 {
		return "", fmt.Errorf("%s is not installed on this server (install the postgresql client package)", name)
	}
	sort.Slice(found, func(i, j int) bool { return pgPathVersion(found[i]) > pgPathVersion(found[j]) })
	return found[0], nil
}

var (
	pathVersion     = regexp.MustCompile(`(\d+)`)
	safeAccountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

func pgPathVersion(p string) int {
	m := pathVersion.FindAllString(filepath.Dir(filepath.Dir(p)), -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1])
	return n
}

// pgToolCatalog is the installed tools' versions.
func pgToolCatalog() (tunnel.BackupCatalog, error) {
	var c tunnel.BackupCatalog
	ver := func(tool string) string {
		bin, err := pgTool(tool)
		if err != nil {
			return ""
		}
		out, err := exec.Command(bin, "--version").Output()
		if err != nil {
			return ""
		}
		return toolVersion(string(out))
	}
	c.DumpVersion = ver("pg_dump")
	c.RestoreVersion = ver("pg_restore")
	c.Compression = "gzip"
	if c.DumpVersion == "" {
		return c, errors.New("pg_dump is not installed on this server (install the postgresql client package)")
	}
	return c, nil
}

// toolVersion turns "pg_dump (PostgreSQL) 16.4 (Debian 16.4-1)" into "16.4".
func toolVersion(s string) string {
	f := strings.Fields(s)
	for i, w := range f {
		if w == "(PostgreSQL)" && i+1 < len(f) {
			return f[i+1]
		}
	}
	if len(f) >= 3 {
		return f[2]
	}
	return strings.TrimSpace(s)
}

// dumpProgress reads pg_dump's --verbose stderr into stage and detail.
func dumpProgress(line string) (stage, detail string, ok bool) {
	line = strings.TrimPrefix(strings.TrimSpace(line), "pg_dump: ")
	switch {
	case strings.HasPrefix(line, "dumping contents of table "):
		return "data", strings.Trim(strings.TrimPrefix(line, "dumping contents of table "), `"`), true
	case strings.HasPrefix(line, "processing data for table "):
		return "data", strings.Trim(strings.TrimPrefix(line, "processing data for table "), `"`), true
	case strings.HasPrefix(line, "saving ") || strings.HasPrefix(line, "reading ") ||
		strings.HasPrefix(line, "finding ") || strings.HasPrefix(line, "flagging ") ||
		strings.HasPrefix(line, "identifying ") || strings.HasPrefix(line, "creating "):
		return "schema", line, true
	}
	return "", "", false
}

// restoreWarnings counts pg_restore's "errors ignored on restore: N".
func restoreWarnings(stderr string) int {
	for _, l := range strings.Split(stderr, "\n") {
		if i := strings.Index(l, "errors ignored on restore: "); i >= 0 {
			n, _ := strconv.Atoi(strings.TrimSpace(l[i+len("errors ignored on restore: "):]))
			return n
		}
	}
	return 0
}

// lineScanner reads lines of any length from a tool's stderr.
func lineScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	return s
}
