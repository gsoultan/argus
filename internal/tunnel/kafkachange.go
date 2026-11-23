package tunnel

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

/*
Changes to a Kafka entry's topics and consumer groups beside creating and
deleting topics: a topic's settings, more partitions, deleting the oldest
messages, moving or deleting a consumer group. They need Manage, are
recorded, and wait for approval of their exact text where the entry's
changes need one.

Deleting messages and moving a group are two steps: a dry run works out the
exact offsets from what the person chose (older than a time, back by a
number, …), and the change carries those offsets, so the recording and the
approval are for what runs, not for a description of it.
*/

// Kafka change operations.
const (
	KafkaOpTopicConfigs  = "topic_configs"  // every setting of a topic, read
	KafkaOpAlterConfigs  = "alter_configs"  // set or reset settings
	KafkaOpAddPartitions = "add_partitions" // more partitions
	KafkaOpDeleteRecords = "delete_records" // delete messages before offsets
	KafkaOpGroup         = "group"          // one consumer group, read
	KafkaOpGroupReset    = "group_reset"    // move a group's committed offsets
	KafkaOpDeleteGroup   = "delete_group"   // delete an empty group
)

// KafkaChangeOps are the operations that change something, at Manage.
var KafkaChangeOps = []string{KafkaOpAlterConfigs, KafkaOpAddPartitions, KafkaOpDeleteRecords, KafkaOpGroupReset, KafkaOpDeleteGroup}

// IsKafkaChange reports whether op is one of KafkaChangeOps.
func IsKafkaChange(op string) bool { return slices.Contains(KafkaChangeOps, op) }

// KafkaChange is what a change asks for. Topic is the request's Name.
type KafkaChange struct {
	// Set and Reset: settings to give a value and settings to return to the
	// cluster's default.
	Set   map[string]string `json:"set,omitempty"`
	Reset []string          `json:"reset,omitempty"`
	// Add is how many partitions to add.
	Add int `json:"add,omitempty"`
	// Group is the consumer group moved or deleted.
	Group string `json:"group,omitempty"`
	// DryRun works out Offsets from To and returns them; nothing runs.
	DryRun bool `json:"dryRun,omitempty"`
	// To is how a dry run finds the offsets: earliest, latest, time (At, in
	// milliseconds since 1970), shift (By, back when negative), offset
	// (Offset on Partition), or all (delete_records: every message).
	To        string `json:"to,omitempty"`
	At        int64  `json:"at,omitempty"`
	By        int64  `json:"by,omitempty"`
	Offset    int64  `json:"offset,omitempty"`
	Partition *int32 `json:"partition,omitempty"`
	// Topics a group reset covers; empty is every topic of the group in
	// the entry (or Name alone when given).
	Topics []string `json:"topics,omitempty"`
	// Offsets are the exact offsets a run takes, topic → partition →
	// offset: the new start of each partition for delete_records, the new
	// committed offset for group_reset.
	Offsets map[string]map[int32]int64 `json:"offsets,omitempty"`
}

// KafkaSetting is a topic setting Argus offers to change, with what it is in
// words and what its value is.
type KafkaSetting struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Kind is ms, bytes, int, bool or one of Choices.
	Kind    string   `json:"kind"`
	Choices []string `json:"choices,omitempty"`
}

