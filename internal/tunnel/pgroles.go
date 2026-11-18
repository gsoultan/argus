package tunnel

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

/*
Roles and privileges, the way pgAdmin manages them: create, change and drop
roles, give and take membership, grant and revoke privileges, and the default
privileges that cover objects made later.

These are administrative operations (PGAdminOp), not statements typed into the
editor, for two reasons found the hard way. A GRANT run as the DBA level role
on a table it does not own and holds no grant option on succeeds with only a
WARNING ("no privileges were granted") -- the person sees success and nothing
changed. And a role's password must never be written into the recording. So
the agent hands these to its pg-admin helper (the server's postgres account),
which grants as if it were each object's owner, and the password becomes a
SCRAM verifier inside the agent before anything else sees it.

The builder is pure and lives here so that the control plane, which decides
whether a change waits for approval, and the agent, which records and runs
it, compute exactly the same text. Approval is for that redacted text.

What it refuses, and why:
  - a role whose name begins pg_ (Postgres reserves them);
  - a comment beginning "argus-managed" (Argus would then treat the role as
    its own and lock it between sessions);
  - ALL / names / privileges it does not know, rather than passing them on.
The helper refuses more, with the database in hand: any role Argus manages
(people's and level roles are changed from the database's settings), and
taking SUPERUSER or LOGIN from the account the helper signs in as.
*/

// Role and privilege operation kinds.
const (
	// AdminRoleList lists every role on the server with its attributes,
	// memberships, comment and connections to this database.
	AdminRoleList = "role.list"
	// AdminRolePrivileges says what one role (RoleSpec.Name) may do in this
	// database: on it, its schemas, tables, sequences and functions, and the
	// default privileges that name it.
	AdminRolePrivileges = "role.privileges"
	// AdminGrantObjects lists the schemas, and for GrantSpec.Schema its
	// tables, sequences and functions, with their owners: what a grant can
	// name.
	AdminGrantObjects = "grant.objects"

	AdminRoleCreate   = "role.create"
	AdminRoleAlter    = "role.alter"
	AdminRoleDrop     = "role.drop"
	AdminRoleMember   = "role.member"
	AdminRoleUnmember = "role.unmember"
	AdminGrant        = "grant"
	AdminRevoke       = "revoke"
)

// What a grant is on.
const (
	OnDatabase     = "database"
	OnSchema       = "schema"
	OnTable        = "table"
	OnSequence     = "sequence"
	OnFunction     = "function"
	OnAllTables    = "all-tables"
	OnAllSequences = "all-sequences"
	OnAllFunctions = "all-functions"
)

// PGRoleSpec is a role to create, change or drop. A nil attribute is left as
// it is (on create, as Postgres's default).
type PGRoleSpec struct {
	Name string `json:"name"`
	// Rename is the new name, for role.alter.
	Rename      string `json:"rename,omitempty"`
	Login       *bool  `json:"login,omitempty"`
	Superuser   *bool  `json:"superuser,omitempty"`
	CreateDB    *bool  `json:"createdb,omitempty"`
	CreateRole  *bool  `json:"createrole,omitempty"`
	Inherit     *bool  `json:"inherit,omitempty"`
	Replication *bool  `json:"replication,omitempty"`
	BypassRLS   *bool  `json:"bypassrls,omitempty"`
	// ConnLimit is the most connections at once; -1 is no limit.
	ConnLimit *int `json:"connLimit,omitempty"`
	// ValidUntil is when the password stops working: "infinity", or a date
	// and optional time ("2027-01-31", "2027-01-31 18:00 UTC").
	ValidUntil *string `json:"validUntil,omitempty"`
	// Password is the new password. The agent turns it into a SCRAM
	// verifier and clears it before the operation goes any further; it is
	// never recorded, audited or sent to the helper.
	Password string `json:"password,omitempty"`
	// NoPassword removes the role's password.
	NoPassword bool `json:"noPassword,omitempty"`
	// Comment is the role's description; "" removes it.
	Comment *string `json:"comment,omitempty"`
	// MemberOf are roles a new role joins, for role.create.
	MemberOf []string `json:"memberOf,omitempty"`
	// For role.drop: whose its objects in this database become, and
	// whether to drop what it still owns and its privileges here.
	ReassignTo string `json:"reassignTo,omitempty"`
	DropOwned  bool   `json:"dropOwned,omitempty"`
}

