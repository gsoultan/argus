package tunnel

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

/*
Redis and RabbitMQ, the two engines after Postgres.

They are opened the way a Postgres database is -- a grant naming the entry, the
address on the server and the level -- and recorded in the same hash-chained
line format (PGRecord), so verification, storage and replay do not care which
engine wrote a recording. What differs is the conversation: there are no row
batches, only a request and one JSON answer.

	toward the agent     'q' StoreRequest (JSON)
	toward the plane     'a' StoreAnswer (JSON)
	                     'l' PGRecord (JSON), before the command runs

The boundary is the engine's own permissions. Each person gets a user named
StoreUser(role, level) for the length of their session: a Redis ACL user, or a
RabbitMQ user with a tag and vhost permissions. Argus only ever creates,
changes or deletes users whose name starts with StoreUserPrefix.
*/

// Service names on the tunnel, and the engines a database entry may be.
const (
	RedisService    = "redis"
	RabbitMQService = "rabbitmq"

	EnginePostgres = "postgres"
	EngineRedis    = "redis"
	EngineRabbitMQ = "rabbitmq"
)

// ServiceForEngine is the tunnel service that opens an engine's sessions.
func ServiceForEngine(engine string) string {
	switch engine {
	case EngineRedis:
		return RedisService
	case EngineRabbitMQ:
		return RabbitMQService
	case EngineMySQL:
		return MySQLService
	case EngineMongoDB:
		return MongoDBService
	case EngineKafka:
		return KafkaService
	}
	return PostgresService
}

// StoreUserPrefix starts the name of every user Argus makes in Redis or
// RabbitMQ. A user without it is never touched.
const StoreUserPrefix = "argus-"

// StoreUser is the engine user a person's sessions at one level open as. One
// per level, so a read session and a DBA session of the same person can be
// open together without one changing the other's rules.
func StoreUser(role, level string) string { return StoreUserPrefix + role + "-" + level }

// IsStoreUser reports whether name is one Argus made.
func IsStoreUser(name string) bool { return strings.HasPrefix(name, StoreUserPrefix) }

// Limits on one request.
const (
	// MaxCommand bounds the text of one Redis command or RabbitMQ payload.
	// Each is recorded in full before it is sent, so it has the bound a SQL
	// statement has; see MaxSQL and RecordedTextFits.
	MaxCommand = MaxSQL
	// MaxStoreItems bounds the items one answer carries: keys from a scan,
	// elements of a value, messages peeked.
	MaxStoreItems = 10_000
	// DefaultStoreItems is what a request that names no limit gets.
	DefaultStoreItems = 200
	// MaxStoreAnswer bounds the bytes of values one answer carries; past it
	// values are cut and marked.
	MaxStoreAnswer = 16 << 20
	// DefaultKeysLimit is the largest Redis db KEYS may run on unless the
	// entry says otherwise. Past it, the key browser (SCAN) is the way.
	DefaultKeysLimit = 10_000
)

// StoreRequest is one thing the browser asks of a Redis or RabbitMQ session.
type StoreRequest struct {
	ID uint32 `json:"id"`
	// Op is cancel, or for Redis: run, scan, value; for RabbitMQ: overview,
	// queues, exchanges, exchange, bindings, queue, consumers, peek, publish,
	// purge, move, declare_queue, delete_queue, declare_exchange, bind,
	// connections, channels, close_connection.
	Op string `json:"op"`
	// Command is one Redis command line, as redis-cli takes it.
	Command string `json:"command,omitempty"`
	// Confirm is the entry's name, typed by the person, for a command that
	// wipes or reconfigures. The agent refuses such a command without it.
	Confirm string `json:"confirm,omitempty"`
	// Scan: where to continue, what to match, and how many to look at.
	Cursor  string `json:"cursor,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Type    string `json:"type,omitempty"`
	Count   int    `json:"count,omitempty"`
	// Key is the Redis key a value is read from.
	Key string `json:"key,omitempty"`
	// KeyQuoted says Key is shown quoted, as a key that is not UTF-8 is.
	KeyQuoted bool `json:"keyQ,omitempty"`
	// Name is a queue, an exchange or a connection.
	Name string `json:"name,omitempty"`
	// Publish and bind.
	Exchange   string            `json:"exchange,omitempty"`
	RoutingKey string            `json:"routingKey,omitempty"`
	Payload    string            `json:"payload,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	// Properties are a published message's: content_type, message_id,
	// correlation_id and the like, and delivery_mode "1" for transient.
	Properties map[string]string `json:"properties,omitempty"`
	// Declare a queue or an exchange: durable or not.
	Durable bool `json:"durable,omitempty"`
	// Move: the queue messages go to (Name is where they come from).
	Target string `json:"target,omitempty"`
	// Declare an exchange: direct, topic, fanout or headers.
	Kind string `json:"kind,omitempty"`
	// Close a connection: why, as the client is told.
	Reason string `json:"reason,omitempty"`
}

