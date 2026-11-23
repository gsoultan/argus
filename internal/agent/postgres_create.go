package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Creating a Postgres database, in the pg-admin helper as the postgres account.
See tunnel/dbcreate.go for the service and agent/dbcreate.go for the order.

	create-options        on the maintenance database: version, installed
	                      extensions, roles that may own, the default
	                      encoding and locale, the names taken
	create-database       on the maintenance database: refuse a taken name,
	                      make the owner (Argus's Owner-level role, or a new
	                      application role from the agent's SCRAM verifier),
	                      then CREATE DATABASE as tunnel.CreateDBPlan built it;
	                      a role made here is dropped again if that fails
	prepare-new-database  in the new database: the level roles and their
	                      grants, then each extension
	set-owner             on the maintenance database: make a new owner role
	                      if asked, then ALTER DATABASE … OWNER TO
	set-password          on the maintenance database: ALTER ROLE … PASSWORD,
	                      only for the role that owns the database

Every statement is tunnel.CreatePlan's or tunnel.SetOwnerPlan's text, from
fields tunnel.GrantCreate.Validate checked, so what the console previewed is
what ran.
*/

func pgCreateOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	c := req.Create
	if c == nil {
		return PGAdminResult{Error: "nothing to create"}
	}
	cc := *c
	if err := cc.Validate(); err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	version := conn.ParameterStatus("server_version")
	switch req.Op {
	case "create-options":
		o, err := pgCreateOptions(ctx, conn)
		if err != nil {
			return PGAdminResult{Error: err.Error(), Version: version}
		}
		o.Version = version
		return PGAdminResult{OK: true, Version: version, Options: o}
	case "create-database":
		return pgCreateDatabase(ctx, conn, cc, req, version)
	case "prepare-new-database":
		return pgPrepareNew(ctx, conn, cc, req, version)
	case "set-owner":
		return pgSetOwner(ctx, conn, cc, req, version)
	case "set-password":
		return pgSetPassword(ctx, conn, cc, req, version)
	}
	return PGAdminResult{Error: "unknown operation " + req.Op}
}

func pgDatabaseExists(ctx context.Context, conn *pgconn.PgConn, name string) (bool, error) {
	r := conn.ExecParams(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, [][]byte{[]byte(name)}, nil, nil, nil).Read()
	return len(r.Rows) > 0, r.Err
}

func pgRoleExists(ctx context.Context, conn *pgconn.PgConn, name string) (bool, error) {
	r := conn.ExecParams(ctx, `SELECT 1 FROM pg_roles WHERE rolname = $1`, [][]byte{[]byte(name)}, nil, nil, nil).Read()
	return len(r.Rows) > 0, r.Err
}

// pgEnsureOwner makes sure the role that is to own the database exists:
// Argus's Owner-level role is made, NOLOGIN, with Argus's mark; a new
// application role is made with the verifier the agent computed; a role the
// admin named must already be there. made says this call created a role
// that must be dropped again if what follows fails.
func pgEnsureOwner(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, req PGAdminRequest) (step tunnel.CreateStep, made bool, err error) {
	role := c.PGOwnerRole()
	ok, err := pgRoleExists(ctx, conn, role)
	if err != nil {
		return tunnel.CreateStep{}, false, err
	}
	switch {
	case c.NewOwner():
		if ok {
			return tunnel.CreateStep{}, false, fmt.Errorf("a role named %s is already on this server; pick it under an existing role, or another name", role)
		}
		if !strings.HasPrefix(req.Verifier, "SCRAM-SHA-256$") {
			return tunnel.CreateStep{}, false, errors.New("the new role's password did not reach the helper")
		}
		pre, _ := tunnel.OwnerPlan(c, quoteLiteral(req.Verifier))
		if err := execAll(ctx, conn, pre...); err != nil {
			return tunnel.CreateStep{}, false, err
		}
		step = tunnel.CreateStep{Text: "Made the role " + role + ", which can sign in", OK: true}
		made = true
	case c.Postgres.Owner != tunnel.PGOwnerArgus:
		if !ok {
			return tunnel.CreateStep{}, false, errNoRole(role)
		}
		step = tunnel.CreateStep{Text: "The owner " + role + " is a role on this server", OK: true}
	case ok:
		return tunnel.CreateStep{Text: "Argus's Owner-level role " + role + " is there", OK: true}, false, nil
	default:
		if err := execAll(ctx, conn,
			"CREATE ROLE "+pgx(role)+" NOLOGIN",
			"COMMENT ON ROLE "+pgx(role)+" IS "+quoteLiteral(roleMarker+" level")); err != nil {
			return tunnel.CreateStep{}, false, err
		}
		return tunnel.CreateStep{Text: "Made Argus's Owner-level role " + role, OK: true}, false, nil
	}
	if req.Pooled {
		// Argus's PgBouncer lookup answers for the app's role too, so it
		// signs in through PgBouncer like the people's roles do.
		if err := pgJoinPooledApps(ctx, conn, role); err != nil {
			return step, made, err
		}
	}
	return step, made, nil
}

