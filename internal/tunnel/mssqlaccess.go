package tunnel

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf16"
)

/*
SQL Server logins and access, the way SSMS's Security folder manages them for
one database: its users (with a new SQL login, an existing login, or none) and
its roles, membership, and GRANT, REVOKE and DENY on the database, a schema or
objects in one.

Like Postgres's roles (pgroles.go) these are administrative operations, not
statements typed into the editor: the agent runs them as SQL Server's admin
login, so a grant takes effect whoever owns the object, and a password never
reaches the recording. The password becomes SQL Server's own salted SHA-512
hash inside the agent (MSSQLPasswordHash) and is sent as PASSWORD = 0x...
HASHED; the recording, the approval and the preview say [redacted].

The builder is pure and lives here so the control plane, which decides whether
a change waits for approval, and the agent, which records and runs it, compute
the same text.

What it refuses, and why:
  - any principal named argus-... (people's logins are Argus's, set from the
    database's settings);
  - dbo, guest, sys, INFORMATION_SCHEMA and public as something to create,
    change or drop, and the fixed db_ roles as something to change or drop
    (they can still be given and taken);
  - permissions it does not know for what they are on.
The agent refuses more, with the server in hand: changing the password of,
enabling, disabling or dropping a login that is not mapped to this user, that
holds sysadmin or CONTROL SERVER, or (to drop it) that has a user in any other
database.
*/

// SQL Server access operation kinds.
const (
	// AdminMSPrincipals lists the database's users and roles, with their
	// logins, memberships and what they own.
	AdminMSPrincipals = "ms.principals"
	// AdminMSPermissions says what one principal (Principal.Name) may do:
	// its memberships, what is granted or denied to it directly, and for a
	// user what it can do in effect.
	AdminMSPermissions = "ms.permissions"
	// AdminMSObjects lists the schemas, and for MSGrant.Schema its tables,
	// views, procedures, functions and sequences: what a grant can name.
	AdminMSObjects = "ms.objects"

	AdminMSUserCreate = "ms.user.create"
	AdminMSUserAlter  = "ms.user.alter"
	AdminMSUserDrop   = "ms.user.drop"
	AdminMSRoleCreate = "ms.role.create"
	AdminMSRoleAlter  = "ms.role.alter"
	AdminMSRoleDrop   = "ms.role.drop"
	AdminMSMember     = "ms.member"
	AdminMSUnmember   = "ms.unmember"
	AdminMSGrant      = "ms.grant"
	AdminMSRevoke     = "ms.revoke"
	AdminMSDeny       = "ms.deny"
)

// How a new user signs in.
const (
	// MSAuthSQL makes a SQL Server login of the same name with a password.
	MSAuthSQL = "sql"
	// MSAuthLogin maps the user to a login that is already there.
	MSAuthLogin = "login"
	// MSAuthNone is a user without a login: for EXECUTE AS, or ownership.
	MSAuthNone = "none"
)

// What a SQL Server grant is on.
const (
	MSOnDatabase = "database"
	MSOnSchema   = "schema"
	MSOnObject   = "object"
)

// MSSQLPrincipalSpec is a user or role to create, change or drop.
type MSSQLPrincipalSpec struct {
	Name string `json:"name"`
	// Rename is the new name, for alter.
	Rename string `json:"rename,omitempty"`
	// Auth is how a new user signs in: MSAuthSQL, MSAuthLogin or
	// MSAuthNone.
	Auth string `json:"auth,omitempty"`
	// Login is the login a user maps to: for MSAuthLogin on create, and to
	// change its password or enable or disable it on alter. The agent checks
	// it is this user's.
	Login string `json:"login,omitempty"`
	// Password is a new login's password, or a login's new one. The agent
	// turns it into SQL Server's hash and clears it before the operation
	// goes any further; it is never recorded, audited or shown.
	Password string `json:"password,omitempty"`
	// DefaultSchema is a user's default schema; nil leaves it.
	DefaultSchema *string `json:"defaultSchema,omitempty"`
	// Enabled enables or disables the user's login, on alter.
	Enabled *bool `json:"enabled,omitempty"`
	// MemberOf are roles a new user or role joins, on create.
	MemberOf []string `json:"memberOf,omitempty"`
	// DropLogin also drops the user's login, on drop.
	DropLogin bool `json:"dropLogin,omitempty"`
}

