package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
NATS key-value buckets. A bucket is the stream KV_<bucket>, and a key the
subject $KV.<bucket>.<key> in it, so every read here is the stream's own
direct get: no watcher or consumer is made, which is also why Read can do
it. Putting and deleting a key go through the bucket, as apps do.
*/

const natsKVShowKeys = 500

// NATSKVBucket is one bucket as the console lists it.
type NATSKVBucket struct {
	Bucket       string `json:"bucket"`
	Description  string `json:"description,omitempty"`
	Values       uint64 `json:"values"`
	History      int64  `json:"history"`
	TTLS         int64  `json:"ttlS"`
	Bytes        uint64 `json:"bytes"`
	Storage      string `json:"storage"`
	Replicas     int    `json:"replicas"`
	MaxBytes     int64  `json:"maxBytes"`
	MaxValueSize int32  `json:"maxValueSize"`
	Compressed   bool   `json:"compressed,omitempty"`
}

// NATSKVKey is one key: its latest revision and whether that is a delete.
type NATSKVKey struct {
	Key      string `json:"key"`
	Revision uint64 `json:"revision"`
	Time     string `json:"time,omitempty"`
	Size     int    `json:"size"`
	// Op is put, or del or purge when the key's latest value is a delete
	// marker that the bucket keeps until it is compacted.
	Op string `json:"op"`
	// Kept is how many values of the key the bucket holds.
	Kept uint64 `json:"kept"`
}

// NATSKVEntry is one value of a key, newest first in a kv_get.
type NATSKVEntry struct {
	Revision uint64      `json:"revision"`
	Op       string      `json:"op"`
	Value    NATSMessage `json:"value"`
}

func natsKVOp(h map[string][]string) string {
	for k, v := range h {
		if strings.EqualFold(k, "KV-Operation") && len(v) > 0 {
			switch strings.ToUpper(v[0]) {
			case "DEL":
				return "del"
			case "PURGE":
				return "purge"
			}
		}
	}
	return "put"
}

func (s *natsSession) kvStream(ctx context.Context, bucket string) (jetstream.Stream, error) {
	if !tunnel.ValidNATSBucket(bucket) {
		return nil, errors.New("no bucket named")
	}
	st, err := s.js.Stream(ctx, "KV_"+bucket)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil, errors.New("there is no bucket " + bucket)
	}
	return st, err
}

func (s *natsSession) kvBuckets(ctx context.Context) (tunnel.StoreAnswer, error) {
	l := s.js.KeyValueStores(ctx)
	out := []NATSKVBucket{}
	for st := range l.Status() {
		c := st.Config()
		out = append(out, NATSKVBucket{Bucket: st.Bucket(), Description: c.Description, Values: st.Values(), History: st.History(),
			TTLS: int64(st.TTL() / time.Second), Bytes: st.Bytes(), Storage: c.Storage.String(), Replicas: max(c.Replicas, 1),
			MaxBytes: c.MaxBytes, MaxValueSize: c.MaxValueSize, Compressed: st.IsCompressed()})
	}
	if err := l.Error(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return tunnel.StoreAnswer{Type: "result", Data: out, Total: int64(len(out))}, nil
}

// kvKeys lists a bucket's keys, filtered by n.Key (with * and >), with the
// latest revision of each: at most 500, in key order.
func (s *natsSession) kvKeys(ctx context.Context, n *tunnel.NATSRequest) (tunnel.StoreAnswer, error) {
	filter := n.Key
	if filter == "" {
		filter = ">"
	}
	if filter != ">" && !tunnel.ValidNATSKey(filter, true) {
		return tunnel.StoreAnswer{Type: "error", Error: "the key filter is not a key; * stands for one part and > for the rest"}, nil
	}
	st, err := s.kvStream(ctx, n.Bucket)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	pre := "$KV." + n.Bucket + "."
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(pre+filter))
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	keys := make([]string, 0, len(info.State.Subjects))
	for subj := range info.State.Subjects {
		keys = append(keys, strings.TrimPrefix(subj, pre))
	}
	sort.Strings(keys)
	total := len(keys)
	truncated := total > natsKVShowKeys
	if truncated {
		keys = keys[:natsKVShowKeys]
	}
	out := make([]NATSKVKey, 0, len(keys))
	for _, k := range keys {
		kk := NATSKVKey{Key: k, Kept: info.State.Subjects[pre+k], Op: "put"}
		if m, err := st.GetLastMsgForSubject(ctx, pre+k); err == nil {
			kk.Revision, kk.Size, kk.Op = m.Sequence, len(m.Data), natsKVOp(m.Header)
			kk.Time = m.Time.UTC().Format(time.RFC3339Nano)
		} else if ctx.Err() != nil {
			return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
		}
		out = append(out, kk)
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, Total: int64(total), Truncated: truncated}, nil
}

