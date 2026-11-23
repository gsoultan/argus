package tunnel

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

/*
The RabbitMQ half of a broker's Users tab (brokerusers.go): an entry is a
vhost and a user is the node's, so an entry shows and changes the user's
permissions on its vhost and counts the vhosts it has elsewhere.

The browser makes the password_hash RabbitMQ stores
(rabbit_password_hashing_sha256: 4 random salt bytes, then SHA-256 of the
salt and the password, base64), so only the hash crosses the tunnel; the
sign-in check right after carries the password once.

Never changed from an entry: Argus's own users, and any user tagged
administrator, policymaker or impersonator, which reach past one vhost.
Those tags are never given either.
*/

// RabbitPerms are a user's permissions on a vhost: three regular
// expressions on the names of the queues and exchanges it may configure,
// write to and read from.
type RabbitPerms struct {
	Configure string `json:"configure"`
	Write     string `json:"write"`
	Read      string `json:"read"`
}

// RabbitTopicPerm is a user's topic permission on one topic exchange of a
// vhost: regular expressions on the routing keys it may publish and bind.
type RabbitTopicPerm struct {
	Exchange string `json:"exchange"`
	Write    string `json:"write"`
	Read     string `json:"read"`
}

// RabbitLimits are a user's limits on the node; 0 is no limit.
type RabbitLimits struct {
	MaxConnections int `json:"maxConnections,omitempty"`
	MaxChannels    int `json:"maxChannels,omitempty"`
}

// RabbitUserTags are the tags a user may be given from an entry.
var RabbitUserTags = []string{"monitoring", "management"}

// rabbitWideTags reach past one vhost: a user holding one is never changed
// from an entry.
var rabbitWideTags = []string{"administrator", "policymaker", "impersonator"}

// RabbitWideTag reports whether tags hold one that reaches past a vhost.
func RabbitWideTag(tags []string) string {
	for _, t := range tags {
		if slices.Contains(rabbitWideTags, t) {
			return t
		}
	}
	return ""
}

var rabbitNewUserRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// MaxRabbitPattern bounds one permission's regular expression.
const MaxRabbitPattern = 512

func validRabbitPattern(p string) string {
	if len(p) > MaxRabbitPattern {
		return "a permission is at most " + strconv.Itoa(MaxRabbitPattern) + " characters"
	}
	if _, err := regexp.Compile(p); err != nil {
		return strconv.Quote(p) + " is not a regular expression"
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return "a permission has no control characters"
		}
	}
	return ""
}

// ValidRabbitPasswordHash says why h is not a rabbit_password_hashing_sha256
// hash, or "".
func ValidRabbitPasswordHash(h string) string {
	b, err := base64.StdEncoding.DecodeString(h)
	if err != nil || len(b) != 4+32 {
		return "the password hash is not RabbitMQ's: 4 salt bytes and a SHA-256, base64"
	}
	return ""
}

// ValidRabbitUserChange says why a RabbitMQ user operation may not run on
// the entry for vhost, or "". The agent also refuses what only it sees: a
// user that exists on create, a user with a tag that reaches past a vhost,
// permissions elsewhere when deleting.
func ValidRabbitUserChange(vhost string, req StoreRequest) string {
	if req.Op == BrokerOpUsers {
		return ""
	}
	u := req.BrokerUser
	if u == nil || u.Name == "" {
		return "no user named"
	}
	if IsStoreUser(u.Name) {
		return "users starting " + StoreUserPrefix + " are Argus's own, made for each session"
	}
	create := u.Create && req.Op == BrokerOpUserSet
	if create && !rabbitNewUserRE.MatchString(u.Name) {
		return "a name is 1 to 64 letters, digits, . _ @ and -"
	}
	if len(u.Name) > 255 {
		return "no user named"
	}
	for _, c := range u.Name {
		if c < 0x20 || c == 0x7f {
			return "a user's name has no control characters"
		}
	}
	switch req.Op {
	case BrokerOpUserDel, BrokerOpUserScopeDel:
		return ""
	case BrokerOpUserLogin:
		if req.Password == "" || len(req.Password) > MaxRedisPassword {
			return "the sign-in check needs the password"
		}
		return ""
	case BrokerOpUserSet:
	default:
		return "unknown operation " + req.Op
	}
	if create && u.PasswordHash == "" {
		return "a new user needs a password"
	}
	if u.PasswordHash != "" {
		if why := ValidRabbitPasswordHash(u.PasswordHash); why != "" {
			return why
		}
	}
	if u.Tags != nil {
		for _, t := range *u.Tags {
			if !slices.Contains(RabbitUserTags, t) {
				return "a user is given no tag, monitoring or management from an entry, not " + strconv.Quote(t)
			}
		}
	}
	if p := u.Perms; p != nil {
		for _, x := range []string{p.Configure, p.Write, p.Read} {
			if why := validRabbitPattern(x); why != "" {
				return why
			}
		}
	}
	if len(u.TopicPerms) > 50 {
		return "at most 50 topic permissions"
	}
	seen := map[string]bool{}
	for _, t := range u.TopicPerms {
		if t.Exchange == "" || len(t.Exchange) > 255 {
			return "a topic permission names its exchange"
		}
		if seen[t.Exchange] {
			return "the exchange " + strconv.Quote(t.Exchange) + " is named twice"
		}
		seen[t.Exchange] = true
		for _, x := range []string{t.Write, t.Read} {
			if why := validRabbitPattern(x); why != "" {
				return why
			}
		}
	}
	if l := u.Limits; l != nil && (l.MaxConnections < 0 || l.MaxChannels < 0 || l.MaxConnections > 1_000_000 || l.MaxChannels > 1_000_000) {
		return "a limit is a positive number, or none"
	}
	if create && u.Perms == nil {
		return "a new user needs its permissions on " + vhost
	}
	if !create && u.PasswordHash == "" && u.Tags == nil && u.Perms == nil && u.TopicPerms == nil && u.Limits == nil {
		return "nothing to change"
	}
	return ""
}

