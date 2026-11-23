package tunnel

import (
	"errors"
	"strings"
)

/*
NATS key-value buckets, as the console reaches them: a bucket is a stream
named KV_<bucket> storing subjects $KV.<bucket>.<key>, so reading a key is
a direct get by subject (Read), putting or deleting one is a publish
(Write), and making or removing a bucket is making or removing a stream
(Manage). Values read and written are recorded; the bucket list and its
keys are browsing and are not.

Which machines can connect to the NATS server is the network and
network_set store requests, at Admin: Argus changes the server's firewall
for the client port only, and leaves nats-server's own listen address as
its config says, like Kafka's.
*/

const (
	NATSOpKVBuckets = "kv_buckets"
	NATSOpKVKeys    = "kv_keys"
	NATSOpKVGet     = "kv_get"
	NATSOpKVPut     = "kv_put"
	NATSOpKVDel     = "kv_del"
	NATSOpKVCreate  = "kv_create"
	NATSOpKVDelete  = "kv_delete"
)

// NATSKVSpec is a bucket the console makes.
type NATSKVSpec struct {
	Bucket      string `json:"bucket"`
	Description string `json:"description,omitempty"`
	// History is how many values of each key are kept, 1 to 64.
	History int `json:"history,omitempty"`
	// TTLS is how long a value is kept, in seconds; 0 for ever.
	TTLS         int64  `json:"ttlS,omitempty"`
	MaxBytes     int64  `json:"maxBytes,omitempty"`
	MaxValueSize int32  `json:"maxValueSize,omitempty"`
	Storage      string `json:"storage,omitempty"`
	Replicas     int    `json:"replicas,omitempty"`
}

// ValidNATSBucket reports whether b is a bucket's name: letters, digits,
// - and _, as NATS takes them.
func ValidNATSBucket(b string) bool {
	return len(b) <= 64 && ValidNATSName(b)
}

// ValidNATSKey reports whether k is a key: letters, digits and -/_=. with
// no empty part between dots, and nothing starting with _kv (kept for NATS).
// With filter, * and > stand for one part and the rest.
func ValidNATSKey(k string, filter bool) bool {
	if k == "" || len(k) > 255 || strings.HasPrefix(k, "_kv") {
		return false
	}
	parts := strings.Split(k, ".")
	for i, p := range parts {
		if p == "" {
			return false
		}
		if filter && (p == "*" || (p == ">" && i == len(parts)-1)) {
			continue
		}
		for _, c := range p {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-/_=", c)) {
				return false
			}
		}
	}
	return true
}

// Validate refuses a bucket the console should not have asked for.
func (k NATSKVSpec) Validate() error {
	if !ValidNATSBucket(k.Bucket) {
		return errors.New("a bucket's name is letters, digits, - and _, at most 64")
	}
	if k.History < 0 || k.History > 64 {
		return errors.New("a bucket keeps 1 to 64 values of each key")
	}
	if k.TTLS < 0 || k.MaxBytes < -1 || k.MaxValueSize < -1 {
		return errors.New("a limit cannot be below zero")
	}
	switch k.Storage {
	case "", "file", "memory":
	default:
		return errors.New("a bucket is stored on file or in memory")
	}
	if k.Replicas < 0 || k.Replicas > 5 {
		return errors.New("a bucket has 1 to 5 copies")
	}
	if len(k.Description) > 512 {
		return errors.New("the description is too long")
	}
	return nil
}

// NATSNetworkText is a network_set on NATS as the lines an approval is
// given for: the firewall alone.
func NATSNetworkText(req StoreRequest) string {
	return FirewallText(req.Reach, req.Sources)
}
