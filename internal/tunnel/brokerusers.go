package tunnel

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/*
The users that applications sign in to a message broker with, managed from an
entry's Users tab: list them, make one, change what it may do, give it a new
password, set limits, take away its access to the entry, delete it. These are
the rules both sides of the tunnel share, so the control plane and the agent
refuse the same things, and the text a change is approved and recorded under
is the same on both:

  - which operation needs which level (BrokerUserOpLevel);
  - what a change says, with the credential as [redacted] (KafkaUserText);
  - what a Kafka change may ask for: ACLs inside the entry's scope only, on
    topics, consumer groups and transactional IDs, never on the cluster
    (ValidKafkaUserChange).

A user is cluster-wide and an entry is a scope, so an entry shows and changes
only what is inside it and counts the rest. Argus's own session users
(argus-…) and the user the agent signs in as are never changed from here.

The password never crosses the tunnel, with one exception. The browser salts
and hashes it (SCRAM's salted password, RabbitMQ's password_hash), so the
control plane and the agent carry only the hash; the sign-in check right
after carries the password once, to sign in with it, and it is not recorded,
logged or kept.
*/

// Broker user operations, beside an engine's own.
const (
	BrokerOpUsers        = "users"          // the users that can reach this entry
	BrokerOpUserSet      = "user_set"       // make or change one
	BrokerOpUserDel      = "user_del"       // delete one, where all it has is in view
	BrokerOpUserScopeDel = "user_scope_del" // take away its access to this entry only
	BrokerOpUserLogin    = "user_login"     // sign in as one, to check a password works
)

// BrokerUserOpLevel is the least level a broker user operation needs; ""
// is an operation that is not one of these.
func BrokerUserOpLevel(op string) string {
	switch op {
	case BrokerOpUsers:
		return LevelOwner
	case BrokerOpUserSet, BrokerOpUserDel, BrokerOpUserScopeDel, BrokerOpUserLogin:
		return LevelDBA
	}
	return ""
}

// BrokerUserChanges reports whether op changes a user, so it is recorded,
// audited and waits for approval where the entry's changes need one.
func BrokerUserChanges(op string) bool {
	switch op {
	case BrokerOpUserSet, BrokerOpUserDel, BrokerOpUserScopeDel:
		return true
	}
	return false
}

// BrokerUser is a user change: who, and what to set. A field left nil is
// left as it is.
type BrokerUser struct {
	Name string `json:"name"`
	// Create says the user must not exist yet.
	Create bool `json:"create,omitempty"`
	// Kafka: the SCRAM mechanism and the salted credential, made in the
	// browser from the password.
	Mechanism  string           `json:"mechanism,omitempty"`
	Credential *ScramCredential `json:"credential,omitempty"`
	// ACLs, when not nil, are every ACL the user should have inside the
	// entry: those it has there and are not listed are deleted. Never
	// omitempty: an empty list takes them all away, nil leaves them.
	ACLs []KafkaACL `json:"acls"`
	// Quotas, when not nil, replace the user's Kafka quotas.
	Quotas *KafkaQuotas `json:"quotas,omitempty"`

	// RabbitMQ: the password_hash made in the browser, the tags (nil
	// leaves them), the permissions on the entry's vhost, its topic
	// permissions there (nil leaves them, empty takes them away) and its
	// limits.
	PasswordHash string            `json:"passwordHash,omitempty"`
	Tags         *[]string         `json:"tags,omitempty"`
	Perms        *RabbitPerms      `json:"perms,omitempty"`
	TopicPerms   []RabbitTopicPerm `json:"topicPerms"`
	Limits       *RabbitLimits     `json:"limits,omitempty"`

	// NATS: the user's public key (U…), made in the browser with its seed,
	// which never leaves the browser, and what the user may do.
	PublicKey string       `json:"publicKey,omitempty"`
	NATS      *NATSAppUser `json:"nats,omitempty"`
}

// ScramCredential is a SCRAM credential as Kafka stores it: the salt and the
// salted password (PBKDF2 of the password with the salt), each base64, and
// the iterations.
type ScramCredential struct {
	Salt           string `json:"salt"`
	SaltedPassword string `json:"saltedPassword"`
	Iterations     int    `json:"iterations"`
}

// KafkaACL is one ACL binding of a user. Resource is topic, group or
// transactional_id; Pattern LITERAL or PREFIXED; Operation one of Kafka's
// (READ, WRITE, …); Permission ALLOW or DENY; Host * or an address.
type KafkaACL struct {
	Resource   string `json:"resource"`
	Pattern    string `json:"pattern"`
	Name       string `json:"name"`
	Operation  string `json:"operation"`
	Permission string `json:"permission"`
	Host       string `json:"host"`
}

// KafkaQuotas are a user's Kafka quotas: bytes a second it may produce and
// consume, and the share of a broker's request time; 0 is no limit.
type KafkaQuotas struct {
	ProduceBytes float64 `json:"produceBytes,omitempty"`
	ConsumeBytes float64 `json:"consumeBytes,omitempty"`
	RequestPct   float64 `json:"requestPct,omitempty"`
}

