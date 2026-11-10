package agent

import (
	"context"
	"strconv"

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
	                      make Argus's Owner-level role when it is to own the
	                      database, then CREATE DATABASE as tunnel.CreatePlan
	                      built it
	prepare-new-database  in the new database: the level roles and their
	                      grants, then each extension
	set-owner             on the maintenance database: ALTER DATABASE … OWNER TO

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
		return pgCreateDatabase(ctx, conn, cc, version)
	case "prepare-new-database":
		return pgPrepareNew(ctx, conn, cc, version)
	case "set-owner":
		return pgSetOwner(ctx, conn, cc, version)
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
// Argus's Owner-level role is made, NOLOGIN, with Argus's mark; a role
// the admin named must already be there.
func pgEnsureOwner(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate) (tunnel.CreateStep, error) {
	role := c.PGOwnerRole()
	ok, err := pgRoleExists(ctx, conn, role)
	if err != nil {
		return tunnel.CreateStep{}, err
	}
	if c.Postgres.Owner != tunnel.PGOwnerArgus {
		if !ok {
			return tunnel.CreateStep{}, errNoRole(role)
		}
		return tunnel.CreateStep{Text: "The owner " + role + " is a role on this server", OK: true}, nil
	}
	if ok {
		return tunnel.CreateStep{Text: "Argus's Owner-level role " + role + " is there", OK: true}, nil
	}
	if err := execAll(ctx, conn,
		"CREATE ROLE "+pgx(role)+" NOLOGIN",
		"COMMENT ON ROLE "+pgx(role)+" IS "+quoteLiteral(roleMarker+" level")); err != nil {
		return tunnel.CreateStep{}, err
	}
	return tunnel.CreateStep{Text: "Made Argus's Owner-level role " + role, OK: true}, nil
}

type errNoRole string

func (e errNoRole) Error() string { return "there is no role named " + string(e) + " on this server" }

func pgCreateDatabase(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, version string) PGAdminResult {
	plan := tunnel.CreatePlan(c)
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
	step, err := pgEnsureOwner(ctx, conn, c)
	if err != nil {
		out.Error = err.Error()
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
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Created the database " + c.Name, OK: true})
	out.SQL = plan[0]
	out.OK = true
	return out
}

func pgPrepareNew(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, version string) PGAdminResult {
	out := PGAdminResult{Version: version, OK: true}
	if err := pgEnsureLevels(ctx, conn, c.Name, tunnel.LevelOwner); err != nil {
		out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up Argus's level roles", Error: err.Error()})
		out.OK, out.Error = false, err.Error()
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Set up Argus's Read, Write and Owner roles in it", OK: true})
	plan := tunnel.CreatePlan(c)
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

func pgSetOwner(ctx context.Context, conn *pgconn.PgConn, c tunnel.GrantCreate, version string) PGAdminResult {
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
	step, err := pgEnsureOwner(ctx, conn, c)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Steps = append(out.Steps, step)
	plan := tunnel.SetOwnerPlan(c)
	if _, err := conn.Exec(ctx, plan[0]).ReadAll(); err != nil {
		out.Error = pgErrText(err)
		return out
	}
	out.Steps = append(out.Steps, tunnel.CreateStep{Text: "Gave " + c.Name + " to " + c.PGOwnerRole(), OK: true})
	out.SQL, out.OK = plan[0], true
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
