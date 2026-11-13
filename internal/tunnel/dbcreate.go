package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

/*
The create service: make a new database, vhost or topic on this server, so
an admin does not need a shell or a superuser session to start one.

	argus-control ──"dbcreate" stream──▶ agent ──▶ pg-admin helper / MySQL root / Mongo admin / rabbitmqctl / Kafka admin

One stream per request. After the header the agent sends one 'c' CreateResult
(JSON) and closes. The grant carries a GrantCreate: the engine, where it
listens, and the fields of what to make. Nothing in it is passed through as
SQL or a command line: each field is checked here and the statements are
built here, by CreatePlan, so the preview the console shows, the statement
that runs and the Activity entry are the same text.

The agent never runs over something that is there: it looks first and
answers Exists, and the engine refuses a duplicate again if two admins race.
An old agent refuses the service as unknown, which the console turns into
"update the agent".

Ops:

	create     make it, as GrantCreate says
	options    what the dialog may offer: Postgres's installed extensions,
	           roles and default locale; MySQL's character sets and
	           collations; a Kafka cluster's broker count
	set-owner  Postgres only: ALTER DATABASE … OWNER TO …
*/

// CreateService is the tunnel service name.
const CreateService = "dbcreate"

// FrameCreateResult carries the one CreateResult.
const FrameCreateResult byte = 'c'

// Create operations.
const (
	CreateOpCreate   = "create"
	CreateOpOptions  = "options"
	CreateOpSetOwner = "set-owner"
)

// PGOwnerArgus is the Owner value that means "Argus's Owner-level role".
const PGOwnerArgus = ""

// GrantCreate is what a create grant may make; nil on every other grant.
type GrantCreate struct {
	Op     string `json:"op"`
	Engine string `json:"engine"`
	// Host and Port are where the engine listens, as a database entry
	// names them; AdminUser and ConfigPath are as for GrantDiscover.
	Host       string `json:"host"`
	Port       int    `json:"port"`
	AdminUser  string `json:"adminUser,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
	// Name is the database, vhost or topic.
	Name string `json:"name"`

	Postgres *PGCreate     `json:"postgres,omitempty"`
	MySQL    *MySQLCreate  `json:"mysql,omitempty"`
	Mongo    *MongoCreate  `json:"mongo,omitempty"`
	Rabbit   *RabbitCreate `json:"rabbit,omitempty"`
	Kafka    *KafkaCreate  `json:"kafka,omitempty"`
}

// PGCreate is a new Postgres database's settings. Empty fields are the
// server's defaults.
type PGCreate struct {
	// Owner is a role on the server, or PGOwnerArgus for Argus's Owner
	// level in the new database.
	Owner      string   `json:"owner,omitempty"`
	Encoding   string   `json:"encoding,omitempty"`
	Locale     string   `json:"locale,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// MySQLCreate is a new MySQL database's character set and collation.
type MySQLCreate struct {
	Charset   string `json:"charset,omitempty"`
	Collation string `json:"collation,omitempty"`
}

// MongoCreate is a new MongoDB database's first collection.
type MongoCreate struct {
	Collection string `json:"collection"`
	// Kind is plain, timeseries or capped.
	Kind      string `json:"kind,omitempty"`
	TimeField string `json:"timeField,omitempty"`
	// ExpireAfterS is a time series collection's expiry; 0 keeps forever.
	ExpireAfterS int64 `json:"expireAfterS,omitempty"`
	// CappedBytes is a capped collection's size.
	CappedBytes int64 `json:"cappedBytes,omitempty"`
}

// RabbitCreate is a new vhost's settings.
type RabbitCreate struct {
	Description string `json:"description,omitempty"`
	// QueueType is the vhost's default queue type: classic, quorum or
	// stream; empty is RabbitMQ's default.
	QueueType      string `json:"queueType,omitempty"`
	MaxConnections int    `json:"maxConnections,omitempty"`
	MaxQueues      int    `json:"maxQueues,omitempty"`
}

// KafkaCreate is a new topic's settings.
type KafkaCreate struct {
	Partitions int `json:"partitions"`
	// Replicas is the replication factor; 0 is the cluster's default.
	Replicas int `json:"replicas,omitempty"`
	// RetentionMS is how long messages are kept; 0 is the broker's
	// default, -1 forever.
	RetentionMS int64 `json:"retentionMs,omitempty"`
	// Cleanup is delete or compact; empty is the broker's default.
	Cleanup string `json:"cleanup,omitempty"`
}

// CreateStep is one step the agent took, in order.
type CreateStep struct {
	Text  string `json:"text"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// CreateResult is the agent's answer.
type CreateResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	// Exists says the name is taken on the server; nothing was changed.
	Exists bool `json:"exists,omitempty"`
	// Statements are what ran, as CreatePlan built them.
	Statements []string     `json:"statements,omitempty"`
	Steps      []CreateStep `json:"steps,omitempty"`
	// Options answers the options op.
	Options *CreateOptions `json:"options,omitempty"`
}

// CreateOptions is what the console's create dialog may offer.
type CreateOptions struct {
	Version string `json:"version,omitempty"`
	// Postgres.
	Extensions []string `json:"extensions,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Encoding   string   `json:"encoding,omitempty"`
	Locale     string   `json:"locale,omitempty"`
	// MySQL: each character set's collations, and the server's defaults.
	Charsets         map[string][]string `json:"charsets,omitempty"`
	DefaultCharset   string              `json:"defaultCharset,omitempty"`
	DefaultCollation string              `json:"defaultCollation,omitempty"`
	// Kafka.
	Brokers int `json:"brokers,omitempty"`
	// Taken are the names already on the server, so the dialog says so
	// before anything runs.
	Taken     []string `json:"taken,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

var (
	rePGExtension = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]{0,62}$`)
	rePGRole      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)
	rePGEncoding  = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
	rePGLocale    = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	reMySQLName   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	reMongoField  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	reSafePG      = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// MaxExtensions bounds the extensions one create enables.