// KafkaSettings are the topic settings that can be changed from Argus.
// Settings that change how replicas follow (unclean leader election,
// throttles) are not among them.
var KafkaSettings = []KafkaSetting{
	{Key: "retention.ms", Name: "Keep messages for", Kind: "ms"},
	{Key: "retention.bytes", Name: "Keep at most, per partition", Kind: "bytes"},
	{Key: "cleanup.policy", Name: "Clean-up", Kind: "choice", Choices: []string{"delete", "compact", "compact,delete"}},
	{Key: "min.insync.replicas", Name: "In-sync replicas to write", Kind: "int"},
	{Key: "max.message.bytes", Name: "Largest message", Kind: "bytes"},
	{Key: "compression.type", Name: "Compression", Kind: "choice", Choices: []string{"producer", "uncompressed", "gzip", "snappy", "lz4", "zstd"}},
	{Key: "segment.ms", Name: "Start a new segment after", Kind: "ms"},
	{Key: "segment.bytes", Name: "Start a new segment at", Kind: "bytes"},
	{Key: "delete.retention.ms", Name: "Keep deleted keys' markers for", Kind: "ms"},
	{Key: "min.compaction.lag.ms", Name: "Compact messages older than", Kind: "ms"},
	{Key: "max.compaction.lag.ms", Name: "Compact messages by", Kind: "ms"},
	{Key: "message.timestamp.type", Name: "Message time", Kind: "choice", Choices: []string{"CreateTime", "LogAppendTime"}},
}

// KafkaSettingFor is the setting with key, if Argus offers it.
func KafkaSettingFor(key string) (KafkaSetting, bool) {
	for _, s := range KafkaSettings {
		if s.Key == key {
			return s, true
		}
	}
	return KafkaSetting{}, false
}

func validKafkaSettingValue(s KafkaSetting, v string) string {
	switch s.Kind {
	case "ms", "bytes":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < -1 {
			return s.Name + " is a number, or -1 for no limit"
		}
		if s.Key == "segment.bytes" && n < 14 {
			return "a segment is at least 14 bytes"
		}
	case "int":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return s.Name + " is a whole number from 1"
		}
	case "choice":
		if !slices.Contains(s.Choices, v) {
			return s.Name + " is one of " + strings.Join(s.Choices, ", ")
		}
	}
	return ""
}

// MaxKafkaPartitions bounds a topic's partitions after adding.
const MaxKafkaPartitions = 1000

// ValidKafkaChange says why a Kafka change may not run on the entry with
// this scope, or "".
func ValidKafkaChange(scope string, req StoreRequest) string {
	c := req.KafkaChange
	if c == nil {
		c = &KafkaChange{}
	}
	if req.Name != "" && (!ValidKafkaTopic(req.Name) || !InKafkaScope(scope, req.Name)) {
		return strconv.Quote(req.Name) + " is not one of this entry's topics"
	}
	needTopic := req.Op == KafkaOpAlterConfigs || req.Op == KafkaOpAddPartitions || req.Op == KafkaOpDeleteRecords || req.Op == KafkaOpTopicConfigs
	if needTopic && req.Name == "" {
		return "no topic named"
	}
	for t := range c.Offsets {
		if !ValidKafkaTopic(t) || !InKafkaScope(scope, t) {
			return strconv.Quote(t) + " is not one of this entry's topics"
		}
	}
	for _, t := range c.Topics {
		if !ValidKafkaTopic(t) || !InKafkaScope(scope, t) {
			return strconv.Quote(t) + " is not one of this entry's topics"
		}
	}
	switch req.Op {
	case KafkaOpAlterConfigs:
		if len(c.Set)+len(c.Reset) == 0 {
			return "nothing to change"
		}
		for k, v := range c.Set {
			s, ok := KafkaSettingFor(k)
			if !ok {
				return k + " is not a setting Argus changes"
			}
			if why := validKafkaSettingValue(s, v); why != "" {
				return why
			}
		}
		for _, k := range c.Reset {
			if _, ok := KafkaSettingFor(k); !ok {
				return k + " is not a setting Argus changes"
			}
			if _, both := c.Set[k]; both {
				return k + " is both set and reset"
			}
		}
	case KafkaOpAddPartitions:
		if c.Add < 1 || c.Add > MaxKafkaPartitions {
			return "add 1 to " + strconv.Itoa(MaxKafkaPartitions) + " partitions"
		}
	case KafkaOpDeleteRecords, KafkaOpGroupReset:
		if req.Op == KafkaOpGroupReset && !validKafkaGroup(c.Group) {
			return "no consumer group named"
		}
		if c.DryRun {
			return validKafkaTo(req.Op, c)
		}
		if len(c.Offsets) == 0 {
			return "no offsets: work them out first"
		}
		if req.Op == KafkaOpDeleteRecords && (len(c.Offsets) != 1 || c.Offsets[req.Name] == nil) {
			return "messages are deleted on the one topic named"
		}
		n := 0
		for _, parts := range c.Offsets {
			for p, o := range parts {
				if p < 0 || o < 0 {
					return "a partition and an offset are 0 or more"
				}
				n++
			}
		}
		if n > 10_000 {
			return "at most 10000 partitions in one change"
		}
	case KafkaOpDeleteGroup, KafkaOpGroup:
		if !validKafkaGroup(c.Group) {
			return "no consumer group named"
		}
	case KafkaOpTopicConfigs:
	default:
		return "unknown operation " + req.Op
	}
	return ""
}

