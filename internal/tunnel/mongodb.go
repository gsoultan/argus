package tunnel

import (
	"strings"
	"unicode/utf8"
)

/*
MongoDB, through the same store frames Redis and RabbitMQ use: StoreRequest
toward the agent, StoreAnswer back, and a PGRecord line for every command
before it runs.

One entry is one database on one mongod. Each person gets a user per level,
StoreUser(role, level), created in that database with the level's built-in
roles on that database only, and a one-time password; the last session to
close drops it. Argus never touches a user whose name does not start with
StoreUserPrefix.

The agent signs in as MongoDB's administrator with the credentials in a file on
the server (MongoAdminFile): it is read there for each session and never sent
anywhere. A mongod without access control is refused, because a person's level
would mean nothing on it.
*/

// MongoDB engine and service.
const (
	EngineMongoDB  = "mongodb"
	MongoDBService = "mongodb"
)

// MongoAdminFile is where the agent looks for the admin credentials when the
// entry names no file.
const MongoAdminFile = "/etc/argus/mongodb.conf"

// mongoSystemDBs are MongoDB's own databases, never an entry.
var mongoSystemDBs = map[string]bool{"admin": true, "local": true, "config": true}

// IsMongoSystemDB reports whether name is one of MongoDB's own databases.
func IsMongoSystemDB(name string) bool { return mongoSystemDBs[name] }

// ValidMongoDB reports whether name can be an entry: what MongoDB allows in a
// database name on every platform (1 to 63 bytes, none of /\. "$*<>:|? or
// space or NUL), and not one of its own databases.
func ValidMongoDB(name string) bool {
	return name != "" && len(name) < 64 && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\\. \"$*<>:|?\x00") && !IsMongoSystemDB(name)
}

// MongoRole is one role a level holds, on the database it names.
type MongoRole struct {
	Role string `json:"role" bson:"role"`
	DB   string `json:"db" bson:"db"`
}

// MongoRoles are a level's built-in roles on db, least to most. Owner may
// change collections and indexes but not users; dba holds dbOwner on the
// database and clusterMonitor, to see what the server is doing.
func MongoRoles(level, db string) []MongoRole {
	switch level {
	case LevelRead:
		return []MongoRole{{"read", db}}
	case LevelWrite:
		return []MongoRole{{"readWrite", db}}
	case LevelOwner:
		return []MongoRole{{"readWrite", db}, {"dbAdmin", db}}
	case LevelDBA:
		return []MongoRole{{"dbOwner", db}, {"clusterMonitor", "admin"}}
	}
	return nil
}

// ValidMongoAdminFile reports whether p may be read for the admin sign-in:
// an absolute path to a .conf file with no way out of where it says.
func ValidMongoAdminFile(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && strings.HasSuffix(p, ".conf") &&
		!strings.ContainsAny(p, "\x00\n")
}