// MSSQLGrantSpec is a GRANT, REVOKE or DENY.
type MSSQLGrantSpec struct {
	// Grantee is the user or role, or "public".
	Grantee string `json:"grantee"`
	// On is MSOnDatabase, MSOnSchema or MSOnObject.
	On string `json:"on"`
	// Schema is the schema, for schema and object.
	Schema string `json:"schema,omitempty"`
	// Objects are names in Schema, for object.
	Objects    []string `json:"objects,omitempty"`
	Privileges []string `json:"privileges"`
	// WithGrant is WITH GRANT OPTION for a grant; for a revoke it takes only
	// the grant option (GRANT OPTION FOR).
	WithGrant bool `json:"withGrant,omitempty"`
	// Cascade, for a revoke or deny, also takes what the grantee gave on.
	Cascade bool `json:"cascade,omitempty"`
}

// MSSQLAccessKind says whether kind is a SQL Server access operation.
func MSSQLAccessKind(kind string) bool {
	switch kind {
	case AdminMSPrincipals, AdminMSPermissions, AdminMSObjects,
		AdminMSUserCreate, AdminMSUserAlter, AdminMSUserDrop,
		AdminMSRoleCreate, AdminMSRoleAlter, AdminMSRoleDrop,
		AdminMSMember, AdminMSUnmember, AdminMSGrant, AdminMSRevoke, AdminMSDeny:
		return true
	}
	return false
}

// mssqlReserved are principals no one creates, changes or drops here.
var mssqlReserved = map[string]bool{"dbo": true, "guest": true, "sys": true, "information_schema": true, "public": true}

// MSSQLFixedRole says whether name is one of SQL Server's fixed database
// roles.
func MSSQLFixedRole(name string) bool {
	return slices.Contains(mssqlAllRoles, strings.ToLower(name))
}

// mssqlName checks a principal, schema or object name: 1 to 128 characters,
// no control characters, no surrounding spaces.
func mssqlName(what, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%s is required", what)
	}
	if len([]rune(name)) > MSSQLMaxName {
		return "", fmt.Errorf("%s is at most %d characters", what, MSSQLMaxName)
	}
	if strings.TrimSpace(name) != name {
		return "", fmt.Errorf("%s cannot begin or end with a space", what)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%s cannot contain control characters", what)
		}
	}
	return MSSQLIdent(name), nil
}

// mssqlPrincipal checks a principal to create, change or drop.
func mssqlPrincipal(name string, fixedOK bool) (string, error) {
	q, err := mssqlName("a name", name)
	if err != nil {
		return "", err
	}
	l := strings.ToLower(name)
	switch {
	case strings.HasPrefix(l, StoreUserPrefix):
		return "", fmt.Errorf("%s is Argus's own; people's access is set from the database's settings", name)
	case mssqlReserved[l]:
		return "", fmt.Errorf("%s is SQL Server's own", name)
	case !fixedOK && MSSQLFixedRole(name):
		return "", fmt.Errorf("%s is one of SQL Server's fixed roles; give and take it, but it cannot be changed", name)
	}
	return q, nil
}

// mssqlRoleOrUser checks a principal that is given or takes something: any
// but Argus's, dbo and the system ones.
func mssqlRoleOrUser(name string) (string, error) {
	q, err := mssqlName("a name", name)
	if err != nil {
		return "", err
	}
	l := strings.ToLower(name)
	if strings.HasPrefix(l, StoreUserPrefix) {
		return "", fmt.Errorf("%s is Argus's own; people's access is set from the database's settings", name)
	}
	if l == "dbo" || l == "sys" || l == "information_schema" || l == "guest" {
		return "", fmt.Errorf("%s is SQL Server's own", name)
	}
	if MSSQLFixedRole(name) {
		// One spelling, so the text approved names the role the way
		// MSSQLTextAlwaysApproval reads it.
		return MSSQLIdent(l), nil
	}
	return q, nil
}