func validKafkaGroup(g string) bool {
	if g == "" || len(g) > 249 {
		return false
	}
	for _, c := range g {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validKafkaTo(op string, c *KafkaChange) string {
	switch c.To {
	case "earliest", "latest":
		if op == KafkaOpDeleteRecords {
			return "messages are deleted older than a time, before an offset, or all"
		}
	case "all":
		if op != KafkaOpDeleteRecords {
			return "a group moves to earliest, latest, a time, by a number or to an offset"
		}
	case "time":
		if c.At <= 0 {
			return "no time given"
		}
	case "shift":
		if op == KafkaOpDeleteRecords || c.By == 0 {
			return "move it by a number of messages, not 0"
		}
	case "offset":
		if c.Partition == nil || *c.Partition < 0 || c.Offset < 0 {
			return "an offset is on one partition, 0 or more"
		}
	default:
		return "say where to: earliest, latest, a time, by a number or an offset"
	}
	return ""
}

// KafkaChangeText is a change as the recording and an approval show it,
// with every offset it sets.
func KafkaChangeText(req StoreRequest) string {
	c := req.KafkaChange
	if c == nil {
		c = &KafkaChange{}
	}
	switch req.Op {
	case KafkaOpAlterConfigs:
		lines := []string{"ALTER TOPIC " + req.Name}
		keys := make([]string, 0, len(c.Set))
		for k := range c.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, "  SET "+k+" = "+c.Set[k])
		}
		reset := slices.Clone(c.Reset)
		sort.Strings(reset)
		for _, k := range reset {
			lines = append(lines, "  RESET "+k+" (to the cluster's default)")
		}
		return strings.Join(lines, "\n")
	case KafkaOpAddPartitions:
		return fmt.Sprintf("ADD %d PARTITION%s TO TOPIC %s", c.Add, map[bool]string{true: "S"}[c.Add != 1], req.Name)
	case KafkaOpDeleteRecords:
		return "DELETE MESSAGES ON TOPIC " + req.Name + " BEFORE\n" + offsetLines(c.Offsets)
	case KafkaOpGroupReset:
		return "MOVE CONSUMER GROUP " + strconv.Quote(c.Group) + " TO\n" + offsetLines(c.Offsets)
	case KafkaOpDeleteGroup:
		return "DELETE CONSUMER GROUP " + strconv.Quote(c.Group)
	}
	return strings.ToUpper(req.Op)
}

func offsetLines(offs map[string]map[int32]int64) string {
	topics := make([]string, 0, len(offs))
	for t := range offs {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	var lines []string
	for _, t := range topics {
		parts := make([]int32, 0, len(offs[t]))
		for p := range offs[t] {
			parts = append(parts, p)
		}
		slices.Sort(parts)
		for _, p := range parts {
			lines = append(lines, fmt.Sprintf("  %s partition %d offset %d", t, p, offs[t][p]))
		}
	}
	return strings.Join(lines, "\n")
}
