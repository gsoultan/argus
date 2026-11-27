package vault

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
)

type harness struct {
	t       *testing.T
	store   *Store
	rules   *Rules
	srv     *httptest.Server
	control ed25519.PrivateKey
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, _, _ := testStore(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	rules := NewRules(15 * time.Minute)
	api, err := NewAPI(APIOptions{Store: s, Rules: rules, ControlKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	h := &harness{t: t, store: s, rules: rules, srv: srv, control: priv}
	if code, body := h.manage(http.MethodPut, "/manage/v1/rules", ManageActor{Kind: ActorControl, ID: "argus-control"}, bundle(1)); code != 200 {
		t.Fatalf("push rules: %d %s", code, body)
	}
	return h
}

var controlActor = ManageActor{Kind: ActorControl, ID: "argus-control"}

func (h *harness) signedReq(method, uri string, actor ManageActor, body any) *http.Request {
	h.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	hdr, err := SignManage(h.control, method, uri, actor, raw, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	req, _ := http.NewRequest(method, h.srv.URL+uri, bytes.NewReader(raw))
	for k, v := range hdr {
		req.Header[k] = v
	}
	return req
}

func (h *harness) do(req *http.Request) (int, []byte) {
	h.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		h.t.Fatalf("Cache-Control %q", cc)
	}
	return resp.StatusCode, buf.Bytes()
}

func (h *harness) manage(method, uri string, actor ManageActor, body any) (int, []byte) {
	return h.do(h.signedReq(method, uri, actor, body))
}

func (h *harness) agentRead(cred string, req secretwire.ReadRequest) (int, []byte) {
	raw, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/read", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Agent "+cred)
	return h.do(r)
}

func oneTimeKey(t *testing.T) (*ecdh.PrivateKey, string) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, secretwire.EncodeKey(priv.PublicKey().Bytes())
}

func TestAgentReadsASealedValueOnlyItCanOpen(t *testing.T) {
	h := newHarness(t)
	put(t, h.store, "prod/payments/db", TypeKV, `{"password":"s3cret"}`)
	priv, recipient := oneTimeKey(t)

	code, body := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Field: "password", Recipient: recipient, Purpose: "argus-agent run"})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if bytes.Contains(body, []byte("s3cret")) {
		t.Fatal("the value travelled in the clear")
	}
	var resp secretwire.ReadResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	v, err := secretwire.OpenSealed(priv, resp.Sealed, secretwire.ReadAAD("prod/payments/db", resp.Version, "password"))
	if err != nil || string(v) != "s3cret" {
		t.Fatalf("open: %q %v", v, err)
	}
}

func TestDataPlaneRefusals(t *testing.T) {
	h := newHarness(t)
	put(t, h.store, "prod/payments/db", TypeText, "a")
	put(t, h.store, "prod/crm/db", TypeText, "b")
	_, recipient := oneTimeKey(t)

	if code, _ := h.agentRead("argus-agent-revoked", secretwire.ReadRequest{Name: "prod/payments/db", Recipient: recipient}); code != 401 {
		t.Fatalf("unknown credential: %d", code)
	}
	r, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/read", strings.NewReader(`{}`))
	if code, _ := h.do(r); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
	// A secret outside the grant reads the same as one that does not exist,
	// and the refusal is logged.
	code, body := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/crm/db", Recipient: recipient})
	if code != 404 {
		t.Fatalf("outside the grant: %d %s", code, body)
	}
	if code, _ := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/none", Recipient: recipient}); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	items, _ := h.store.LogAfter(0, 0)
	found := false
	for _, it := range items {
		var e Entry
		_ = json.Unmarshal(it.Line, &e)
		if e.Outcome == OutcomeRefused && e.Secret == "prod/crm/db" && e.Actor.ID == "srv-1" && e.Route == RouteDirect {
			found = true
		}
	}
	if !found {
		t.Fatal("refused read is not in the log")
	}
	if code, _ := h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Recipient: "short"}); code != 400 {
		t.Fatalf("bad recipient: %d", code)
	}
}

func TestListShowsOnlyWhatTheCallerMaySee(t *testing.T) {
	h := newHarness(t)
	put(t, h.store, "prod/payments/db", TypeText, "a")
	put(t, h.store, "prod/crm/db", TypeText, "b")
	r, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/list?prefix=prod", nil)
	r.Header.Set("Authorization", "Agent argus-agent-good")
	code, body := h.do(r)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var out []secretwire.Listed
	_ = json.Unmarshal(body, &out)
	if len(out) != 1 || out[0].Name != "prod/payments/db" || !out[0].CanRead {
		t.Fatalf("listed %+v", out)
	}
}

