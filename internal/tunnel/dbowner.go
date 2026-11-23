package tunnel

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

/*
Who owns a database made from Argus, and the user an application signs in
with. See plans/create-db-owner/design.md.

A create (and a change of owner later) names one of three owners:

	argus     Argus's own: the Owner-level role on Postgres, the admin login
	          on SQL Server, nobody on MySQL and MongoDB. Today's default.
	existing  a user already on the server, which is given the database
	new       a user made in the same step, with a password, for the app

"Owner" means what each engine can mean by it: Postgres's database owner,
SQL Server's dbo (ALTER AUTHORIZATION), and on MySQL and MongoDB every
privilege on that one database (GRANT ALL ON db.*, dbOwner on db).

The password is never part of a grant, a plan's text, a log line or
Activity. The control plane sends it to the agent in its own frame after the
grant, the agent turns it into the engine's hash where the engine takes one
(a SCRAM verifier for Postgres, SQL Server's salted SHA-512), and every
statement shown or recorded says PasswordRedacted where it would be. The
same builder makes the shown text and, with the secret, what runs.
*/

// Owner kinds.
const (
	OwnerArgus    = ""
	OwnerExisting = "existing"
	OwnerNew      = "new"
)

// CreateOwnedService is the create service for anything that names an
// owner user or sets its password. An agent from before owners refuses it
// as unknown, which the console turns into "update the agent", instead of
// making a database and quietly leaving the owner out.
const CreateOwnedService = "dbcreate-owner"

// CreateServiceFor is the service a create request goes to.
// A Postgres database owned by a role that is already there is not new:
// agents have made and changed those since Create new came, so it stays on
// the older service and keeps working with them.
func CreateServiceFor(c GrantCreate) string {
	switch {
	case c.Op == CreateOpSetPassword:
		return CreateOwnedService
	case c.Owner == nil || c.Owner.Kind == OwnerArgus:
		return CreateService
	case c.Owner.Kind == OwnerExisting && c.Engine == EnginePostgres:
		return CreateService
	}
	return CreateOwnedService
}

// CreateOpSetPassword gives the database's owner user a new password.
const CreateOpSetPassword = "set-password"

// FrameCreatePassword carries the new user's password, control plane to
// agent, right after the stream opens: never inside the signed grant.
const FrameCreatePassword byte = 'p'

// Owner password bounds.
const (
	MinOwnerPassword = 12
	MaxOwnerPassword = 128
)

// CreateOwner is who owns the database.
type CreateOwner struct {
	Kind string `json:"kind,omitempty"`
	// Name is the user, role or login.
	Name string `json:"name,omitempty"`
	// From is MySQL's host part ('%' when empty) and, for an existing
	// MongoDB user, the database it is defined in ("admin" when empty).
	From string `json:"from,omitempty"`
	// Policy applies SQL Server's password policy to a new login.
	Policy bool `json:"policy,omitempty"`
	// ConnLimit is a new Postgres role's most connections; 0 is no limit.
	ConnLimit int `json:"connLimit,omitempty"`
}

// OwnerEngine says whether an engine's databases have an owner Argus sets.
func OwnerEngine(engine string) bool {
	switch engine {
	case EnginePostgres, EngineMySQL, EngineMSSQL, EngineMongoDB:
		return true
	}
	return false
}

// BrokerOwnerEngine says whether an engine's new vhost or topic may come
// with its app's user (plans/broker-users/design.md, picture 08). Only on
// create: the user is changed afterwards on the entry's Users tab.
func BrokerOwnerEngine(engine string) bool {
	return engine == EngineRabbitMQ || engine == EngineKafka
}

// NewOwner says the operation makes a user and so needs a password.
func (c GrantCreate) NewOwner() bool {
	return c.Owner != nil && c.Owner.Kind == OwnerNew &&
		(c.Op == CreateOpCreate || c.Op == CreateOpSetOwner)
}

// NeedsPassword says the agent waits for a FrameCreatePassword.
func (c GrantCreate) NeedsPassword() bool { return c.NewOwner() || c.Op == CreateOpSetPassword }