const MaxExtensions = 20

// Validate checks every field, so the agent and the control plane refuse
// the same things. It also fills defaults.
func (c *GrantCreate) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	d := GrantDiscover{Engine: c.Engine, Host: c.Host, Port: c.Port, AdminUser: c.AdminUser, ConfigPath: c.ConfigPath}
	if c.Engine == EngineRedis {
		return errors.New(RedisNothingToCreate)
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if c.Engine == EnginePostgres && !LocalPGHost(c.Host) {
		// Listing and sessions reach a Postgres behind this server's
		// PgBouncer; creating databases there is for its own server's agent.
		return fmt.Errorf("Postgres must be on this server to create a database here; %s is not", c.Host)
	}
	switch c.Op {
	case CreateOpOptions:
		return nil
	case CreateOpSetOwner:
		if c.Engine != EnginePostgres {
			return errors.New("only a Postgres database has an owner to change")
		}
		if err := validPGDatabase(c.Name); err != nil {
			return err
		}
		if c.Postgres == nil {
			return errors.New("the new owner is required")
		}
		return validPGOwner(c.Postgres.Owner)
	case CreateOpCreate:
	default:
		return errors.New("unknown operation " + c.Op)
	}
	switch c.Engine {
	case EnginePostgres:
		if err := validPGDatabase(c.Name); err != nil {
			return err
		}
		if c.Postgres == nil {
			c.Postgres = &PGCreate{}
		}
		p := c.Postgres
		if err := validPGOwner(p.Owner); err != nil {
			return err
		}
		if p.Encoding != "" && !rePGEncoding.MatchString(p.Encoding) {
			return errors.New("the encoding must be a name such as UTF8")
		}
		if p.Locale != "" && !rePGLocale.MatchString(p.Locale) {
			return errors.New("the locale must be a name such as en_US.UTF-8")
		}
		if len(p.Extensions) > MaxExtensions {
			return fmt.Errorf("at most %d extensions at once", MaxExtensions)
		}
		seen := map[string]bool{}
		for _, e := range p.Extensions {
			if !rePGExtension.MatchString(e) {
				return fmt.Errorf("%q is not an extension name", e)
			}
			if seen[e] {
				return fmt.Errorf("%s is listed twice", e)
			}
			seen[e] = true
		}
	case EngineMySQL:
		if !ValidMySQLDB(c.Name) || len(c.Name) > 63 || strings.HasSuffix(c.Name, " ") {
			return errors.New("the database name must be 1 to 63 characters, not one of MySQL's own, with no / \\ or . and no trailing space")
		}
		if strings.HasPrefix(c.Name, TestDBPrefix) {
			return errors.New(TestDBPrefix + " names are kept for Argus's restore tests")
		}
		if c.MySQL == nil {
			c.MySQL = &MySQLCreate{}
		}
		m := c.MySQL
		m.Charset, m.Collation = strings.ToLower(m.Charset), strings.ToLower(m.Collation)
		if m.Charset != "" && !reMySQLName.MatchString(m.Charset) {
			return errors.New("the character set must be a name such as utf8mb4")
		}
		if m.Collation != "" && !reMySQLName.MatchString(m.Collation) {
			return errors.New("the collation must be a name such as utf8mb4_0900_ai_ci")
		}
		if m.Collation != "" && m.Charset != "" && !strings.HasPrefix(m.Collation, m.Charset+"_") {
			return fmt.Errorf("%s is not a collation of %s", m.Collation, m.Charset)
		}
	case EngineMongoDB:
		if !ValidMongoDB(c.Name) {
			return errors.New("the MongoDB database name must be 1 to 63 bytes with none of / \\ . \" $ * < > : | ? or spaces, and not admin, local or config")
		}
		if strings.HasPrefix(c.Name, TestDBPrefix) {
			return errors.New(TestDBPrefix + " names are kept for Argus's restore tests")
		}
		if c.Mongo == nil {
			return errors.New("MongoDB keeps a database only while it has a collection: name the first one")
		}
		if err := c.Mongo.validate(); err != nil {
			return err
		}
	case EngineRabbitMQ:
		if !ValidVhost(c.Name) || len(c.Name) > 63 || strings.ContainsAny(c.Name, "\"'\\") {
			return errors.New("the vhost must be 1 to 63 characters with no quotes")
		}
		if c.Rabbit == nil {
			c.Rabbit = &RabbitCreate{}
		}
		r := c.Rabbit
		r.Description = strings.TrimSpace(r.Description)
		if len(r.Description) > 200 || !utf8.ValidString(r.Description) || strings.ContainsAny(r.Description, "\x00\n\r") {
			return errors.New("the description must be one line of at most 200 characters")
		}
		switch r.QueueType {
		case "", "classic", "quorum", "stream":
		default:
			return errors.New("the default queue type is classic, quorum or stream")
		}
		if r.MaxConnections < 0 || r.MaxQueues < 0 || r.MaxConnections > 1_000_000 || r.MaxQueues > 1_000_000 {
			return errors.New("a limit must be between 1 and 1,000,000, or empty for none")
		}
	case EngineKafka:
		if !ValidKafkaTopic(c.Name) {
			return errors.New("a topic is 1 to 249 letters, digits, . _ or -")
		}
		if strings.HasPrefix(c.Name, "__") {
			return errors.New("topics starting with __ are Kafka's own")
		}
		if c.Kafka == nil {
			c.Kafka = &KafkaCreate{}
		}
		if err := c.Kafka.Validate(); err != nil {
			return err
		}
	default:
		return errors.New("Argus cannot create on engine " + c.Engine)
	}
	return nil
}