func TestManagementPlaneNeedsArgusControlsSignature(t *testing.T) {
	h := newHarness(t)
	// Unsigned.
	r, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/manage/v1/status", nil)
	if code, _ := h.do(r); code != 401 {
		t.Fatalf("unsigned: %d", code)
	}
	// Signed by someone else.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	hdr, _ := SignManage(other, http.MethodGet, "/manage/v1/status", controlActor, nil, time.Now())
	r, _ = http.NewRequest(http.MethodGet, h.srv.URL+"/manage/v1/status", nil)
	for k, v := range hdr {
		r.Header[k] = v
	}
	if code, _ := h.do(r); code != 401 {
		t.Fatalf("another key: %d", code)
	}
	// Replayed.
	req := h.signedReq(http.MethodGet, "/manage/v1/status", controlActor, nil)
	replay := req.Clone(req.Context())
	if code, _ := h.do(req); code != 200 {
		t.Fatalf("first use: %d", code)
	}
	if code, _ := h.do(replay); code != 401 {
		t.Fatalf("replay: %d", code)
	}
	// The same request again, signed afresh in the same second, is not a
	// replay: argus-control adds a server and syncs it straight away.
	now := time.Now()
	for i := range 2 {
		hdr, _ := SignManage(h.control, http.MethodGet, "/manage/v1/status", controlActor, nil, now)
		r, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/manage/v1/status", nil)
		for k, v := range hdr {
			r.Header[k] = v
		}
		if code, _ := h.do(r); code != 200 {
			t.Fatalf("identical request %d in one second: %d", i+1, code)
		}
	}
	// A signature without its nonce, or with the nonce changed, is refused.
	stripped := h.signedReq(http.MethodGet, "/manage/v1/status", controlActor, nil)
	sigHdr := stripped.Header.Get(SignatureHeader)
	var tsPart, sigPart string
	for _, p := range strings.Split(sigHdr, ",") {
		if strings.HasPrefix(p, "t=") {
			tsPart = p
		}
		if strings.HasPrefix(p, "s=") {
			sigPart = p
		}
	}
	stripped.Header.Set(SignatureHeader, tsPart+","+sigPart)
	if code, _ := h.do(stripped); code != 401 {
		t.Fatalf("no nonce: %d", code)
	}
	stripped.Header.Set(SignatureHeader, tsPart+",n=AAAAAAAAAAAAAAAAAAAAAA,"+sigPart)
	if code, _ := h.do(stripped); code != 401 {
		t.Fatalf("changed nonce: %d", code)
	}
	// Body changed after signing.
	req = h.signedReq(http.MethodPut, "/manage/v1/rules", controlActor, bundle(7))
	tampered, _ := json.Marshal(bundle(8))
	req.Body = http.NoBody
	req2, _ := http.NewRequest(http.MethodPut, req.URL.String(), bytes.NewReader(tampered))
	req2.Header = req.Header
	if code, _ := h.do(req2); code != 401 {
		t.Fatalf("tampered body: %d", code)
	}
	// Actor changed after signing: a person cannot be swapped for argus-control.
	req = h.signedReq(http.MethodGet, "/manage/v1/log", ManageActor{Kind: ActorPerson, ID: "a@x"}, nil)
	raw, _ := json.Marshal(controlActor)
	req.Header.Set(ActorHeader, secretwire.EncodeKey(raw))
	if code, _ := h.do(req); code != 401 {
		t.Fatalf("swapped actor: %d", code)
	}
	// Too old.
	hdr, _ = SignManage(h.control, http.MethodGet, "/manage/v1/status", controlActor, nil, time.Now().Add(-10*time.Minute))
	r, _ = http.NewRequest(http.MethodGet, h.srv.URL+"/manage/v1/status", nil)
	for k, v := range hdr {
		r.Header[k] = v
	}
	if code, _ := h.do(r); code != 401 {
		t.Fatalf("old signature: %d", code)
	}
}

