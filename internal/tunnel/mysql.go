package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

/*
MySQL and MariaDB, one engine: they speak the same protocol and enforce access
the same way, with users and GRANTs.

A session speaks the Postgres session's frames (PGRequest, PGAnswer, row
batches, PGRecord), so the console's SQL workspace, the recording,
verification, watch live and replay are the ones Postgres has. What the agent
does behind them differs: see internal/agent/mysql.go.

One entry is one database (a schema, in MySQL's words) on one server. Each
person gets a user per level, MySQLUser, with the level's privileges on that
database and nothing else. Between sessions the user is locked; a session
gives it a one-time password and unlocks it, and the last session to close
locks it again and ends its connections. Users are kept, not dropped, because
a view or routine an owner made names its user as DEFINER and would stop
working if the user went.
*/

// MySQL engine and service.
const (
	EngineMySQL  = "mysql"
	MySQLService = "mysql"
)

// MySQLMaxUser is the longest user name MySQL takes (MariaDB takes more).
const MySQLMaxUser = 32

// MySQLUser is the MySQL user a person's sessions at one level open as:
// StoreUser's name, or when that is longer than MySQL allows, the role cut
// short with a hash of the whole of it, so two long logins never share one.
func MySQLUser(role, level string) string {
	u := StoreUser(role, level)
	if len(u) <= MySQLMaxUser {
		return u
	}
	sum := sha256.Sum256([]byte(role))
	tag := hex.EncodeToString(sum[:])[:6]
	keep := MySQLMaxUser - len(StoreUserPrefix) - len(tag) - len(level) - 2
	return StoreUserPrefix + role[:keep] + tag + "-" + level
}

// mysqlSystemDBs are MySQL's own schemas, never an entry.
var mysqlSystemDBs = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
}

// IsMySQLSystemDB reports whether name is one of MySQL's own schemas.
func IsMySQLSystemDB(name string) bool { return mysqlSystemDBs[strings.ToLower(name)] }

// ValidMySQLDB reports whether name can be an entry: 1 to 64 characters of
// text, no NUL, no trailing space, not one of MySQL's own schemas. Anything
// else MySQL allows in a quoted name is allowed; MySQLIdent quotes it.
func ValidMySQLDB(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 64 && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "\x00/\\.") && strings.TrimRight(name, " ") == name &&
		!IsMySQLSystemDB(name)
}

// MySQLIdent quotes an identifier with backticks.
func MySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// MySQLGrantDB quotes a database name for GRANT and REVOKE, where _ and % in
// a database name are wildcards unless escaped: `app_db` would otherwise
// also grant on appXdb.
func MySQLGrantDB(name string) string {
	r := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	return MySQLIdent(r.Replace(name))
}

// MySQLString quotes a string literal. Backslashes are doubled so it means
// the same whether or not the server has NO_BACKSLASH_ESCAPES.
func MySQLString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// MySQLPrivileges are what each level holds on its database, least to most.
// Each includes the one below it.
var MySQLPrivileges = map[string][]string{
	LevelRead:  {"SELECT", "SHOW VIEW"},
	LevelWrite: {"SELECT", "SHOW VIEW", "INSERT", "UPDATE", "DELETE", "EXECUTE", "LOCK TABLES", "CREATE TEMPORARY TABLES"},
	LevelOwner: {"SELECT", "SHOW VIEW", "INSERT", "UPDATE", "DELETE", "EXECUTE", "LOCK TABLES", "CREATE TEMPORARY TABLES",
		"CREATE", "ALTER", "DROP", "INDEX", "REFERENCES", "CREATE VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "TRIGGER", "EVENT"},
	LevelDBA: {"ALL PRIVILEGES"},
}

// MySQLGrantSQL is what gives a level's user its privileges on db: the
// database's privileges, and for dba the grant option there and PROCESS on
// the server, to see every connection's statement in Activity. Nothing is
// granted on any other database, and never SUPER.
func MySQLGrantSQL(user, host, db, level string) []string {
	privs := MySQLPrivileges[level]
	if len(privs) == 0 {
		return nil
	}
	who := MySQLString(user) + "@" + MySQLString(host)
	out := []string{"GRANT " + strings.Join(privs, ", ") + " ON " + MySQLGrantDB(db) + ".* TO " + who}
	if level == LevelDBA {
		out[0] += " WITH GRANT OPTION"
		out = append(out, "GRANT PROCESS ON *.* TO "+who)
	}
	return out
}

// ValidMySQLOptionFile reports whether p may be read for the MySQL admin
// sign-in: an absolute path to a .cnf file with no way out of where it says.
func ValidMySQLOptionFile(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && strings.HasSuffix(p, ".cnf") &&
		!strings.ContainsAny(p, "\x00\n")
}