// PGMemberSpec is membership: Member joins (or leaves) Role.
type PGMemberSpec struct {
	Role   string `json:"role"`
	Member string `json:"member"`
	// Admin is WITH ADMIN OPTION: the member may give Role to others.
	Admin bool `json:"admin,omitempty"`
}

// PGGrantSpec is a grant or revoke of privileges.
type PGGrantSpec struct {
	// Grantee is the role that gets or loses them, or "PUBLIC".
	Grantee string `json:"grantee"`
	// On is one of the On* kinds.
	On string `json:"on"`
	// Schema is the schema, for every kind but database.
	Schema string `json:"schema,omitempty"`
	// Objects are the tables or sequences (names in Schema), or functions
	// as name(argument types), for table, sequence and function.
	Objects    []string `json:"objects,omitempty"`
	Privileges []string `json:"privileges"`
	// WithGrant is WITH GRANT OPTION, for a grant; for a revoke it takes
	// only the grant option (GRANT OPTION FOR).
	WithGrant bool `json:"withGrant,omitempty"`
	// SchemaUsage also grants USAGE on Schema, without which nothing in it
	// can be reached.
	SchemaUsage bool `json:"schemaUsage,omitempty"`
	// Future also sets the default privileges of ForRoles in Schema, so
	// objects they make later are covered too (all-* kinds only).
	Future   bool     `json:"future,omitempty"`
	ForRoles []string `json:"forRoles,omitempty"`
	// Cascade, for a revoke, also takes what the grantee gave on from it.
	Cascade bool `json:"cascade,omitempty"`
}

// PGRoleKind says whether kind is a role or privilege operation.
func PGRoleKind(kind string) bool {
	switch kind {
	case AdminRoleList, AdminRolePrivileges, AdminGrantObjects, AdminRoleCreate, AdminRoleAlter,
		AdminRoleDrop, AdminRoleMember, AdminRoleUnmember, AdminGrant, AdminRevoke:
		return true
	}
	return false
}

// PGRoleAlwaysApproval says the change waits for someone else's approval on
// every database, whatever its rules: making a role a superuser hands it the
// whole server, including every other database and the files Postgres can
// read.
func PGRoleAlwaysApproval(op *PGAdminOp) bool {
	if op == nil || op.Preview || op.RoleSpec == nil {
		return false
	}
	if op.Kind != AdminRoleCreate && op.Kind != AdminRoleAlter {
		return false
	}
	return op.RoleSpec.Superuser != nil && *op.RoleSpec.Superuser
}

// PGRoleTextMakesSuperuser says text, as PGRoleText writes it, creates or
// changes a role WITH SUPERUSER: an approval for it may be asked on any
// database.
func PGRoleTextMakesSuperuser(text string) bool {
	first, _, _ := strings.Cut(text, ";\n")
	if !strings.HasPrefix(first, "CREATE ROLE ") && !strings.HasPrefix(first, "ALTER ROLE ") {
		return false
	}
	_, attrs, ok := strings.Cut(first, `" WITH `)
	return ok && slices.Contains(strings.Fields(attrs), "SUPERUSER")
}

