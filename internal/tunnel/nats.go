package tunnel

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

/*
NATS, a broker beside RabbitMQ and Kafka, through the same store frames: a
StoreRequest toward the agent, a StoreAnswer back, and a PGRecord line before
anything that reads messages or changes the account.

One entry is one NATS account, as one RabbitMQ entry is one vhost. Only a
server in operator mode (accounts and users as JWTs) is supported: the agent
holds a signing key of the account in a file on the server (NATSAdminFile) and,
for each session, signs a user JWT that carries the person's level as publish
and subscribe permissions (NATSPermissions) and expires a little after the
session's longest allowed length. Nothing is written to the server, nothing
needs a reload, and the user's key never leaves the agent's memory, so there
is nothing to delete afterwards. A server anyone can connect to without
credentials is refused, because a level would mean nothing on it.

The agent also only ever sends the fixed operations NATSStoreOps lists for the
level; the permissions are what NATS itself holds the session to.
*/

// NATS engine and service.
const (
	EngineNATS  = "nats"
	NATSService = "nats"
)

// NATSAdminFile is where the agent looks for the accounts' signing keys when
// the entry names no file.
const NATSAdminFile = "/etc/argus/nats.conf"

// NATSDefaultPort and NATSDefaultMonitorPort are nats-server's own defaults.
const (
	NATSDefaultPort        = 4222
	NATSDefaultMonitorPort = 8222
)

// NATSInboxPrefix starts the inbox of every session; each session adds its
// own token, and may subscribe only under it, so no person reads another
// app's replies.
const NATSInboxPrefix = "_INBOX_argus_"

// ValidNATSAdminFile reports whether p may be read for the signing keys: an
// absolute path to a .conf file with no way out of where it says.
func ValidNATSAdminFile(p string) bool { return ValidKafkaAdminFile(p) }

