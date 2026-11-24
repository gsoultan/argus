package tunnel

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

/*
The users that applications sign in to NATS with, from a NATS entry's Users
tab, beside the Kafka and RabbitMQ ones (brokerusers.go): the same three
operations, the same levels, the same approval and Activity, and these rules
shared by the control plane and the agent.

In operator mode a user is not stored on the server. It is a JWT the account
signs and the app presents with its own key, and the server checks the
signature. So "making" a user is signing one:

  - the browser makes the user's key pair and sends only the public key (U…);
    the seed, which is the user's secret, never leaves the browser, which
    puts it in the .creds file it hands over;
  - the agent signs a user JWT for that key with the account's signing key,
    carrying what the user may publish and subscribe to, and an expiry;
  - the agent keeps a list of the users it signed (public facts only: name,
    key, rules, expiry, who), because the server keeps none.

A signed user cannot be taken back by the account: revoking one is a change
to the account's JWT, which only the operator can sign. So every user Argus
signs expires (NATSMaxUserDays at most), and taking one off the list says how
to revoke it with nsc (NATSRevokeCommand).
*/

// NATSAppUser is what an app's user may do in the account.
type NATSAppUser struct {
	// PubAllow and SubAllow are the subjects it may publish and subscribe
	// to; PubDeny and SubDeny take some back (deny wins in NATS).
	PubAllow []string `json:"pubAllow"`
	PubDeny  []string `json:"pubDeny,omitempty"`
	SubAllow []string `json:"subAllow"`
	SubDeny  []string `json:"subDeny,omitempty"`
	// Requests: it makes requests, so it reads replies under _INBOX.
	Requests bool `json:"requests,omitempty"`
	// Replies: it answers requests: one reply to each reply subject it was
	// sent, within a minute, without publishing there otherwise.
	Replies bool `json:"replies,omitempty"`
	// JetStream is "" (none), "use" (publish to streams, read them, make and
	// use consumers) or "manage" (the whole JetStream API of the account).
	JetStream string `json:"jetstream,omitempty"`
	// Days is how long the user is valid, from when it is signed.
	Days int `json:"days"`
	// Sources, when set, are the addresses (IP or CIDR) it may connect from.
	Sources []string `json:"sources,omitempty"`
}

// NATS user limits.
const (
	NATSMaxUserDays     = 365
	NATSDefaultUserDays = 90
	natsMaxUserSubjects = 64
	natsMaxUserSources  = 32
)

// natsUseAPI is the JetStream API an app that uses streams needs: read
// streams, make, read and pull from consumers, ack and answer flow control.
// Nothing that makes, changes or deletes a stream.
var natsUseAPI = []string{
	"$JS.API.INFO",
	"$JS.API.STREAM.NAMES", "$JS.API.STREAM.LIST", "$JS.API.STREAM.INFO.*",
	"$JS.API.STREAM.MSG.GET.*", "$JS.API.DIRECT.GET.>",
	"$JS.API.CONSUMER.>",
	"$JS.ACK.>", "$JS.FC.>",
}

// natsManageUserAPI is the whole JetStream API of the account.
var natsManageUserAPI = []string{"$JS.API.>", "$JS.ACK.>", "$JS.FC.>"}

// natsUserNever is what no app user is given, whatever it asks for.
var natsUserNever = []string{"$SYS.>"}

// NATSInboxes is where replies to an app's requests arrive.
const NATSInboxes = "_INBOX.>"

// NATSAppUserPerms is the user's rules as they go in its JWT: what it asked
// for, the JetStream API for its choice, its inbox when it makes requests or
// uses JetStream (whose answers come there), and $SYS denied.
func NATSAppUserPerms(u NATSAppUser) NATSPerms {
	p := NATSPerms{
		PubAllow: append([]string{}, u.PubAllow...),
		PubDeny:  append(append([]string{}, u.PubDeny...), natsUserNever...),
		SubAllow: append([]string{}, u.SubAllow...),
		SubDeny:  append(append([]string{}, u.SubDeny...), natsUserNever...),
	}
	switch u.JetStream {
	case "use":
		p.PubAllow = appendNew(p.PubAllow, natsUseAPI...)
	case "manage":
		p.PubAllow = appendNew(p.PubAllow, natsManageUserAPI...)
	}
	if u.Requests || u.JetStream != "" {
		p.SubAllow = appendNew(p.SubAllow, NATSInboxes)
	}
	return p
}

func appendNew(l []string, add ...string) []string {
	for _, a := range add {
		found := false
		for _, x := range l {
			if x == a {
				found = true
				break
			}
		}
		if !found {
			l = append(l, a)
		}
	}
	return l
}

var natsUserNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidNATSAppUserName says why name may not be an app user's, or "".
func ValidNATSAppUserName(name string) string {
	if IsStoreUser(name) {
		return "users starting " + StoreUserPrefix + " are Argus's own, made for each session"
	}
	if !natsUserNameRE.MatchString(name) {
		return "a name is 1 to 64 letters, digits, . _ and -"
	}
	return ""
}

