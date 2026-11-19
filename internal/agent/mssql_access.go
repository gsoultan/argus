package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server logins and access for a DBA session: the database's users and
roles, membership, and GRANT, REVOKE and DENY (see tunnel/mssqlaccess.go).

They run as the admin login in the admin file, not as the person's login,
for the same reason Postgres's run through its helper: a db_owner can grant
only what it may grant, and a login's password is the server's. Each change
is recorded before it runs, runs in one transaction, and its password is SQL
Server's salted hash by the time anything but this file sees it.
*/

// MSPrincipal is one user or role of the database.
type MSPrincipal struct {
	Name string `json:"name"`
	// Type is user, role, windows-user, windows-group, external-user,
	// external-group, certificate-user or key-user.
	Type string `json:"type"`
	// Auth is how a user signs in: sql (a SQL Server login), windows,
	// none (no login), database (a contained user's own password),
	// external.
	Auth          string   `json:"auth,omitempty"`
	Login         string   `json:"login,omitempty"`
	LoginDisabled bool     `json:"loginDisabled,omitempty"`
	DefaultSchema string   `json:"defaultSchema,omitempty"`
	Fixed         bool     `json:"fixed,omitempty"`
	System        bool     `json:"system,omitempty"`
	Argus         bool     `json:"argus,omitempty"`
	MemberOf      []string `json:"memberOf,omitempty"`
	Members       int      `json:"members,omitempty"`
	Owns          int      `json:"owns,omitempty"`
	Created       string   `json:"created,omitempty"`
}

// MSPrincipals answers ms.principals.
type MSPrincipals struct {
	Principals []MSPrincipal `json:"principals"`
	// Logins are the server's logins with no user here: what a new user can
	// be mapped to.
	Logins []string `json:"logins"`
}

// MSPermission is one GRANT, DENY or GRANT WITH GRANT OPTION.
type MSPermission struct {
	// State is GRANT, DENY or GRANT_WITH_GRANT_OPTION.
	State      string `json:"state"`
	Permission string `json:"permission"`
	// On is database, schema, object, or the class SQL Server names.
	On      string `json:"on"`
	Schema  string `json:"schema,omitempty"`
	Object  string `json:"object,omitempty"`
	Column  string `json:"column,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Grantor string `json:"grantor,omitempty"`
	// Via is the role it comes through; empty when it is the principal's own.
	Via string `json:"via,omitempty"`
}

// MSEffective is what a user can do in effect on one schema or object.
type MSEffective struct {
	Schema string   `json:"schema"`
	Name   string   `json:"name,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Can    []string `json:"can"`
}

