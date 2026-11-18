package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Roles and privileges through the helper (see internal/tunnel/pgroles.go for
why these are not statements typed into the editor).

The helper runs as the server's postgres account, so a grant here is made as
if by each object's owner and takes effect, where the same GRANT run as the
DBA level role on someone else's table succeeds with only a warning. Every
change runs inside one transaction: a grant on a schema's tables plus the
default privileges for tables made later either all hold or none does.

Argus's own roles stay Argus's: a person's role and a level role are changed
from the database's settings, never here, so the helper refuses to create,
change, drop, grant to or take from any role whose comment carries Argus's
mark -- except that a level role may still be given to an application role,
which is how the Roles panel has always handed out a level.
*/

// PGRole is one row of role.list.
type PGRole struct {
	Name        string `json:"name"`
	Superuser   bool   `json:"superuser"`
	Login       bool   `json:"login"`
	Inherit     bool   `json:"inherit"`
	CreateRole  bool   `json:"createrole"`
	CreateDB    bool   `json:"createdb"`
	Replication bool   `json:"replication"`
	BypassRLS   bool   `json:"bypassrls"`
	ConnLimit   int    `json:"connLimit"`
	ValidUntil  string `json:"validUntil,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	Comment     string `json:"comment,omitempty"`
	// MemberOf are the roles it is in; AdminOf those it may also give.
	MemberOf []string `json:"memberOf,omitempty"`
	AdminOf  []string `json:"adminOf,omitempty"`
	// Connections is how many sessions it has open to this database.
	Connections int `json:"connections"`
	// Owns is how many tables, sequences and functions it owns here.
	Owns int `json:"owns"`
	// Helper marks the account Argus's helper signs in as.
	Helper bool `json:"helper,omitempty"`
}

// PGPrivRow is what a role holds on one schema, table, sequence or function.
type PGPrivRow struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	// Kind is table, view, matview, partitioned, foreign, sequence or
	// function; empty for a schema.
	Kind  string `json:"kind,omitempty"`
	Owner string `json:"owner"`
	// Effective is what the role may do, however it came to (directly,
	// through a role it is in, through PUBLIC, by owning it). Direct is
	// what was granted to the role itself; Grantable the part of that it
	// may pass on.
	Effective []string `json:"effective"`
	Direct    []string `json:"direct,omitempty"`
	Grantable []string `json:"grantable,omitempty"`
	// Objects is how many tables a schema holds, for a schema.
	Objects int `json:"objects,omitempty"`
}

// PGDefaultPriv is a default privilege that names the role: what it gets on
// objects ForRole makes later, in Schema (everywhere when empty).
type PGDefaultPriv struct {
	ForRole    string   `json:"forRole"`
	Schema     string   `json:"schema,omitempty"`
	Objects    string   `json:"objects"`
	Privileges []string `json:"privileges"`
}

// PGRolePrivileges answers role.privileges.
type PGRolePrivileges struct {
	Role      string          `json:"role"`
	Superuser bool            `json:"superuser"`
	Database  PGPrivRow       `json:"database"`
	Schemas   []PGPrivRow     `json:"schemas"`
	Tables    []PGPrivRow     `json:"tables"`
	Sequences []PGPrivRow     `json:"sequences"`
	Functions []PGPrivRow     `json:"functions"`
	Defaults  []PGDefaultPriv `json:"defaults"`
	// Truncated says there were more tables than were listed.
	Truncated bool `json:"truncated,omitempty"`
}

// PGGrantObject is one thing a grant can name.
type PGGrantObject struct {
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Owner string `json:"owner"`
}

// PGGrantObjects answers grant.objects.
type PGGrantObjects struct {
	Schemas   []PGGrantObject `json:"schemas"`
	Tables    []PGGrantObject `json:"tables,omitempty"`
	Sequences []PGGrantObject `json:"sequences,omitempty"`
	Functions []PGGrantObject `json:"functions,omitempty"`
	// Owners are the roles that own the schema or anything in it: whose
	// objects made later a default privilege should cover.
	Owners    []string `json:"owners,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// maxListed bounds each list the helper returns.
const maxListed = 5000

// passwordMarker stands in for a role's password once the agent has turned
// it into a verifier: the helper only ever sees the verifier.
const passwordMarker = "[verifier]"

// rolePassword turns a role operation's password into a SCRAM verifier, in
// the agent, and returns the operation with the password replaced by a
// marker. Nothing after this point holds the password.
func rolePassword(op *tunnel.PGAdminOp) (*tunnel.PGAdminOp, string, error) {
	if op.RoleSpec == nil || op.RoleSpec.Password == "" || op.Preview {
		return op, "", nil
	}
	if len(op.RoleSpec.Password) > 1024 {
		return nil, "", errors.New("a password is at most 1,024 characters")
	}
	v, err := scramVerifier(op.RoleSpec.Password)
	if err != nil {
		return nil, "", err
	}
	c := *op
	r := *op.RoleSpec
	r.Password = passwordMarker
	c.RoleSpec = &r
	return &c, v, nil
}

// pgRoleOp runs a role or privilege operation on conn, a superuser's.
func pgRoleOp(ctx context.Context, conn *pgconn.PgConn, req PGAdminRequest) PGAdminResult {
	op := req.Admin
	if op.RoleSpec != nil && op.RoleSpec.Password != "" && !op.Preview {
		if op.RoleSpec.Password != passwordMarker || !scramPattern.MatchString(req.Verifier) {
			return PGAdminResult{Error: "a role's password reaches the helper only as a SCRAM verifier"}
		}
	}
	run, text, err := tunnel.PGRoleSQL(op, req.Database, req.Verifier)
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	joined := strings.Join(text, ";\n")
	if op.Preview {
		return PGAdminResult{OK: true, SQL: joined}
	}
	var data any
	switch op.Kind {
	case tunnel.AdminRoleList:
		data, err = listRoles(ctx, conn)
	case tunnel.AdminRolePrivileges:
		if op.RoleSpec == nil || op.RoleSpec.Name == "" {
			return PGAdminResult{Error: "which role?"}
		}
		data, err = rolePrivileges(ctx, conn, op.RoleSpec.Name)
	case tunnel.AdminGrantObjects:
		schema := ""
		if op.GrantSpec != nil {
			schema = op.GrantSpec.Schema
		}
		data, err = grantObjects(ctx, conn, schema)
	default:
		if msg, err := roleRefusal(ctx, conn, op); err != nil {
			return PGAdminResult{Error: err.Error(), SQL: joined}
		} else if msg != "" {
			return PGAdminResult{Error: msg, SQL: joined}
		}
		built := adminSQL{run: run, redacted: text}
		if _, err := conn.Exec(ctx, "BEGIN").ReadAll(); err != nil {
			return pgAdminFailed(err, built)
		}
		tag := ""
		for _, s := range run {
			t, err := conn.Exec(ctx, s).ReadAll()
			if err != nil {
				_, _ = conn.Exec(context.WithoutCancel(ctx), "ROLLBACK").ReadAll()
				return pgAdminFailed(err, built)
			}
			if len(t) > 0 && !strings.HasPrefix(s, "COMMENT") {
				tag = t[len(t)-1].CommandTag.String()
			}
		}
		if _, err := conn.Exec(ctx, "COMMIT").ReadAll(); err != nil {
			return pgAdminFailed(err, built)
		}
		data = PGAdminDone{SQL: joined, Tag: tag}
	}
	if err != nil {
		return pgAdminFailed(err, adminSQL{})
	}
	b, err := json.Marshal(data)
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	return PGAdminResult{OK: true, SQL: joined, Data: b}
}

// roleRefusal says why op may not run, with the database in hand: a role
// Argus manages, or taking the helper's own account away from it.
func roleRefusal(ctx context.Context, conn *pgconn.PgConn, op *tunnel.PGAdminOp) (string, error) {
	// Each role the operation changes, and whether a level role may stand
	// there (a level is given to an application role through membership).
	type named struct {
		name       string
		levelOK    bool
		changesOwn bool // drops, renames or alters the role itself
	}
	var names []named
	switch op.Kind {
	case tunnel.AdminRoleCreate, tunnel.AdminRoleAlter, tunnel.AdminRoleDrop:
		if op.RoleSpec == nil {
			return "which role?", nil
		}
		names = append(names, named{name: op.RoleSpec.Name, changesOwn: true})
		if op.RoleSpec.Rename != "" {
			names = append(names, named{name: op.RoleSpec.Rename})
		}
		for _, g := range op.RoleSpec.MemberOf {
			names = append(names, named{name: g, levelOK: true})
		}
		if op.RoleSpec.ReassignTo != "" {
			names = append(names, named{name: op.RoleSpec.ReassignTo})
		}
	case tunnel.AdminRoleMember, tunnel.AdminRoleUnmember:
		if op.Member == nil {
			return "which membership?", nil
		}
		names = append(names, named{name: op.Member.Role, levelOK: true}, named{name: op.Member.Member})
	case tunnel.AdminGrant, tunnel.AdminRevoke:
		if op.GrantSpec == nil {
			return "what is being granted?", nil
		}
		if !strings.EqualFold(op.GrantSpec.Grantee, "public") {
			names = append(names, named{name: op.GrantSpec.Grantee})
		}
	}
	list := make([]string, 0, len(names))
	for _, n := range names {
		list = append(list, n.name)
	}
	res := conn.ExecParams(ctx, `SELECT r.rolname, coalesce(shobj_description(r.oid, 'pg_authid'), ''), r.rolname = current_user
FROM pg_roles r WHERE r.rolname = ANY($1::text[])`, [][]byte{[]byte(pgTextArray(list))}, nil, nil, nil).Read()
	if res.Err != nil {
		return "", res.Err
	}
	type found struct {
		comment string
		helper  bool
	}
	have := map[string]found{}
	for _, row := range res.Rows {
		have[string(row[0])] = found{comment: string(row[1]), helper: string(row[2]) == "t"}
	}
	for _, n := range names {
		f, ok := have[n.name]
		if !ok {
			continue
		}
		if strings.HasPrefix(f.comment, roleMarker) {
			level := strings.HasPrefix(f.comment, roleMarker+" level")
			if !(n.levelOK && level) {
				if strings.HasPrefix(f.comment, roleMarker+" owner=") {
					return n.name + " is a person's role that Argus manages; give them a level from the database's settings instead", nil
				}
				return n.name + " is one of Argus's own roles and is managed from the database's settings", nil
			}
		}
		if f.helper && n.changesOwn {
			r := op.RoleSpec
			switch {
			case op.Kind == tunnel.AdminRoleDrop:
				return n.name + " is the account Argus manages this server with; it cannot be dropped from here", nil
			case r.Rename != "" && r.Rename != r.Name:
				return n.name + " is the account Argus manages this server with; renaming it would lock Argus out", nil
			case r.Superuser != nil && !*r.Superuser, r.Login != nil && !*r.Login:
				return n.name + " is the account Argus manages this server with; it keeps SUPERUSER and LOGIN", nil
			}
		}
	}
	if op.Kind == tunnel.AdminRoleCreate {
		if _, ok := have[op.RoleSpec.Name]; ok {
			return "a role named " + op.RoleSpec.Name + " already exists", nil
		}
	}
	return "", nil
}

// pgTextArray writes a text[] literal for a bind parameter.
func pgTextArray(v []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// splitList splits a comma-joined aggregate; empty is none.
func splitList(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), ",")
}