// ValidNATSUserKey reports whether k looks like a user's public nkey: 56
// characters of base32 starting U. The agent checks its checksum too.
func ValidNATSUserKey(k string) bool {
	if len(k) != 56 || k[0] != 'U' {
		return false
	}
	for _, c := range k {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func validNATSUserSubjects(what string, l []string) string {
	if len(l) > natsMaxUserSubjects {
		return "at most " + strconv.Itoa(natsMaxUserSubjects) + " subjects to " + what
	}
	seen := map[string]bool{}
	for _, s := range l {
		if !ValidNATSSubject(s) {
			return strconv.Quote(s) + " is not a subject or a subject filter"
		}
		if s == "$SYS" || strings.HasPrefix(s, "$SYS.") {
			return "the system account's subjects are never given to an app"
		}
		if seen[s] {
			return s + " is named twice"
		}
		seen[s] = true
	}
	return ""
}

// ValidNATSSource says why s may not be an address a user connects from,
// or "": an IP address or a CIDR range.
func ValidNATSSource(s string) string {
	if p, err := netip.ParsePrefix(s); err == nil && p == p.Masked() {
		return ""
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return ""
	}
	return strconv.Quote(s) + " is not an IP address or a range like 10.0.0.0/8"
}

// ValidNATSUserChange says why a NATS user operation may not run, or "".
func ValidNATSUserChange(req StoreRequest) string {
	if req.Op == BrokerOpUsers {
		return ""
	}
	u := req.BrokerUser
	if u == nil {
		return "no user named"
	}
	if why := ValidNATSAppUserName(u.Name); why != "" {
		return why
	}
	switch req.Op {
	case BrokerOpUserDel:
		if !ValidNATSUserKey(u.PublicKey) {
			return "say which of its keys to take off the list"
		}
		return ""
	case BrokerOpUserSet:
	case BrokerOpUserLogin, BrokerOpUserScopeDel:
		return "a NATS user is signed, not stored: there is no password to check and nothing to take away but the user"
	default:
		return "unknown operation " + req.Op
	}
	if !u.Create {
		return "a NATS user cannot be changed once signed; sign a new one and let the old one expire"
	}
	if !ValidNATSUserKey(u.PublicKey) {
		return "a new user needs its public key (U…), made in the browser"
	}
	n := u.NATS
	if n == nil {
		return "say what the user may do"
	}
	if n.Days < 1 || n.Days > NATSMaxUserDays {
		return "a user is valid 1 to " + strconv.Itoa(NATSMaxUserDays) + " days; it can be signed again before then"
	}
	for _, l := range []struct {
		what string
		l    []string
	}{{"publish to", n.PubAllow}, {"keep from publishing to", n.PubDeny}, {"subscribe to", n.SubAllow}, {"keep from subscribing to", n.SubDeny}} {
		if why := validNATSUserSubjects(l.what, l.l); why != "" {
			return why
		}
	}
	switch n.JetStream {
	case "", "use", "manage":
	default:
		return "JetStream is none, use or manage"
	}
	if len(n.PubAllow) == 0 && len(n.SubAllow) == 0 && !n.Requests && !n.Replies && n.JetStream == "" {
		return "the user may do nothing; give it a subject to publish or subscribe to"
	}
	if len(n.Sources) > natsMaxUserSources {
		return "at most " + strconv.Itoa(natsMaxUserSources) + " addresses"
	}
	for _, s := range n.Sources {
		if why := ValidNATSSource(s); why != "" {
			return why
		}
	}
	return ""
}

// NATSRevokeCommand is how the operator revokes a user Argus signed: add it
// to the account's revocations and push the account to the servers.
func NATSRevokeCommand(account, key string) string {
	return "nsc revocations add-user --account " + account + " --user-public-key " + key + " && nsc push --account " + account
}

func natsSubjectWords(l []string) string {
	if len(l) == 0 {
		return "nothing"
	}
	return strings.Join(l, ", ")
}

// NATSUserText is a user operation as the recording, Activity and an
// approval show it. Only public facts: the seed is never sent.
func NATSUserText(account string, req StoreRequest) string {
	u := req.BrokerUser
	if u == nil {
		return strings.ToUpper(req.Op)
	}
	user := strconv.Quote(u.Name)
	switch req.Op {
	case BrokerOpUserDel:
		return "FORGET USER " + user + " key " + u.PublicKey + " in account " + account +
			": Argus stops listing it; it still signs in until it expires unless the operator revokes it: " +
			NATSRevokeCommand(account, u.PublicKey)
	case BrokerOpUserSet:
	default:
		return strings.ToUpper(req.Op) + " " + user
	}
	n := u.NATS
	if n == nil {
		return "CREATE USER " + user
	}
	lines := []string{"CREATE USER " + user + " in account " + account + ", key " + u.PublicKey + ", valid " + strconv.Itoa(n.Days) + " days"}
	lines = append(lines, "  publish to "+natsSubjectWords(n.PubAllow))
	if len(n.PubDeny) > 0 {
		lines = append(lines, "    except "+natsSubjectWords(n.PubDeny))
	}
	lines = append(lines, "  subscribe to "+natsSubjectWords(n.SubAllow))
	if len(n.SubDeny) > 0 {
		lines = append(lines, "    except "+natsSubjectWords(n.SubDeny))
	}
	if n.Requests {
		lines = append(lines, "  makes requests: reads replies on "+NATSInboxes)
	}
	if n.Replies {
		lines = append(lines, "  answers requests: one reply each, within a minute")
	}
	switch n.JetStream {
	case "use":
		lines = append(lines, "  JetStream: reads streams, makes and pulls from consumers, acks")
	case "manage":
		lines = append(lines, "  JetStream: the whole API, making and deleting streams and consumers")
	}
	if len(n.Sources) > 0 {
		lines = append(lines, "  connects only from "+strings.Join(n.Sources, ", "))
	}
	return strings.Join(lines, "\n")
}
