package vault

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
)

// Management requests are signed by argus-control with its Ed25519 key. The
// signature covers the method, the path and query, the time, the actor and a
// digest of the body, so none of them can be changed in flight, and a request
// can be used once, within a short window.
const (
	SignatureHeader = "Argus-Control-Signature"
	ActorHeader     = "Argus-Actor"
	signWindow      = 2 * time.Minute
	maxSeen         = 100_000
)

// ManageActor is who argus-control says a management request is for. A
// person acting in the console, or argus-control itself.
type ManageActor struct {
	Kind     string   `json:"kind"`
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Teams    []string `json:"teams,omitempty"`
	Role     string   `json:"role,omitempty"`
	Approved bool     `json:"approved,omitempty"`
	// Remote is the person's own address, for the log.
	Remote string `json:"remote,omitempty"`
}

// signedMessage is what a signature covers. The nonce makes every request's
// signature its own: Ed25519 is deterministic, so without it two identical
// requests in the same second would sign the same and the second would be
// refused as a replay.
func signedMessage(method, uri, ts, nonce, actor string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return fmt.Appendf(nil, "argus-secret manage v1\n%s\n%s\n%s\n%s\n%s\n%s", method, uri, ts, nonce, actor, hex.EncodeToString(sum[:]))
}

// SignManage returns the headers argus-control sends with a management
// request: the signature, and the actor it covers.
func SignManage(priv ed25519.PrivateKey, method, uri string, actor ManageActor, body []byte, now time.Time) (http.Header, error) {
	raw, err := json.Marshal(actor)
	if err != nil {
		return nil, err
	}
	act := base64.RawURLEncoding.EncodeToString(raw)
	ts := strconv.FormatInt(now.Unix(), 10)
	n := make([]byte, 16)
	if _, err := rand.Read(n); err != nil {
		return nil, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(n)
	sig := ed25519.Sign(priv, signedMessage(method, uri, ts, nonce, act, body))
	h := http.Header{}
	h.Set(SignatureHeader, "t="+ts+",n="+nonce+",s="+base64.RawURLEncoding.EncodeToString(sig))
	h.Set(ActorHeader, act)
	return h, nil
}

type controlVerifier struct {
	key  ed25519.PublicKey
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newControlVerifier(key []byte) (*controlVerifier, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("control_public_key must be an Ed25519 public key")
	}
	return &controlVerifier{key: key, seen: map[string]time.Time{}, now: time.Now}, nil
}

var errUnsigned = errors.New("this request is not signed by argus-control")

// verify checks a management request and returns its body and actor.
func (c *controlVerifier) verify(r *http.Request, limit int64) ([]byte, ManageActor, error) {
	var actor ManageActor
	ts, nonce, sigB64, ok := parseSignature(r.Header.Get(SignatureHeader))
	if !ok {
		return nil, actor, errUnsigned
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, actor, errUnsigned
	}
	now := c.now()
	if d := now.Sub(time.Unix(unix, 0)); d > signWindow || d < -signWindow {
		return nil, actor, errors.New("the request's signature is too old or from the future; check both clocks")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, actor, errUnsigned
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, actor, InvalidError{"the request body is too large"}
	}
	act := r.Header.Get(ActorHeader)
	if !ed25519.Verify(c.key, signedMessage(r.Method, r.URL.RequestURI(), ts, nonce, act, body), sig) {
		return nil, actor, errUnsigned
	}
	if !c.firstUse(sigB64, now) {
		return nil, actor, errors.New("this signed request was already used")
	}
	raw, err := base64.RawURLEncoding.DecodeString(act)
	if err != nil || json.Unmarshal(raw, &actor) != nil {
		return nil, actor, InvalidError{"the actor header is not readable"}
	}
	if actor.Kind != ActorPerson && actor.Kind != ActorControl {
		return nil, actor, InvalidError{"the actor is a person or argus-control"}
	}
	return body, actor, nil
}

// firstUse remembers a signature for as long as it could be replayed. The map
// holds only signatures argus-control made, and is bounded anyway.
func (c *controlVerifier) firstUse(sig string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[sig]; ok {
		return false
	}
	if len(c.seen) >= maxSeen {
		for k, exp := range c.seen {
			if now.After(exp) {
				delete(c.seen, k)
			}
		}
		if len(c.seen) >= maxSeen {
			return false
		}
	}
	c.seen[sig] = now.Add(2 * signWindow)
	return true
}

func parseSignature(h string) (ts, nonce, sig string, ok bool) {
	for part := range strings.SplitSeq(h, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "n":
			nonce = v
		case "s":
			sig = v
		}
	}
	return ts, nonce, sig, ts != "" && len(nonce) >= 16 && len(nonce) <= 64 && sig != ""
}