func listRoles(ctx context.Context, conn *pgconn.PgConn) ([]PGRole, error) {
	const sql = `SELECT a.rolname, a.rolsuper, a.rolcanlogin, a.rolinherit, a.rolcreaterole, a.rolcreatedb,
  a.rolreplication, a.rolbypassrls, a.rolconnlimit,
  coalesce(to_char(a.rolvaliduntil AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI') || ' UTC', ''),
  a.rolpassword IS NOT NULL,
  coalesce(shobj_description(a.oid, 'pg_authid'), ''),
  coalesce((SELECT string_agg(g.rolname, E'\x1f' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = a.oid), ''),
  coalesce((SELECT string_agg(g.rolname, E'\x1f' ORDER BY g.rolname) FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = a.oid AND m.admin_option), ''),
  (SELECT count(*) FROM pg_stat_activity s WHERE s.usename = a.rolname AND s.datname = current_database()),
  (SELECT count(*) FROM pg_class c WHERE c.relowner = a.oid AND c.relkind IN ('r','v','m','p','f','S'))
    + (SELECT count(*) FROM pg_proc p WHERE p.proowner = a.oid AND p.pronamespace NOT IN ('pg_catalog'::regnamespace, 'information_schema'::regnamespace)),
  a.rolname = current_user
FROM pg_authid a
WHERE a.rolname NOT LIKE 'pg\_%'
ORDER BY a.rolsuper DESC, a.rolname
LIMIT 5000`
	res := conn.ExecParams(ctx, sql, nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]PGRole, 0, len(res.Rows))
	t := func(b []byte) bool { return string(b) == "t" }
	n := func(b []byte) int { v, _ := strconv.Atoi(string(b)); return v }
	sep := func(b []byte) []string {
		if len(b) == 0 {
			return nil
		}
		return strings.Split(string(b), "\x1f")
	}
	for _, r := range res.Rows {
		out = append(out, PGRole{Name: string(r[0]), Superuser: t(r[1]), Login: t(r[2]), Inherit: t(r[3]),
			CreateRole: t(r[4]), CreateDB: t(r[5]), Replication: t(r[6]), BypassRLS: t(r[7]), ConnLimit: n(r[8]),
			ValidUntil: string(r[9]), HasPassword: t(r[10]), Comment: string(r[11]), MemberOf: sep(r[12]),
			AdminOf: sep(r[13]), Connections: n(r[14]), Owns: n(r[15]), Helper: t(r[16])})
	}
	return out, nil
}

