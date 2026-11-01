package agent

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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
	Op string `json:"op"` // open, close
	// Database to connect to and grant in.
	Database string `json:"database"`
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
}

// PGAdminResult is the answer.
type PGAdminResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Taken says the role exists and is not Argus's.
	Taken   bool   `json:"taken,omitempty"`
	Version string `json:"version,omitempty"`
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
	if err := pgEnsureLevels(ctx, conn, req.Database, req.Level); err != nil {
		return PGAdminResult{Error: err.Error(), Version: version}
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
		fmt.Fprintf(&b, "GRANT %s TO %s;\nGRANT pg_signal_backend, pg_read_all_stats TO %s;\n",
			pgx(names[tunnel.LevelOwner]), pgx(d), pgx(d))
	}
	b.WriteString("FOR s IN SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\\_%' " +
		"AND nspname <> 'information_schema' LOOP\n")
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

// quoteLiteral quotes a string literal for Postgres with standard strings.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