func (a *API) manageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /manage/v1/status", a.signed(maxBody, a.mStatus))
	mux.HandleFunc("PUT /manage/v1/rules", a.signed(maxRulesBody, a.mRules))
	mux.HandleFunc("GET /manage/v1/secrets", a.signed(maxBody, a.mList))
	mux.HandleFunc("GET /manage/v1/secret", a.signed(maxBody, a.mGet))
	mux.HandleFunc("PUT /manage/v1/secret", a.signed(maxBody, a.mPut))
	mux.HandleFunc("PATCH /manage/v1/secret", a.signed(maxBody, a.mMeta))
	mux.HandleFunc("POST /manage/v1/read", a.signed(maxBody, a.mRead))
	mux.HandleFunc("POST /manage/v1/label", a.signed(maxBody, a.mLabel))
	mux.HandleFunc("POST /manage/v1/delete", a.signed(maxBody, a.mDelete))
	mux.HandleFunc("POST /manage/v1/restore", a.signed(maxBody, a.mRestore))
	mux.HandleFunc("GET /manage/v1/log", a.signed(maxBody, a.mLog))
	mux.HandleFunc("POST /manage/v1/log/ack", a.signed(maxBody, a.mAck))
}

type manageHandler func(w http.ResponseWriter, r *http.Request, body []byte, actor ManageActor)

// signed admits only requests argus-control signed. Each one also tells
// argus-secret that argus-control is reachable, which keeps the rules fresh.
func (a *API) signed(limit int64, h manageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, actor, err := a.control.verify(r, limit)
		if err != nil {
			var inv InvalidError
			if errors.As(err, &inv) {
				writeErr(w, http.StatusBadRequest, inv.Msg)
				return
			}
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		a.rules.Contacted()
		h(w, r, body, actor)
	}
}

func principalOf(m ManageActor) Principal {
	return Principal{Actor: Actor{Kind: m.Kind, ID: m.ID, Name: m.Name}, Groups: m.Teams, Role: m.Role, Approved: m.Approved}
}

// requireControl refuses a person on a route only argus-control may use.
func requireControl(w http.ResponseWriter, actor ManageActor) bool {
	if actor.Kind != ActorControl {
		writeErr(w, http.StatusForbidden, "only argus-control itself may do this")
		return false
	}
	return true
}

// allowPerson checks a person's permission; argus-control itself may only use
// the routes that are its own. Refusals are logged and answered as not found.
func (a *API) allowPerson(w http.ResponseWriter, actor ManageActor, action, name, reason string) bool {
	if actor.Kind != ActorPerson {
		writeErr(w, http.StatusForbidden, "secrets are changed and read on behalf of a person")
		return false
	}
	p := principalOf(actor)
	d, err := a.rules.Allow(p, action, name)
	if err != nil {
		a.fail(w, err)
		return false
	}
	if d.Allowed {
		return true
	}
	if err := a.store.Refused(Entry{Actor: p.Actor, Action: action, Secret: name, Reason: reason,
		Route: RouteConsole, Remote: actor.Remote, Detail: "no rule allows it"}); err != nil {
		a.fail(w, err)
		return false
	}
	if d.NeedsApproval {
		writeErr(w, http.StatusForbidden, ErrNeedsApproval.Error())
		return false
	}
	a.fail(w, ErrNotFound)
	return false
}

// Status is what argus-control shows on Settings › Secrets servers.
type Status struct {
	Org     string `json:"org"`
	Version string `json:"version"`
	SealKey string `json:"sealKey"`
	// SealKeyPQ is the post-quantum seal key (format v2), to prefer.
	SealKeyPQ string `json:"sealKeyPQ"`
	// PostQuantumOnly says format v1 is refused.
	PostQuantumOnly bool     `json:"postQuantumOnly,omitempty"`
	TLSPin          string   `json:"tlsPin,omitempty"`
	Rules           RulesAt  `json:"rules"`
	Log             LogState `json:"log"`
	Secrets         int      `json:"secrets"`
}

// PreferredSealKey is the key a new value should be sealed to: the
// post-quantum one, or the X25519 one from a server that has none.
func (s Status) PreferredSealKey() string {
	if s.SealKeyPQ != "" {
		return s.SealKeyPQ
	}
	return s.SealKey
}