// PGRoleSQL is a role or privilege operation's statements, in order: run is
// what the helper runs, text the same with the password redacted. database
// is the database the session is on. verifier is the SCRAM verifier of
// RoleSpec.Password; with none, run holds a placeholder and is only fit to
// be shown.
func PGRoleSQL(op *PGAdminOp, database, verifier string) (run, text []string, err error) {
	if op == nil {
		return nil, nil, errors.New("no operation")
	}
	both := func(s ...string) { run = append(run, s...); text = append(text, s...) }
	switch op.Kind {
	case AdminRoleList, AdminRolePrivileges, AdminGrantObjects:
		return nil, nil, nil

	case AdminRoleCreate, AdminRoleAlter:
		r := op.RoleSpec
		if r == nil {
			return nil, nil, errors.New("which role?")
		}
		name, err := roleIdent(r.Name)
		if err != nil {
			return nil, nil, err
		}
		attrs, err := roleAttrs(r)
		if err != nil {
			return nil, nil, err
		}
		pw, pwText := "", ""
		switch {
		case r.Password != "" && r.NoPassword:
			return nil, nil, errors.New("set a password or remove it, not both")
		case r.Password != "":
			if len(r.Password) > 1024 {
				return nil, nil, errors.New("a password is at most 1,024 characters")
			}
			v := verifier
			if v == "" {
				v = "[redacted]"
			}
			pw, pwText = " PASSWORD "+QuoteLiteral(v), " PASSWORD [redacted]"
		case r.NoPassword:
			pw, pwText = " PASSWORD NULL", " PASSWORD NULL"
		}
		verb := "CREATE ROLE "
		if op.Kind == AdminRoleAlter {
			verb = "ALTER ROLE "
		}
		if attrs != "" || pw != "" || op.Kind == AdminRoleCreate {
			s := verb + name
			if attrs != "" || pw != "" {
				s += " WITH" + attrs
			}
			run = append(run, s+pw)
			text = append(text, s+pwText)
		}
		if r.Comment != nil {
			c := strings.TrimSpace(*r.Comment)
			if strings.HasPrefix(c, "argus-managed") {
				return nil, nil, errors.New("a comment beginning argus-managed is Argus's own mark; choose other words")
			}
			if len(c) > 1024 {
				return nil, nil, errors.New("a comment is at most 1,024 characters")
			}
			if c == "" {
				both("COMMENT ON ROLE " + name + " IS NULL")
			} else {
				both("COMMENT ON ROLE " + name + " IS " + QuoteLiteral(c))
			}
		}
		if op.Kind == AdminRoleCreate {
			for _, g := range r.MemberOf {
				gi, err := roleIdent(g)
				if err != nil {
					return nil, nil, err
				}
				both("GRANT " + gi + " TO " + name)
			}
		} else if len(r.MemberOf) > 0 {
			return nil, nil, errors.New("change membership one role at a time")
		}
		if op.Kind == AdminRoleAlter && r.Rename != "" && r.Rename != r.Name {
			to, err := roleIdent(r.Rename)
			if err != nil {
				return nil, nil, err
			}
			both("ALTER ROLE " + name + " RENAME TO " + to)
		}
		if len(run) == 0 {
			return nil, nil, errors.New("nothing to change")
		}
		return run, text, nil

	case AdminRoleDrop:
		r := op.RoleSpec
		if r == nil {
			return nil, nil, errors.New("which role?")
		}
		name, err := roleIdent(r.Name)
		if err != nil {
			return nil, nil, err
		}
		if r.ReassignTo != "" {
			to, err := roleIdent(r.ReassignTo)
			if err != nil {
				return nil, nil, err
			}
			if r.ReassignTo == r.Name {
				return nil, nil, errors.New("hand its objects to another role")
			}
			both("REASSIGN OWNED BY " + name + " TO " + to)
		}
		if r.DropOwned {
			both("DROP OWNED BY " + name)
		}
		both("DROP ROLE " + name)
		return run, text, nil

	case AdminRoleMember, AdminRoleUnmember:
		m := op.Member
		if m == nil {
			return nil, nil, errors.New("which membership?")
		}
		role, err := roleIdent(m.Role)
		if err != nil {
			return nil, nil, err
		}
		member, err := roleIdent(m.Member)
		if err != nil {
			return nil, nil, err
		}
		if m.Role == m.Member {
			return nil, nil, errors.New("a role cannot be a member of itself")
		}
		if op.Kind == AdminRoleMember {
			s := "GRANT " + role + " TO " + member
			if m.Admin {
				s += " WITH ADMIN OPTION"
			}
			both(s)
		} else if m.Admin {
			both("REVOKE ADMIN OPTION FOR " + role + " FROM " + member)
		} else {
			both("REVOKE " + role + " FROM " + member)
		}
		return run, text, nil

	case AdminGrant, AdminRevoke:
		s, err := grantSQL(op.GrantSpec, op.Kind == AdminGrant, database)
		if err != nil {
			return nil, nil, err
		}
		both(s...)
		return run, text, nil
	}
	return nil, nil, fmt.Errorf("unknown operation %q", op.Kind)
}

// PGRoleText is what a role or privilege operation runs, as one text with
// the password redacted: what is recorded, shown and approved.
func PGRoleText(op *PGAdminOp, database string) (string, error) {
	_, text, err := PGRoleSQL(op, database, "")
	if err != nil {
		return "", err
	}
	return strings.Join(text, ";\n"), nil
}