// ValidNATSAccount reports whether a is an account's name as nsc allows it:
// letters, digits, - and _, at most 63, and never the system account.
func ValidNATSAccount(a string) bool {
	if a == "" || len(a) > 63 || a == "$SYS" || a == "SYS" {
		return false
	}
	for _, c := range a {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ValidNATSName reports whether n can name a stream, a consumer or a
// key-value bucket: letters, digits, - and _, at most 255. NATS refuses
// names with spaces, dots, *, > or path separators.
func ValidNATSName(n string) bool {
	if n == "" || len(n) > 255 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ValidNATSSubject reports whether s is a subject or a subject filter:
// tokens separated by dots, none empty, * a whole token, > only the last
// one, no spaces, at most 255 bytes.
func ValidNATSSubject(s string) bool {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\r\n\x00") {
		return false
	}
	toks := strings.Split(s, ".")
	for i, t := range toks {
		switch {
		case t == "":
			return false
		case t == ">":
			if i != len(toks)-1 {
				return false
			}
		case strings.ContainsAny(t, "*>") && t != "*":
			return false
		}
	}
	return true
}

// NATSLiteralSubject reports whether s is a subject a message can be
// published to: valid and without wildcards.
func NATSLiteralSubject(s string) bool {
	return ValidNATSSubject(s) && !strings.ContainsAny(s, "*>")
}

// NATSAppSubject reports whether a person may publish to s from the console:
// a literal subject that is not one of NATS's own ($JS, $KV, $SYS, $O and
// the rest start with $) and not an inbox, so a publish can never ack, pull
// or answer for an app.
func NATSAppSubject(s string) bool {
	return NATSLiteralSubject(s) && !strings.HasPrefix(s, "$") && !strings.HasPrefix(s, "_INBOX")
}

// NATSSubjectMatches reports whether a literal subject matches a filter,
// by NATS's rules: * is one token, > is one or more at the end.
func NATSSubjectMatches(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, t := range f {
		if t == ">" {
			return len(s) > i
		}
		if i >= len(s) || (t != "*" && t != s[i]) {
			return false
		}
	}
	return len(f) == len(s)
}

// NATSPerms are the publish and subscribe rules of a session's user. Deny
// wins over allow in NATS.
type NATSPerms struct {
	PubAllow, PubDeny, SubAllow, SubDeny []string
}

// natsReadAPI is the read-only half of the JetStream API.
var natsReadAPI = []string{
	"$JS.API.INFO",
	"$JS.API.STREAM.NAMES", "$JS.API.STREAM.LIST", "$JS.API.STREAM.INFO.*",
	"$JS.API.CONSUMER.NAMES.*", "$JS.API.CONSUMER.LIST.*", "$JS.API.CONSUMER.INFO.*.*",
	"$JS.API.STREAM.MSG.GET.*", "$JS.API.DIRECT.GET.>",
}

// natsNever are subjects no level publishes to: the system account, other
// apps' replies, acks and flow control for apps' consumers, pulling from an
// app's consumer, and the account- and cluster-wide parts of the JetStream
// API.
var natsNever = []string{
	"$SYS.>", "_INBOX.>", "$JS.ACK.>", "$JS.FC.>",
	"$JS.API.CONSUMER.MSG.NEXT.>", "$JS.API.ACCOUNT.>", "$JS.API.SERVER.>", "$JS.API.META.>",
	"$JS.API.STREAM.SNAPSHOT.>", "$JS.API.STREAM.RESTORE.>", "$JS.API.STREAM.PEER.>",
	"$JS.API.STREAM.LEADER.>", "$JS.API.CONSUMER.LEADER.>", "$JS.API.CONSUMER.UNPIN.>",
}

// natsManageAPI changes streams and consumers.
var natsManageAPI = []string{
	"$JS.API.STREAM.CREATE.*", "$JS.API.STREAM.UPDATE.*", "$JS.API.STREAM.DELETE.*",
	"$JS.API.STREAM.PURGE.*", "$JS.API.STREAM.MSG.DELETE.*",
	"$JS.API.CONSUMER.CREATE.>", "$JS.API.CONSUMER.DURABLE.CREATE.>",
	"$JS.API.CONSUMER.DELETE.*.*", "$JS.API.CONSUMER.PAUSE.*.*",
}

// NATSPermissions are the rules of a level's user, whose inbox is inbox. Read
// publishes only to the read half of the JetStream API; Write publishes to
// any subject but changes no stream or consumer; Manage and Admin change
// streams and consumers too. Every level may subscribe to the account's
// subjects (to read live messages), never to other inboxes or $SYS.
func NATSPermissions(level, inbox string) (NATSPerms, bool) {
	p := NATSPerms{
		SubAllow: []string{">"},
		SubDeny:  []string{"$SYS.>", "_INBOX.>", "$JS.ACK.>", "$JS.FC.>"},
	}
	if !strings.HasPrefix(inbox, NATSInboxPrefix) || !NATSLiteralSubject(inbox) {
		return NATSPerms{}, false
	}
	switch level {
	case LevelRead:
		p.PubAllow = append([]string{}, natsReadAPI...)
	case LevelWrite:
		p.PubAllow = []string{">"}
		p.PubDeny = append(append([]string{}, natsNever...), natsManageAPI...)
	case LevelOwner, LevelDBA:
		p.PubAllow = []string{">"}
		p.PubDeny = append([]string{}, natsNever...)
	default:
		return NATSPerms{}, false
	}
	return p, true
}

// natsOps is the least level each NATS operation needs.
var natsOps = map[string]string{
	"overview":         LevelRead,
	"streams":          LevelRead,
	"stream":           LevelRead,
	"consumers":        LevelRead,
	"consumer":         LevelRead,
	"subjects":         LevelRead,
	"connections":      LevelRead,
	"get":              LevelRead,
	"tail":             LevelRead,
	"publish":          LevelWrite,
	"stream_create":    LevelOwner,
	"stream_update":    LevelOwner,
	"stream_purge":     LevelOwner,
	"stream_delete":    LevelOwner,
	"msg_delete":       LevelOwner,
	"consumer_create":  LevelOwner,
	"consumer_delete":  LevelOwner,
	"consumer_pause":   LevelOwner,
	"consumer_move":    LevelOwner,
	NATSOpKVBuckets:    LevelRead,
	NATSOpKVKeys:       LevelRead,
	NATSOpKVGet:        LevelRead,
	NATSOpKVPut:        LevelWrite,
	NATSOpKVDel:        LevelWrite,
	NATSOpKVCreate:     LevelOwner,
	NATSOpKVDelete:     LevelOwner,
	RabbitOpNetwork:    LevelDBA,
	RabbitOpNetworkSet: LevelDBA,
	BrokerOpUsers:      LevelOwner,
	BrokerOpUserSet:    LevelDBA,
	BrokerOpUserDel:    LevelDBA,
}

var natsOpOrder = []string{"overview", "streams", "stream", "consumers", "consumer", "subjects", "connections",
	"get", "tail", "publish", "stream_create", "stream_update", "stream_purge", "stream_delete", "msg_delete",
	"consumer_create", "consumer_delete", "consumer_pause", "consumer_move",
	NATSOpKVBuckets, NATSOpKVKeys, NATSOpKVGet, NATSOpKVPut, NATSOpKVDel, NATSOpKVCreate, NATSOpKVDelete,
	RabbitOpNetwork, RabbitOpNetworkSet, BrokerOpUsers, BrokerOpUserSet, BrokerOpUserDel}

// NATSOpAllowed reports whether level may ask for op.
func NATSOpAllowed(op, level string) bool {
	need, ok := natsOps[op]
	return ok && LevelRank(level) > 0 && LevelRank(level) >= LevelRank(need)
}

// NATSStoreOps lists what a level may ask for, in a fixed order; nil for an
// unknown level.
func NATSStoreOps(level string) []string {
	if LevelRank(level) == 0 {
		return nil
	}
	var out []string
	for _, op := range natsOpOrder {
		if NATSOpAllowed(op, level) {
			out = append(out, op)
		}
	}
	return out
}

// NATSRequest is what a NATS operation names beyond StoreRequest's own
// fields. Stream and Consumer name what it acts on.
type NATSRequest struct {
	Stream   string `json:"stream,omitempty"`
	Consumer string `json:"consumer,omitempty"`
	// Subject filters a get or a tail, or is where a publish goes.
	Subject string `json:"subject,omitempty"`
	// From is where a get starts: newest (the default), seq or time.
	From string `json:"from,omitempty"`
	Seq  uint64 `json:"seq,omitempty"`
	// Time is RFC 3339, for a get from a time, a consumer moved to a time,
	// or a pause until.
	Time string `json:"time,omitempty"`
	// Mode is how a publish goes: core (the default), stream (wait for the
	// stream to store it) or request (wait for a reply).
	Mode string `json:"mode,omitempty"`
	// Seconds bounds a tail or a request's wait.
	Seconds int `json:"seconds,omitempty"`
	// Purge says what a stream_purge removes: everything when empty, the
	// messages of one subject, those below Seq, or all but Keep.
	Keep uint64 `json:"keep,omitempty"`
	// StreamSpec and ConsumerSpec are a stream or a consumer to make or
	// change.
	StreamSpec   *NATSStreamSpec   `json:"streamSpec,omitempty"`
	ConsumerSpec *NATSConsumerSpec `json:"consumerSpec,omitempty"`
	// Bucket and Key name a key-value bucket and one of its keys; Key is a
	// filter (with * and >) for kv_keys. KVSpec is a bucket to make.
	Bucket string      `json:"bucket,omitempty"`
	Key    string      `json:"key,omitempty"`
	KVSpec *NATSKVSpec `json:"kvSpec,omitempty"`
}

// NATSStreamSpec is the part of a stream's configuration the console sets.
type NATSStreamSpec struct {
	Name     string   `json:"name"`
	Subjects []string `json:"subjects"`
	// Retention is limits, interest or workqueue.
	Retention string `json:"retention,omitempty"`
	// Storage is file or memory.
	Storage   string `json:"storage,omitempty"`
	Replicas  int    `json:"replicas,omitempty"`
	MaxAgeS   int64  `json:"maxAgeS,omitempty"`
	MaxBytes  int64  `json:"maxBytes,omitempty"`
	MaxMsgs   int64  `json:"maxMsgs,omitempty"`
	MaxPerSub int64  `json:"maxPerSubject,omitempty"`
	// Discard is old or new: what goes when the stream is full.
	Discard     string `json:"discard,omitempty"`
	DuplicatesS int64  `json:"duplicatesS,omitempty"`
	Description string `json:"description,omitempty"`
	AllowDelete bool   `json:"allowDelete,omitempty"`
	DenyPurge   bool   `json:"denyPurge,omitempty"`
}

// Validate refuses a stream the console should not have asked for.
func (s NATSStreamSpec) Validate() error {
	if !ValidNATSName(s.Name) {
		return errors.New("a stream's name is letters, digits, - and _")
	}
	if len(s.Subjects) == 0 || len(s.Subjects) > 32 {
		return errors.New("a stream takes 1 to 32 subjects")
	}
	for _, sub := range s.Subjects {
		if !ValidNATSSubject(sub) {
			return fmt.Errorf("%q is not a subject", sub)
		}
		if strings.HasPrefix(sub, "$") || strings.HasPrefix(sub, "_INBOX") || sub == ">" {
			return fmt.Errorf("a stream cannot take %s: it is NATS's own, or every subject", sub)
		}
	}
	switch s.Retention {
	case "", "limits", "interest", "workqueue":
	default:
		return errors.New("a stream keeps messages by limits, interest or workqueue")
	}
	switch s.Storage {
	case "", "file", "memory":
	default:
		return errors.New("a stream is stored on file or in memory")
	}
	switch s.Discard {
	case "", "old", "new":
	default:
		return errors.New("a full stream discards old or new messages")
	}
	if s.Replicas < 0 || s.Replicas > 5 {
		return errors.New("a stream has 1 to 5 copies")
	}
	if s.MaxAgeS < 0 || s.MaxBytes < -1 || s.MaxMsgs < -1 || s.MaxPerSub < -1 || s.DuplicatesS < 0 {
		return errors.New("a limit cannot be below zero")
	}
	if len(s.Description) > 512 {
		return errors.New("the description is too long")
	}
	return nil
}

// NATSConsumerSpec is the part of a consumer's configuration the console
// sets. Every consumer Argus makes is durable and pulled, or pushed to
// DeliverSubject.
type NATSConsumerSpec struct {
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	FilterSubjects []string `json:"filterSubjects,omitempty"`
	// Deliver is all, new, last, last_per_subject, by_start_sequence or
	// by_start_time.
	Deliver   string `json:"deliver,omitempty"`
	StartSeq  uint64 `json:"startSeq,omitempty"`
	StartTime string `json:"startTime,omitempty"`
	// Ack is explicit (the default), all or none.
	Ack           string  `json:"ack,omitempty"`
	AckWaitS      int64   `json:"ackWaitS,omitempty"`
	MaxDeliver    int     `json:"maxDeliver,omitempty"`
	BackoffS      []int64 `json:"backoffS,omitempty"`
	MaxAckPending int     `json:"maxAckPending,omitempty"`
	// DeliverSubject makes a push consumer; empty is a pull consumer.
	DeliverSubject string `json:"deliverSubject,omitempty"`
	DeliverGroup   string `json:"deliverGroup,omitempty"`
}

// Validate refuses a consumer the console should not have asked for.
func (c NATSConsumerSpec) Validate() error {
	if !ValidNATSName(c.Name) {
		return errors.New("a consumer's name is letters, digits, - and _")
	}
	if len(c.FilterSubjects) > 32 {
		return errors.New("at most 32 subjects")
	}
	for _, f := range c.FilterSubjects {
		if !ValidNATSSubject(f) {
			return fmt.Errorf("%q is not a subject", f)
		}
	}
	switch c.Deliver {
	case "", "all", "new", "last", "last_per_subject":
	case "by_start_sequence":
		if c.StartSeq == 0 {
			return errors.New("say which sequence it starts at")
		}
	case "by_start_time":
		if _, err := time.Parse(time.RFC3339, c.StartTime); err != nil {
			return errors.New("say the time it starts at")
		}
	default:
		return errors.New("a consumer starts at all, new, last, last_per_subject, a sequence or a time")
	}
	switch c.Ack {
	case "", "explicit", "all", "none":
	default:
		return errors.New("a consumer acks explicit, all or none")
	}
	if c.AckWaitS < 0 || c.MaxDeliver < -1 || c.MaxAckPending < -1 || len(c.BackoffS) > 16 {
		return errors.New("a limit is out of range")
	}
	for _, b := range c.BackoffS {
		if b <= 0 {
			return errors.New("each back-off is at least a second")
		}
	}
	if c.DeliverSubject != "" && !NATSAppSubject(c.DeliverSubject) {
		return errors.New("a push consumer delivers to an app's subject")
	}
	if c.DeliverGroup != "" && (c.DeliverSubject == "" || !ValidNATSName(c.DeliverGroup)) {
		return errors.New("a delivery group is a name, on a push consumer")
	}
	if len(c.Description) > 512 {
		return errors.New("the description is too long")
	}
	return nil
}

// NATSNeedsConfirm reports whether an operation destroys messages, so the
// person must type the stream's name: deleting a stream, or purging all of it.
func NATSNeedsConfirm(op string, n *NATSRequest) bool {
	switch op {
	case "stream_delete", NATSOpKVDelete:
		return true
	case "stream_purge":
		return n == nil || (n.Subject == "" && n.Seq == 0 && n.Keep == 0)
	}
	return false
}