// MSSQLAlwaysApproval says the change waits for someone else's approval on
// every database, whatever its rules: making someone db_owner, or giving
// CONTROL or TAKE OWNERSHIP of the whole database, hands them everything in
// it, including who else may do what; and simple recovery ends the log
// backups the database can be put back from.
func MSSQLAlwaysApproval(op *PGAdminOp) bool {
	if op == nil || op.Preview {
		return false
	}
	if MSSQLReplAlwaysApproval(op) {
		return true
	}
	switch op.Kind {
	case AdminMSDBSet:
		// Simple recovery ends the log backups a restore to a point in
		// time needs, and breaks log shipping and availability groups.
		return op.MSDBA != nil && strings.EqualFold(op.MSDBA.Setting, "RECOVERY") && strings.EqualFold(strings.TrimSpace(op.MSDBA.Value), "SIMPLE")
	case AdminMSMember:
		return op.Member != nil && strings.EqualFold(op.Member.Role, "db_owner")
	case AdminMSUserCreate, AdminMSRoleCreate:
		return op.Principal != nil && slices.ContainsFunc(op.Principal.MemberOf, func(r string) bool { return strings.EqualFold(r, "db_owner") })
	case AdminMSGrant:
		g := op.MSGrant
		return g != nil && g.On == MSOnDatabase && slices.ContainsFunc(g.Privileges, func(p string) bool {
			return p == "CONTROL" || p == "TAKE OWNERSHIP"
		})
	}
	return false
}

// MSSQLTextAlwaysApproval says text, as MSSQLAdminText writes it, makes
// someone db_owner, gives CONTROL or TAKE OWNERSHIP of the database, or puts
// it in simple recovery: an
// approval for it may be asked on any database.
func MSSQLTextAlwaysApproval(text string) bool {
	for _, s := range strings.Split(text, ";\n") {
		if msReplTextAlwaysApproval(s) {
			return true
		}
		if strings.HasPrefix(s, "ALTER DATABASE ") && strings.HasSuffix(s, " SET RECOVERY SIMPLE") {
			return true
		}
		if strings.HasPrefix(s, "ALTER ROLE [db_owner] ADD MEMBER ") {
			return true
		}
		if strings.HasPrefix(s, "GRANT ") && strings.Contains(s, " ON DATABASE::") {
			privs := strings.Split(strings.SplitN(strings.TrimPrefix(s, "GRANT "), " ON DATABASE::", 2)[0], ", ")
			if slices.Contains(privs, "CONTROL") || slices.Contains(privs, "TAKE OWNERSHIP") {
				return true
			}
		}
	}
	return false
}

// Permissions each kind of securable takes, in a fixed order.
var msPermissions = map[string][]string{
	MSOnDatabase: {"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "REFERENCES", "VIEW DEFINITION", "SHOWPLAN",
		"VIEW DATABASE STATE", "CREATE TABLE", "CREATE VIEW", "CREATE PROCEDURE", "CREATE FUNCTION", "CREATE SCHEMA",
		"CREATE TYPE", "ALTER", "ALTER ANY SCHEMA", "BACKUP DATABASE", "CONNECT", "CONTROL", "TAKE OWNERSHIP"},
	MSOnSchema: {"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "REFERENCES", "VIEW DEFINITION",
		"VIEW CHANGE TRACKING", "ALTER", "CONTROL", "TAKE OWNERSHIP"},
	MSOnObject: {"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "REFERENCES", "VIEW DEFINITION",
		"VIEW CHANGE TRACKING", "ALTER", "CONTROL", "TAKE OWNERSHIP"},
}

// MSSQLPermissionsFor lists the permissions a securable kind takes.
func MSSQLPermissionsFor(on string) []string { return msPermissions[on] }

