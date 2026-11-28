package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"

	"go.etcd.io/bbolt"
)

// wrapV1 is the format before key commitment, to stand for an older store.
func wrapV1(t *testing.T, key, plain, aad []byte) []byte {
	t.Helper()
	gcm, err := newGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plain, aad)
}

func TestWrapIsCommittingAndBoundToItsContext(t *testing.T) {
	k, _ := randomKey()
	other, _ := randomKey()
	ct, err := wrap(k, []byte("hunter2"), []byte("value\x00prod/db\x001"))
	if err != nil {
		t.Fatal(err)
	}
	if ct[0] != wrapV2 || len(ct) != 1+32+12+7+16 {
		t.Fatalf("layout: % x (%d bytes)", ct[:1], len(ct))
	}
	if got, err := unwrap(k, ct, []byte("value\x00prod/db\x001"), false); err != nil || string(got) != "hunter2" {
		t.Fatalf("unwrap: %q %v", got, err)
	}
	for _, c := range []struct {
		name string
		key  []byte
		aad  string
	}{{"another key", other, "value\x00prod/db\x001"}, {"another version", k, "value\x00prod/db\x002"}, {"another secret", k, "value\x00prod/x\x001"}} {
		if _, err := unwrap(c.key, ct, []byte(c.aad), true); err == nil {
			t.Errorf("%s opened it", c.name)
		}
	}
	for _, i := range []int{1, 32, 33, 44, 45, len(ct) - 1} {
		bad := bytes.Clone(ct)
		bad[i] ^= 1
		if _, err := unwrap(k, bad, []byte("value\x00prod/db\x001"), true); err == nil {
			t.Errorf("opened with byte %d flipped", i)
		}
	}
}

// The commitment is what ties a v2 value to one key: a value whose
// commitment was made for another key is refused before GCM is tried.
func TestCommitmentForAnotherKeyIsRefused(t *testing.T) {
	k, _ := randomKey()
	other, _ := randomKey()
	aad := []byte("dek\x00impack\x00prod/db\x001")
	ct, _ := wrap(k, []byte("data key"), aad)
	_, ck, _ := wrapKeys(other)
	forged := bytes.Clone(ct)
	copy(forged[1:33], commitment(ck, ct[33:45], aad))
	if _, err := unwrapV2(k, forged, aad); err == nil {
		t.Fatal("a commitment made for another key was accepted")
	}
	if _, err := unwrapV2(other, forged, aad); err == nil {
		t.Fatal("GCM opened under a key it was not made with")
	}
}

func TestFormatV1OpensOnlyWhileLegacyIsAllowed(t *testing.T) {
	k, _ := randomKey()
	v1 := wrapV1(t, k, []byte("old"), []byte("aad"))
	if got, err := unwrap(k, v1, []byte("aad"), true); err != nil || string(got) != "old" {
		t.Fatalf("legacy: %q %v", got, err)
	}
	if _, err := unwrap(k, v1, []byte("aad"), false); err == nil {
		t.Fatal("v1 opened with legacy off")
	}
}

