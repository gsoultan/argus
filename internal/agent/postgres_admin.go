package agent

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Provisioning a person's Postgres role.

The agent stores no Postgres password. These operations run as the server's
own "postgres" account over the local socket (peer authentication), in a
short helper process -- `argus-agent pg-admin` -- that reads one request on
stdin and writes one result. They are a fixed list, never free SQL.

	admin  one administrative operation -- an extension, a publication or a
	       subscription -- from the fixed list in postgres_admin_ops.go, for
	       a session whose grant is dba
	open   make sure the level roles exist and hold their grants in the
	       database; make sure the person's role exists, is Argus's, is no
	       more than an ordinary role, and is a member of exactly the level
	       the grant names; let it log in with this session's password until
	       the session's end at the latest
	close  stop it logging in, take its memberships away, and end anything
	       still connected as it through Argus

A role Argus did not create is never touched: it is reported as taken.
*/

// PGAdminRequest is one provisioning operation.
type PGAdminRequest struct {
	Op string `json:"op"` // open, close, admin, prepare-replace, rollback-replace, drop-database, list-databases, discover-databases, database-size, create-test, drop-test, test-checks, run-checks
	// Database to connect to and grant in. The replace operations connect
	// to the maintenance database and act on Target, since a database
	// cannot be renamed from inside itself.
	Database string `json:"database"`
	// Target is the database a replace operation acts on, and RenameTo
	// where it goes.
	Target   string `json:"target,omitempty"`
	RenameTo string `json:"renameTo,omitempty"`
	// Socket is the directory of the server's Unix socket; Port its port.
	Socket string `json:"socket"`
	Port   int    `json:"port"`
	// AdminUser is the account peer authentication signs in as. Empty is
	// "postgres".
	AdminUser string `json:"adminUser,omitempty"`
	Role      string `json:"role"`
	Owner     string `json:"owner"`
	Level     string `json:"level"`
	// Verifier is the SCRAM-SHA-256 verifier of the session's password;
	// the password itself never leaves the agent.
	Verifier   string    `json:"verifier,omitempty"`
	ValidUntil time.Time `json:"validUntil,omitzero"`
	// Pooled says sessions reach this database through PgBouncer, so the
	// lookup PgBouncer authenticates Argus roles with must exist.
	Pooled bool `json:"pooled,omitempty"`
	// Admin is the operation, for admin. Owner names who asked, for the
	// comment on what it creates.
	Admin *tunnel.PGAdminOp `json:"admin,omitempty"`
	// Checks are a test restore's custom checks, for test-checks and
	// run-checks.
	Checks []tunnel.TestCheck `json:"checks,omitempty"`
}

// PGAdminResult is the answer.
type PGAdminResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Taken says the role exists and is not Argus's.
	Taken   bool   `json:"taken,omitempty"`
	Version string `json:"version,omitempty"`
	// For admin: the statement that ran or would run, with any password
	// redacted; Postgres's SQLSTATE, detail and hint on a failure; the list
	// or PGAdminDone on success.
	SQL    string          `json:"sql,omitempty"`
	Code   string          `json:"code,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Hint   string          `json:"hint,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	// Databases is what list-databases found.
	Databases []string `json:"databases,omitempty"`
	// Found is what discover-databases found, with owners and sizes.
	Found     []tunnel.Discovered `json:"found,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
	// Bytes and DataDir are what database-size found.
	Bytes   int64  `json:"bytes,omitempty"`
	DataDir string `json:"dataDir,omitempty"`
	// Test is what test-checks read from a test copy; Checks what
	// run-checks found.
	Test   *tunnel.TestData     `json:"test,omitempty"`
	Checks []tunnel.CheckResult `json:"checks,omitempty"`
}

// pgAdminRunner performs provisioning; the production one runs the helper as
// the postgres account, tests connect with a superuser's DSN.
type pgAdminRunner func(ctx context.Context, req PGAdminRequest) PGAdminResult

// roleMarker begins the comment on every role Argus created.
const roleMarker = "argus-managed"

// peopleRole is the group every personal role is in, so pg_hba.conf can name
// all of them in one line: local all +argus_people scram-sha-256.
const peopleRole = "argus_people"

var (
	safeIdent    = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	scramPattern = regexp.MustCompile(`^SCRAM-SHA-256\$[0-9]+:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)
)

