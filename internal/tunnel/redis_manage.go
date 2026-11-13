package tunnel

import (
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/*
Managing a Redis from the console, beyond the command line: editing a key
with an undo copy, changing keys in bulk, the server's configuration and its
ACL users. These are the rules both sides of the tunnel share, so the control
plane and the agent refuse the same things:

  - which operation needs which level (RedisOpLevel);
  - which of them wait for someone else's approval, and the exact text that
    approval is for (RedisApproval);
  - which settings may change from Argus at all (RedisConfigSettable), and
    which are never shown (RedisConfigSecret);
  - what an ACL rule may say when Argus writes it (ValidRedisRules).

The ACL is still the boundary for data: edits and bulk changes run as the
person's own user. Configuration and the server's users change through the
agent's admin connection, which is why they are checked here as well.
*/

// Redis management operations, beside run, scan and value.
const (
	RedisOpInfo      = "info"       // INFO and the slow log, as admin
	RedisOpEdit      = "edit"       // commands on one key, with an undo copy
	RedisOpCount     = "count"      // how many keys a bulk change would touch
	RedisOpBulk      = "bulk"       // the bulk change itself
	RedisOpUndoList  = "undo_list"  // this person's undo copies
	RedisOpUndo      = "undo"       // put an undo copy back
	RedisOpConfigGet = "config_get" // the configuration, without secrets
	RedisOpConfigSet = "config_set" // change settings, optionally in the file
	RedisOpSave      = "save"       // BGSAVE or BGREWRITEAOF
	RedisOpACLList   = "acl_list"   // the server's users
	RedisOpACLSet    = "acl_setuser"
	RedisOpACLDel    = "acl_deluser"
	RedisOpACLDryRun = "acl_dryrun" // would these rules allow this command
	RedisOpACLLogin  = "acl_login"  // sign in as a user, on every address Redis listens on

	RedisOpNetwork       = "network"        // where Redis listens and who can reach it
	RedisOpNetworkSet    = "network_set"    // which LAN addresses it listens on
	RedisOpDefaultSecure = "default_secure" // give the default user a password
)

// MaxRedisPassword bounds a password sent for a sign-in check.
const MaxRedisPassword = 512

// RedisBulkApprovalOver is how many keys a bulk change may touch before it
// waits for approval where changes need it.
const RedisBulkApprovalOver = 100

// MaxRedisBulkKeys bounds one bulk change: the agent holds the key names it
// counted, so it can stop before changing anything when more match.
const MaxRedisBulkKeys = 1_000_000

// RedisFilter picks keys for a bulk change.
type RedisFilter struct {
	Pattern string `json:"pattern"`
	// Type is string, hash, list, set, zset or stream; empty is any.
	Type string `json:"type,omitempty"`
	// IdleDays keeps keys nobody has read or written for this long.
	IdleDays int `json:"idleDays,omitempty"`
	// NoExpiry keeps keys that have no TTL.
	NoExpiry bool `json:"noExpiry,omitempty"`
}

// RedisConfigItem is one setting to change.
type RedisConfigItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RedisApprovalNeed is the answer to a change that waits for approval: what
// to ask for, exactly, and the most keys it may touch.
type RedisApprovalNeed struct {
	Text string `json:"text"`
	Cap  int64  `json:"cap,omitempty"`
}

// RedisOpLevel is the least level an operation needs; "" is an operation
// that is not one of these.
func RedisOpLevel(op string) string {
	switch op {
	case RedisOpInfo, RedisOpUndoList:
		return LevelRead
	case RedisOpEdit, RedisOpCount, RedisOpBulk, RedisOpUndo:
		return LevelWrite
	case RedisOpConfigGet, RedisOpACLList, RedisOpACLDryRun, RedisOpNetwork:
		return LevelOwner
	case RedisOpConfigSet, RedisOpSave, RedisOpACLSet, RedisOpACLDel, RedisOpACLLogin,
		RedisOpNetworkSet, RedisOpDefaultSecure:
		return LevelDBA
	}
	return ""
}

// RedisApproval is what a request is asking to do, in the words an approval
// is given for, and whether it waits for one where the database's changes
// need approval. The control plane matches an approval against this text and
// the agent asks for the same; anything that changes between the ask and the
// run changes the text, so the approval no longer fits.
func RedisApproval(req StoreRequest) (RedisApprovalNeed, bool) {
	switch req.Op {
	case RedisOpBulk:
		if req.Max <= RedisBulkApprovalOver {
			return RedisApprovalNeed{}, false
		}
		return RedisApprovalNeed{Text: RedisBulkText(req), Cap: req.Max}, true
	case RedisOpConfigSet:
		args := []string{"CONFIG", "SET"}
		for _, it := range req.Config {
			args = append(args, it.Name, it.Value)
		}
		text := JoinRedisCommand(args)
		if req.Rewrite {
			text += "\nCONFIG REWRITE"
		}
		return RedisApprovalNeed{Text: text}, true
	case RedisOpACLSet:
		return RedisApprovalNeed{Text: JoinRedisCommand(RedisSetUserArgs(req))}, true
	case RedisOpACLDel:
		return RedisApprovalNeed{Text: JoinRedisCommand([]string{"ACL", "DELUSER", req.User})}, true
	case RedisOpNetworkSet:
		return RedisApprovalNeed{Text: JoinRedisCommand([]string{"CONFIG", "SET", "bind", RedisBind(req.Addresses)}) +
			"\nCONFIG REWRITE"}, true
	case RedisOpDefaultSecure:
		return RedisApprovalNeed{Text: "CONFIG SET requirepass <a new password made on the server>\nCONFIG REWRITE"}, true
	}
	return RedisApprovalNeed{}, false
}

// RedisBulkText is a bulk change as one line: what it does, to which keys.
func RedisBulkText(req StoreRequest) string {
	f := req.Filter
	args := []string{"BULK", strings.ToUpper(req.Action)}
	if req.Action == "expire" {
		args = append(args, strconv.FormatInt(req.Seconds, 10))
	}
	args = append(args, "MATCH", f.Pattern)
	if f.Type != "" {
		args = append(args, "TYPE", f.Type)
	}
	if f.IdleDays > 0 {
		args = append(args, "IDLEDAYS", strconv.Itoa(f.IdleDays))
	}
	if f.NoExpiry {
		args = append(args, "NOEXPIRY")
	}
	if req.Undo {
		args = append(args, "KEEPCOPY")
	}
	return JoinRedisCommand(args)
}

// RedisSetUserArgs is the ACL SETUSER a request makes. Rules, when given,
// replace the user's keys, channels and commands; passwords are only ever
// hashes (#), added or removed (!).
func RedisSetUserArgs(req StoreRequest) []string {
	args := []string{"ACL", "SETUSER", req.User}
	if req.State == "on" || req.State == "off" {
		args = append(args, req.State)
	}
	for _, h := range req.AddHashes {
		args = append(args, "#"+strings.ToLower(h))
	}
	for _, h := range req.RemoveHashes {
		args = append(args, "!"+strings.ToLower(h))
	}
	if req.Rules != nil {
		args = append(args, "resetkeys", "resetchannels", "-@all")
		args = append(args, req.Rules...)
	}
	return args
}

var (
	redisUserRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	redisHashRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	redisCmdRE  = regexp.MustCompile(`^[+-](@[a-z]+|[a-z][a-z0-9_-]*(\|[a-z0-9_-]+)?)$`)
)

// ValidRedisUserName reports whether name may be a user Argus makes or
// changes for a person: not one of Argus's own session users.
func ValidRedisUserName(name string) bool {
	return redisUserRE.MatchString(name) && !IsStoreUser(name)
}

// ValidRedisHash reports whether h is a password's SHA-256, in hex.
func ValidRedisHash(h string) bool { return redisHashRE.MatchString(h) }

// redisAdminOnly are what no level below DBA is given, whatever an admin
// writes for it: they reconfigure the server or reach other users.
var redisAdminOnly = []string{"+@admin", "+@all", "allcommands", "+acl", "+config", "+shutdown", "+debug",
	"+module", "+replicaof", "+slaveof", "+failover", "+flushall", "+save", "+bgsave", "+bgrewriteaof"}

// ValidRedisRules checks rules Argus writes into an ACL user: key patterns
// (~, %R~, %W~, %RW~), channels (&), and commands or categories (+ and -).
// Passwords, selectors and reset are never among them; level is the Argus
// level the rules are for, or "" for a server user, which may have any
// command. It returns why not, or "".
func ValidRedisRules(rules []string, level string) string {
	if len(rules) > 64 {
		return "at most 64 rules"
	}
	hasAll, minusAdmin := false, false
	for _, r := range rules {
		if r == "" || len(r) > 200 || strings.ContainsAny(r, " \t\r\n\x00()") {
			return "a rule is one word: " + strconv.Quote(r)
		}
		switch {
		case r == "allkeys" || r == "allchannels" || r == "allcommands" || r == "nocommands":
		case strings.HasPrefix(r, "~"), strings.HasPrefix(r, "%R~"), strings.HasPrefix(r, "%W~"), strings.HasPrefix(r, "%RW~"):
			if strings.HasSuffix(r, "~") {
				return "a key pattern needs a pattern after ~: " + r
			}
		case strings.HasPrefix(r, "&"):
			if len(r) == 1 {
				return "a channel needs a pattern after &"
			}
		case redisCmdRE.MatchString(strings.ToLower(r)):
		default:
			return "not a rule Argus writes: " + r +
				" (key patterns ~, channels &, commands or categories + and -)"
		}
		lr := strings.ToLower(r)
		if lr == "+@all" || lr == "allcommands" {
			hasAll = true
		}
		if lr == "-@admin" {
			minusAdmin = true
		}
		if level != "" && level != LevelDBA && lr != "+@all" && lr != "allcommands" {
			for _, a := range redisAdminOnly {
				if lr == a {
					return r + " is DBA's; below DBA no rule may reach the server's configuration or users"
				}
			}
		}
	}
	if level != "" && level != LevelDBA && hasAll && !minusAdmin {
		return "below DBA, +@all needs -@admin with it"
	}
	return ""
}

// RedisLevelRules are the whole rules a level's user is given: key patterns
// and commands. custom is what an admin set for this entry and level, nil
// for Argus's own.
func RedisLevelRules(level string, custom []string) []string {
	if custom != nil {
		return custom
	}
	return append([]string{"~*"}, RedisACLRules(level)...)
}

// RedisSettingGroup is where a setting is shown.
type RedisSettingGroup struct {
	Group string
	// Settable says it may change from Argus without a restart.
	Settable bool
	// Memory says its value is a size, so 4gb and 4294967296 are the same.
	Memory bool
	// Default is Redis 7's, for telling a changed setting from the file
	// when the file does not name it.
	Default string
}

// RedisSettings are the settings Argus shows in groups and may change.
// Anything else Redis reports is shown read only under Advanced, unless it
// is a secret (RedisConfigSecret) or locked (RedisConfigLocked).
var RedisSettings = map[string]RedisSettingGroup{
	// Memory & eviction
	"maxmemory":                   {"memory", true, true, "0"},
	"maxmemory-policy":            {"memory", true, false, "noeviction"},
	"maxmemory-samples":           {"memory", true, false, "5"},
	"maxmemory-eviction-tenacity": {"memory", true, false, "10"},
	"lazyfree-lazy-eviction":      {"memory", true, false, "no"},
	"lazyfree-lazy-expire":        {"memory", true, false, "no"},
	"lazyfree-lazy-server-del":    {"memory", true, false, "no"},
	"lazyfree-lazy-user-del":      {"memory", true, false, "no"},
	"active-expire-effort":        {"memory", true, false, "1"},
	"activedefrag":                {"memory", true, false, "no"},
	// Persistence
	"save":                        {"persistence", true, false, "3600 1 300 100 60 10000"},
	"appendonly":                  {"persistence", true, false, "no"},
	"appendfsync":                 {"persistence", true, false, "everysec"},
	"no-appendfsync-on-rewrite":   {"persistence", true, false, "no"},
	"auto-aof-rewrite-percentage": {"persistence", true, false, "100"},
	"auto-aof-rewrite-min-size":   {"persistence", true, true, "67108864"},
	"aof-use-rdb-preamble":        {"persistence", true, false, "yes"},
	"stop-writes-on-bgsave-error": {"persistence", true, false, "yes"},
	"rdbcompression":              {"persistence", true, false, "yes"},
	"rdbchecksum":                 {"persistence", true, false, "yes"},
	// Replication
	"replica-read-only":        {"replication", true, false, "yes"},
	"replica-serve-stale-data": {"replication", true, false, "yes"},
	"repl-backlog-size":        {"replication", true, true, "1048576"},
	"repl-timeout":             {"replication", true, false, "60"},
	"repl-diskless-sync":       {"replication", true, false, "yes"},
	"min-replicas-to-write":    {"replication", true, false, "0"},
	"min-replicas-max-lag":     {"replication", true, false, "10"},
	// Clients & network
	"timeout":                    {"clients", true, false, "0"},
	"maxclients":                 {"clients", true, false, "10000"},
	"tcp-keepalive":              {"clients", true, false, "300"},
	"client-output-buffer-limit": {"clients", true, false, ""},
	"client-query-buffer-limit":  {"clients", true, true, "1073741824"},
	"busy-reply-threshold":       {"clients", true, false, "5000"},
	"proto-max-bulk-len":         {"clients", true, true, "536870912"},
	"tcp-backlog":                {"clients", false, false, "511"},
	"io-threads":                 {"clients", false, false, "1"},
	// Slow log & latency
	"slowlog-log-slower-than":   {"slowlog", true, false, "10000"},
	"slowlog-max-len":           {"slowlog", true, false, "128"},
	"latency-monitor-threshold": {"slowlog", true, false, "0"},
	"latency-tracking":          {"slowlog", true, false, "yes"},
	// Security
	"protected-mode": {"security", true, false, "yes"},
	"acllog-max-len": {"security", true, false, "128"},
	// Advanced, settable
	"notify-keyspace-events":    {"advanced", true, false, ""},
	"hz":                        {"advanced", true, false, "10"},
	"dynamic-hz":                {"advanced", true, false, "yes"},
	"hash-max-listpack-entries": {"advanced", true, false, "128"},
	"hash-max-listpack-value":   {"advanced", true, false, "64"},
	"list-max-listpack-size":    {"advanced", true, false, "-2"},
	"set-max-intset-entries":    {"advanced", true, false, "512"},
	"zset-max-listpack-entries": {"advanced", true, false, "128"},
	"zset-max-listpack-value":   {"advanced", true, false, "64"},
	// Restart only, shown read only
	"databases": {"advanced", false, false, "16"},
	"port":      {"advanced", false, false, "6379"},
	"bind":      {"advanced", false, false, ""},
}

// RedisConfigLocked are settings that never change from Argus: where Redis
// writes files (CONFIG SET dir with dbfilename writes any file Redis's user
// can), the server's own passwords, and what loads code.
func RedisConfigLocked(name string) bool {
	switch name {
	case "dir", "dbfilename", "logfile", "aclfile", "pidfile", "appenddirname", "appendfilename",
		"requirepass", "masterauth", "masteruser", "unixsocket", "unixsocketperm",
		"enable-protected-configs", "enable-debug-command", "enable-module-command",
		"loadmodule", "include", "rename-command", "daemonize", "supervised", "syslog-enabled",
		"syslog-ident", "syslog-facility", "rdb-del-sync-files", "set-proc-title", "proc-title-template":
		return true
	}
	return strings.HasPrefix(name, "tls-")
}

// RedisConfigSecret are settings whose value is never sent to the console,
// only whether it is set.
func RedisConfigSecret(name string) bool {
	switch name {
	case "requirepass", "masterauth", "tls-key-file-pass", "tls-client-key-file-pass":
		return true
	}
	return false
}

// RedisConfigSettable reports whether name may be changed from Argus.
func RedisConfigSettable(name string) bool {
	g, ok := RedisSettings[name]
	return ok && g.Settable && !RedisConfigLocked(name)
}

// ValidRedisConfigItems checks a change: settable names, each once, and
// values that are one line.
func ValidRedisConfigItems(items []RedisConfigItem) string {
	if len(items) == 0 || len(items) > 32 {
		return "change 1 to 32 settings at a time"
	}
	seen := map[string]bool{}
	for _, it := range items {
		name := strings.ToLower(it.Name)
		if !RedisConfigSettable(name) {
			return it.Name + " does not change from Argus"
		}
		if seen[name] {
			return it.Name + " is named twice"
		}
		seen[name] = true
		if len(it.Value) > 256 || strings.ContainsAny(it.Value, "\r\n\x00") {
			return "the value for " + it.Name + " is not one short line"
		}
	}
	return ""
}

// RedisMemoryBytes reads a Redis size (4gb, 512mb, 100k, 1073741824) as
// bytes; ok is false for anything else.
func RedisMemoryBytes(s string) (int64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"g", 1e9}, {"m", 1e6}, {"k", 1e3}, {"b", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * mult, true
}

// RedisSameSetting reports whether two spellings of a setting's value mean
// the same: sizes in any unit, yes/no in any case, spaces collapsed.
func RedisSameSetting(name, a, b string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	if g, ok := RedisSettings[name]; ok && g.Memory {
		x, ok1 := RedisMemoryBytes(a)
		y, ok2 := RedisMemoryBytes(b)
		if ok1 && ok2 {
			return x == y
		}
	}
	return norm(a) == norm(b)
}

// RedisConfFile reads redis.conf's directives: each name with its value as
// CONFIG GET would give it (repeated save lines joined).
func RedisConfFile(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		args, err := SplitRedisCommand(line)
		if err != nil || len(args) == 0 {
			continue
		}
		name := strings.ToLower(args[0])
		val := strings.Join(args[1:], " ")
		if name == "save" && out[name] != "" && val != "" {
			out[name] += " " + val
			continue
		}
		out[name] = val
	}
	return out
}