// PasswordRedacted is what a password reads as in the recording, the
// approval and a preview.
const PasswordRedacted = "[redacted]"

// MSSQLAccessSQL is an access operation's statements, in order: run is what
// the agent runs, text the same with the password redacted. database is the
// session's database. hash is MSSQLPasswordHash of Principal.Password; with
// none, run holds the placeholder and is only fit to be shown.
func MSSQLAccessSQL(op *PGAdminOp, database, hash string) (run, text []string, err error) {
	if op == nil {
		return nil, nil, errors.New("no operation")
	}
	both := func(s ...string) { run = append(run, s...); text = append(text, s...) }
	pw := func(prefix, suffix string) {
		h := hash
		if h == "" {
			h = PasswordRedacted
		} else {
			h += " HASHED"
		}
		run = append(run, prefix+"PASSWORD = "+h+suffix)
		text = append(text, prefix+"PASSWORD = "+PasswordRedacted+suffix)
	}
	p := op.Principal
	switch op.Kind {
	case AdminMSPrincipals, AdminMSPermissions, AdminMSObjects:
		return nil, nil, nil

	case AdminMSUserCreate:
		if p == nil {
			return nil, nil, errors.New("which user?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		if err := mssqlPasswordFits(p.Password); err != nil {
			return nil, nil, err
		}
		with := ""
		if p.DefaultSchema != nil && *p.DefaultSchema != "" {
			s, err := mssqlName("the default schema", *p.DefaultSchema)
			if err != nil {
				return nil, nil, err
			}
			with = " WITH DEFAULT_SCHEMA = " + s
		}
		switch p.Auth {
		case MSAuthSQL:
			if p.Password == "" {
				return nil, nil, errors.New("a new login needs a password")
			}
			db, err := mssqlName("the database", database)
			if err != nil {
				return nil, nil, err
			}
			pw("CREATE LOGIN "+name+" WITH ", ", DEFAULT_DATABASE = "+db+", CHECK_EXPIRATION = OFF, CHECK_POLICY = OFF")
			both("CREATE USER " + name + " FOR LOGIN " + name + with)
		case MSAuthLogin:
			if p.Password != "" {
				return nil, nil, errors.New("the login is already there; change its password from the user")
			}
			login, err := mssqlPrincipal(p.Login, false)
			if err != nil {
				return nil, nil, fmt.Errorf("the login: %w", err)
			}
			both("CREATE USER " + name + " FOR LOGIN " + login + with)
		case MSAuthNone:
			if p.Password != "" {
				return nil, nil, errors.New("a user without a login has no password")
			}
			both("CREATE USER " + name + " WITHOUT LOGIN" + with)
		default:
			return nil, nil, errors.New("how does the user sign in: a new SQL login, an existing login, or none?")
		}
		if err := memberOf(p, name, both); err != nil {
			return nil, nil, err
		}
		return run, text, nil

	case AdminMSUserAlter:
		if p == nil {
			return nil, nil, errors.New("which user?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		if len(p.MemberOf) > 0 || p.Auth != "" || p.DropLogin {
			return nil, nil, errors.New("change membership one role at a time")
		}
		if err := mssqlPasswordFits(p.Password); err != nil {
			return nil, nil, err
		}
		if p.Password != "" || p.Enabled != nil {
			login, err := mssqlPrincipal(p.Login, false)
			if err != nil {
				return nil, nil, fmt.Errorf("the login: %w", err)
			}
			if p.Password != "" {
				pw("ALTER LOGIN "+login+" WITH ", "")
			}
			if p.Enabled != nil {
				if *p.Enabled {
					both("ALTER LOGIN " + login + " ENABLE")
				} else {
					both("ALTER LOGIN " + login + " DISABLE")
				}
			}
		}
		if p.DefaultSchema != nil {
			s := "NULL"
			if *p.DefaultSchema != "" {
				if s, err = mssqlName("the default schema", *p.DefaultSchema); err != nil {
					return nil, nil, err
				}
			}
			both("ALTER USER " + name + " WITH DEFAULT_SCHEMA = " + s)
		}
		if p.Rename != "" && p.Rename != p.Name {
			to, err := mssqlPrincipal(p.Rename, false)
			if err != nil {
				return nil, nil, err
			}
			both("ALTER USER " + name + " WITH NAME = " + to)
		}
		if len(run) == 0 {
			return nil, nil, errors.New("nothing to change")
		}
		return run, text, nil

	case AdminMSUserDrop:
		if p == nil {
			return nil, nil, errors.New("which user?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		both("DROP USER " + name)
		if p.DropLogin {
			login, err := mssqlPrincipal(p.Login, false)
			if err != nil {
				return nil, nil, fmt.Errorf("the login: %w", err)
			}
			both("DROP LOGIN " + login)
		}
		return run, text, nil

	case AdminMSRoleCreate:
		if p == nil {
			return nil, nil, errors.New("which role?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		if p.Password != "" || p.Auth != "" {
			return nil, nil, errors.New("a role has no login")
		}
		both("CREATE ROLE " + name)
		if err := memberOf(p, name, both); err != nil {
			return nil, nil, err
		}
		return run, text, nil

	case AdminMSRoleAlter:
		if p == nil {
			return nil, nil, errors.New("which role?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		if p.Rename == "" || p.Rename == p.Name {
			return nil, nil, errors.New("nothing to change")
		}
		to, err := mssqlPrincipal(p.Rename, false)
		if err != nil {
			return nil, nil, err
		}
		both("ALTER ROLE " + name + " WITH NAME = " + to)
		return run, text, nil

	case AdminMSRoleDrop:
		if p == nil {
			return nil, nil, errors.New("which role?")
		}
		name, err := mssqlPrincipal(p.Name, false)
		if err != nil {
			return nil, nil, err
		}
		both("DROP ROLE " + name)
		return run, text, nil

	case AdminMSMember, AdminMSUnmember:
		m := op.Member
		if m == nil {
			return nil, nil, errors.New("which membership?")
		}
		role, err := mssqlRoleOrUser(m.Role)
		if err != nil {
			return nil, nil, err
		}
		if strings.EqualFold(m.Role, "public") {
			return nil, nil, errors.New("everyone is in public")
		}
		member, err := mssqlRoleOrUser(m.Member)
		if err != nil {
			return nil, nil, err
		}
		if strings.EqualFold(m.Role, m.Member) {
			return nil, nil, errors.New("a role cannot be a member of itself")
		}
		if m.Admin {
			return nil, nil, errors.New("SQL Server has no admin option on a role; make the member the role's owner instead")
		}
		if op.Kind == AdminMSMember {
			both("ALTER ROLE " + role + " ADD MEMBER " + member)
		} else {
			both("ALTER ROLE " + role + " DROP MEMBER " + member)
		}
		return run, text, nil

	case AdminMSGrant, AdminMSRevoke, AdminMSDeny:
		s, err := mssqlGrantSQL(op.MSGrant, op.Kind, database)
		if err != nil {
			return nil, nil, err
		}
		both(s...)
		return run, text, nil
	}
	return nil, nil, fmt.Errorf("unknown operation %q", op.Kind)
}

func memberOf(p *MSSQLPrincipalSpec, name string, both func(...string)) error {
	for _, g := range p.MemberOf {
		r, err := mssqlRoleOrUser(g)
		if err != nil {
			return err
		}
		if strings.EqualFold(g, "public") {
			continue
		}
		both("ALTER ROLE " + r + " ADD MEMBER " + name)
	}
	return nil
}

func mssqlPasswordFits(pw string) error {
	if len([]rune(pw)) > 128 {
		return errors.New("a password is at most 128 characters")
	}
	if strings.ContainsRune(pw, 0) {
		return errors.New("a password cannot contain NUL")
	}
	return nil
}

func mssqlGrantSQL(g *MSSQLGrantSpec, kind, database string) ([]string, error) {
	if g == nil {
		return nil, errors.New("what is being granted?")
	}
	var grantee string
	if strings.EqualFold(g.Grantee, "public") {
		grantee = "[public]"
	} else {
		var err error
		if grantee, err = mssqlRoleOrUser(g.Grantee); err != nil {
			return nil, err
		}
	}
	allowed := msPermissions[g.On]
	if allowed == nil {
		return nil, fmt.Errorf("cannot grant on %q", g.On)
	}
	if len(g.Privileges) == 0 {
		return nil, errors.New("choose at least one permission")
	}
	var keep []string
	for _, p := range allowed {
		if slices.Contains(g.Privileges, p) {
			keep = append(keep, p)
		}
	}
	for _, p := range g.Privileges {
		if !slices.Contains(allowed, p) {
			return nil, fmt.Errorf("%s is not a permission on a %s", p, g.On)
		}
	}
	privs := strings.Join(keep, ", ")

	var targets []string
	switch g.On {
	case MSOnDatabase:
		d, err := mssqlName("the database", database)
		if err != nil {
			return nil, err
		}
		targets = []string{"DATABASE::" + d}
	case MSOnSchema:
		s, err := mssqlName("the schema", g.Schema)
		if err != nil {
			return nil, err
		}
		targets = []string{"SCHEMA::" + s}
	case MSOnObject:
		s, err := mssqlName("the schema", g.Schema)
		if err != nil {
			return nil, err
		}
		if len(g.Objects) == 0 {
			return nil, errors.New("choose at least one object")
		}
		if len(g.Objects) > 500 {
			return nil, errors.New("at most 500 at a time; grant on the schema instead")
		}
		for _, o := range g.Objects {
			n, err := mssqlName("an object's name", o)
			if err != nil {
				return nil, err
			}
			targets = append(targets, "OBJECT::"+s+"."+n)
		}
	}

	out := make([]string, 0, len(targets))
	for _, t := range targets {
		var s string
		switch kind {
		case AdminMSGrant:
			if g.Cascade {
				return nil, errors.New("cascade goes with revoke or deny")
			}
			s = "GRANT " + privs + " ON " + t + " TO " + grantee
			if g.WithGrant {
				s += " WITH GRANT OPTION"
			}
		case AdminMSRevoke:
			s = "REVOKE "
			if g.WithGrant {
				s += "GRANT OPTION FOR "
			}
			s += privs + " ON " + t + " FROM " + grantee
			if g.Cascade || g.WithGrant {
				// SQL Server needs CASCADE to take a grant option that was
				// used; asking for it when it was not is harmless.
				s += " CASCADE"
			}
		case AdminMSDeny:
			if g.WithGrant {
				return nil, errors.New("a deny has no grant option")
			}
			s = "DENY " + privs + " ON " + t + " TO " + grantee
			if g.Cascade {
				s += " CASCADE"
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// MSSQLAccessText is what an access operation runs, as one text with the
// password redacted: what is recorded, shown and approved.
func MSSQLAccessText(op *PGAdminOp, database string) (string, error) {
	_, text, err := MSSQLAccessSQL(op, database, "")
	if err != nil {
		return "", err
	}
	return strings.Join(text, ";\n"), nil
}

// MSSQLPasswordHash is SQL Server's own hash of a login password (2012 and
// later): 0x0200, a 4-byte salt, and SHA-512 of the password in UTF-16LE
// followed by the salt. CREATE or ALTER LOGIN ... PASSWORD = <it> HASHED sets
// the password without it crossing the wire.
func MSSQLPasswordHash(password string) (string, error) {
	salt := make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return mssqlHashWithSalt(password, salt), nil
}

func mssqlHashWithSalt(password string, salt []byte) string {
	u := utf16.Encode([]rune(password))
	b := make([]byte, 0, len(u)*2+len(salt))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	sum := sha512.Sum512(append(b, salt...))
	return "0x0200" + strings.ToUpper(hex.EncodeToString(salt)+hex.EncodeToString(sum[:]))
}