// levelRole names the group role for a level in a database: argus_read_orders.
// Always a plain identifier; a database name that would not fit is shortened
// and given a hash so two databases cannot share a role.
func levelRole(level, database string) string {
	b := make([]byte, 0, len(database))
	for i := 0; i < len(database); i++ {
		c := database[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+32)
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	name := "argus_" + level + "_" + string(b)
	if len(name) > 63 || string(b) != database {
		sum := sha256.Sum256([]byte(database))
		short := name
		if len(short) > 54 {
			short = short[:54]
		}
		name = short + "_" + hex.EncodeToString(sum[:4])
	}
	return name
}

// levels lists the levels up to and including level, least first.
func levelsUpTo(level string) []string {
	all := []string{tunnel.LevelRead, tunnel.LevelWrite, tunnel.LevelOwner, tunnel.LevelDBA}
	return all[:tunnel.LevelRank(level)]
}

// RunPGAdmin performs req on conn, which is signed in as a superuser to
// req.Database. It is what the helper runs.
func RunPGAdmin(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	switch req.Op {
	case "prepare-replace", "rollback-replace", "drop-database", "list-databases":
		return pgReplaceOp(ctx, conn, req)
	case "discover-databases":
		return pgDiscover(ctx, conn)
	case "database-size", "create-test", "drop-test", "test-checks", "run-checks":
		return pgTestOp(ctx, conn, req)
	}
	if !tunnel.ValidPersonalRole(req.Role) {
		return PGAdminResult{Error: "not a role name Argus will manage: " + req.Role}
	}
	if req.Op == "open" && tunnel.LevelRank(req.Level) == 0 {
		return PGAdminResult{Error: "unknown access level " + req.Level}
	}
	switch req.Op {
	case "open":
		return pgOpen(ctx, conn, req)
	case "close":
		return pgClose(ctx, conn, req)
	case "admin":
		return pgAdminOp(ctx, conn, req)
	}
	return PGAdminResult{Error: "unknown operation " + req.Op}
}

func pgOpen(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	if !scramPattern.MatchString(req.Verifier) {
		return PGAdminResult{Error: "the session password is not a SCRAM verifier"}
	}
	if req.ValidUntil.IsZero() {
		return PGAdminResult{Error: "a session password must expire"}
	}
	version := conn.ParameterStatus("server_version")

	// The level roles and their grants, every time: a table made since the
	// last session is covered by the next one without anyone re-granting.
	if req.Pooled {
		if err := pgEnsurePoolerAuth(ctx, conn); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
	}
	if err := pgEnsureLevels(ctx, conn, req.Database, req.Level); err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
	}
	if err := pgEnsureTrash(ctx, conn); err != nil {
		return PGAdminResult{Error: "setting up the trash: " + err.Error(), Version: version}
	}

	// The person's role.
	role := pgx(req.Role)
	res := conn.ExecParams(ctx,
		`SELECT coalesce(shobj_description(oid, 'pg_authid'), '') FROM pg_roles WHERE rolname = $1`,
		[][]byte{[]byte(req.Role)}, nil, nil, nil).Read()
	if res.Err != nil {
		return PGAdminResult{Error: res.Err.Error(), Version: version}
	}
	comment := fmt.Sprintf("%s owner=%s", roleMarker, req.Owner)
	if len(res.Rows) == 0 {
		if err := execAll(ctx, conn,
			"CREATE ROLE "+role+" NOLOGIN INHERIT",
			"COMMENT ON ROLE "+role+" IS "+quoteLiteral(comment)); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
	} else if !strings.HasPrefix(string(res.Rows[0][0]), roleMarker) {
		return PGAdminResult{Taken: true, Version: version, Error: "a role named " + req.Role +
			" already exists in this Postgres and Argus did not create it, so it is left alone"}
	}

	stmts := []string{
		// Whatever anyone changed by hand, a personal role is an ordinary one.
		"ALTER ROLE " + role + " NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"GRANT " + pgx(peopleRole) + " TO " + role,
	}
	// Exactly the level asked for: a lower level left over from an earlier
	// session would be harmless, a higher one would not.
	for _, l := range []string{tunnel.LevelRead, tunnel.LevelWrite, tunnel.LevelOwner, tunnel.LevelDBA} {
		if l != req.Level {
			stmts = append(stmts, revokeIfExists(levelRole(l, req.Database), req.Role))
		}
	}
	stmts = append(stmts,
		"GRANT "+pgx(levelRole(req.Level, req.Database))+" TO "+role,
		"ALTER ROLE "+role+" LOGIN PASSWORD "+quoteLiteral(req.Verifier)+
			" VALID UNTIL "+quoteLiteral(req.ValidUntil.UTC().Format(time.RFC3339)))
	if err := execAll(ctx, conn, stmts...); err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
	}
	return PGAdminResult{OK: true, Version: version}
}