// pgJoinPooledApps makes role a member of tunnel.PoolerAppsRole, for
// Argus's PgBouncer lookup. A superuser is never added: its password must
// not reach PgBouncer's auth_user.
func pgJoinPooledApps(ctx context.Context, conn *pgconn.PgConn, role string) error {
	r := conn.ExecParams(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = $1`, [][]byte{[]byte(role)}, nil, nil, nil).Read()
	if r.Err != nil {
		return r.Err
	}
	if len(r.Rows) == 1 && string(r.Rows[0][0]) == "t" {
		return nil
	}
	return execAll(ctx, conn, append(tunnel.PoolerAppsSetupSQL(), "GRANT "+pgx(tunnel.PoolerAppsRole)+" TO "+pgx(role))...)
}

type errNoRole string

func (e errNoRole) Error() string { return "there is no role named " + string(e) + " on this server" }

func pgCreateDatabase(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, req PGAdminRequest, version string) PGAdminResult {
	plan := tunnel.CreateDBPlan(c)
	out := PGAdminResult{Version: version}
	taken, err := pgDatabaseExists(ctx, conn, c.Name)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if taken {
		out.Exists = true
		out.Error = "this server already has a database named " + c.Name
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "The name " + c.Name + " is free", OK: true})
	step, made, err := pgEnsureOwner(ctx, conn, c, req)
	if err != nil {
		out.Error = pgErrText(err)
		if made {
			out.Steps = append(out.Steps, pgUndoRole(ctx, conn, c.PGOwnerRole()))
		}
		return out
	}
	out.Steps = append(out.Steps, step)
	// CREATE DATABASE cannot run in a transaction block: one statement,
	// sent alone.
	if _, err := conn.Exec(ctx, plan[0]).ReadAll(); err != nil {
		out.Error = pgErrText(err)
		if pgCode(err) == "42P04" {
			out.Exists = true
		}
		if made {
			out.Steps = append(out.Steps, pgUndoRole(ctx, conn, c.PGOwnerRole()))
		}
		return out
	}
	what := "Created the database " + c.Name
	if c.OwnerName() != "" {
		what += ", owned by " + c.OwnerName()
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: what, OK: true})
	out.SQL = plan[0]
	out.OK = true
	return out
}

// pgUndoRole drops a role this create made a moment ago, when the database
// it was for could not be made: nothing else can own anything of it yet.
func pgUndoRole(ctx context.Context, conn *pgconn.PgConn, role string) tunnel.CreateStep {
	if err := execAll(ctx, conn, "DROP ROLE IF EXISTS "+pgx(role)); err != nil {
		return tunnel.CreateStep{Text: "Remove the role " + role + " again", Error: pgErrText(err)}
	}
	return tunnel.CreateStep{Text: "Removed the role " + role + " again, since the database was not made", OK: true}
}

func pgPrepareNew(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, req PGAdminRequest, version string) PGAdminResult {
	out := PGAdminResult{Version: version, OK: true}
	if err := pgEnsureLevels(ctx, conn, c.Name, tunnel.LevelOwner); err != nil {
		out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up Argus's level roles", Error: err.Error()})
		out.OK, out.Error = false, err.Error()
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up Argus's Read, Write and Owner roles in it", OK: true})
	if req.Pooled {
		// PgBouncer runs its auth_query in the database being opened.
		if err := pgEnsurePoolerAuth(ctx, conn); err != nil {
			out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up PgBouncer's password lookup in it", Error: pgErrText(err)})
			out.OK, out.Error = false, pgErrText(err)
			return out
		}
		out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up PgBouncer's password lookup in it", OK: true})
	}
	plan := tunnel.CreateDBPlan(c)
	for i, e := range c.Postgres.Extensions {
		r := conn.ExecParams(ctx, `SELECT 1 FROM pg_available_extensions WHERE name = $1`, [][]byte{[]byte(e)}, nil, nil, nil).Read()
		if r.Err == nil && len(r.Rows) == 0 {
			out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Enable " + e, Error: "no extension named " + e + " is installed on this server"})
			out.OK = false
			continue
		}
		if _, err := conn.Exec(ctx, plan[1+i]).ReadAll(); err != nil {
			out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Enable " + e, Error: pgErrText(err)})
			out.OK = false
			continue
		}
		out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Enabled " + e, OK: true})
	}
	if !out.OK {
		out.Error = "the database was made, but not every step after it worked"
	}
	return out
}

func pgSetOwner(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, req PGAdminRequest, version string) PGAdminResult {
	out := PGAdminResult{Version: version}
	there, err := pgDatabaseExists(ctx, conn, c.Name)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if !there {
		out.Error = "this server has no database named " + c.Name
		return out
	}
	step, made, err := pgEnsureOwner(ctx, conn, c, req)
	if err != nil {
		out.Error = pgErrText(err)
		if made {
			out.Steps = append(out.Steps, pgUndoRole(ctx, conn, c.PGOwnerRole()))
		}
		return out
	}
	out.Steps = append(out.Steps, step)
	alter := tunnel.PGAlterOwnerSQL(c)
	if _, err := conn.Exec(ctx, alter).ReadAll(); err != nil {
		out.Error = pgErrText(err)
		if made {
			out.Steps = append(out.Steps, pgUndoRole(ctx, conn, c.PGOwnerRole()))
		}
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Gave " + c.Name + " to " + c.PGOwnerRole(), OK: true})
	out.SQL, out.OK = alter, true
	return out
}

// pgSetPassword gives the role that owns the database a new password. Only
// that role, never a superuser and never one of Argus's own: a new password
// here is for the app that owns the database.
func pgSetPassword(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, req PGAdminRequest, version string) PGAdminResult {
	out := PGAdminResult{Version: version}
	role := c.OwnerName()
	r := conn.ExecParams(ctx, `SELECT r.rolsuper, coalesce(shobj_description(r.oid, 'pg_authid'), ''), d.datdba = r.oid
		FROM pg_roles r, pg_database d WHERE r.rolname = $1 AND d.datname = $2`,
		[][]byte{[]byte(role), []byte(c.Name)}, nil, nil, nil).Read()
	switch {
	case r.Err != nil:
		out.Error = pgErrText(r.Err)
		return out
	case len(r.Rows) != 1:
		out.Error = "there is no role " + role + " or no database " + c.Name + " on this server"
		return out
	case string(r.Rows[0][0]) == "t":
		out.Error = role + " is a superuser; change its password from a session, where it waits for approval"
		return out
	case strings.HasPrefix(string(r.Rows[0][1]), roleMarker):
		out.Error = role + " is Argus's own role"
		return out
	case string(r.Rows[0][2]) != "t":
		out.Error = role + " does not own " + c.Name + "; only the owner's password is set here"
		return out
	}
	if !strings.HasPrefix(req.Verifier, "SCRAM-SHA-256$") {
		out.Error = "the new password did not reach the helper"
		return out
	}
	if err := execAll(ctx, conn, tunnel.SetPasswordPlanWith(c, quoteLiteral(req.Verifier))...); err != nil {
		out.Error = pgErrText(err)
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Gave " + role + " a new password", OK: true})
	out.OK = true
	return out
}

func pgCreateOptions(ctx context.Context, conn *pgconn.PgConn) (*tunnel.CreateOptions, error) {
	o := &tunnel.CreateOptions{}
	r := conn.ExecParams(ctx, `SELECT name FROM pg_available_extensions ORDER BY name LIMIT 500`, nil, nil, nil, nil).Read()
	if r.Err != nil {
		return nil, r.Err
	}
	for _, row := range r.Rows {
		o.Extensions = append(o.Extensions, string(row[0]))
	}
	// Roles that may own: not Postgres's own, not Argus's level or personal
	// roles, and none that can sign in as a superuser would be a surprise.
	r = conn.ExecParams(ctx, `SELECT rolname FROM pg_roles
		WHERE rolname NOT LIKE 'pg\_%' AND rolname NOT LIKE 'argus\_%'
		  AND coalesce(shobj_description(oid, 'pg_authid'), '') NOT LIKE 'argus-managed%'
		ORDER BY rolsuper, rolname LIMIT 500`, nil, nil, nil, nil).Read()
	if r.Err != nil {
		return nil, r.Err
	}
	for _, row := range r.Rows {
		o.Roles = append(o.Roles, string(row[0]))
	}
	// The same roles as owners to pick from, with what each owns today.
	r = conn.ExecParams(ctx, `SELECT r.rolname, r.rolsuper, r.rolcanlogin,
		  (SELECT string_agg(d.datname, ', ' ORDER BY d.datname) FROM pg_database d WHERE d.datdba = r.oid AND NOT d.datistemplate)
		FROM pg_roles r
		WHERE r.rolname NOT LIKE 'pg\_%' AND r.rolname NOT LIKE 'argus\_%'
		  AND coalesce(shobj_description(r.oid, 'pg_authid'), '') NOT LIKE 'argus-managed%'
		ORDER BY r.rolsuper, r.rolname LIMIT 500`, nil, nil, nil, nil).Read()
	if r.Err != nil {
		return nil, r.Err
	}
	for _, row := range r.Rows {
		u := tunnel.OwnerUser{Name: string(row[0]), All: string(row[1]) == "t"}
		switch {
		case u.All:
			u.Reach = "superuser: every database"
		case row[3] != nil:
			u.Reach = "owns " + string(row[3])
		case string(row[2]) != "t":
			u.Reach = "cannot sign in"
		}
		o.Users = append(o.Users, u)
	}
	r = conn.ExecParams(ctx, `SELECT pg_encoding_to_char(encoding), datcollate FROM pg_database WHERE datname = 'template1'`,
		nil, nil, nil, nil).Read()
	if r.Err == nil && len(r.Rows) == 1 {
		o.Encoding, o.Locale = string(r.Rows[0][0]), string(r.Rows[0][1])
	}
	r = conn.ExecParams(ctx, `SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1 LIMIT $1`,
		[][]byte{[]byte(strconv.Itoa(tunnel.MaxDiscovered + 1))}, nil, nil, nil).Read()
	if r.Err != nil {
		return nil, r.Err
	}
	for i, row := range r.Rows {
		if i == tunnel.MaxDiscovered {
			o.Truncated = true
			break
		}
		o.Taken = append(o.Taken, string(row[0]))
	}
	return o, nil
}

func pgCode(err error) string {
	if pe, ok := err.(*pgconn.PgError); ok {
		return pe.Code
	}
	return ""
}

func pgErrText(err error) string {
	if pe, ok := err.(*pgconn.PgError); ok {
		return pe.Message
	}
	return err.Error()
}