// OwnerName is the user that will own the database, or "" for Argus's.
func (c GrantCreate) OwnerName() string {
	if c.Owner == nil || c.Owner.Kind == OwnerArgus {
		return ""
	}
	return c.Owner.Name
}

var (
	reMySQLUser   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	reMySQLHost   = regexp.MustCompile(`^[A-Za-z0-9.%:_/-]{1,60}$`)
	reMSSQLLogin  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.@#$-]{0,127}$`)
	reMSSQLWinLog = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}\\[A-Za-z0-9_.$ -]{1,63}$`)
	reMongoUser   = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
)

// reservedOwner are names no owner may have: the engines' own superusers
// and principals, and Argus's.
var reservedOwner = map[string]bool{"postgres": true, "root": true, "sa": true, "dbo": true, "guest": true,
	"sys": true, "public": true, "information_schema": true, "admin": true, "mysql.sys": true,
	"mysql.session": true, "mysql.infoschema": true, "mariadb.sys": true, "__system": true}

// normalizeOwner settles Owner and, for Postgres, PGCreate.Owner, which
// carries the role for the statements: an older console sends only the
// latter.
func (c *GrantCreate) normalizeOwner() {
	if c.Engine == EnginePostgres {
		if c.Postgres == nil {
			c.Postgres = &PGCreate{}
		}
		switch {
		case c.Owner == nil && c.Postgres.Owner != PGOwnerArgus:
			c.Owner = &CreateOwner{Kind: OwnerExisting, Name: c.Postgres.Owner}
		case c.Owner != nil:
			c.Postgres.Owner = c.OwnerName()
		}
	}
	if c.Owner != nil {
		c.Owner.Name = strings.TrimSpace(c.Owner.Name)
		c.Owner.From = strings.TrimSpace(c.Owner.From)
		if c.Owner.Kind == OwnerArgus {
			*c.Owner = CreateOwner{}
		}
	}
}

// validateOwner checks the owner against the engine.
func (c *GrantCreate) validateOwner() error {
	c.normalizeOwner()
	o := c.Owner
	if o == nil || o.Kind == OwnerArgus {
		if c.Op == CreateOpSetPassword {
			return errors.New("name the user whose password to set")
		}
		if c.Op == CreateOpSetOwner && c.Engine != EnginePostgres && c.Engine != EngineMSSQL {
			return errors.New("name the user that is to own it")
		}
		return nil
	}
	if BrokerOwnerEngine(c.Engine) {
		if c.Op != CreateOpCreate {
			return errors.New("a broker's app users are changed on the entry's Users tab")
		}
	} else if !OwnerEngine(c.Engine) {
		return errors.New(engineWord(c.Engine) + " has no owner to set")
	}
	switch o.Kind {
	case OwnerExisting, OwnerNew:
	default:
		return errors.New(`the owner is "existing", "new" or empty for Argus's`)
	}
	if c.Op == CreateOpSetPassword && o.Kind != OwnerExisting {
		return errors.New("a new password is for a user that is there")
	}
	low := strings.ToLower(o.Name)
	if o.Name == "" {
		return errors.New("the owner's name is required")
	}
	if strings.HasPrefix(low, "argus") {
		return errors.New("names beginning argus are Argus's own")
	}
	if reservedOwner[low] {
		return errors.New(o.Name + " is the server's own and cannot own a database here")
	}
	if o.ConnLimit < 0 || o.ConnLimit > 100_000 {
		return errors.New("most connections must be between 1 and 100,000, or empty for no limit")
	}
	if o.ConnLimit > 0 && (c.Engine != EnginePostgres || o.Kind != OwnerNew) {
		return errors.New("most connections is for a new Postgres role")
	}
	if o.Policy && (c.Engine != EngineMSSQL || o.Kind != OwnerNew) {
		return errors.New("the password policy is for a new SQL Server login")
	}
	switch c.Engine {
	case EnginePostgres:
		if err := validPGOwner(o.Name); err != nil {
			return err
		}
		if o.From != "" {
			return errors.New("a Postgres role has no host part")
		}
	case EngineMySQL:
		if !reMySQLUser.MatchString(o.Name) {
			return errors.New("a MySQL user name is 1 to 32 letters, digits, _ . or -")
		}
		if o.From == "" {
			o.From = "%"
		}
		if !reMySQLHost.MatchString(o.From) {
			return errors.New("where it signs in from is %, localhost, an address, or a pattern such as 10.20.%")
		}
	case EngineMSSQL:
		ok := reMSSQLLogin.MatchString(o.Name) || (o.Kind == OwnerExisting && reMSSQLWinLog.MatchString(o.Name))
		if !ok {
			return errors.New("a login is a letter or _, then letters, digits, _ . @ # $ or - (a Windows login is DOMAIN\\name)")
		}
		if o.From != "" {
			return errors.New("a SQL Server login has no host part")
		}
	case EngineRabbitMQ:
		if !rabbitNewUserRE.MatchString(o.Name) {
			return errors.New("a RabbitMQ user name is 1 to 64 letters, digits, . _ @ or -")
		}
		if o.From != "" {
			return errors.New("a RabbitMQ user has no host part")
		}
	case EngineKafka:
		if !kafkaNewUserRE.MatchString(o.Name) {
			return errors.New("a Kafka user name is 1 to 64 letters, digits, . _ or -")
		}
		if o.From != "" {
			return errors.New("a Kafka user has no host part")
		}
	case EngineMongoDB:
		if !reMongoUser.MatchString(o.Name) {
			return errors.New("a MongoDB user name is 1 to 64 letters, digits, _ . @ or -")
		}
		if o.Kind == OwnerNew {
			o.From = ""
		} else if o.From == "" {
			o.From = "admin"
		} else if !ValidMongoDB(o.From) && o.From != "admin" {
			return errors.New("the database the user is defined in must be a database name")
		}
	}
	return nil
}