// olderStore makes a store as it was before key commitment: values and keys
// in format v1, no commit-only mark.
func olderStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	s, dir, root := testStore(t)
	put(t, s, "prod/payments/db", TypeKV, `{"password":"s3cret"}`)
	put(t, s, "prod/payments/db", TypeKV, `{"password":"s3cret2"}`)
	put(t, s, "prod/edge/key", TypeText, "edge")
	err := s.db.Update(func(tx *bbolt.Tx) error {
		vals := tx.Bucket(bucketValues)
		type kv struct{ k, v []byte }
		var redo []kv
		_ = vals.ForEach(func(k, v []byte) error {
			name, n, _ := parseValueKey(k)
			plain, err := s.getValueRaw(v, name, n, true)
			if err != nil {
				return err
			}
			dek, _ := randomKey()
			raw, _ := json.Marshal(sealedValue{DEK: wrapV1(t, s.orgKey, dek, dekAAD(s.org, name, n)), Value: wrapV1(t, dek, plain, valueAAD(name, n))})
			redo = append(redo, kv{bytes.Clone(k), raw})
			return nil
		})
		for _, r := range redo {
			_ = vals.Put(r.k, r.v)
		}
		meta := tx.Bucket(bucketMeta)
		_ = meta.Put(metaOrgKey, wrapV1(t, root, s.orgKey, []byte("org-key\x00"+s.org)))
		_ = meta.Put(metaCheck, wrapV1(t, root, rootCheck, []byte("root-check")))
		return meta.Delete(metaCommitOnly)
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, root
}

func readText(t *testing.T, s *Store, name, field string) string {
	t.Helper()
	v, _, err := s.Read(ReadInput{Name: name, Field: field, Actor: alice, Route: RouteConsole})
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(v)
}

func TestRekeyMovesAnOlderStoreToCommittingEncryption(t *testing.T) {
	s, dir, root := olderStore(t)
	st, _ := s.Encryption()
	if st.CommitOnly || st.V2Values != 0 || st.Values != 3 {
		t.Fatalf("before: %+v", st)
	}
	if readText(t, s, "prod/payments/db", "password") != "s3cret2" {
		t.Fatal("an older value did not read")
	}
	res, err := s.Rekey(root, nil, Actor{Kind: ActorSystem, ID: "argus-secret rekey"}, "yearly rotation")
	if err != nil || res.Values != 3 {
		t.Fatalf("rekey: %+v %v", res, err)
	}
	st, _ = s.Encryption()
	if !st.CommitOnly || st.V2Values != 3 {
		t.Fatalf("after: %+v", st)
	}
	if readText(t, s, "prod/payments/db", "password") != "s3cret2" || readText(t, s, "prod/edge/key", "") != "edge" {
		t.Fatal("a value changed in the rekey")
	}
	if err := s.VerifyLog(); err != nil {
		t.Fatal(err)
	}
	// After a reopen, a value planted in format v1 does not open.
	s.Close()
	s2, err := Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_ = s2.db.Update(func(tx *bbolt.Tx) error {
		dek, _ := randomKey()
		raw, _ := json.Marshal(sealedValue{DEK: wrapV1(t, s2.orgKey, dek, dekAAD(s2.org, "prod/edge/key", 1)),
			Value: wrapV1(t, dek, []byte("planted"), valueAAD("prod/edge/key", 1))})
		return tx.Bucket(bucketValues).Put(valueKey("prod/edge/key", 1), raw)
	})
	if _, _, err := s2.Read(ReadInput{Name: "prod/edge/key", Actor: alice, Route: RouteConsole}); err == nil {
		t.Fatal("a format v1 value opened after the rekey")
	}
}

func TestRekeyRotatesTheRootKey(t *testing.T) {
	s, dir, root := olderStore(t)
	newRoot, _ := randomKey()
	if _, err := s.Rekey(newRoot, newRoot, alice, ""); err == nil {
		t.Fatal("rekey with the wrong current root key went ahead")
	}
	if _, err := s.Rekey(root, root, alice, ""); err == nil {
		t.Fatal("a new root key equal to the old one was accepted")
	}
	if _, err := s.Rekey(root, newRoot, alice, "custodian change"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(dir, root, "impack"); err == nil {
		t.Fatal("the old root key still opens the store")
	}
	s2, err := Open(dir, newRoot, "impack")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if readText(t, s2, "prod/payments/db", "password") != "s3cret2" {
		t.Fatal("value lost in the root rotation")
	}
	if !bytes.Equal(s2.SealPublicKeyPQ(), s.SealPublicKeyPQ()) {
		t.Fatal("the post-quantum seal key changed: argus-control would hold a stale one")
	}
}

func TestNewStoreIsCommitOnly(t *testing.T) {
	s, _, _ := testStore(t)
	put(t, s, "prod/db", TypeText, "x")
	st, err := s.Encryption()
	if err != nil || !st.CommitOnly || st.V2Values != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}