// RabbitUserText is a RabbitMQ user operation as the recording and an
// approval show it: the hash is [redacted].
func RabbitUserText(vhost string, req StoreRequest) string {
	u := req.BrokerUser
	if u == nil {
		return strings.ToUpper(req.Op)
	}
	name := strconv.Quote(u.Name)
	on := " on vhost " + strconv.Quote(vhost)
	switch req.Op {
	case BrokerOpUserDel:
		return "DELETE USER " + name
	case BrokerOpUserScopeDel:
		return "REMOVE ACCESS " + name + on
	case BrokerOpUserLogin:
		return "SIGN IN AS " + name + on
	}
	head := "CHANGE USER " + name
	if u.Create {
		head = "CREATE USER " + name
	}
	lines := []string{head}
	if u.PasswordHash != "" {
		lines = append(lines, "  password_hash [redacted]")
	}
	if u.Tags != nil {
		tags := "none"
		if len(*u.Tags) > 0 {
			tags = strings.Join(*u.Tags, ", ")
		}
		lines = append(lines, "  tags "+tags)
	}
	if p := u.Perms; p != nil {
		lines = append(lines, "  permissions"+on+": configure "+strconv.Quote(p.Configure)+
			" write "+strconv.Quote(p.Write)+" read "+strconv.Quote(p.Read))
	}
	if u.TopicPerms != nil {
		if len(u.TopicPerms) == 0 {
			lines = append(lines, "  no topic permissions"+on)
		}
		for _, t := range u.TopicPerms {
			lines = append(lines, "  topic permission on exchange "+strconv.Quote(t.Exchange)+": write "+
				strconv.Quote(t.Write)+" read "+strconv.Quote(t.Read))
		}
	}
	if l := u.Limits; l != nil {
		lines = append(lines, "  limits: "+RabbitLimitWords(*l))
	}
	return strings.Join(lines, "\n")
}

// RabbitLimitWords is a user's limits in words.
func RabbitLimitWords(l RabbitLimits) string {
	var parts []string
	if l.MaxConnections > 0 {
		parts = append(parts, "at most "+strconv.Itoa(l.MaxConnections)+" connections")
	}
	if l.MaxChannels > 0 {
		parts = append(parts, "at most "+strconv.Itoa(l.MaxChannels)+" channels")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// BrokerUserOpLevelFor is BrokerUserOpLevel on an engine. RabbitMQ shows
// users only to its administrators, so its list needs DBA, the level whose
// session user is one.
func BrokerUserOpLevelFor(engine, op string) string {
	need := BrokerUserOpLevel(op)
	if engine == EngineRabbitMQ && need != "" {
		return LevelDBA
	}
	return need
}

// BrokerUserText is a broker user operation's text for the engine.
func BrokerUserText(engine, scope string, req StoreRequest) string {
	if engine == EngineRabbitMQ {
		return RabbitUserText(scope, req)
	}
	return KafkaUserText(scope, req)
}

// ValidBrokerUserChange is the engine's check of a broker user operation.
func ValidBrokerUserChange(engine, scope string, req StoreRequest) string {
	if engine == EngineRabbitMQ {
		return ValidRabbitUserChange(scope, req)
	}
	return ValidKafkaUserChange(scope, req)
}