func engineWord(engine string) string {
	switch engine {
	case EngineRabbitMQ:
		return "a RabbitMQ vhost"
	case EngineKafka:
		return "a Kafka topic"
	case EngineRedis:
		return "Redis"
	}
	return engine
}

// OwnerPasswordProblem says why a password will not do, or "".
func OwnerPasswordProblem(c GrantCreate, pw string) string {
	switch {
	case len([]rune(pw)) < MinOwnerPassword:
		return fmt.Sprintf("the password must be at least %d characters", MinOwnerPassword)
	case len([]rune(pw)) > MaxOwnerPassword:
		return fmt.Sprintf("the password must be at most %d characters", MaxOwnerPassword)
	}
	for _, r := range pw {
		if unicode.IsControl(r) {
			return "the password must have no control characters"
		}
	}
	if c.Engine == EngineMSSQL && c.Owner != nil && c.Owner.Policy {
		kinds := 0
		for _, f := range []func(rune) bool{unicode.IsUpper, unicode.IsLower, unicode.IsDigit,
			func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }} {
			for _, r := range pw {
				if f(r) {
					kinds++
					break
				}
			}
		}
		if kinds < 3 {
			return "the Windows password policy wants three of: capitals, small letters, digits, symbols"
		}
	}
	return ""
}

func mysqlAccount(o *CreateOwner) string { return MySQLString(o.Name) + "@" + MySQLString(o.From) }

