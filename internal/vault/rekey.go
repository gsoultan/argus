package vault

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

// RekeyResult is what Rekey changed.
type RekeyResult struct {
	Values     int  `json:"values"`
	RootRotate bool `json:"rootRotated"`
}

// Rekey replaces the organization key, gives every stored value a new data
// key, and re-encrypts everything in wrapping format v2 (committing). With
// newRoot, the keys the root key wraps are wrapped under newRoot instead.
//
// It runs in one transaction: a failure part way leaves the store as it was.
// Afterwards format v1 no longer opens. The store must not be in use by a
// running argus-secret; bbolt's file lock refuses a second opener.
func (s *Store) Rekey(root, newRoot []byte, actor Actor, reason string) (RekeyResult, error) {
	if len(root) != KeySize || (newRoot != nil && len(newRoot) != KeySize) {
		return RekeyResult{}, fmt.Errorf("root keys are %d bytes", KeySize)
	}
	if newRoot != nil && bytes.Equal(root, newRoot) {
		return RekeyResult{}, errors.New("the new root key is the old one")
	}
	target := root
	if newRoot != nil {
		target = newRoot
	}
	orgKey, err := randomKey()
	if err != nil {
		return RekeyResult{}, err
	}
	var res RekeyResult
	res.RootRotate = newRoot != nil
	err = s.db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		check, err := unwrap(root, meta.Get(metaCheck), []byte("root-check"), true)
		if err != nil || !bytes.Equal(check, rootCheck) {
			return ErrWrongRootKey
		}
		// Every value: open with its old data key, seal with a new one.
		vals := tx.Bucket(bucketValues)
		type item struct{ key, raw []byte }
		var redo []item
		err = vals.ForEach(func(k, v []byte) error {
			name, n, ok := parseValueKey(k)
			if !ok {
				return fmt.Errorf("a stored value has a malformed key %q", k)
			}
			plain, err := s.getValueRaw(v, name, n, true)
			if err != nil {
				return err
			}
			raw, err := sealValue(orgKey, s.org, name, n, plain)
			clear(plain)
			if err != nil {
				return err
			}
			redo = append(redo, item{bytes.Clone(k), raw})
			return nil
		})
		if err != nil {
			return err
		}
		for _, it := range redo {
			if err := vals.Put(it.key, it.raw); err != nil {
				return err
			}
		}
		res.Values = len(redo)
		// What the root key wraps.
		sealRaw := s.seal.Bytes()
		pqRaw, err := s.sealPQ.Bytes()
		if err != nil {
			return err
		}
		defer clear(pqRaw)
		for _, w := range []struct {
			key, plain, aad []byte
		}{
			{metaCheck, rootCheck, []byte("root-check")},
			{metaOrgKey, orgKey, []byte("org-key\x00" + s.org)},
			{metaSealKey, sealRaw, []byte("seal-key")},
			{metaSealKeyPQ, pqRaw, []byte("seal-key-pq")},
		} {
			ct, err := wrap(target, w.plain, w.aad)
			if err != nil {
				return err
			}
			if err := meta.Put(w.key, ct); err != nil {
				return err
			}
		}
		if err := meta.Put(metaCommitOnly, []byte("1")); err != nil {
			return err
		}
		detail := fmt.Sprintf("re-encrypted %d values under a new organization key", res.Values)
		if res.RootRotate {
			detail += " and a new root key"
		}
		return s.appendLog(tx, Entry{Actor: actor, Action: "rekey", Reason: reason, Outcome: OutcomeOK, Detail: detail})
	})
	if err != nil {
		clear(orgKey)
		return RekeyResult{}, err
	}
	clear(s.orgKey)
	s.orgKey, s.legacy = orgKey, false
	lockMemory(s.orgKey)
	return res, nil
}

// sealValue encrypts plain under a new data key, wrapped under orgKey.
func sealValue(orgKey []byte, org, name string, n int, plain []byte) ([]byte, error) {
	dek, err := randomKey()
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	ct, err := wrap(dek, plain, valueAAD(name, n))
	if err != nil {
		return nil, err
	}
	wrapped, err := wrap(orgKey, dek, dekAAD(org, name, n))
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealedValue{DEK: wrapped, Value: ct})
}

// getValueRaw opens a stored value row with the store's organization key.
func (s *Store) getValueRaw(raw []byte, name string, n int, legacy bool) ([]byte, error) {
	var sv sealedValue
	if err := json.Unmarshal(raw, &sv); err != nil {
		return nil, err
	}
	dek, err := unwrap(s.orgKey, sv.DEK, dekAAD(s.org, name, n), legacy)
	if err != nil {
		return nil, fmt.Errorf("%s version %d: data key %w", name, n, err)
	}
	defer clear(dek)
	plain, err := unwrap(dek, sv.Value, valueAAD(name, n), legacy)
	if err != nil {
		return nil, fmt.Errorf("%s version %d: value %w", name, n, err)
	}
	return plain, nil
}

func parseValueKey(k []byte) (string, int, bool) {
	if len(k) < 1+8 || k[len(k)-9] != 0 {
		return "", 0, false
	}
	return string(k[:len(k)-9]), int(binary.BigEndian.Uint64(k[len(k)-8:])), true
}

// EncryptionState says how much of the store is in wrapping format v2.
type EncryptionState struct {
	CommitOnly bool `json:"commitOnly"`
	Values     int  `json:"values"`
	V2Values   int  `json:"v2Values"`
}

// Encryption reports the store's wrapping formats, for argus-secret status.
func (s *Store) Encryption() (EncryptionState, error) {
	st := EncryptionState{CommitOnly: !s.legacy}
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketValues).ForEach(func(k, v []byte) error {
			name, n, ok := parseValueKey(k)
			if !ok {
				return nil
			}
			st.Values++
			var sv sealedValue
			if json.Unmarshal(v, &sv) == nil && isWrapV2(s.orgKey, sv.DEK, dekAAD(s.org, name, n)) {
				st.V2Values++
			}
			return nil
		})
	})
	return st, err
}
