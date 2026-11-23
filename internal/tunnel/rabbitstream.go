package tunnel

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

/*
RabbitMQ streams beyond declaring one (rabbitqueue.go): reading a stream from
an offset or a time, the readers of a stream, and super streams.

A stream is read as a reader would: one AMQP consumer with x-stream-offset,
as the person's own user, cancelled after the messages asked for. Nothing
is taken and nobody else's place moves, so it is Read, as peek is, and
recorded as peek is.

A super stream is an exchange with x-super-stream and one stream per
partition, bound with x-super-stream-partition-order, as
rabbitmq-streams add_super_stream makes it.
*/

// RabbitMQ stream operations.
const (
	RabbitOpStreamRead         = "stream_read"
	RabbitOpStreamReaders      = "stream_readers"
	RabbitOpSuperStreams       = "super_streams"
	RabbitOpDeclareSuperStream = "declare_super_stream"
	RabbitOpDeleteSuperStream  = "delete_super_stream"
)

// RabbitStreamRead is where a stream read starts.
type RabbitStreamRead struct {
	// From is first, last, offset or time. last is the newest Count
	// messages.
	From   string `json:"from"`
	Offset int64  `json:"offset,omitempty"`
	// At is the time to start from, in Unix milliseconds.
	At int64 `json:"at,omitempty"`
}

// MaxStreamRead bounds the messages one read returns.
const MaxStreamRead = 200

// ValidStreamRead says why a stream read may not run, or "".
func ValidStreamRead(r *RabbitStreamRead) string {
	if r == nil {
		return "say where to start reading: first, last, an offset or a time"
	}
	switch r.From {
	case "first", "last":
	case "offset":
		if r.Offset < 0 {
			return "an offset is 0 or more"
		}
	case "time":
		if r.At <= 0 {
			return "a time to start from is needed"
		}
	default:
		return "a read starts from first, last, an offset or a time"
	}
	return ""
}

// StreamReadText is a stream read as the recording shows it.
func StreamReadText(name string, count int, r *RabbitStreamRead) string {
	from := "first"
	if r != nil {
		switch r.From {
		case "offset":
			from = "offset " + strconv.FormatInt(r.Offset, 10)
		case "time":
			from = "time " + strconv.FormatInt(r.At, 10)
		default:
			from = r.From
		}
	}
	return "READ STREAM " + strconv.Quote(name) + " from " + from + " count " + strconv.Itoa(count)
}

// RabbitSuperStream is a new super stream's partitions and what each keeps.
type RabbitSuperStream struct {
	// Partitions named 0 to n-1, picked by hashing the routing key; or Keys,
	// one partition per routing key, named after it.
	Partitions     int      `json:"partitions,omitempty"`
	Keys           []string `json:"keys,omitempty"`
	MaxAgeS        int64    `json:"maxAgeS,omitempty"`
	MaxLengthBytes int64    `json:"maxLengthBytes,omitempty"`
}

// MaxSuperStreamPartitions bounds a super stream made from the console.
const MaxSuperStreamPartitions = 64

var (
	superStreamNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	superStreamKeyRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,50}$`)
)

// SuperStreamKeys are the routing keys of a new super stream's partitions,
// in order: its own Keys, or 0 to n-1.
func (s RabbitSuperStream) SuperStreamKeys() []string {
	if len(s.Keys) > 0 {
		return s.Keys
	}
	keys := make([]string, max(s.Partitions, 0))
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

// ValidSuperStream says why a super stream may not be made, or "".
func ValidSuperStream(name string, s *RabbitSuperStream) string {
	if !superStreamNameRE.MatchString(name) {
		return "a super stream's name is 1 to 200 letters, digits, . _ and -"
	}
	if strings.HasPrefix(name, "amq.") {
		return "names starting amq. are RabbitMQ's own"
	}
	if s == nil {
		return "say how many partitions"
	}
	if len(s.Keys) > 0 && s.Partitions > 0 && s.Partitions != len(s.Keys) {
		return "give a number of partitions or their routing keys, not both"
	}
	keys := s.SuperStreamKeys()
	if len(keys) == 0 || len(keys) > MaxSuperStreamPartitions {
		return "a super stream has 1 to " + strconv.Itoa(MaxSuperStreamPartitions) + " partitions"
	}
	for i, k := range keys {
		if !superStreamKeyRE.MatchString(k) {
			return "a routing key is 1 to 50 letters, digits, . _ and -"
		}
		if slices.Contains(keys[:i], k) {
			return "the routing key " + strconv.Quote(k) + " is named twice"
		}
	}
	if s.MaxAgeS < 0 || s.MaxLengthBytes < 0 {
		return "a limit cannot be negative"
	}
	return ""
}

// SuperStreamText is making a super stream as the recording shows it.
func SuperStreamText(name string, s *RabbitSuperStream) string {
	if s == nil {
		return "DECLARE SUPER STREAM " + strconv.Quote(name)
	}
	keys := s.SuperStreamKeys()
	lines := []string{"DECLARE SUPER STREAM " + strconv.Quote(name) + " partitions " + strconv.Itoa(len(keys))}
	lines = append(lines, "  exchange "+strconv.Quote(name)+" direct x-super-stream true")
	keep := ""
	if s.MaxAgeS > 0 {
		keep += " x-max-age " + strconv.FormatInt(s.MaxAgeS, 10) + "s"
	}
	if s.MaxLengthBytes > 0 {
		keep += " x-max-length-bytes " + strconv.FormatInt(s.MaxLengthBytes, 10)
	}
	for i, k := range keys {
		lines = append(lines, "  stream "+strconv.Quote(name+"-"+k)+keep+" bound with key "+strconv.Quote(k)+
			" order "+strconv.Itoa(i))
	}
	return strings.Join(lines, "\n")
}