// OwnerPlan is what the owner adds around making the database: pre runs
// before it (making the user), post after (giving it the database). secret
// is what stands where the password goes: PasswordRedacted for the text a
// person sees, the engine's quoted hash or literal for what runs.
func OwnerPlan(c GrantCreate, secret string) (pre, post []string) {
	o := c.Owner
	if o == nil || o.Kind == OwnerArgus {
		return nil, nil
	}
	newUser := o.Kind == OwnerNew
	switch c.Engine {
	case EnginePostgres:
		if newUser {
			s := "CREATE ROLE " + PGIdent(o.Name) + " LOGIN PASSWORD " + secret
			if o.ConnLimit > 0 {
				s += " CONNECTION LIMIT " + strconv.Itoa(o.ConnLimit)
			}
			pre = append(pre, s, "COMMENT ON ROLE "+PGIdent(o.Name)+" IS "+PGLiteral(PGOwnerComment(c.Name)))
		}
	case EngineMySQL:
		if newUser {
			pre = append(pre, "CREATE USER "+mysqlAccount(o)+" IDENTIFIED BY "+secret)
		}
		post = append(post, "GRANT ALL PRIVILEGES ON "+MySQLGrantDB(c.Name)+".* TO "+mysqlAccount(o))
	case EngineMSSQL:
		if newUser {
			policy := "OFF"
			if o.Policy {
				policy = "ON"
			}
			pre = append(pre, "CREATE LOGIN "+MSSQLIdent(o.Name)+" WITH PASSWORD = "+secret+
				" HASHED, CHECK_POLICY = "+policy+", CHECK_EXPIRATION = OFF")
		}
		post = append(post, "ALTER AUTHORIZATION ON DATABASE::"+MSSQLIdent(c.Name)+" TO "+MSSQLIdent(o.Name))
		if newUser {
			post = append(post, "ALTER LOGIN "+MSSQLIdent(o.Name)+" WITH DEFAULT_DATABASE = "+MSSQLIdent(c.Name))
		}
	case EngineRabbitMQ:
		if newUser {
			pre = append(pre, "rabbitmqctl add_user "+shellWord(o.Name)+" "+secret)
		}
		post = append(post, "rabbitmqctl set_permissions -p "+shellWord(c.Name)+" "+shellWord(o.Name)+` ".*" ".*" ".*"`)
	case EngineKafka:
		if newUser {
			pre = append(pre, "AlterUserSCRAMs upsert "+o.Name+" SCRAM-SHA-512 password "+secret)
		}
		post = append(post, "CreateACLs User:"+o.Name+" ALLOW READ, WRITE, DESCRIBE on topic LITERAL "+c.Name,
			"CreateACLs User:"+o.Name+" ALLOW READ on group PREFIXED "+c.Name)
	case EngineMongoDB:
		if newUser {
			post = append(post, fmt.Sprintf(`db.createUser({ user: %q, pwd: %s, roles: [{ role: "dbOwner", db: %q }] })`,
				o.Name, secret, c.Name))
		} else {
			post = append(post, "use "+o.From, fmt.Sprintf(`db.grantRolesToUser(%q, [{ role: "dbOwner", db: %q }])`, o.Name, c.Name))
		}
	}
	return pre, post
}

// PGOwnerComment is the comment on a Postgres role Argus made to own a
// database. It does not begin "argus-managed": the role is the app's, and
// Argus must never lock it between sessions.
func PGOwnerComment(database string) string { return "Owner of " + database + ", made in Argus" }

// CreatePlan is what a create runs, in order, as a person sees it: the
// owner's user, the database, then giving it to the owner. The agent runs
// the same statements, with the password's hash where this says
// PasswordRedacted. For MongoDB they describe the commands it sends.
func CreatePlan(c GrantCreate) []string {
	pre, post := OwnerPlan(c, PasswordRedacted)
	out := append([]string{}, pre...)
	out = append(out, CreateDBPlan(c)...)
	return append(out, post...)
}

// SetOwnerPlan is what changing a database's owner runs, as a person sees
// it.
func SetOwnerPlan(c GrantCreate) []string { return SetOwnerPlanWith(c, PasswordRedacted) }