// Key is the ACL as one comparable line.
func (a KafkaACL) Key() string {
	return strings.Join([]string{a.Resource, a.Pattern, a.Name, a.Operation, a.Permission, a.Host}, "\x00")
}

// String is the ACL as the recording and an approval show it.
func (a KafkaACL) String() string {
	s := a.Permission + " " + a.Resource + " " + a.Pattern + " " + strconv.Quote(a.Name) + " " + a.Operation
	if a.Host != "*" {
		s += " from " + a.Host
	}
	return s
}

// kafkaUserOps are the operations an ACL Argus writes may hold, by
// resource. Nothing on the cluster, and nothing that reaches delegation
// tokens or other users.
var kafkaUserOps = map[string]map[string]bool{
	"topic": {"READ": true, "WRITE": true, "DESCRIBE": true, "DESCRIBE_CONFIGS": true,
		"CREATE": true, "DELETE": true, "ALTER": true, "ALTER_CONFIGS": true},
	"group":            {"READ": true, "DESCRIBE": true, "DELETE": true},
	"transactional_id": {"WRITE": true, "DESCRIBE": true},
}

// KafkaUserOperation reports whether op may be held on resource in an ACL
// Argus writes.
func KafkaUserOperation(resource, op string) bool { return kafkaUserOps[resource][op] }

var (
	kafkaNewUserRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	kafkaGroupRE   = regexp.MustCompile(`^[A-Za-z0-9._:@/=+-]{1,249}$`)
)

// ValidKafkaUserName says why name may not be a user changed from Argus, or
// "". A new user's name is letters, digits, . _ and -; an existing one may be
// a certificate's (CN=…), which is changed but never made here.
func ValidKafkaUserName(name string, create bool) string {
	if IsStoreUser(name) {
		return "users starting " + StoreUserPrefix + " are Argus's own, made for each session"
	}
	if create {
		if !kafkaNewUserRE.MatchString(name) {
			return "a name is 1 to 64 letters, digits, . _ and -"
		}
		return ""
	}
	if name == "" || len(name) > 256 || strings.TrimSpace(name) != name {
		return "no user named"
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return "a user's name has no control characters"
		}
	}
	return ""
}

// KafkaACLInScope reports whether an ACL is inside an entry's scope: a topic
// ACL whose topics the scope covers, or a consumer group or transactional ID
// ACL, which go with the topics.
func KafkaACLInScope(scope string, a KafkaACL) bool {
	switch a.Resource {
	case "topic":
		if scope == KafkaAllTopics {
			return true
		}
		if a.Name == "*" {
			return false
		}
		return strings.HasPrefix(a.Name, scope)
	case "group", "transactional_id":
		return true
	}
	return false
}

// kafkaACLWords are the ACL's resource and pattern as Kafka writes them.
var (
	kafkaResources = map[string]bool{"topic": true, "group": true, "transactional_id": true}
	kafkaPatterns  = map[string]bool{"LITERAL": true, "PREFIXED": true}
)

// ValidKafkaACL says why an ACL may not be one Argus writes on an entry,
// or "". DENY is checked by the agent: it may stay where it is, never be
// made.
func ValidKafkaACL(scope string, a KafkaACL) string {
	if !kafkaResources[a.Resource] {
		return "an ACL is on a topic, a consumer group or a transactional ID, not " + strconv.Quote(a.Resource)
	}
	if !kafkaPatterns[a.Pattern] {
		return "an ACL's name is LITERAL or PREFIXED"
	}
	if a.Permission != "ALLOW" && a.Permission != "DENY" {
		return "an ACL allows or denies"
	}
	if !KafkaUserOperation(a.Resource, a.Operation) {
		return a.Operation + " on a " + strings.ReplaceAll(a.Resource, "_", " ") + " is not one Argus gives"
	}
	if a.Host != "*" {
		if ip, err := netip.ParseAddr(a.Host); err != nil || ip.Zone() != "" {
			return "an ACL's host is * or an IP address"
		}
	}
	switch a.Resource {
	case "topic":
		ok := ValidKafkaTopic(a.Name) || (a.Name == "*" && a.Pattern == "LITERAL")
		if !ok {
			return strconv.Quote(a.Name) + " is not a topic name"
		}
	default:
		if a.Name != "*" && !kafkaGroupRE.MatchString(a.Name) {
			return strconv.Quote(a.Name) + " is not a name Argus writes in an ACL"
		}
	}
	if !KafkaACLInScope(scope, a) {
		return "topics " + scopeWords(scope) + " only; " + a.String() + " is outside this entry"
	}
	return ""
}

func scopeWords(scope string) string {
	if scope == KafkaAllTopics {
		return "anywhere"
	}
	return "starting " + scope
}

// MaxKafkaUserACLs bounds one change.
const MaxKafkaUserACLs = 200