// RedisNothingToCreate is why Redis has no create.
const RedisNothingToCreate = "a Redis has a fixed set of numbered databases, all there already, and its one entry in Argus covers every one; raise databases in redis.conf for more"

func validPGDatabase(n string) error {
	switch {
	case n == "" || len(n) > 63 || strings.ContainsAny(n, "\x00\"'\\"):
		return errors.New("the database name must be 1 to 63 characters with no quotes")
	case strings.HasPrefix(n, TestDBPrefix):
		return errors.New(TestDBPrefix + " names are kept for Argus's restore tests")
	case n == "postgres" || n == "template0" || n == "template1":
		return errors.New(n + " is Postgres's own")
	}
	return nil
}

func validPGOwner(o string) error {
	if o == PGOwnerArgus {
		return nil
	}
	if !rePGRole.MatchString(o) {
		return errors.New("the owner must be a role name of letters, digits, _ . or -")
	}
	if strings.HasPrefix(o, "pg_") {
		return errors.New("a pg_ role cannot own a database")
	}
	return nil
}

func (m *MongoCreate) validate() error {
	m.Collection = strings.TrimSpace(m.Collection)
	if m.Collection == "" || len(m.Collection) > 120 || strings.HasPrefix(m.Collection, "system.") ||
		strings.ContainsAny(m.Collection, "$\x00") || !utf8.ValidString(m.Collection) {
		return errors.New("the collection name must be 1 to 120 characters, with no $, not starting with system.")
	}
	switch m.Kind {
	case "", "plain":
		m.Kind = "plain"
		m.TimeField, m.ExpireAfterS, m.CappedBytes = "", 0, 0
	case "timeseries":
		if !reMongoField.MatchString(m.TimeField) {
			return errors.New("a time series names its time field: letters, digits and _")
		}
		if m.ExpireAfterS < 0 || m.ExpireAfterS > 100*365*86400 {
			return errors.New("keep for must be a positive number of seconds, or empty for ever")
		}
		m.CappedBytes = 0
	case "capped":
		if m.CappedBytes < 4096 || m.CappedBytes > 1<<50 {
			return errors.New("a capped collection's size must be at least 4 KB")
		}
		m.TimeField, m.ExpireAfterS = "", 0
	default:
		return errors.New("a collection is plain, timeseries or capped")
	}
	return nil
}