// SetOwnerPlanWith is SetOwnerPlan with secret where the password goes.
func SetOwnerPlanWith(c GrantCreate, secret string) []string {
	pre, post := OwnerPlan(c, secret)
	out := append([]string{}, pre...)
	switch c.Engine {
	case EnginePostgres, "":
		out = append(out, PGAlterOwnerSQL(c))
	case EngineMSSQL:
		if c.OwnerName() == "" {
			// Back to Argus's admin login, which is what made it.
			out = append(out, "ALTER AUTHORIZATION ON DATABASE::"+MSSQLIdent(c.Name)+" TO "+MSSQLIdent(MSSQLAdminPlaceholder))
		}
	case EngineMongoDB:
		if c.Owner != nil && c.Owner.Kind == OwnerNew {
			out = append(out, "use "+c.Name)
		}
	}
	return append(out, post...)
}

// MSSQLAdminPlaceholder stands for the admin login's name in a preview; the
// agent knows the real one.
const MSSQLAdminPlaceholder = "the admin login"

// PGAlterOwnerSQL is ALTER DATABASE … OWNER TO the owner.
func PGAlterOwnerSQL(c GrantCreate) string {
	return "ALTER DATABASE " + PGIdent(c.Name) + " OWNER TO " + PGIdent(c.PGOwnerRole())
}

// SetPasswordPlan is what giving the owner a new password runs, as a
// person sees it.
func SetPasswordPlan(c GrantCreate) []string { return SetPasswordPlanWith(c, PasswordRedacted) }

// SetPasswordPlanWith is SetPasswordPlan with secret where the password goes.
func SetPasswordPlanWith(c GrantCreate, secret string) []string {
	o := c.Owner
	if o == nil {
		return nil
	}
	switch c.Engine {
	case EnginePostgres:
		return []string{"ALTER ROLE " + PGIdent(o.Name) + " PASSWORD " + secret}
	case EngineMySQL:
		return []string{"ALTER USER " + mysqlAccount(o) + " IDENTIFIED BY " + secret}
	case EngineMSSQL:
		return []string{"ALTER LOGIN " + MSSQLIdent(o.Name) + " WITH PASSWORD = " + secret + " HASHED"}
	case EngineMongoDB:
		return []string{"use " + o.From, fmt.Sprintf(`db.updateUser(%q, { pwd: %s })`, o.Name, secret)}
	}
	return nil
}

// OwnerUser is a user the dialog offers as an owner.
type OwnerUser struct {
	Name string `json:"name"`
	// From is MySQL's host part, or the MongoDB database it lives in.
	From string `json:"from,omitempty"`
	// Reach says, in a few words, what it reaches today.
	Reach string `json:"reach,omitempty"`
	// All says it already reaches every database (a superuser, root, sa):
	// shown, but greyed.
	All bool `json:"all,omitempty"`
}

// PoolerInfo is a PgBouncer in front of a Postgres, as the create dialog
// needs to know it.
type PoolerInfo struct {
	Port   int    `json:"port"`
	Config string `json:"config"`
	// Covers says a [databases] line for "*" already sends every name.
	Covers bool `json:"covers,omitempty"`
	// Auth is how it learns passwords: "argus" (Argus's auth_query),
	// "file" (its userlist.txt), "query" (someone else's auth_query) or
	// "trust".
	Auth string `json:"auth"`
	// Mode is its pool_mode: session, transaction or statement.
	Mode string `json:"mode,omitempty"`
	// Writable says the agent can change its files.
	Writable bool `json:"writable"`
}

// PoolerAppsRole is the no-login role whose members Argus's PgBouncer lookup
// answers for besides people's roles: application roles Argus made or was
// told own a database.
const PoolerAppsRole = "argus_pooled_apps"

// PoolerLineMarker is the comment line before each line Argus adds to
// pgbouncer.ini or userlist.txt, so it changes only its own.
const PoolerLineMarker = "# argus:"

// PoolerDatabaseLine is the [databases] line that sends name to Postgres at
// host:port, with no fixed user, so the app signs in as itself.
func PoolerDatabaseLine(name, host string, port int) string {
	return name + " = host=" + host + " port=" + strconv.Itoa(port) + " dbname=" + name
}

// PoolerUserLine is a userlist.txt line: the user and its SCRAM verifier.
func PoolerUserLine(user, verifier string) string {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	return q(user) + " " + q(verifier)
}
