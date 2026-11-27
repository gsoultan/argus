package vault

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/secretwire"
	"go.etcd.io/bbolt"
)

func pqOneTimeKey(t *testing.T) (interface {
	Bytes() ([]byte, error)
}, string) {
	t.Helper()
	k, err := secretwire.NewPQKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, secretwire.EncodeKey(k.PublicKey().Bytes())
}

// An agent that sends a post-quantum key gets a format v2 answer, which only
// that key opens.
func TestPostQuantumReadIsAnsweredInFormatV2(t *testing.T) {
	h := newHarness(t)
	put(t, h.store, "prod/payments/db", TypeKV, `{"password":"s3cret"}`)
	k, err := secretwire.NewPQKey()
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Field: "password",
		Recipient: secretwire.EncodeKey(k.PublicKey().Bytes())})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if bytes.Contains(body, []byte("s3cret")) {
		t.Fatal("the value travelled in the clear")
	}
	var resp secretwire.ReadResponse
	_ = json.Unmarshal(body, &resp)
	if secretwire.SealVersion(resp.Sealed) != 2 {
		t.Fatalf("answered in format %d", secretwire.SealVersion(resp.Sealed))
	}
	v, err := secretwire.OpenPQ(k, resp.Sealed, secretwire.ReadAAD("prod/payments/db", resp.Version, "password"))
	if err != nil || string(v) != "s3cret" {
		t.Fatalf("open: %q %v", v, err)
	}
}

func TestWriteSealedPostQuantumOpens(t *testing.T) {
	h := newHarness(t)
	r, err := secretwire.ParseRecipient(secretwire.EncodeKey(h.store.SealPublicKeyPQ()))
	if err != nil || !r.PostQuantum() {
		t.Fatalf("seal key: %v", err)
	}
	sealed, _ := r.Seal([]byte("hunter2"), secretwire.WriteAAD("prod/db"))
	got, err := h.store.OpenWrite("prod/db", sealed)
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := h.store.OpenWrite("prod/other", sealed); err == nil {
		t.Fatal("a value sealed for prod/db opened as prod/other")
	}
}

// post_quantum_only: format v1 is refused both ways, so nothing travels
// sealed with X25519 alone.
func TestPostQuantumOnlyRefusesFormatV1(t *testing.T) {
	h := newHarness(t)
	h.store.RequirePostQuantum(true)
	put(t, h.store, "prod/payments/db", TypeText, "a")

	_, v1Recipient := oneTimeKey(t)
	code, body := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Recipient: v1Recipient})
	if code != 400 || !strings.Contains(string(body), "post-quantum") {
		t.Fatalf("X25519-only read: %d %s", code, body)
	}
	_, pq := pqOneTimeKey(t)
	if code, body := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Recipient: pq}); code != 200 {
		t.Fatalf("post-quantum read: %d %s", code, body)
	}

	v1, _ := secretwire.SealTo(mustKey(t, h.store.SealPublicKey()), []byte("x"), secretwire.WriteAAD("prod/payments/db"))
	admin := ManageActor{Kind: ActorPerson, ID: "admin@x", Role: "admin"}
	if code, body := h.manage(http.MethodPut, "/manage/v1/secret", admin, PutRequest{Name: "prod/payments/db", SealedValue: v1}); code != 400 || !strings.Contains(string(body), "post-quantum") {
		t.Fatalf("X25519-only write: %d %s", code, body)
	}
	code, body = h.manage(http.MethodGet, "/manage/v1/status", controlActor, nil)
	var st Status
	_ = json.Unmarshal(body, &st)
	if code != 200 || !st.PostQuantumOnly || st.PreferredSealKey() != st.SealKeyPQ {
		t.Fatalf("status: %d %+v", code, st)
	}
}

// A store made before post-quantum sealing gets its key on the next start,
// and keeps it after.
func TestOlderStoreGetsAPostQuantumKey(t *testing.T) {
	s, dir, root := testStore(t)
	first := bytes.Clone(s.SealPublicKeyPQ())
	if err := s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketMeta).Delete(metaSealKeyPQ) }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	made := bytes.Clone(s2.SealPublicKeyPQ())
	if len(made) != secretwire.PQPublicKeySize || bytes.Equal(made, first) {
		t.Fatal("no new post-quantum key was made")
	}
	s2.Close()
	s3, err := Open(dir, root, "impack")
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if !bytes.Equal(s3.SealPublicKeyPQ(), made) {
		t.Fatal("the post-quantum key changed between starts")
	}
}