// StoreAnswer is the agent's reply to a request, or news about the session.
type StoreAnswer struct {
	ID uint32 `json:"id"`
	// Type is ready, result, error or closed.
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
	// NeedsConfirm says the command was refused only because it was not
	// confirmed; the console asks and sends it again with Confirm.
	NeedsConfirm bool  `json:"needsConfirm,omitempty"`
	MS           int64 `json:"ms,omitempty"`
	// Value is a Redis reply.
	Value *RedisValue `json:"value,omitempty"`
	// Keys and Cursor answer a scan; Cursor "0" is the end.
	Keys   []RedisKey `json:"keys,omitempty"`
	Cursor string     `json:"cursor,omitempty"`
	// KeyType and TTL describe the key a value was read from; TTL -1 is no
	// expiry.
	KeyType string `json:"keyType,omitempty"`
	TTL     int64  `json:"ttl,omitempty"`
	// Total is how many items there are (keys in the db, elements of a
	// value, messages in a queue), when the engine says.
	Total int64 `json:"total,omitempty"`
	// Truncated says the answer holds fewer items or bytes than there were.
	Truncated bool `json:"truncated,omitempty"`
	// Data is a RabbitMQ management API answer, as JSON, or the documents a
	// MongoDB command answered, each as relaxed Extended JSON.
	Data any `json:"data,omitempty"`
	// Tag says what a command did, in a few words: "find 12", "update 3".
	Tag string `json:"tag,omitempty"`
	// Ready carries what the session opened as.
	Engine  string `json:"engine,omitempty"`
	User    string `json:"user,omitempty"`
	Level   string `json:"level,omitempty"`
	Version string `json:"version,omitempty"`
	// Server names what answered when one engine covers several: "valkey"
	// for a Valkey on the redis engine. Empty means the engine's own name.
	Server  string `json:"server,omitempty"`
	Session string `json:"session,omitempty"`
	// Ops are the operations this level may ask for, so the console offers
	// only those.
	Ops []string `json:"ops,omitempty"`
}

// RedisValue is a Redis reply, as JSON can carry it.
type RedisValue struct {
	// T is s (simple string), e (error), i (integer), b (bulk string),
	// n (nil) or a (array).
	T string `json:"t"`
	// V is a string's text. A bulk string that is not UTF-8 is shown quoted,
	// as redis-cli shows it, with Q set.
	V string       `json:"v,omitempty"`
	Q bool         `json:"q,omitempty"`
	I int64        `json:"i,omitempty"`
	A []RedisValue `json:"a,omitempty"`
	// N is an array's length when not every element is in A.
	N int64 `json:"n,omitempty"`
	// Cut says V or A was cut short.
	Cut bool `json:"cut,omitempty"`
}

// RedisKey is one key from the key browser.
type RedisKey struct {
	Name string `json:"name"`
	Q    bool   `json:"q,omitempty"`
	Type string `json:"type"`
	TTL  int64  `json:"ttl"`
	// Count is how many documents a MongoDB collection holds, as the server
	// estimates it.
	Count int64 `json:"count,omitempty"`
}