// MSPermissions answers ms.permissions.
type MSPermissions struct {
	Principal MSPrincipal `json:"principal"`
	// MemberOf are every role it is in, directly or through another; Via
	// names the role it is in through.
	MemberOf []MSMembership `json:"memberOf"`
	Direct   []MSPermission `json:"direct"`
	// Inherited are permissions given to the roles it is in, and public.
	Inherited []MSPermission `json:"inherited"`
	// Database is what a user can do on the database in effect, all of it;
	// Schemas and Objects what it can read, change and run. Empty for a role,
	// and for a user SQL Server cannot impersonate (a Windows group).
	Database  []string      `json:"database,omitempty"`
	Schemas   []MSEffective `json:"schemas,omitempty"`
	Objects   []MSEffective `json:"objects,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	// Unknown says why there is no effective view, when there is none.
	Unknown string `json:"unknown,omitempty"`
}

// MSMembership is one role a principal is in.
type MSMembership struct {
	Role string `json:"role"`
	Via  string `json:"via,omitempty"`
}

// MSObject is a schema, or an object in one, that a grant can name.
type MSObject struct {
	Name  string `json:"name"`
	Kind  string `json:"kind,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// MSObjects answers ms.objects.
type MSObjects struct {
	Schemas   []MSObject `json:"schemas"`
	Objects   []MSObject `json:"objects,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

const msObjectsMax = 500

// adminOp runs one access operation for a DBA session.
func (s *mssqlSession) adminOp(ctx context.Context, req tunnel.PGRequest) error {
	op := req.Admin
	if op == nil || op.Kind == "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "there is no operation to run"})
	}
	if !tunnel.MSSQLAdminKind(op.Kind) {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "extensions and replication are Postgres's"})
	}
	if s.db.Level != tunnel.LevelDBA {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Code: "42501",
			Error: "this is DBA work, through SQL Server's admin login; this session is at " + s.db.Level})
	}
	if tunnel.MSSQLDBAKind(op.Kind) {
		return s.dbaOp(ctx, req)
	}
	if tunnel.MSSQLReplKind(op.Kind) {
		return s.replOp(ctx, req)
	}
	// The password becomes SQL Server's hash here, before it is recorded,
	// built or sent anywhere.
	hash := ""
	if op.Principal != nil && op.Principal.Password != "" {
		c := *op
		p := *op.Principal
		if !op.Preview {
			h, err := tunnel.MSSQLPasswordHash(p.Password)
			if err != nil {
				return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: "could not hash the password"})
			}
			hash = h
		}
		p.Password = tunnel.PasswordRedacted
		c.Principal = &p
		op = &c
	}
	run, text, err := tunnel.MSSQLAccessSQL(op, s.db.Name, hash)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	joined := strings.Join(text, ";\n")
	if op.Preview {
		b, _ := json.Marshal(PGAdminDone{SQL: joined})
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}

	start := time.Now()
	actx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	admin, err := mssqlAdminConn(actx, s.db.Host, s.db.Port, s.db.ConfigPath)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	defer admin.close()
	c, err := admin.db.Conn(actx)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}
	defer c.Close()
	if _, err := c.ExecContext(actx, "USE "+tunnel.MSSQLIdent(s.db.Name)); err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}

	if !op.Mutates() {
		var data any
		switch op.Kind {
		case tunnel.AdminMSPrincipals:
			data, err = msPrincipals(actx, c)
		case tunnel.AdminMSPermissions:
			name := ""
			if op.Principal != nil {
				name = op.Principal.Name
			}
			data, err = msPermissions(actx, c, name)
		case tunnel.AdminMSObjects:
			schema := ""
			if op.MSGrant != nil {
				schema = op.MSGrant.Schema
			}
			data, err = msObjects(actx, c, schema)
		}
		if err != nil {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
		}
		b, _ := json.Marshal(data)
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, MS: time.Since(start).Milliseconds()})
	}

	// What the statements cannot check for themselves: that a login named
	// is this user's, and is nobody the server trusts more than a database.
	if why := msCheckLogin(actx, c, op); why != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: why})
	}

	// Recorded before it runs. No record, no statement.
	if err := s.rec.write(tunnel.PGRecord{Kind: "admin", Request: req.ID, SQL: joined, Approval: req.Approval}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	p := op.Principal
	if p != nil && op.Kind == tunnel.AdminMSUserDrop && p.DropLogin {
		// SQL Server will not drop a login that is signed in.
		mssqlKillOn(actx, c, p.Login)
	}
	err = msRunAll(actx, c, run)
	if err == nil && p != nil && op.Kind == tunnel.AdminMSUserAlter && p.Enabled != nil && !*p.Enabled {
		// Disabled stops new sign-ins only; the ones open end with it.
		mssqlKillOn(actx, c, p.Login)
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		msg := mssqlMessage(err)
		if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: msg, MS: elapsed}); rerr != nil {
			return rerr
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg, MS: elapsed})
	}
	tag := msTag(text)
	if err := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, MS: elapsed}); err != nil {
		return err
	}
	b, _ := json.Marshal(PGAdminDone{SQL: joined, Tag: tag})
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, Tag: tag, MS: elapsed})
}

// msTag is a change's short name: its first statement's first two words.
func msTag(text []string) string {
	if len(text) == 0 {
		return ""
	}
	f := strings.Fields(text[0])
	if len(f) > 2 {
		f = f[:2]
	}
	return strings.Join(f, " ")
}

// msRunAll runs statements in one transaction: all of them or none.
func msRunAll(ctx context.Context, c *sql.Conn, stmts []string) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// msCheckLogin refuses a change to a login that is not the user's own, or
// that holds more than a database: a server role, CONTROL SERVER, or the
// right to change logins. Dropping a login also needs it to have no user in
// any other database.
func msCheckLogin(ctx context.Context, c *sql.Conn, op *tunnel.PGAdminOp) string {
	p := op.Principal
	if p == nil {
		return ""
	}
	touches := (op.Kind == tunnel.AdminMSUserAlter && (p.Password != "" || p.Enabled != nil)) ||
		(op.Kind == tunnel.AdminMSUserDrop && p.DropLogin)
	if !touches {
		return ""
	}
	var sid []byte
	err := c.QueryRowContext(ctx, `SELECT sp.sid FROM sys.database_principals dp
	  JOIN sys.server_principals sp ON sp.sid = dp.sid
	  WHERE dp.name = @p1 AND sp.name = @p2 AND sp.type IN ('S', 'U', 'G')`, p.Name, p.Login).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "the login " + p.Login + " is not the one " + p.Name + " signs in with"
	}
	if err != nil {
		return mssqlMessage(err)
	}
	var powerful int
	err = c.QueryRowContext(ctx, `SELECT
	  (SELECT COUNT(*) FROM sys.server_role_members m
	     JOIN sys.server_principals r ON r.principal_id = m.role_principal_id
	     JOIN sys.server_principals l ON l.principal_id = m.member_principal_id
	   WHERE l.sid = @p1 AND r.name <> 'public')
	  + (SELECT COUNT(*) FROM sys.server_permissions sp
	     JOIN sys.server_principals l ON l.principal_id = sp.grantee_principal_id
	   WHERE l.sid = @p1 AND sp.state IN ('G', 'W')
	     AND sp.permission_name IN ('CONTROL SERVER', 'ALTER ANY LOGIN', 'IMPERSONATE ANY LOGIN', 'ALTER ANY SERVER ROLE'))`,
		sid).Scan(&powerful)
	if err != nil {
		return mssqlMessage(err)
	}
	if powerful > 0 {
		return "the login " + p.Login + " holds server-wide rights; change it on the server, not from one database"
	}
	if op.Kind != tunnel.AdminMSUserDrop {
		return ""
	}
	rows, err := c.QueryContext(ctx, `SELECT name FROM sys.databases
	  WHERE state = 0 AND name <> DB_NAME() AND HAS_DBACCESS(name) = 1`)
	if err != nil {
		return mssqlMessage(err)
	}
	var dbs []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			dbs = append(dbs, n)
		}
	}
	_ = rows.Close()
	for _, d := range dbs {
		var n int
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tunnel.MSSQLIdent(d)+
			".sys.database_principals WHERE sid = @p1", sid).Scan(&n); err != nil {
			return mssqlMessage(err)
		}
		if n > 0 {
			return "the login " + p.Login + " also has a user in " + d + "; drop only the user here"
		}
	}
	return ""
}

var msTypes = map[string]string{
	"S": "user", "U": "windows-user", "G": "windows-group", "R": "role", "E": "external-user",
	"X": "external-group", "C": "certificate-user", "K": "key-user", "A": "application-role",
}

var msAuths = map[string]string{
	"INSTANCE": "sql", "DATABASE": "database", "WINDOWS": "windows", "NONE": "none", "EXTERNAL": "external",
}

func msPrincipals(ctx context.Context, c *sql.Conn) (MSPrincipals, error) {
	out := MSPrincipals{Principals: []MSPrincipal{}, Logins: []string{}}
	rows, err := c.QueryContext(ctx, `SELECT dp.name, dp.type, dp.authentication_type_desc,
	  ISNULL(sp.name, ''), ISNULL(sp.is_disabled, 0), ISNULL(dp.default_schema_name, ''), dp.is_fixed_role,
	  (SELECT COUNT(*) FROM sys.schemas s WHERE s.principal_id = dp.principal_id),
	  (SELECT COUNT(*) FROM sys.database_role_members m WHERE m.role_principal_id = dp.principal_id),
	  CONVERT(varchar(19), dp.create_date, 126)
	  FROM sys.database_principals dp LEFT JOIN sys.server_principals sp ON sp.sid = dp.sid
	  WHERE dp.type IN ('S','U','G','R','E','X','C','K','A') AND dp.name NOT IN ('sys', 'INFORMATION_SCHEMA')
	  ORDER BY CASE WHEN dp.type = 'R' THEN 1 ELSE 0 END, dp.name`)
	if err != nil {
		return out, err
	}
	idx := map[string]int{}
	for rows.Next() {
		var p MSPrincipal
		var typ, auth string
		var owns, members int
		if err := rows.Scan(&p.Name, &typ, &auth, &p.Login, &p.LoginDisabled, &p.DefaultSchema, &p.Fixed,
			&owns, &members, &p.Created); err != nil {
			_ = rows.Close()
			return out, err
		}
		p.Type = msTypes[strings.TrimSpace(typ)]
		if p.Type != "role" && p.Type != "application-role" {
			p.Auth = msAuths[auth]
			if p.Auth == "sql" && p.Login == "" {
				// A user whose login is gone: a database restored from
				// another server.
				p.Auth = "orphaned"
			}
		} else {
			p.Login = ""
		}
		p.Owns, p.Members = owns, members
		l := strings.ToLower(p.Name)
		p.System = l == "dbo" || l == "guest" || l == "public"
		p.Argus = tunnel.IsStoreUser(p.Name)
		idx[p.Name] = len(out.Principals)
		out.Principals = append(out.Principals, p)
	}
	_ = rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT m.name, r.name FROM sys.database_role_members rm
	  JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
	  JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id ORDER BY r.name`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m, r string
		if rows.Scan(&m, &r) == nil {
			if i, ok := idx[m]; ok {
				out.Principals[i].MemberOf = append(out.Principals[i].MemberOf, r)
			}
		}
	}
	_ = rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT TOP (1000) sp.name FROM sys.server_principals sp
	  WHERE sp.type IN ('S', 'U', 'G') AND sp.name NOT LIKE '##%' AND sp.name NOT LIKE 'NT %'
	    AND sp.name NOT LIKE 'argus-%' AND sp.name <> 'sa'
	    AND NOT EXISTS (SELECT 1 FROM sys.database_principals dp WHERE dp.sid = sp.sid)
	  ORDER BY sp.name`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out.Logins = append(out.Logins, n)
		}
	}
	_ = rows.Close()
	return out, nil
}

// msPermSQL lists permissions given to the principals in @ids, with the
// securable's names.
const msPermSQL = `SELECT p.state_desc, p.permission_name, p.class, p.class_desc,
  ISNULL(CASE p.class WHEN 1 THEN OBJECT_SCHEMA_NAME(p.major_id) WHEN 3 THEN SCHEMA_NAME(p.major_id) END, ''),
  ISNULL(CASE p.class WHEN 1 THEN OBJECT_NAME(p.major_id) END, ''),
  ISNULL(CASE WHEN p.class = 1 AND p.minor_id > 0 THEN COL_NAME(p.major_id, p.minor_id) END, ''),
  ISNULL(CASE p.class WHEN 1 THEN (SELECT o.type_desc FROM sys.objects o WHERE o.object_id = p.major_id) END, ''),
  ISNULL(USER_NAME(p.grantor_principal_id), ''), USER_NAME(p.grantee_principal_id)
  FROM sys.database_permissions p WHERE p.grantee_principal_id = USER_ID(@p1)
  ORDER BY p.class, 5, 6, p.permission_name`

func msPermissions(ctx context.Context, c *sql.Conn, name string) (MSPermissions, error) {
	out := MSPermissions{MemberOf: []MSMembership{}, Direct: []MSPermission{}, Inherited: []MSPermission{}}
	if name == "" {
		return out, errors.New("which user or role?")
	}
	all, err := msPrincipals(ctx, c)
	if err != nil {
		return out, err
	}
	found := false
	for _, p := range all.Principals {
		if p.Name == name {
			out.Principal, found = p, true
		}
	}
	if !found {
		return out, fmt.Errorf("there is no user or role %s in this database", name)
	}
	// Every role it is in, and through which.
	rows, err := c.QueryContext(ctx, `WITH r AS (
	    SELECT rm.role_principal_id AS id, CAST(N'' AS nvarchar(128)) AS via, 1 AS depth
	    FROM sys.database_role_members rm WHERE rm.member_principal_id = USER_ID(@p1)
	    UNION ALL
	    SELECT rm.role_principal_id, CAST(CASE WHEN r.via = N'' THEN USER_NAME(r.id) ELSE r.via END AS nvarchar(128)), r.depth + 1
	    FROM sys.database_role_members rm JOIN r ON rm.member_principal_id = r.id WHERE r.depth < 16)
	  SELECT DISTINCT USER_NAME(id), via FROM r`, name)
	if err != nil {
		return out, err
	}
	var roles []string
	seen := map[string]bool{}
	for rows.Next() {
		var m MSMembership
		if rows.Scan(&m.Role, &m.Via) == nil && !seen[m.Role] {
			seen[m.Role] = true
			out.MemberOf = append(out.MemberOf, m)
			roles = append(roles, m.Role)
		}
	}
	_ = rows.Close()
	if !strings.EqualFold(name, "public") {
		roles = append(roles, "public")
	}

	perms := func(who, via string) ([]MSPermission, error) {
		rows, err := c.QueryContext(ctx, msPermSQL, who)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var got []MSPermission
		for rows.Next() {
			var p MSPermission
			var class int
			var classDesc, grantee string
			if err := rows.Scan(&p.State, &p.Permission, &class, &classDesc, &p.Schema, &p.Object, &p.Column,
				&p.Kind, &p.Grantor, &grantee); err != nil {
				return nil, err
			}
			switch class {
			case 0:
				p.On = "database"
			case 1:
				p.On = "object"
			case 3:
				p.On = "schema"
			default:
				p.On = strings.ToLower(classDesc)
			}
			p.Via = via
			got = append(got, p)
		}
		return got, rows.Err()
	}
	if out.Direct, err = perms(name, ""); err != nil {
		return out, err
	}
	if out.Direct == nil {
		out.Direct = []MSPermission{}
	}
	for _, r := range roles {
		got, err := perms(r, r)
		if err != nil {
			return out, err
		}
		out.Inherited = append(out.Inherited, got...)
	}

	// What a user can do in effect, asked of SQL Server as that user.
	switch out.Principal.Type {
	case "role", "application-role":
		return out, nil
	case "windows-group", "external-group":
		out.Unknown = "SQL Server cannot act as a group to say what it can do in effect; look at a member instead"
		return out, nil
	}
	if err := msEffective(ctx, c, name, &out); err != nil {
		out.Unknown = "SQL Server would not act as " + name + " to say what it can do: " + mssqlMessage(err)
	}
	return out, nil
}

// msEffective fills what a user can do in effect: on the database, on each
// schema, and on the first msObjectsMax tables, views, procedures and
// functions. The list of objects is taken as the admin, so one the user
// cannot see is still asked about.
func msEffective(ctx context.Context, c *sql.Conn, user string, out *MSPermissions) error {
	rows, err := c.QueryContext(ctx, `SET NOCOUNT ON;
	DECLARE @o TABLE (s sysname, n sysname NULL, t varchar(2) NULL, k nvarchar(60) NULL);
	INSERT @o (s) SELECT name FROM sys.schemas
	  WHERE schema_id < 16384 AND name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest')
	    AND name NOT LIKE 'db[_]%';
	INSERT @o SELECT TOP (`+fmt.Sprint(msObjectsMax+1)+`) SCHEMA_NAME(o.schema_id), o.name, o.type, o.type_desc
	  FROM sys.objects o WHERE o.is_ms_shipped = 0 AND o.type IN ('U', 'V', 'P', 'FN', 'IF', 'TF', 'SO')
	  ORDER BY SCHEMA_NAME(o.schema_id), o.name;
	DECLARE @u sysname = @p1;
	EXECUTE AS USER = @u;
	BEGIN TRY
	  SELECT '', '', '', STUFF((SELECT ',' + permission_name FROM fn_my_permissions(NULL, 'DATABASE')
	    ORDER BY permission_name FOR XML PATH('')), 1, 1, '')
	  UNION ALL
	  SELECT s, '', '', CONCAT(
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'SELECT') = 1 THEN 'SELECT,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'INSERT') = 1 THEN 'INSERT,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'UPDATE') = 1 THEN 'UPDATE,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'DELETE') = 1 THEN 'DELETE,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'EXECUTE') = 1 THEN 'EXECUTE,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'ALTER') = 1 THEN 'ALTER,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s), 'SCHEMA', 'CONTROL') = 1 THEN 'CONTROL,' END)
	  FROM @o WHERE n IS NULL
	  UNION ALL
	  SELECT s, n, k, CONCAT(
	    CASE WHEN t IN ('U', 'V', 'IF', 'TF') AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'SELECT') = 1 THEN 'SELECT,' END,
	    CASE WHEN t IN ('U', 'V') AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'INSERT') = 1 THEN 'INSERT,' END,
	    CASE WHEN t IN ('U', 'V') AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'UPDATE') = 1 THEN 'UPDATE,' END,
	    CASE WHEN t IN ('U', 'V') AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'DELETE') = 1 THEN 'DELETE,' END,
	    CASE WHEN t IN ('P', 'FN') AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'EXECUTE') = 1 THEN 'EXECUTE,' END,
	    CASE WHEN t = 'SO' AND HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'UPDATE') = 1 THEN 'UPDATE,' END,
	    CASE WHEN HAS_PERMS_BY_NAME(QUOTENAME(s) + '.' + QUOTENAME(n), 'OBJECT', 'ALTER') = 1 THEN 'ALTER,' END)
	  FROM @o WHERE n IS NOT NULL;
	  REVERT;
	END TRY
	BEGIN CATCH
	  REVERT;
	  THROW;
	END CATCH`, user)
	if err != nil {
		return err
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var schema, name, kind string
		var can sql.NullString
		if err := rows.Scan(&schema, &name, &kind, &can); err != nil {
			return err
		}
		list := []string{}
		for _, p := range strings.Split(can.String, ",") {
			if p != "" {
				list = append(list, p)
			}
		}
		switch {
		case first:
			out.Database = list
			first = false
		case name == "":
			out.Schemas = append(out.Schemas, MSEffective{Schema: schema, Can: list})
		default:
			if len(out.Objects) >= msObjectsMax {
				out.Truncated = true
				continue
			}
			out.Objects = append(out.Objects, MSEffective{Schema: schema, Name: name,
				Kind: strings.ToLower(strings.ReplaceAll(kind, "_", " ")), Can: list})
		}
	}
	return rows.Err()
}

func msObjects(ctx context.Context, c *sql.Conn, schema string) (MSObjects, error) {
	out := MSObjects{Schemas: []MSObject{}}
	rows, err := c.QueryContext(ctx, `SELECT s.name, USER_NAME(s.principal_id) FROM sys.schemas s
	  WHERE s.name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest') AND s.name NOT LIKE 'db[_]%'
	  ORDER BY s.name`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var o MSObject
		if rows.Scan(&o.Name, &o.Owner) == nil {
			out.Schemas = append(out.Schemas, o)
		}
	}
	_ = rows.Close()
	if schema == "" {
		return out, nil
	}
	rows, err = c.QueryContext(ctx, `SELECT TOP (`+fmt.Sprint(msObjectsMax+1)+`) o.name, o.type_desc
	  FROM sys.objects o WHERE o.schema_id = SCHEMA_ID(@p1) AND o.is_ms_shipped = 0
	    AND o.type IN ('U', 'V', 'P', 'FN', 'IF', 'TF', 'SO', 'SN')
	  ORDER BY o.name`, schema)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var o MSObject
		if err := rows.Scan(&o.Name, &o.Kind); err != nil {
			return out, err
		}
		if len(out.Objects) >= msObjectsMax {
			out.Truncated = true
			break
		}
		o.Kind = strings.ToLower(strings.ReplaceAll(o.Kind, "_", " "))
		out.Objects = append(out.Objects, o)
	}
	return out, rows.Err()
}