// RedisSortedNames is a map's keys in order, for answers that read the
// same each time.
func RedisSortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RedisEditable reports whether a command may be one of an edit's: a data
// command on one key (RENAMENX names two), never one that walks or wipes.
func RedisEditable(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch strings.ToUpper(args[0]) {
	case "SET", "HSET", "HDEL", "LSET", "LPUSH", "RPUSH", "LREM", "SADD", "SREM", "ZADD", "ZREM",
		"XADD", "XDEL", "EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT", "PERSIST", "DEL", "UNLINK":
		return true
	case "RENAMENX", "RENAME":
		return len(args) == 3
	}
	return false
}

var redisHashRefRE = regexp.MustCompile(`^[0-9a-f]{8}…[0-9a-f]{8}$`)

// RedisHashRef is how a password's hash is shown: its first and last eight
// hex characters. Enough to tell passwords apart, not to try them.
func RedisHashRef(h string) string {
	h = strings.ToLower(h)
	if len(h) < 16 {
		return h
	}
	return h[:8] + "…" + h[len(h)-8:]
}

// ValidRedisHashRef reports whether r names a password to remove: a whole
// hash or a short one as RedisHashRef shows it.
func ValidRedisHashRef(r string) bool { return ValidRedisHash(r) || redisHashRefRE.MatchString(r) }

// RedisBind is the bind line for listening on the loopback and these LAN
// addresses. Loopback stays, because the agent signs in there; each LAN
// address is optional ("-"), so Redis still starts if it goes away.
func RedisBind(addrs []string) string {
	out := []string{"127.0.0.1", "-::1"}
	for _, a := range addrs {
		out = append(out, "-"+a)
	}
	return strings.Join(out, " ")
}

// ValidRedisLANAddress says why addr may not be one Redis listens on for
// the LAN, or "". Only private (RFC 1918, RFC 4193) addresses: opening a
// Redis to a public address is done on the server, never from here.
func ValidRedisLANAddress(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil || ip.Zone() != "" {
		return addr + " is not an IP address"
	}
	if !ip.IsPrivate() {
		return addr + " is not a private LAN address; Argus opens Redis only to the LAN"
	}
	return ""
}