func pgClose(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	res := conn.ExecParams(ctx,
		`SELECT coalesce(shobj_description(oid, 'pg_authid'), '') FROM pg_roles WHERE rolname = $1`,
		[][]byte{[]byte(req.Role)}, nil, nil, nil).Read()
	if res.Err != nil {
		return PGAdminResult{Error: res.Err.Error()}
	}
	if len(res.Rows) == 0 {
		return PGAdminResult{OK: true}
	}
	if !strings.HasPrefix(string(res.Rows[0][0]), roleMarker) {
		return PGAdminResult{Taken: true, Error: "the role is not Argus's"}
	}
	role := pgx(req.Role)
	stmts := []string{"ALTER ROLE " + role + " NOLOGIN PASSWORD NULL"}
	for _, l := range []string{tunnel.LevelRead, tunnel.LevelWrite, tunnel.LevelOwner, tunnel.LevelDBA} {
		stmts = append(stmts, revokeIfExists(levelRole(l, req.Database), req.Role))
	}
	if err := execAll(ctx, conn, stmts...); err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	// Anything still connected as the role through Argus ends with it.
	_ = conn.ExecParams(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE usename = $1 AND application_name LIKE 'argus:%' AND pid <> pg_backend_pid()`,
		[][]byte{[]byte(req.Role)}, nil, nil, nil).Read()
	return PGAdminResult{OK: true}
}

// pgEnsurePoolerAuth creates the lookup PgBouncer authenticates Argus roles
// with; see tunnel.PoolerAuthSetupSQL. It lives in the connected database,
// which is where PgBouncer runs auth_query unless auth_dbname says otherwise.
func pgEnsurePoolerAuth(ctx context.Context, conn *pgconn.PgConn) error {
	return execAll(ctx, conn, tunnel.PoolerAuthSetupSQL()...)
}

// pgEnsureLevels creates the level roles up to level and grants each what it
// may do in every schema of the database it can see.
func pgEnsureLevels(ctx context.Context, conn *pgconn.PgConn, database, level string) error {
	names := map[string]string{}
	for _, l := range levelsUpTo(level) {
		names[l] = levelRole(l, database)
	}
	var b strings.Builder
	b.WriteString("DO $argus$\nDECLARE s text;\nBEGIN\n")
	ensure := func(role, attrs string) {
		fmt.Fprintf(&b, "IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN "+
			"CREATE ROLE %s NOLOGIN %s; COMMENT ON ROLE %s IS %s; END IF;\n",
			quoteLiteral(role), pgx(role), attrs, pgx(role), quoteLiteral(roleMarker+" level"))
	}
	ensure(peopleRole, "")
	for _, l := range levelsUpTo(level) {
		attrs := ""
		if l == tunnel.LevelDBA {
			attrs = "CREATEROLE"
		}
		ensure(names[l], attrs)
	}
	fmt.Fprintf(&b, "EXECUTE format('GRANT CONNECT ON DATABASE %%I TO %s', current_database());\n",
		pgx(names[tunnel.LevelRead]))
	if w, ok := names[tunnel.LevelWrite]; ok {
		fmt.Fprintf(&b, "GRANT %s TO %s;\n", pgx(names[tunnel.LevelRead]), pgx(w))
	}
	if o, ok := names[tunnel.LevelOwner]; ok {
		fmt.Fprintf(&b, "GRANT %s TO %s;\n", pgx(names[tunnel.LevelWrite]), pgx(o))
	}
	if d, ok := names[tunnel.LevelDBA]; ok {
		// A DBA holds every level with ADMIN OPTION, so they can give an
		// application's own role a level, or take it back, from the console.
		// Nothing below DBA can change who holds what.
		fmt.Fprintf(&b, "GRANT %s, %s, %s TO %s WITH ADMIN OPTION;\n"+
			"GRANT pg_signal_backend, pg_read_all_stats TO %s;\n",
			pgx(names[tunnel.LevelRead]), pgx(names[tunnel.LevelWrite]), pgx(names[tunnel.LevelOwner]), pgx(d), pgx(d))
	}
	// The trash is no one's to read or change: it is reached through its
	// functions only (see postgres_trash.go).
	b.WriteString("FOR s IN SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\\_%' " +
		"AND nspname <> 'information_schema' AND nspname NOT LIKE 'argus\\_trash%' LOOP\n")
	r := pgx(names[tunnel.LevelRead])
	fmt.Fprintf(&b, "EXECUTE format('GRANT USAGE ON SCHEMA %%I TO %s', s);\n", r)
	fmt.Fprintf(&b, "EXECUTE format('GRANT SELECT ON ALL TABLES IN SCHEMA %%I TO %s', s);\n", r)
	fmt.Fprintf(&b, "EXECUTE format('GRANT SELECT ON ALL SEQUENCES IN SCHEMA %%I TO %s', s);\n", r)
	if w, ok := names[tunnel.LevelWrite]; ok {
		fmt.Fprintf(&b, "EXECUTE format('GRANT INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %%I TO %s', s);\n", pgx(w))
		fmt.Fprintf(&b, "EXECUTE format('GRANT USAGE, UPDATE ON ALL SEQUENCES IN SCHEMA %%I TO %s', s);\n", pgx(w))
	}
	if o, ok := names[tunnel.LevelOwner]; ok {
		fmt.Fprintf(&b, "EXECUTE format('GRANT CREATE ON SCHEMA %%I TO %s', s);\n", pgx(o))
	}
	b.WriteString("END LOOP;\nEND\n$argus$")
	return execAll(ctx, conn, b.String())
}

// revokeIfExists takes group away from member when group exists; a level
// nobody has used in this database has no role yet.
func revokeIfExists(group, member string) string {
	// Only a membership that exists is revoked: revoking one that does not
	// is a WARNING in the server log on every session, which is noise an
	// operator reading that log for real problems should not have to filter.
	return fmt.Sprintf("DO $r$ BEGIN IF EXISTS (SELECT 1 FROM pg_auth_members m "+
		"JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member "+
		"WHERE g.rolname = %s AND u.rolname = %s) THEN "+
		"REVOKE %s FROM %s; END IF; END $r$", quoteLiteral(group), quoteLiteral(member), pgx(group), pgx(member))
}

func execAll(ctx context.Context, conn *pgconn.PgConn, stmts ...string) error {
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s).ReadAll(); err != nil {
			return fmt.Errorf("%s: %w", firstWords(s), err)
		}
	}
	return nil
}

func firstWords(s string) string {
	f := strings.Fields(s)
	if len(f) > 3 {
		f = f[:3]
	}
	return strings.Join(f, " ")
}

// pgx quotes an identifier Argus generated. They are all plain already;
// quoting is so a mistake upstream is an error, never SQL.
func pgx(name string) string {
	if !safeIdent.MatchString(name) {
		// Unreachable for names built here; refuse loudly rather than quote.
		panic("agent: unsafe identifier " + name)
	}
	return `"` + name + `"`
}

// quoteLiteral quotes a string literal for Postgres.
//
// As an escape string (E'...') with both quotes and backslashes doubled, so
// the result means the same thing whatever standard_conforming_strings is
// set to. A plain '...' with only quotes doubled is safe only while that
// setting is on; on a server with it off, a backslash in the owner's address
// would escape the closing quote and the rest of the value would run as SQL
// under the superuser this helper signs in as.
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "E'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// scramVerifier computes Postgres's SCRAM-SHA-256 verifier for password, so
// only the verifier is ever sent to the server and logged by it.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const iterations = 4096
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	server := mac(salted, "Server Key")
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc(salt), enc(stored[:]), enc(server)), nil
}

// sessionPassword is 32 random bytes, printable.
func sessionPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// errPGAdmin wraps a provisioning failure for the person.
var errPGAdmin = errors.New("could not prepare your database role")

/*
Replacing a database from a backup, in three fixed steps the daemon drives:

	prepare-replace   end every connection to Target, then rename it to
	                  RenameTo. The restore then creates Target afresh.
	rollback-replace  the restore failed: drop the half-made Target (if any)
	                  and rename RenameTo back. The person has lost nothing.
	drop-database     the kept copy's day is up: drop RenameTo.

The database is renamed, never dropped, until the control plane says the
restore succeeded and the keep window has passed. Every name is checked
against tunnel.ValidDBName and quoted anyway; Argus only renames to names
it made, and only drops a database whose name carries its own marker.
*/

// replaceMarker is in every name Argus renames a database to, so
// drop-database can refuse anything else.
const replaceMarker = tunnel.KeptMarker

func pgReplaceOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	version := conn.ParameterStatus("server_version")
	if req.Op == "list-databases" {
		res := conn.ExecParams(ctx, `SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1`,
			nil, nil, nil, nil).Read()
		if res.Err != nil {
			return PGAdminResult{Error: res.Err.Error(), Version: version}
		}
		out := PGAdminResult{OK: true, Version: version}
		for _, r := range res.Rows {
			out.Databases = append(out.Databases, string(r[0]))
		}
		return out
	}
	if !tunnel.ValidDBName.MatchString(req.Target) {
		return PGAdminResult{Error: "not a database name Argus will act on: " + req.Target}
	}
	if req.Op != "drop-database" && (!tunnel.ValidDBName.MatchString(req.RenameTo) || req.RenameTo == req.Target) {
		return PGAdminResult{Error: "not a name Argus will rename to: " + req.RenameTo}
	}
	exists := func(name string) (bool, error) {
		res := conn.ExecParams(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`,
			[][]byte{[]byte(name)}, nil, nil, nil).Read()
		return len(res.Rows) > 0, res.Err
	}
	terminate := func(name string) error {
		// Nobody may reconnect while the restore runs: the level roles lose
		// CONNECT until the next session re-grants it, and every backend goes.
		res := conn.ExecParams(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid()`, [][]byte{[]byte(name)}, nil, nil, nil).Read()
		return res.Err
	}
	switch req.Op {
	case "prepare-replace":
		if ok, err := exists(req.Target); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		} else if !ok {
			return PGAdminResult{Error: "there is no database named " + req.Target + " to replace", Version: version}
		}
		if ok, _ := exists(req.RenameTo); ok {
			return PGAdminResult{Error: "a database named " + req.RenameTo + " already exists", Version: version}
		}
		if err := execAll(ctx, conn, "ALTER DATABASE "+pgx(req.Target)+" WITH ALLOW_CONNECTIONS false"); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		if err := terminate(req.Target); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		if err := execAll(ctx, conn, "ALTER DATABASE "+pgx(req.Target)+" RENAME TO "+pgx(req.RenameTo)); err != nil {
			_ = execAll(ctx, conn, "ALTER DATABASE "+pgx(req.Target)+" WITH ALLOW_CONNECTIONS true")
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version}
	case "rollback-replace":
		if ok, _ := exists(req.Target); ok {
			_ = terminate(req.Target)
			if err := execAll(ctx, conn, "DROP DATABASE "+pgx(req.Target)); err != nil {
				return PGAdminResult{Error: "the half-restored " + req.Target + " could not be dropped: " + err.Error(), Version: version}
			}
		}
		if ok, err := exists(req.RenameTo); err != nil || !ok {
			return PGAdminResult{Error: "the kept copy " + req.RenameTo + " is not there to put back", Version: version}
		}
		if err := execAll(ctx, conn, "ALTER DATABASE "+pgx(req.RenameTo)+" RENAME TO "+pgx(req.Target),
			"ALTER DATABASE "+pgx(req.Target)+" WITH ALLOW_CONNECTIONS true"); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version}
	case "drop-database":
		if !strings.Contains(req.Target, replaceMarker) {
			return PGAdminResult{Error: "Argus only drops the copies it kept from a restore; " + req.Target + " is not one"}
		}
		if ok, _ := exists(req.Target); !ok {
			return PGAdminResult{OK: true, Version: version}
		}
		_ = terminate(req.Target)
		if err := execAll(ctx, conn, "DROP DATABASE "+pgx(req.Target)); err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		return PGAdminResult{OK: true, Version: version}
	}
	return PGAdminResult{Error: "unknown operation " + req.Op}
}

// pgDiscover lists the databases on the server for the add form: every one
// a session could connect to, with its owner and size. Templates and
// databases that refuse connections are left out; so is nothing else, since
// an admin may well want the maintenance database. It reads the catalog and
// changes nothing.
func pgDiscover(ctx context.Context, conn *pgconn.PgConn) PGAdminResult {
	version := conn.ParameterStatus("server_version")
	res := conn.ExecParams(ctx, `SELECT datname, pg_get_userbyid(datdba),
			CASE WHEN has_database_privilege(oid, 'CONNECT') THEN pg_database_size(oid) ELSE 0 END
		FROM pg_database WHERE NOT datistemplate AND datallowconn
		  AND datname NOT LIKE 'argus\_test\_%' ORDER BY 1 LIMIT $1`,
		[][]byte{[]byte(strconv.Itoa(tunnel.MaxDiscovered + 1))}, nil, nil, nil).Read()
	if res.Err != nil {
		return PGAdminResult{Error: res.Err.Error(), Version: version}
	}
	out := PGAdminResult{OK: true, Version: version}
	for i, r := range res.Rows {
		if i == tunnel.MaxDiscovered {
			out.Truncated = true
			break
		}
		n, _ := strconv.ParseInt(string(r[2]), 10, 64)
		out.Found = append(out.Found, tunnel.Discovered{Name: string(r[0]), Owner: string(r[1]), Bytes: n})
	}
	return out
}