// kvGet reads a key's values, newest first: what the bucket keeps of its
// history. Recorded, as reading a stream's messages is.
func (s *natsSession) kvGet(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidNATSBucket(n.Bucket) || !tunnel.ValidNATSKey(n.Key, false) {
		return tunnel.StoreAnswer{Type: "error", Error: "say the bucket and the key"}, nil
	}
	subj := "$KV." + n.Bucket + "." + n.Key
	return s.run(req.ID, rec, "KV GET "+n.Bucket+" "+n.Key, "KV GET", func() (any, int64, bool, error) {
		st, err := s.kvStream(ctx, n.Bucket)
		if err != nil {
			return nil, 0, false, err
		}
		state := st.CachedInfo().State
		var out []NATSKVEntry
		seq := state.FirstSeq
		for i := 0; i < 64 && seq <= state.LastSeq && seq > 0 && ctx.Err() == nil; i++ {
			m, err := st.GetMsg(ctx, seq, jetstream.WithGetMsgSubject(subj))
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				break
			}
			if err != nil {
				return nil, 0, false, err
			}
			out = append(out, NATSKVEntry{Revision: m.Sequence, Op: natsKVOp(m.Header),
				Value: natsMessage(m.Sequence, m.Subject, m.Time, m.Header, m.Data)})
			seq = m.Sequence + 1
		}
		if len(out) == 0 {
			return nil, 0, false, errors.New("there is no key " + n.Key + " in " + n.Bucket)
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return map[string]any{"key": n.Key, "entries": out}, int64(len(out)), false, nil
	})
}

func (s *natsSession) kvHandle(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if !tunnel.ValidNATSBucket(bucket) {
		return nil, errors.New("no bucket named")
	}
	kv, err := s.js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, errors.New("there is no bucket " + bucket)
	}
	return kv, err
}

func (s *natsSession) kvPut(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidNATSBucket(n.Bucket) || !tunnel.ValidNATSKey(n.Key, false) {
		return tunnel.StoreAnswer{Type: "error", Error: "say the bucket, and a key of letters, digits and -/_=. "}, nil
	}
	if len(req.Payload) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the value is too long"}, nil
	}
	return s.run(req.ID, rec, "KV PUT "+n.Bucket+" "+n.Key+" "+req.Payload, "KV PUT", func() (any, int64, bool, error) {
		kv, err := s.kvHandle(ctx, n.Bucket)
		if err != nil {
			return nil, 0, false, err
		}
		rev, err := kv.Put(ctx, n.Key, []byte(req.Payload))
		if err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"revision": rev}, 1, false, nil
	})
}

func (s *natsSession) kvDel(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidNATSBucket(n.Bucket) || !tunnel.ValidNATSKey(n.Key, false) {
		return tunnel.StoreAnswer{Type: "error", Error: "say the bucket and the key"}, nil
	}
	return s.run(req.ID, rec, "KV DEL "+n.Bucket+" "+n.Key, "KV DEL", func() (any, int64, bool, error) {
		kv, err := s.kvHandle(ctx, n.Bucket)
		if err != nil {
			return nil, 0, false, err
		}
		if err := kv.Delete(ctx, n.Key); err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"deleted": n.Key}, 1, false, nil
	})
}

func natsKVConfig(k tunnel.NATSKVSpec) jetstream.KeyValueConfig {
	c := jetstream.KeyValueConfig{Bucket: k.Bucket, Description: k.Description, History: uint8(max(k.History, 1)),
		TTL: time.Duration(k.TTLS) * time.Second, MaxBytes: k.MaxBytes, MaxValueSize: k.MaxValueSize, Replicas: max(k.Replicas, 1)}
	if c.MaxBytes == 0 {
		c.MaxBytes = -1
	}
	if c.MaxValueSize == 0 {
		c.MaxValueSize = -1
	}
	if k.Storage == "memory" {
		c.Storage = jetstream.MemoryStorage
	}
	return c
}

func (s *natsSession) kvCreate(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.KVSpec == nil {
		return tunnel.StoreAnswer{Type: "error", Error: "say what the bucket is"}, nil
	}
	if err := n.KVSpec.Validate(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	cfg := natsKVConfig(*n.KVSpec)
	b, _ := json.Marshal(cfg)
	return s.run(req.ID, rec, "KV CREATE "+string(b), "KV CREATE", func() (any, int64, bool, error) {
		if _, err := s.js.CreateKeyValue(ctx, cfg); err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"bucket": cfg.Bucket}, 1, false, nil
	})
}

func (s *natsSession) kvDelete(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidNATSBucket(n.Bucket) {
		return tunnel.StoreAnswer{Type: "error", Error: "no bucket named"}, nil
	}
	text := "KV DELETE " + n.Bucket
	if req.Confirm != n.Bucket {
		return refuseNATS(req.ID, rec, text, "removing the bucket "+n.Bucket+" and every key in it needs you to type the bucket's name to confirm", true)
	}
	return s.run(req.ID, rec, text, "KV DELETE", func() (any, int64, bool, error) {
		if err := s.js.DeleteKeyValue(ctx, n.Bucket); err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"deleted": n.Bucket}, 1, false, nil
	})
}