// userSchemas excludes Postgres's own schemas.
const userSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'`

var relKinds = map[string]string{"r": "table", "v": "view", "m": "matview", "p": "partitioned", "f": "foreign", "S": "sequence"}

func rolePrivileges(ctx context.Context, conn *pgconn.PgConn, role string) (*PGRolePrivileges, error) {
	res := conn.ExecParams(ctx, `SELECT oid::text, rolsuper FROM pg_roles WHERE rolname = $1`,
		[][]byte{[]byte(role)}, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	if len(res.Rows) == 0 {
		return nil, fmt.Errorf("there is no role named %s", role)
	}
	oid := res.Rows[0][0]
	out := &PGRolePrivileges{Role: role, Superuser: string(res.Rows[0][1]) == "t",
		Schemas: []PGPrivRow{}, Tables: []PGPrivRow{}, Sequences: []PGPrivRow{}, Functions: []PGPrivRow{}, Defaults: []PGDefaultPriv{}}
	query := func(sql string) ([][][]byte, error) {
		r := conn.ExecParams(ctx, sql, [][]byte{oid}, nil, nil, nil).Read()
		return r.Rows, r.Err
	}
	// effective lists, in order, the privileges has_<fn>_privilege grants.
	effective := func(fn, obj string, privs ...string) string {
		return `coalesce((SELECT string_agg(p, ',' ORDER BY o) FROM unnest(ARRAY['` + strings.Join(privs, `','`) +
			`']) WITH ORDINALITY u(p, o) WHERE ` + fn + `($1::oid, ` + obj + `, p)), '')`
	}
	direct := func(acl string, grantable bool) string {
		g := ""
		if grantable {
			g = " AND a.is_grantable"
		}
		return `coalesce((SELECT string_agg(a.privilege_type, ',') FROM aclexplode(` + acl + `) a WHERE a.grantee = $1::oid` + g + `), '')`
	}

	rows, err := query(`SELECT current_database(), pg_get_userbyid(d.datdba), ` +
		effective("has_database_privilege", "d.oid", "CONNECT", "CREATE", "TEMPORARY") + `, ` +
		direct("d.datacl", false) + `, ` + direct("d.datacl", true) + `
FROM pg_database d WHERE d.datname = current_database()`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 1 {
		r := rows[0]
		out.Database = PGPrivRow{Name: string(r[0]), Owner: string(r[1]), Effective: splitList(r[2]), Direct: splitList(r[3]), Grantable: splitList(r[4])}
	}

	rows, err = query(`SELECT n.nspname, pg_get_userbyid(n.nspowner), ` +
		effective("has_schema_privilege", "n.oid", "USAGE", "CREATE") + `, ` +
		direct("n.nspacl", false) + `, ` + direct("n.nspacl", true) + `,
  (SELECT count(*) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r','v','m','p','f'))
FROM pg_namespace n WHERE ` + userSchemas + ` ORDER BY n.nspname LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		cnt, _ := strconv.Atoi(string(r[5]))
		out.Schemas = append(out.Schemas, PGPrivRow{Name: string(r[0]), Owner: string(r[1]), Effective: splitList(r[2]),
			Direct: splitList(r[3]), Grantable: splitList(r[4]), Objects: cnt})
	}

	rel := func(kinds string, privs ...string) ([]PGPrivRow, bool, error) {
		rows, err := query(`SELECT * FROM (SELECT n.nspname, c.relname, c.relkind::text, pg_get_userbyid(c.relowner), ` +
			effective("has_table_privilege", "c.oid", privs...) + ` AS eff, ` +
			direct("c.relacl", false) + `, ` + direct("c.relacl", true) + `
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN (` + kinds + `) AND ` + userSchemas + `) x WHERE eff <> '' ORDER BY 1, 2 LIMIT ` + strconv.Itoa(maxListed+1))
		if err != nil {
			return nil, false, err
		}
		more := len(rows) > maxListed
		if more {
			rows = rows[:maxListed]
		}
		list := make([]PGPrivRow, 0, len(rows))
		for _, r := range rows {
			list = append(list, PGPrivRow{Schema: string(r[0]), Name: string(r[1]), Kind: relKinds[string(r[2])], Owner: string(r[3]),
				Effective: splitList(r[4]), Direct: splitList(r[5]), Grantable: splitList(r[6])})
		}
		return list, more, nil
	}
	var more bool
	if out.Tables, more, err = rel(`'r','v','m','p','f'`, "SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"); err != nil {
		return nil, err
	}
	out.Truncated = more
	// Sequences through has_sequence_privilege, which knows USAGE.
	rows, err = query(`SELECT * FROM (SELECT n.nspname, c.relname, pg_get_userbyid(c.relowner), ` +
		effective("has_sequence_privilege", "c.oid", "USAGE", "SELECT", "UPDATE") + ` AS eff, ` +
		direct("c.relacl", false) + `, ` + direct("c.relacl", true) + `
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'S' AND ` + userSchemas + `) x WHERE eff <> '' ORDER BY 1, 2 LIMIT ` + strconv.Itoa(maxListed+1))
	if err != nil {
		return nil, err
	}
	if len(rows) > maxListed {
		rows, out.Truncated = rows[:maxListed], true
	}
	for _, r := range rows {
		out.Sequences = append(out.Sequences, PGPrivRow{Schema: string(r[0]), Name: string(r[1]), Kind: "sequence", Owner: string(r[2]),
			Effective: splitList(r[3]), Direct: splitList(r[4]), Grantable: splitList(r[5])})
	}
	// Functions: only those granted to the role itself, or that it owns.
	// Every function is executable by PUBLIC unless someone took that
	// away, so the effective list would be every function on the server.
	rows, err = query(`SELECT n.nspname, p.proname || '(' || oidvectortypes(p.proargtypes) || ')', pg_get_userbyid(p.proowner), ` +
		direct("p.proacl", false) + `, ` + direct("p.proacl", true) + `
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE ` + userSchemas + ` AND (p.proowner = $1::oid OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a WHERE a.grantee = $1::oid))
ORDER BY 1, 2 LIMIT ` + strconv.Itoa(maxListed))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out.Functions = append(out.Functions, PGPrivRow{Schema: string(r[0]), Name: string(r[1]), Kind: "function", Owner: string(r[2]),
			Effective: []string{"EXECUTE"}, Direct: splitList(r[3]), Grantable: splitList(r[4])})
	}
	rows, err = query(`SELECT pg_get_userbyid(d.defaclrole), coalesce(n.nspname, ''), d.defaclobjtype::text,
  string_agg(a.privilege_type, ',' ORDER BY a.privilege_type)
FROM pg_default_acl d LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace, aclexplode(d.defaclacl) a
WHERE a.grantee = $1::oid
GROUP BY 1, 2, 3 ORDER BY 2, 1, 3`)
	if err != nil {
		return nil, err
	}
	objs := map[string]string{"r": "tables", "S": "sequences", "f": "functions", "T": "types", "n": "schemas"}
	for _, r := range rows {
		out.Defaults = append(out.Defaults, PGDefaultPriv{ForRole: string(r[0]), Schema: string(r[1]),
			Objects: objs[string(r[2])], Privileges: splitList(r[3])})
	}
	return out, nil
}

func grantObjects(ctx context.Context, conn *pgconn.PgConn, schema string) (*PGGrantObjects, error) {
	out := &PGGrantObjects{Schemas: []PGGrantObject{}}
	res := conn.ExecParams(ctx, `SELECT n.nspname, pg_get_userbyid(n.nspowner) FROM pg_namespace n WHERE `+userSchemas+` ORDER BY 1 LIMIT 5000`,
		nil, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	for _, r := range res.Rows {
		out.Schemas = append(out.Schemas, PGGrantObject{Name: string(r[0]), Owner: string(r[1])})
	}
	if schema == "" {
		return out, nil
	}
	owners := map[string]bool{}
	for _, s := range out.Schemas {
		if s.Name == schema {
			owners[s.Owner] = true
		}
	}
	res = conn.ExecParams(ctx, `SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r','v','m','p','f','S') ORDER BY 1 LIMIT `+strconv.Itoa(maxListed+1),
		[][]byte{[]byte(schema)}, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	if len(res.Rows) > maxListed {
		res.Rows, out.Truncated = res.Rows[:maxListed], true
	}
	for _, r := range res.Rows {
		o := PGGrantObject{Name: string(r[0]), Kind: relKinds[string(r[1])], Owner: string(r[2])}
		owners[o.Owner] = true
		if o.Kind == "sequence" {
			out.Sequences = append(out.Sequences, o)
		} else {
			out.Tables = append(out.Tables, o)
		}
	}
	res = conn.ExecParams(ctx, `SELECT quote_ident(p.proname) || '(' || oidvectortypes(p.proargtypes) || ')', p.prokind::text, pg_get_userbyid(p.proowner)
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = $1 ORDER BY 1 LIMIT `+strconv.Itoa(maxListed),
		[][]byte{[]byte(schema)}, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	kinds := map[string]string{"f": "function", "p": "procedure", "a": "aggregate", "w": "window"}
	for _, r := range res.Rows {
		o := PGGrantObject{Name: string(r[0]), Kind: kinds[string(r[1])], Owner: string(r[2])}
		owners[o.Owner] = true
		out.Functions = append(out.Functions, o)
	}
	for o := range owners {
		out.Owners = append(out.Owners, o)
	}
	slices.Sort(out.Owners)
	return out, nil
}