// RedisText is how a bulk string is shown: as it is when it is text, quoted
// like redis-cli when it is not.
func RedisText(b []byte) (string, bool) {
	if utf8.Valid(b) {
		return string(b), false
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, c := range b {
		switch {
		case c == '"' || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c == '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteString(`\x`)
			sb.WriteString(strconv.FormatUint(uint64(c)>>4, 16))
			sb.WriteString(strconv.FormatUint(uint64(c)&0xf, 16))
		}
	}
	sb.WriteByte('"')
	return sb.String(), true
}

// ErrCommandSyntax is a command line that cannot be split into arguments.
var ErrCommandSyntax = errors.New("unbalanced quotes in the command")

// SplitRedisCommand splits a command line the way redis-cli does: spaces
// between arguments, "double quotes" with \n \t \" \\ \xHH escapes, 'single
// quotes' taken as they are.
func SplitRedisCommand(line string) ([]string, error) {
	var (
		out []string
		cur []byte
		in  bool
	)
	i := 0
	for i < len(line) {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				out = append(out, string(cur))
				cur, in = cur[:0], false
			}
			i++
		case c == '"' && !in:
			in = true
			i++
			closed := false
			for i < len(line) {
				c = line[i]
				if c == '\\' && i+1 < len(line) {
					n := line[i+1]
					switch n {
					case 'n':
						cur = append(cur, '\n')
					case 'r':
						cur = append(cur, '\r')
					case 't':
						cur = append(cur, '\t')
					case 'b':
						cur = append(cur, '\b')
					case 'a':
						cur = append(cur, '\a')
					case 'x':
						if i+3 < len(line) {
							if v, err := strconv.ParseUint(line[i+2:i+4], 16, 8); err == nil {
								cur = append(cur, byte(v))
								i += 4
								continue
							}
						}
						cur = append(cur, 'x')
					default:
						cur = append(cur, n)
					}
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				cur = append(cur, c)
				i++
			}
			if !closed {
				return nil, ErrCommandSyntax
			}
			if i < len(line) && line[i] != ' ' && line[i] != '\t' {
				return nil, errors.New("a closing quote must be followed by a space")
			}
		case c == '\'' && !in:
			in = true
			i++
			closed := false
			for i < len(line) {
				c = line[i]
				if c == '\\' && i+1 < len(line) && line[i+1] == '\'' {
					cur = append(cur, '\'')
					i += 2
					continue
				}
				if c == '\'' {
					closed = true
					i++
					break
				}
				cur = append(cur, c)
				i++
			}
			if !closed {
				return nil, ErrCommandSyntax
			}
			if i < len(line) && line[i] != ' ' && line[i] != '\t' {
				return nil, errors.New("a closing quote must be followed by a space")
			}
		default:
			in = true
			cur = append(cur, c)
			i++
		}
	}
	if in {
		out = append(out, string(cur))
	}
	return out, nil
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_:.\-*?/@#$%^&+=<>!,\[\]{}|~]+$`)

// JoinRedisCommand is the command as it is recorded: each argument plain
// when it can be, quoted when it cannot, so the line splits back into the
// same arguments.
func JoinRedisCommand(args []string) string {
	var sb strings.Builder
	for i, a := range args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if plainArg.MatchString(a) {
			sb.WriteString(a)
			continue
		}
		q, _ := RedisText([]byte(a))
		if !strings.HasPrefix(q, `"`) {
			q = strconv.Quote(a)
		}
		sb.WriteString(q)
	}
	return sb.String()
}

// RedisACLRules are the ACL SETUSER rules of a level, after the user's
// password and key pattern. The categories are Redis's own; Argus adds no
// rule of its own beyond choosing them.
func RedisACLRules(level string) []string {
	switch level {
	case LevelRead:
		return []string{"-@all", "+@read", "+@connection", "-@dangerous"}
	case LevelWrite:
		return []string{"-@all", "+@read", "+@write", "+@connection", "+@transaction", "-@dangerous"}
	case LevelOwner:
		// Everything on the data, nothing on the server: FLUSHDB, KEYS and
		// INFO stay; CONFIG, SHUTDOWN, ACL, DEBUG and replication go.
		return []string{"+@all", "-@admin", "-flushall"}
	case LevelDBA:
		return []string{"+@all"}
	}
	return nil
}

// RedisRefusal is why the console will not send a command, or "". These are
// commands that would take the session's connection away from it (a
// subscription, a monitor, a blocking pop) or change who it is (AUTH,
// HELLO). They are not the security boundary; the ACL is.
func RedisRefusal(args []string) string {
	if len(args) == 0 {
		return "there is no command to run"
	}
	name := strings.ToUpper(args[0])
	switch name {
	case "AUTH", "HELLO":
		return name + " would change who this session is; it always runs as your own Argus user"
	case "MONITOR", "SUBSCRIBE", "PSUBSCRIBE", "SSUBSCRIBE", "UNSUBSCRIBE", "PUNSUBSCRIBE",
		"SUNSUBSCRIBE", "SYNC", "PSYNC", "REPLCONF", "QUIT", "RESET":
		return name + " would hold or end the session's connection, so it cannot run from the console"
	case "BLPOP", "BRPOP", "BRPOPLPUSH", "BLMOVE", "BLMPOP", "BZPOPMIN", "BZPOPMAX", "BZMPOP",
		"WAIT", "WAITAOF":
		return name + " blocks until something arrives; use its non-blocking form (" +
			strings.TrimPrefix(name, "B") + ")"
	case "CLIENT":
		if len(args) > 1 {
			switch strings.ToUpper(args[1]) {
			case "REPLY", "PAUSE", "SETNAME", "NO-EVICT", "NO-TOUCH", "TRACKING":
				return "CLIENT " + strings.ToUpper(args[1]) + " changes the session's own connection, so it cannot run from the console"
			}
		}
	case "XREAD", "XREADGROUP":
		for _, a := range args[1:] {
			if strings.EqualFold(a, "BLOCK") {
				return name + " with BLOCK waits for new entries; leave BLOCK out"
			}
		}
	}
	return ""
}

