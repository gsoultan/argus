package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

/*
Microsoft SQL Server, one more SQL engine.

A session speaks the Postgres session's frames (PGRequest, PGAnswer, row
batches, PGRecord), as MySQL's does, so the console's SQL workspace, the
recording, verification, watch live and replay are the ones Postgres has. What
the agent does behind them differs: see internal/agent/mssql.go.

One entry is one database on one SQL Server instance. Each person gets a
login per level and database, MSSQLLogin, mapped to a user in that database
with the level's roles there and nothing anywhere else. Between sessions the
login is disabled with a password nobody knows; a session gives it a one-time
password and enables it, and the last session to close disables it again and
ends its connections. Logins and users are kept, not dropped, because a
schema or object a person created may be owned by their user, and SQL Server
refuses to drop a user that owns something.

The agent signs in as SQL Server's administrator with the credentials in a
file on the server (MSSQLAdminFile, or the entry's): it is read there for each
session and never sent anywhere.
*/

// SQL Server engine and service.
const (
	EngineMSSQL  = "mssql"
	MSSQLService = "mssql"
)

// MSSQLAdminFile is where the agent looks for the admin credentials when the
// entry names no file.
const MSSQLAdminFile = "/etc/argus/mssql.conf"

// MSSQLMaxName is the longest name SQL Server takes for a login, user or
// database (sysname).
const MSSQLMaxName = 128

// MSSQLLogin is the login a person's sessions at one level of one database
// open as: StoreUser's name and a short hash of the database. A login is the
// server's, not the database's, so one per database keeps a read session on
// one database from reaching another the same person reads.
func MSSQLLogin(role, level, db string) string {
	sum := sha256.Sum256([]byte(db))
	tag := hex.EncodeToString(sum[:])[:8]
	u := StoreUser(role, level)
	if len(u)+1+len(tag) > MSSQLMaxName {
		rs := sha256.Sum256([]byte(role))
		keep := MSSQLMaxName - len(StoreUserPrefix) - 6 - len(level) - 2 - 1 - len(tag)
		u = StoreUserPrefix + role[:keep] + hex.EncodeToString(rs[:])[:6] + "-" + level
	}
	return u + "-" + tag
}

// mssqlSystemDBs are SQL Server's own databases, never an entry.
var mssqlSystemDBs = map[string]bool{
	"master": true, "model": true, "msdb": true, "tempdb": true, "resource": true,
	"mssqlsystemresource": true, "distribution": true,
}

// IsMSSQLSystemDB reports whether name is one of SQL Server's own databases.
// The distribution database replication makes is counted among them.
func IsMSSQLSystemDB(name string) bool { return mssqlSystemDBs[strings.ToLower(name)] }

// ValidMSSQLDB reports whether name can be an entry: 1 to 63 characters of
// text (Argus's limit for every engine), no control characters, no
// surrounding spaces, not one of SQL Server's own databases. Anything else
// SQL Server allows in a bracketed name is allowed; MSSQLIdent quotes it.
func ValidMSSQLDB(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 63 || !utf8.ValidString(name) ||
		strings.TrimSpace(name) != name || IsMSSQLSystemDB(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// MSSQLIdent quotes an identifier with brackets.
func MSSQLIdent(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// MSSQLString quotes a Unicode string literal.
func MSSQLString(s string) string {
	return "N'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// MSSQLRoles are the fixed database roles each level's user is a member of,
// least to most. Each includes the one below it. Owner can create, alter and
// drop objects (db_ddladmin) but not change who may do what; dba is
// db_owner of this database, never sysadmin.
var MSSQLRoles = map[string][]string{
	LevelRead:  {"db_datareader"},
	LevelWrite: {"db_datareader", "db_datawriter"},
	LevelOwner: {"db_datareader", "db_datawriter", "db_ddladmin"},
	LevelDBA:   {"db_owner"},
}

// MSSQLPermissions are database permissions each level's user is granted
// beyond its roles: seeing definitions everywhere, running procedures from
// write up, and plans for tuning.
var MSSQLPermissions = map[string][]string{
	LevelRead:  {"VIEW DEFINITION", "SHOWPLAN"},
	LevelWrite: {"VIEW DEFINITION", "SHOWPLAN", "EXECUTE"},
	LevelOwner: {"VIEW DEFINITION", "SHOWPLAN", "EXECUTE"},
	LevelDBA:   {"VIEW DEFINITION", "SHOWPLAN", "EXECUTE"},
}

// MSSQLServerPermissions are what a level's login holds on the server: dba
// sees every connection and what it runs, for Activity. Nothing else, and
// never a server role.
var MSSQLServerPermissions = map[string][]string{
	LevelDBA: {"VIEW SERVER STATE"},
}

// mssqlAllRoles are every fixed database role a level user may hold, so
// opening a session can take away the ones its level does not have.
var mssqlAllRoles = []string{"db_owner", "db_ddladmin", "db_datawriter", "db_datareader",
	"db_securityadmin", "db_accessadmin", "db_backupoperator", "db_denydatareader", "db_denydatawriter"}

// MSSQLAllRoles returns every fixed database role a level user may hold.
func MSSQLAllRoles() []string { return append([]string(nil), mssqlAllRoles...) }

// ValidMSSQLAdminFile reports whether p may be read for the admin sign-in:
// an absolute path to a .conf file with no way out of where it says.
func ValidMSSQLAdminFile(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && strings.HasSuffix(p, ".conf") &&
		!strings.ContainsAny(p, "\x00\n")
}

// mssqlWriteWords are T-SQL's reserved words that change something. They are
// reserved, so a table or column named after one has to be written in
// brackets or quotes, which the lexer reads as a name: a bare one anywhere in
// a batch is the statement it starts.
var mssqlWriteWords = map[string]bool{
	"insert": true, "update": true, "delete": true, "merge": true, "truncate": true,
	"drop": true, "create": true, "alter": true, "exec": true, "execute": true,
	"grant": true, "revoke": true, "deny": true, "into": true, "bulk": true, "dbcc": true,
	"kill": true, "backup": true, "restore": true, "shutdown": true, "reconfigure": true,
	"writetext": true, "updatetext": true, "setuser": true, "trigger": true,
	"openrowset": true, "opendatasource": true, "openquery": true,
}

// MSSQLWrites reports whether a T-SQL batch may change anything. A batch
// holds as many statements as it likes, with no semicolon between them, so
// "SELECT 1 DELETE FROM t" is two; the first word alone would call it a read.
// Any reserved word that changes something, at any depth, counts. Calling a
// procedure without EXEC is a batch's first word, which is not a read either.
func MSSQLWrites(sql string) bool {
	c := ClassifySQL(sql)
	if c.Writes || (!readVerbs[c.Verb] && c.Verb != "print") {
		return true
	}
	for _, t := range lexSQL(sql) {
		if t.punct == 0 && !t.quoted && mssqlWriteWords[t.text] {
			return true
		}
	}
	return false
}