// QuoteIdent quotes any identifier Postgres would accept.
func QuoteIdent(name string) (string, error) {
	if name == "" {
		return "", errors.New("a name is required")
	}
	if len(name) > 63 {
		return "", fmt.Errorf("%q is longer than 63 characters", name)
	}
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("a name cannot contain NUL")
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// QuoteLiteral quotes a string literal so it means the same whatever
// standard_conforming_strings is set to (see the agent's quoteLiteral).
func QuoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "E'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// roleIdent quotes a role name, refusing Postgres's reserved ones.
func roleIdent(name string) (string, error) {
	if strings.HasPrefix(name, "pg_") {
		return "", fmt.Errorf("%s is one of Postgres's own roles", name)
	}
	if strings.EqualFold(name, "public") {
		return "", errors.New("PUBLIC is everyone, not a role")
	}
	return QuoteIdent(name)
}

var validUntilRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}( \d{2}:\d{2}(:\d{2})?)?( ?(UTC|Z|[+-]\d{2}(:?\d{2})?))?$`)

func roleAttrs(r *PGRoleSpec) (string, error) {
	var b strings.Builder
	flag := func(v *bool, on, off string) {
		if v != nil {
			if *v {
				b.WriteString(" " + on)
			} else {
				b.WriteString(" " + off)
			}
		}
	}
	flag(r.Login, "LOGIN", "NOLOGIN")
	flag(r.Superuser, "SUPERUSER", "NOSUPERUSER")
	flag(r.CreateDB, "CREATEDB", "NOCREATEDB")
	flag(r.CreateRole, "CREATEROLE", "NOCREATEROLE")
	flag(r.Inherit, "INHERIT", "NOINHERIT")
	flag(r.Replication, "REPLICATION", "NOREPLICATION")
	flag(r.BypassRLS, "BYPASSRLS", "NOBYPASSRLS")
	if r.ConnLimit != nil {
		if *r.ConnLimit < -1 || *r.ConnLimit > 100000 {
			return "", errors.New("the connection limit is -1 (none) or a number of connections")
		}
		b.WriteString(" CONNECTION LIMIT " + strconv.Itoa(*r.ConnLimit))
	}
	if r.ValidUntil != nil {
		v := strings.TrimSpace(*r.ValidUntil)
		if v == "" || strings.EqualFold(v, "infinity") {
			v = "infinity"
		} else if !validUntilRE.MatchString(v) {
			return "", fmt.Errorf("%q is not a date (2027-01-31, or 2027-01-31 18:00 UTC)", v)
		}
		b.WriteString(" VALID UNTIL " + QuoteLiteral(v))
	}
	return b.String(), nil
}

// Privileges each kind of object takes, in Postgres's order.
var grantPrivileges = map[string][]string{
	OnDatabase: {"CONNECT", "CREATE", "TEMPORARY"},
	OnSchema:   {"USAGE", "CREATE"},
	OnTable:    {"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"},
	OnSequence: {"USAGE", "SELECT", "UPDATE"},
	OnFunction: {"EXECUTE"},
}

// GrantPrivilegesFor lists the privileges an On kind takes.
func GrantPrivilegesFor(on string) []string {
	switch on {
	case OnAllTables:
		on = OnTable
	case OnAllSequences:
		on = OnSequence
	case OnAllFunctions:
		on = OnFunction
	}
	return grantPrivileges[on]
}

// funcSigRE is a function as name(argument types), the way the helper's
// grant.objects lists it: a quoted or plain name, optionally schema-
// qualified, and types made of letters, digits, spaces, commas, dots,
// brackets and double quotes. No quote, semicolon, dash or slash, so it
// cannot end the statement or open a comment.
var funcSigRE = regexp.MustCompile(`^(?:"(?:[^"\x00]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)(?:\.(?:"(?:[^"\x00]|"")+"|[A-Za-z_][A-Za-z0-9_$]*))?\([A-Za-z0-9_ ,.\[\]"$]*\)$`)

func grantSQL(g *PGGrantSpec, grant bool, database string) ([]string, error) {
	if g == nil {
		return nil, errors.New("what is being granted?")
	}
	var grantee string
	if strings.EqualFold(g.Grantee, "public") {
		grantee = "PUBLIC"
	} else {
		var err error
		if grantee, err = roleIdent(g.Grantee); err != nil {
			return nil, err
		}
	}
	allowed := GrantPrivilegesFor(g.On)
	if allowed == nil {
		return nil, fmt.Errorf("cannot grant on %q", g.On)
	}
	if len(g.Privileges) == 0 {
		return nil, errors.New("choose at least one privilege")
	}
	privs := "ALL PRIVILEGES"
	if !slices.Contains(g.Privileges, "ALL") {
		var keep []string
		for _, p := range allowed {
			if slices.Contains(g.Privileges, p) {
				keep = append(keep, p)
			}
		}
		for _, p := range g.Privileges {
			if !slices.Contains(allowed, p) {
				return nil, fmt.Errorf("%s is not a privilege on a %s", p, strings.TrimPrefix(g.On, "all-"))
			}
		}
		privs = strings.Join(keep, ", ")
	} else if len(g.Privileges) > 1 {
		return nil, errors.New("ALL is every privilege; choose it alone")
	}

	var schema string
	if g.On != OnDatabase {
		var err error
		if schema, err = QuoteIdent(g.Schema); err != nil {
			return nil, fmt.Errorf("the schema: %w", err)
		}
	}
	var target string
	switch g.On {
	case OnDatabase:
		d, err := QuoteIdent(database)
		if err != nil {
			return nil, fmt.Errorf("the database: %w", err)
		}
		target = "DATABASE " + d
	case OnSchema:
		target = "SCHEMA " + schema
	case OnTable, OnSequence, OnFunction:
		if len(g.Objects) == 0 {
			return nil, errors.New("choose at least one " + g.On)
		}
		if len(g.Objects) > 1000 {
			return nil, errors.New("at most 1,000 at a time; use every one in the schema instead")
		}
		names := make([]string, 0, len(g.Objects))
		for _, o := range g.Objects {
			if g.On == OnFunction {
				if !funcSigRE.MatchString(o) {
					return nil, fmt.Errorf("%q is not a function as name(argument types)", o)
				}
				if !strings.Contains(strings.SplitN(o, "(", 2)[0], ".") {
					o = schema + "." + o
				}
				names = append(names, o)
				continue
			}
			n, err := QuoteIdent(o)
			if err != nil {
				return nil, err
			}
			names = append(names, schema+"."+n)
		}
		target = map[string]string{OnTable: "TABLE ", OnSequence: "SEQUENCE ", OnFunction: "ROUTINE "}[g.On] + strings.Join(names, ", ")
	case OnAllTables:
		target = "ALL TABLES IN SCHEMA " + schema
	case OnAllSequences:
		target = "ALL SEQUENCES IN SCHEMA " + schema
	case OnAllFunctions:
		target = "ALL ROUTINES IN SCHEMA " + schema
	}

	var out []string
	if g.SchemaUsage {
		if !grant {
			return nil, errors.New("schema usage goes with a grant; revoke it on the schema itself")
		}
		if g.On == OnDatabase || g.On == OnSchema {
			return nil, errors.New("schema usage goes with what is in a schema")
		}
		out = append(out, "GRANT USAGE ON SCHEMA "+schema+" TO "+grantee)
	}
	if grant {
		s := "GRANT " + privs + " ON " + target + " TO " + grantee
		if g.WithGrant {
			s += " WITH GRANT OPTION"
		}
		out = append(out, s)
	} else {
		s := "REVOKE "
		if g.WithGrant {
			s += "GRANT OPTION FOR "
		}
		s += privs + " ON " + target + " FROM " + grantee
		if g.Cascade {
			s += " CASCADE"
		}
		out = append(out, s)
	}
	if g.Future {
		objs := map[string]string{OnAllTables: "TABLES", OnAllSequences: "SEQUENCES", OnAllFunctions: "FUNCTIONS"}[g.On]
		if objs == "" {
			return nil, errors.New("objects made later go with every table, sequence or function in a schema")
		}
		if len(g.ForRoles) == 0 {
			return nil, errors.New("whose new objects? name the roles that create them")
		}
		for _, fr := range g.ForRoles {
			f, err := roleIdent(fr)
			if err != nil {
				return nil, err
			}
			s := "ALTER DEFAULT PRIVILEGES FOR ROLE " + f + " IN SCHEMA " + schema
			if grant {
				s += " GRANT " + privs + " ON " + objs + " TO " + grantee
				if g.WithGrant {
					s += " WITH GRANT OPTION"
				}
			} else {
				s += " REVOKE "
				if g.WithGrant {
					s += "GRANT OPTION FOR "
				}
				s += privs + " ON " + objs + " FROM " + grantee
				if g.Cascade {
					s += " CASCADE"
				}
			}
			out = append(out, s)
		}
	}
	return out, nil
}