// RedisNeedsConfirm reports whether a command wipes or reconfigures enough
// that the person must type the entry's name first.
func RedisNeedsConfirm(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := ""
	if len(args) > 1 {
		sub = strings.ToUpper(args[1])
	}
	switch strings.ToUpper(args[0]) {
	case "FLUSHALL", "FLUSHDB", "SHUTDOWN", "DEBUG", "REPLICAOF", "SLAVEOF", "FAILOVER", "SWAPDB":
		return true
	case "CONFIG":
		return sub == "SET" || sub == "REWRITE" || sub == "RESETSTAT"
	case "SCRIPT":
		return sub == "FLUSH"
	case "FUNCTION":
		return sub == "FLUSH" || sub == "DELETE" || sub == "RESTORE"
	case "CLUSTER":
		return sub == "RESET" || sub == "FLUSHSLOTS" || sub == "FORGET"
	case "ACL":
		return sub == "SETUSER" || sub == "DELUSER" || sub == "LOAD"
	case "CLIENT":
		return sub == "KILL"
	}
	return false
}

// RabbitLevel is what a RabbitMQ level is in RabbitMQ's own terms: the user
// tag and the vhost permissions (configure, write, read).
type RabbitLevel struct {
	Tag                    string
	Configure, Write, Read string
}

// RabbitPermissions maps a level to RabbitMQ's tag and permissions.
func RabbitPermissions(level string) (RabbitLevel, bool) {
	switch level {
	case LevelRead:
		return RabbitLevel{"management", "^$", "^$", ".*"}, true
	case LevelWrite:
		return RabbitLevel{"management", "^$", ".*", ".*"}, true
	case LevelOwner:
		return RabbitLevel{"policymaker", ".*", ".*", ".*"}, true
	case LevelDBA:
		return RabbitLevel{"administrator", ".*", ".*", ".*"}, true
	}
	return RabbitLevel{}, false
}

// rabbitOps is the least level each RabbitMQ operation needs. RabbitMQ lets
// anyone who may read a queue purge it, so the agent offers a fixed list of
// operations per level on top of the permissions, never the management API
// itself.
var rabbitOps = map[string]string{
	"overview":         LevelRead,
	"queues":           LevelRead,
	"exchanges":        LevelRead,
	"exchange":         LevelRead,
	"bindings":         LevelRead,
	"queue":            LevelRead,
	"consumers":        LevelRead,
	"peek":             LevelRead,
	"publish":          LevelWrite,
	"purge":            LevelOwner,
	"move":             LevelOwner,
	"declare_queue":    LevelOwner,
	"delete_queue":     LevelOwner,
	"declare_exchange": LevelOwner,
	"bind":             LevelOwner,
	"connections":      LevelDBA,
	"channels":         LevelDBA,
	"close_connection": LevelDBA,
}

// RabbitOpAllowed reports whether level may ask for op.
func RabbitOpAllowed(op, level string) bool {
	need, ok := rabbitOps[op]
	return ok && LevelRank(level) >= LevelRank(need) && LevelRank(level) > 0
}

// RabbitOps lists what a level may ask for, in a fixed order.
func RabbitOps(level string) []string {
	var out []string
	for _, op := range []string{"overview", "queues", "exchanges", "exchange", "bindings", "queue", "consumers",
		"peek", "publish", "purge", "move", "declare_queue", "delete_queue", "declare_exchange", "bind",
		"connections", "channels", "close_connection"} {
		if RabbitOpAllowed(op, level) {
			out = append(out, op)
		}
	}
	return out
}

// RabbitExchangeKinds are the exchange types an owner may declare.
var RabbitExchangeKinds = map[string]bool{"direct": true, "topic": true, "fanout": true, "headers": true}

// RabbitNeedsConfirm reports whether an operation destroys messages or cuts
// someone off, so the person must type the entry's name first.
func RabbitNeedsConfirm(op string) bool {
	switch op {
	case "purge", "delete_queue", "close_connection":
		return true
	}
	return false
}

// LocalLoopback reports whether host is a loopback address, the only place
// an agent reaches a management API or a Redis over TCP.
func LocalLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// ValidRedisConfPath reports whether p may be read for the Redis admin
// password: an absolute path to a .conf file with no way out of where it
// says. The agent reads only its requirepass and never sends it anywhere.
func ValidRedisConfPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && strings.HasSuffix(p, ".conf") &&
		!strings.ContainsAny(p, "\x00\n")
}

// ValidVhost reports whether a RabbitMQ vhost name can be an entry.
func ValidVhost(v string) bool {
	return v != "" && len(v) <= 255 && utf8.ValidString(v) && !strings.ContainsAny(v, "\x00\n\r")
}

// ValidRedisDB reports whether s is a logical Redis db number.
func ValidRedisDB(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n < 10000 && strconv.Itoa(n) == s
}