// Validate checks a topic's settings; sessions use it too.
func (k *KafkaCreate) Validate() error {
	if k.Partitions == 0 {
		k.Partitions = 1
	}
	if k.Partitions < 1 || k.Partitions > 1000 {
		return errors.New("partitions must be between 1 and 1000")
	}
	if k.Replicas < 0 || k.Replicas > 16 {
		return errors.New("replicas must be between 1 and 16, or empty for the cluster's default")
	}
	if k.RetentionMS < -1 {
		return errors.New("keep for must be positive, -1 for ever, or empty for the broker's default")
	}
	switch k.Cleanup {
	case "", "delete", "compact":
	default:
		return errors.New("clean-up is delete or compact")
	}
	return nil
}

// Configs are the topic configs the settings name, for CreateTopics.
func (k KafkaCreate) Configs() map[string]*string {
	out := map[string]*string{}
	if k.RetentionMS != 0 {
		v := strconv.FormatInt(k.RetentionMS, 10)
		out["retention.ms"] = &v
	}
	if k.Cleanup != "" {
		v := k.Cleanup
		out["cleanup.policy"] = &v
	}
	return out
}

// PGLevelRole names the group role for a level in a database:
// argus_owner_orders. Always a plain identifier; a database name that would
// not fit is shortened and given a hash so two databases cannot share a
// role.
func PGLevelRole(level, database string) string {
	b := make([]byte, 0, len(database))
	for i := 0; i < len(database); i++ {
		c := database[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+32)
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	name := "argus_" + level + "_" + string(b)
	if len(name) > 63 || string(b) != database {
		sum := sha256.Sum256([]byte(database))
		short := name
		if len(short) > 54 {
			short = short[:54]
		}
		name = short + "_" + hex.EncodeToString(sum[:4])
	}
	return name
}

// PGOwnerRole is the role that will own a new Postgres database.
func (c GrantCreate) PGOwnerRole() string {
	if c.Postgres == nil || c.Postgres.Owner == PGOwnerArgus {
		return PGLevelRole(LevelOwner, c.Name)
	}
	return c.Postgres.Owner
}

// PGIdent quotes a Postgres identifier, leaving a plain one bare so the
// statement reads as a person would write it.
func PGIdent(n string) string {
	if reSafePG.MatchString(n) && !pgReserved[n] {
		return n
	}
	return `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
}

// pgReserved are the reserved words a new database or role is likely to be
// named; quoting the rest is harmless.
var pgReserved = map[string]bool{"user": true, "order": true, "table": true, "select": true, "group": true,
	"all": true, "default": true, "check": true, "grant": true, "public": true, "analyse": true, "analyze": true}

// PGLiteral quotes a Postgres string literal.
func PGLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// CreatePlan is what a create runs, in order, as text. The agent runs these
// exact statements for Postgres and MySQL; for MongoDB, RabbitMQ and Kafka
// they describe the API calls it makes from the same fields.
func CreatePlan(c GrantCreate) []string {
	switch c.Engine {
	case EnginePostgres:
		p := c.Postgres
		if p == nil {
			p = &PGCreate{}
		}
		s := "CREATE DATABASE " + PGIdent(c.Name) + " OWNER " + PGIdent(c.PGOwnerRole()) + " TEMPLATE template0"
		if p.Encoding != "" {
			s += " ENCODING " + PGLiteral(p.Encoding)
		}
		if p.Locale != "" {
			s += " LC_COLLATE " + PGLiteral(p.Locale) + " LC_CTYPE " + PGLiteral(p.Locale)
		}
		out := []string{s}
		for _, e := range p.Extensions {
			out = append(out, "CREATE EXTENSION IF NOT EXISTS "+PGIdent(e))
		}
		return out
	case EngineMySQL:
		s := "CREATE DATABASE " + MySQLIdent(c.Name)
		if m := c.MySQL; m != nil {
			if m.Charset != "" {
				s += " CHARACTER SET " + m.Charset
			}
			if m.Collation != "" {
				s += " COLLATE " + m.Collation
			}
		}
		return []string{s}
	case EngineMongoDB:
		m := c.Mongo
		if m == nil {
			return nil
		}
		opts := ""
		switch m.Kind {
		case "timeseries":
			opts = fmt.Sprintf(`, { timeseries: { timeField: %q }`, m.TimeField)
			if m.ExpireAfterS > 0 {
				opts += fmt.Sprintf(", expireAfterSeconds: %d", m.ExpireAfterS)
			}
			opts += " }"
		case "capped":
			opts = fmt.Sprintf(", { capped: true, size: %d }", m.CappedBytes)
		}
		return []string{"use " + c.Name, fmt.Sprintf("db.createCollection(%q%s)", m.Collection, opts)}
	case EngineRabbitMQ:
		r := c.Rabbit
		if r == nil {
			r = &RabbitCreate{}
		}
		s := "rabbitmqctl add_vhost " + shellWord(c.Name)
		if r.Description != "" {
			s += " --description " + shellWord(r.Description)
		}
		if r.QueueType != "" {
			s += " --default-queue-type " + r.QueueType
		}
		out := []string{s}
		if l := r.LimitsJSON(); l != "" {
			out = append(out, "rabbitmqctl set_vhost_limits -p "+shellWord(c.Name)+" "+shellWord(l))
		}
		return out
	case EngineKafka:
		k := c.Kafka
		if k == nil {
			k = &KafkaCreate{Partitions: 1}
		}
		s := fmt.Sprintf("CreateTopics %s partitions=%d", c.Name, k.Partitions)
		if k.Replicas > 0 {
			s += fmt.Sprintf(" replication.factor=%d", k.Replicas)
		}
		if k.RetentionMS != 0 {
			s += fmt.Sprintf(" retention.ms=%d", k.RetentionMS)
		}
		if k.Cleanup != "" {
			s += " cleanup.policy=" + k.Cleanup
		}
		return []string{s}
	}
	return nil
}

// LimitsJSON is set_vhost_limits' argument, or "" for no limits.
func (r RabbitCreate) LimitsJSON() string {
	var parts []string
	if r.MaxConnections > 0 {
		parts = append(parts, fmt.Sprintf(`"max-connections":%d`, r.MaxConnections))
	}
	if r.MaxQueues > 0 {
		parts = append(parts, fmt.Sprintf(`"max-queues":%d`, r.MaxQueues))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// SetOwnerPlan is what changing a Postgres database's owner runs.
func SetOwnerPlan(c GrantCreate) []string {
	return []string{"ALTER DATABASE " + PGIdent(c.Name) + " OWNER TO " + PGIdent(c.PGOwnerRole())}
}

// shellWord quotes s for display the way a shell would take it. The agent
// passes arguments as a list, never through a shell.
func shellWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"\\$`!*?[]{}();&|<>#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
