package vault

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/gsoultan/argus/internal/hashchain"
	"go.etcd.io/bbolt"
)

const genesis = hashchain.Genesis

// Actor kinds.
const (
	ActorServer  = "server"
	ActorApp     = "app"
	ActorPerson  = "person"
	ActorControl = "argus-control"
	ActorSystem  = "system"
)

// Routes a request came by.
const (
	RouteDirect  = "direct"
	RouteConsole = "console"
)

// Outcomes.
const (
	OutcomeOK      = "ok"
	OutcomeRefused = "refused"
)

// Actor is who did something: a server by its agent credential, an app token,
// or a person acting through the console.
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Entry is one line of the read log. Never a value.
type Entry struct {
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"at"`
	Org     string    `json:"org"`
	Actor   Actor     `json:"actor"`
	Action  string    `json:"action"`
	Secret  string    `json:"secret,omitempty"`
	Version int       `json:"version,omitempty"`
	Field   string    `json:"field,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Route   string    `json:"route,omitempty"`
	Remote  string    `json:"remote,omitempty"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
	// Prev is the link before this one, so a reader can check the entry
	// follows the one it already has.
	Prev string `json:"prev"`
}

// LogItem is an entry as argus-control collects it: the exact line that was
// chained, and the link it produced. Hash = hashchain.Next(Prev, Line).
type LogItem struct {
	Seq  uint64          `json:"seq"`
	Line json.RawMessage `json:"line"`
	Hash string          `json:"hash"`
}

// maxLogBatch bounds one collection.
const maxLogBatch = 1000

// appendLog chains e onto the log inside tx, so it commits with whatever it
// records or not at all.
func (s *Store) appendLog(tx *bbolt.Tx, e Entry) error {
	meta := tx.Bucket(bucketMeta)
	seq := uint64(0)
	if raw := meta.Get(metaLogSeq); raw != nil {
		seq = binary.BigEndian.Uint64(raw)
	}
	seq++
	head := string(meta.Get(metaLogHead))
	if head == "" {
		head = genesis
	}
	e.Seq, e.Org, e.Prev = seq, s.org, head
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := binary.BigEndian.AppendUint64(nil, seq)
	if err := tx.Bucket(bucketLog).Put(key, line); err != nil {
		return err
	}
	if err := meta.Put(metaLogSeq, key); err != nil {
		return err
	}
	return meta.Put(metaLogHead, []byte(hashchain.Next(head, line)))
}

// LogState is where the log stands.
type LogState struct {
	// Seq is the last entry written, Head its link.
	Seq  uint64 `json:"seq"`
	Head string `json:"head"`
	// Acked is the last entry argus-control said it has stored.
	Acked uint64 `json:"acked"`
	// Base is the link before the oldest entry still kept: Genesis until
	// entries argus-control already has are pruned.
	Base string `json:"base"`
}

// LogState reports the log's position.
func (s *Store) LogState() (LogState, error) {
	var st LogState
	err := s.db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(metaLogSeq); raw != nil {
			st.Seq = binary.BigEndian.Uint64(raw)
		}
		if raw := meta.Get(metaLogAcked); raw != nil {
			st.Acked = binary.BigEndian.Uint64(raw)
		}
		st.Head = string(meta.Get(metaLogHead))
		st.Base = string(meta.Get(metaLogBase))
		if st.Base == "" {
			st.Base = genesis
		}
		return nil
	})
	return st, err
}

// LogAfter returns up to limit entries after seq, oldest first.
func (s *Store) LogAfter(seq uint64, limit int) ([]LogItem, error) {
	if limit <= 0 || limit > maxLogBatch {
		limit = maxLogBatch
	}
	var out []LogItem
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketLog).Cursor()
		for k, v := c.Seek(binary.BigEndian.AppendUint64(nil, seq+1)); k != nil && len(out) < limit; k, v = c.Next() {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			line := append(json.RawMessage(nil), v...)
			out = append(out, LogItem{Seq: binary.BigEndian.Uint64(k), Line: line, Hash: hashchain.Next(e.Prev, v)})
		}
		return nil
	})
	return out, err
}

// AckLog records that argus-control has stored every entry up to seq. It
// never moves backwards.
func (s *Store) AckLog(seq uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		var last, acked uint64
		if raw := meta.Get(metaLogSeq); raw != nil {
			last = binary.BigEndian.Uint64(raw)
		}
		if raw := meta.Get(metaLogAcked); raw != nil {
			acked = binary.BigEndian.Uint64(raw)
		}
		if seq > last {
			return errors.New("acknowledged an entry that was never written")
		}
		if seq <= acked {
			return nil
		}
		return meta.Put(metaLogAcked, binary.BigEndian.AppendUint64(nil, seq))
	})
}

// PruneLog removes entries argus-control has acknowledged and that are older
// than before. The link before the oldest kept entry is saved as the base, so
// what remains still verifies from a known point.
func (s *Store) PruneLog(before time.Time) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		var acked uint64
		if raw := meta.Get(metaLogAcked); raw != nil {
			acked = binary.BigEndian.Uint64(raw)
		}
		b := tx.Bucket(bucketLog)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if binary.BigEndian.Uint64(k) > acked {
				break
			}
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if !e.At.Before(before) {
				break
			}
			if err := meta.Put(metaLogBase, []byte(hashchain.Next(e.Prev, v))); err != nil {
				return err
			}
			if err := c.Delete(); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// VerifyLog checks every kept entry follows the one before it, from the base
// to the head. A broken link means the log was edited.
func (s *Store) VerifyLog() error {
	st, err := s.LogState()
	if err != nil {
		return err
	}
	head := st.Base
	err = s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketLog).ForEach(func(k, v []byte) error {
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.Prev != head {
				return errors.New("the read log is broken: an entry does not follow the one before it")
			}
			head = hashchain.Next(head, v)
			return nil
		})
	})
	if err != nil {
		return err
	}
	if head != st.Head {
		return errors.New("the read log is broken: its last entry is not the recorded head")
	}
	return nil
}