// ValidKafkaUserChange says why a Kafka user operation may not run on an
// entry with this scope, or "". The agent also refuses what it alone can
// see: the admin user, a user that exists on create, a DENY being made.
func ValidKafkaUserChange(scope string, req StoreRequest) string {
	if req.Op == BrokerOpUsers {
		return ""
	}
	u := req.BrokerUser
	if u == nil {
		return "no user named"
	}
	if why := ValidKafkaUserName(u.Name, u.Create && req.Op == BrokerOpUserSet); why != "" {
		return why
	}
	switch req.Op {
	case BrokerOpUserDel, BrokerOpUserScopeDel:
		return ""
	case BrokerOpUserLogin:
		if req.Password == "" || len(req.Password) > MaxRedisPassword {
			return "the sign-in check needs the password"
		}
		if u.Mechanism != "" && u.Mechanism != "SCRAM-SHA-512" && u.Mechanism != "SCRAM-SHA-256" {
			return "a user signs in with SCRAM-SHA-512 or SCRAM-SHA-256"
		}
		return ""
	case BrokerOpUserSet:
	default:
		return "unknown operation " + req.Op
	}
	if u.Create && u.Credential == nil {
		return "a new user needs a password"
	}
	if u.Credential != nil {
		if u.Mechanism != "SCRAM-SHA-512" && u.Mechanism != "SCRAM-SHA-256" {
			return "a user signs in with SCRAM-SHA-512 or SCRAM-SHA-256"
		}
		if why := u.Credential.valid(u.Mechanism); why != "" {
			return why
		}
	}
	if len(u.ACLs) > MaxKafkaUserACLs {
		return "at most " + strconv.Itoa(MaxKafkaUserACLs) + " ACLs"
	}
	seen := map[string]bool{}
	for _, a := range u.ACLs {
		if why := ValidKafkaACL(scope, a); why != "" {
			return why
		}
		if seen[a.Key()] {
			return a.String() + " is named twice"
		}
		seen[a.Key()] = true
	}
	if q := u.Quotas; q != nil {
		if q.ProduceBytes < 0 || q.ConsumeBytes < 0 || q.RequestPct < 0 || q.RequestPct > 10000 {
			return "a limit is a positive number, or none"
		}
	}
	if !u.Create && u.Credential == nil && u.ACLs == nil && u.Quotas == nil {
		return "nothing to change"
	}
	return ""
}

func (c *ScramCredential) valid(mechanism string) string {
	if c.Iterations < 4096 || c.Iterations > 16384 {
		return "Kafka takes 4096 to 16384 SCRAM iterations"
	}
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil || len(salt) < 16 || len(salt) > 128 {
		return "the credential's salt is not 16 to 128 bytes of base64"
	}
	want := 64
	if mechanism == "SCRAM-SHA-256" {
		want = 32
	}
	sp, err := base64.StdEncoding.DecodeString(c.SaltedPassword)
	if err != nil || len(sp) != want {
		return fmt.Sprintf("the salted password is not %d bytes of base64", want)
	}
	return ""
}

// SortKafkaACLs orders ACLs as they are shown: by resource, name, pattern,
// operation.
func SortKafkaACLs(acls []KafkaACL) {
	sort.Slice(acls, func(i, j int) bool { return acls[i].Key() < acls[j].Key() })
}

// KafkaUserText is a user change as the recording, Activity and an
// approval show it. The credential is [redacted]: it is made after the
// approval, and the approver needs to know a password is set, not which.
func KafkaUserText(scope string, req StoreRequest) string {
	u := req.BrokerUser
	if u == nil {
		return strings.ToUpper(req.Op)
	}
	user := strconv.Quote(u.Name)
	var lines []string
	switch req.Op {
	case BrokerOpUserDel:
		return "DELETE USER " + user + " with every ACL, credential and quota it has"
	case BrokerOpUserScopeDel:
		return "REMOVE ACCESS " + user + " to topics " + scopeWords(scope) + ": delete its ACLs there"
	case BrokerOpUserLogin:
		return "SIGN IN AS " + user + " to check its password"
	}
	head := "CHANGE USER " + user
	if u.Create {
		head = "CREATE USER " + user
	}
	lines = append(lines, head)
	if u.Credential != nil {
		lines = append(lines, fmt.Sprintf("  password %s, %d iterations, salted [redacted]", u.Mechanism, u.Credential.Iterations))
	}
	if u.ACLs != nil {
		acls := append([]KafkaACL(nil), u.ACLs...)
		SortKafkaACLs(acls)
		if len(acls) == 0 {
			lines = append(lines, "  ACLs on topics "+scopeWords(scope)+": none")
		} else {
			lines = append(lines, "  ACLs on topics "+scopeWords(scope)+" become:")
			for _, a := range acls {
				lines = append(lines, "    "+a.String())
			}
		}
	}
	if q := u.Quotas; q != nil {
		lines = append(lines, "  quotas "+KafkaQuotaWords(*q))
	}
	return strings.Join(lines, "\n")
}

// KafkaQuotaWords are quotas in a line.
func KafkaQuotaWords(q KafkaQuotas) string {
	part := func(v float64, unit string) string {
		if v <= 0 {
			return "no limit"
		}
		return strconv.FormatFloat(v, 'f', -1, 64) + unit
	}
	return "produce " + part(q.ProduceBytes, " bytes/s") + ", consume " + part(q.ConsumeBytes, " bytes/s") +
		", request time " + part(q.RequestPct, "%")
}