func TestPeopleWriteAndReadOnlyWhatRulesAllow(t *testing.T) {
	h := newHarness(t)
	admin := ManageActor{Kind: ActorPerson, ID: "admin@x", Role: "admin"}
	member := ManageActor{Kind: ActorPerson, ID: "member@x", Role: "member"}
	lead := ManageActor{Kind: ActorPerson, ID: "lead@x", Teams: []string{"payments-leads"}}

	sealed, err := secretwire.SealTo(mustKey(t, h.store.SealPublicKey()), []byte(`{"username":"app"}`), secretwire.WriteAAD("prod/payments/db"))
	if err != nil {
		t.Fatal(err)
	}
	put := PutRequest{Name: "prod/payments/db", Type: TypeKV, SealedValue: sealed,
		Generate: &Generate{Length: 24, Field: "password"}}
	if code, body := h.manage(http.MethodPut, "/manage/v1/secret", member, put); code != 404 {
		t.Fatalf("member without write: %d %s", code, body)
	}
	if code, body := h.manage(http.MethodPut, "/manage/v1/secret", controlActor, put); code != 403 {
		t.Fatalf("argus-control writing on nobody's behalf: %d %s", code, body)
	}
	code, body := h.manage(http.MethodPut, "/manage/v1/secret", admin, put)
	if code != 200 {
		t.Fatalf("admin write: %d %s", code, body)
	}
	var sec Secret
	_ = json.Unmarshal(body, &sec)
	if !sec.Versions[0].Generated || len(sec.Versions[0].Fields) != 2 {
		t.Fatalf("generated version: %+v", sec.Versions[0])
	}

	// A value sealed for a different secret's name does not open here.
	wrong, _ := secretwire.SealTo(mustKey(t, h.store.SealPublicKey()), []byte("x"), secretwire.WriteAAD("prod/other"))
	if code, _ := h.manage(http.MethodPut, "/manage/v1/secret", admin, PutRequest{Name: "prod/payments/db", SealedValue: wrong}); code != 400 {
		t.Fatalf("value sealed for another name: %d", code)
	}

	priv, recipient := oneTimeKey(t)
	read := ConsoleRead{ReadRequest: secretwire.ReadRequest{Name: "prod/payments/db", Field: "password", Recipient: recipient}, Reason: "INC-2291"}
	// The admin writes but has no read rule.
	if code, _ := h.manage(http.MethodPost, "/manage/v1/read", admin, read); code != 404 {
		t.Fatalf("admin read without a read rule: %d", code)
	}
	// The lead needs an approval.
	if code, body := h.manage(http.MethodPost, "/manage/v1/read", lead, read); code != 403 || !strings.Contains(string(body), "approval") {
		t.Fatalf("lead without approval: %d %s", code, body)
	}
	noReason := read
	noReason.Reason = ""
	lead.Approved = true
	if code, _ := h.manage(http.MethodPost, "/manage/v1/read", lead, noReason); code != 400 {
		t.Fatalf("read with no reason: %d", code)
	}
	code, body = h.manage(http.MethodPost, "/manage/v1/read", lead, read)
	if code != 200 {
		t.Fatalf("approved read: %d %s", code, body)
	}
	var resp secretwire.ReadResponse
	_ = json.Unmarshal(body, &resp)
	v, err := secretwire.OpenSealed(priv, resp.Sealed, secretwire.ReadAAD("prod/payments/db", resp.Version, "password"))
	if err != nil || len(v) != 24 {
		t.Fatalf("generated password: %q %v", v, err)
	}
}

func TestArgusControlCollectsTheLog(t *testing.T) {
	h := newHarness(t)
	put(t, h.store, "prod/payments/db", TypeText, "a")
	_, recipient := oneTimeKey(t)
	h.agentRead("argus-agent-good", secretwire.ReadRequest{Name: "prod/payments/db", Recipient: recipient})

	person := ManageActor{Kind: ActorPerson, ID: "a@x", Role: "admin"}
	if code, _ := h.manage(http.MethodGet, "/manage/v1/log?after=0", person, nil); code != 403 {
		t.Fatalf("a person collecting the log: %d", code)
	}
	code, body := h.manage(http.MethodGet, "/manage/v1/log?after=0", controlActor, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var batch LogBatch
	_ = json.Unmarshal(body, &batch)
	if len(batch.Entries) != 3 || batch.State.Head != batch.Entries[2].Hash {
		t.Fatalf("batch: %d entries, head %s", len(batch.Entries), batch.State.Head)
	}
	if code, _ := h.manage(http.MethodPost, "/manage/v1/log/ack", controlActor, map[string]uint64{"seq": 3}); code != 200 {
		t.Fatalf("ack: %d", code)
	}
	if st, _ := h.store.LogState(); st.Acked != 3 {
		t.Fatalf("acked %d", st.Acked)
	}
}

func mustKey(t *testing.T, raw []byte) *ecdh.PublicKey {
	k, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