// RulesAt is the rules' serial and age.
type RulesAt struct {
	Serial    int64     `json:"serial"`
	Contacted time.Time `json:"contacted"`
	Fresh     bool      `json:"fresh"`
}

func (a *API) mStatus(w http.ResponseWriter, _ *http.Request, _ []byte, actor ManageActor) {
	if !requireControl(w, actor) {
		return
	}
	serial, contacted, fresh := a.rules.Status()
	ls, err := a.store.LogState()
	if err != nil {
		a.fail(w, err)
		return
	}
	secs, err := a.store.List("", true)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Status{Org: a.store.Organization(), Version: a.version,
		SealKey: secretwire.EncodeKey(a.store.SealPublicKey()), SealKeyPQ: secretwire.EncodeKey(a.store.SealPublicKeyPQ()),
		PostQuantumOnly: a.store.requirePQ, TLSPin: a.tlsPin,
		Rules: RulesAt{Serial: serial, Contacted: contacted, Fresh: fresh}, Log: ls, Secrets: len(secs)})
}

func (a *API) mRules(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	if !requireControl(w, actor) {
		return
	}
	var b Bundle
	if err := decodeJSON(body, &b); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.store.ApplyRules(a.rules, b); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"serial": b.Serial})
}

func (a *API) mList(w http.ResponseWriter, r *http.Request, _ []byte, actor ManageActor) {
	prefix := r.URL.Query().Get("prefix")
	if err := ValidPrefix(prefix); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	deleted := r.URL.Query().Get("deleted") == "1"
	secs, err := a.store.List(prefix, deleted)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := []Secret{}
	for _, s := range secs {
		if actor.Kind == ActorPerson {
			d, err := a.rules.Allow(principalOf(actor), CanList, s.Name)
			if err != nil {
				a.fail(w, err)
				return
			}
			if !d.Allowed {
				continue
			}
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) mGet(w http.ResponseWriter, r *http.Request, _ []byte, actor ManageActor) {
	name := r.URL.Query().Get("name")
	if err := ValidName(name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if actor.Kind == ActorPerson && !a.allowPerson(w, actor, CanList, name, "") {
		return
	}
	s, err := a.store.Get(name)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// Generate asks argus-secret to make a value, so nobody types or sees it.
type Generate struct {
	Length  int    `json:"length"`
	Charset string `json:"charset"`
	// Field, for a key-value secret, is the field the generated value goes
	// in; the other fields come from the sealed value, if any.
	Field string `json:"field,omitempty"`
}

// PutRequest is a new secret or a new version.
type PutRequest struct {
	Name        string            `json:"name"`
	Type        string            `json:"type,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	// SealedValue is the value sealed to argus-secret's seal key with
	// secretwire.WriteAAD(name), so argus-control never holds it.
	SealedValue []byte    `json:"sealedValue,omitempty"`
	Generate    *Generate `json:"generate,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

func (a *API) mPut(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req PutRequest
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := ValidName(req.Name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if !a.allowPerson(w, actor, CanWrite, req.Name, req.Reason) {
		return
	}
	value, generated, err := a.newValue(req)
	if err != nil {
		a.fail(w, err)
		return
	}
	s, err := a.store.Put(PutInput{Name: req.Name, Type: req.Type, Description: req.Description, Owner: req.Owner,
		Tags: req.Tags, Value: value, Generated: generated, Actor: principalOf(actor).Actor, Reason: req.Reason,
		Route: RouteConsole})
	clear(value)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// newValue opens the sealed value, generates one, or both.
func (a *API) newValue(req PutRequest) ([]byte, bool, error) {
	var value []byte
	if len(req.SealedValue) > 0 {
		v, err := a.store.OpenWrite(req.Name, req.SealedValue)
		if err != nil {
			return nil, false, err
		}
		value = v
	}
	if req.Generate == nil {
		if value == nil {
			return nil, false, InvalidError{"send a sealed value, or ask argus-secret to generate one"}
		}
		return value, false, nil
	}
	gen, err := generate(req.Generate.Length, req.Generate.Charset)
	if err != nil {
		return nil, false, err
	}
	if req.Generate.Field == "" {
		if value != nil {
			return nil, false, InvalidError{"send a sealed value or generate one, not both, unless generating one field"}
		}
		return gen, true, nil
	}
	fields := map[string]string{}
	if value != nil {
		f, err := parseFields(value)
		if err != nil {
			return nil, false, InvalidError{err.Error()}
		}
		fields = f
	}
	fields[req.Generate.Field] = string(gen)
	out, err := json.Marshal(fields)
	return out, true, err
}

// Character sets for generated values.
var charsets = map[string]string{
	"alnum":   "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	"symbols": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#%+-=?@^_~",
	"hex":     "0123456789abcdef",
}

func generate(length int, charset string) ([]byte, error) {
	if length == 0 {
		length = 32
	}
	if length < 12 || length > 256 {
		return nil, InvalidError{"a generated value is 12 to 256 characters"}
	}
	if charset == "" {
		charset = "alnum"
	}
	set, ok := charsets[charset]
	if !ok {
		return nil, InvalidError{"charset is one of alnum, symbols, hex"}
	}
	out := make([]byte, length)
	max := big.NewInt(int64(len(set)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return nil, err
		}
		out[i] = set[n.Int64()]
	}
	return out, nil
}

// MetaRequest changes what is said about a secret.
type MetaRequest struct {
	Name        string             `json:"name"`
	Description *string            `json:"description,omitempty"`
	Owner       *string            `json:"owner,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
}

func (a *API) mMeta(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req MetaRequest
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := ValidName(req.Name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if !a.allowPerson(w, actor, CanWrite, req.Name, "") {
		return
	}
	in := MetaInput{Description: req.Description, Owner: req.Owner}
	if req.Tags != nil {
		in.Tags, in.SetTags = *req.Tags, true
	}
	s, err := a.store.UpdateMeta(req.Name, in, principalOf(actor).Actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// ConsoleRead is a person reading in the console, through argus-control. The
// value is sealed to a one-time key the person's browser made.
type ConsoleRead struct {
	secretwire.ReadRequest
	Reason string `json:"reason"`
}

func (a *API) mRead(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req ConsoleRead
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if actor.Kind != ActorPerson {
		writeErr(w, http.StatusForbidden, "secrets are read on behalf of a person")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		a.fail(w, InvalidError{"say why you need it"})
		return
	}
	if len(req.Reason) > 500 {
		a.fail(w, InvalidError{"a reason is at most 500 characters"})
		return
	}
	req.Purpose = req.Reason
	resp, err := a.readSealed(principalOf(actor), req.ReadRequest, RouteConsole, actor.Remote)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// LabelRequest points a label at a version.
type LabelRequest struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Version int    `json:"version"`
	Reason  string `json:"reason,omitempty"`
}

func (a *API) mLabel(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req LabelRequest
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := ValidName(req.Name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if !a.allowPerson(w, actor, CanManage, req.Name, req.Reason) {
		return
	}
	s, err := a.store.SetLabel(req.Name, req.Label, req.Version, principalOf(actor).Actor, req.Reason)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// DeleteRequest schedules a deletion; RestoreRequest cancels one.
type DeleteRequest struct {
	Name         string `json:"name"`
	RecoveryDays int    `json:"recoveryDays,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (a *API) mDelete(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req DeleteRequest
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := ValidName(req.Name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if !a.allowPerson(w, actor, CanManage, req.Name, req.Reason) {
		return
	}
	s, err := a.store.Delete(req.Name, req.RecoveryDays, principalOf(actor).Actor, req.Reason)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) mRestore(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := ValidName(req.Name); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	if !a.allowPerson(w, actor, CanManage, req.Name, "") {
		return
	}
	s, err := a.store.Restore(req.Name, principalOf(actor).Actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// LogBatch is what argus-control collects.
type LogBatch struct {
	State   LogState  `json:"state"`
	Entries []LogItem `json:"entries"`
}

func (a *API) mLog(w http.ResponseWriter, r *http.Request, _ []byte, actor ManageActor) {
	if !requireControl(w, actor) {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	st, err := a.store.LogState()
	if err != nil {
		a.fail(w, err)
		return
	}
	items, err := a.store.LogAfter(after, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	if items == nil {
		items = []LogItem{}
	}
	writeJSON(w, http.StatusOK, LogBatch{State: st, Entries: items})
}

func (a *API) mAck(w http.ResponseWriter, _ *http.Request, body []byte, actor ManageActor) {
	if !requireControl(w, actor) {
		return
	}
	var req struct {
		Seq uint64 `json:"seq"`
	}
	if err := decodeJSON(body, &req); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.store.AckLog(req.Seq); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint64{"acked": req.Seq})
}
