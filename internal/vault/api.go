package vault

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gsoultan/argus/internal/secretwire"
)

// API serves both planes. Build it with NewAPI.
type API struct {
	store   *Store
	rules   *Rules
	ip      ClientIP
	fails   *failLimiter
	control *controlVerifier
	log     *slog.Logger
	version string
	tlsPin  string
}

// APIOptions configures an API.
type APIOptions struct {
	Store *Store
	Rules *Rules
	// ControlKey is argus-control's Ed25519 public key; only requests it
	// signed reach the management plane.
	ControlKey []byte
	ClientIP   ClientIP
	Log        *slog.Logger
	Version    string
	// TLSPin is this server's certificate pin, reported to argus-control so
	// agents can pin it. Empty behind a TLS-terminating proxy.
	TLSPin string
}

// NewAPI builds the handler for both planes.
func NewAPI(o APIOptions) (*API, error) {
	if o.Store == nil || o.Rules == nil {
		return nil, errors.New("store and rules are required")
	}
	cv, err := newControlVerifier(o.ControlKey)
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &API{store: o.Store, rules: o.Rules, ip: o.ClientIP, fails: newFailLimiter(20),
		control: cv, log: o.Log, version: o.Version, tlsPin: o.TLSPin}, nil
}

// Handler routes both planes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", a.health)
	mux.HandleFunc("POST /v1/read", a.read)
	mux.HandleFunc("GET /v1/list", a.list)
	a.manageRoutes(mux)
	return mux
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	_, _, fresh := a.rules.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"sealKey": secretwire.EncodeKey(a.store.SealPublicKey()),
		// Whether reads would be answered now. Not why: that is for
		// argus-control, over the management plane.
		"serving": fresh,
	})
}

// caller authenticates a data-plane request: "Authorization: Agent <cred>" or
// "Bearer <token>". Failures count against the client's address.
func (a *API) caller(w http.ResponseWriter, r *http.Request) (Principal, string, bool) {
	remote := a.ip.Of(r)
	if a.fails.Blocked(remote) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts from this address; wait a minute")
		return Principal{}, remote, false
	}
	scheme, secret, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	secret = strings.TrimSpace(secret)
	if (scheme != "Agent" && scheme != "Bearer") || secret == "" {
		a.fails.Failed(remote)
		writeErr(w, http.StatusUnauthorized, "send the server's agent credential or an app token in Authorization")
		return Principal{}, remote, false
	}
	p, ok := a.rules.Authenticate(secret)
	if !ok {
		a.fails.Failed(remote)
		writeErr(w, http.StatusUnauthorized, "argus-secret does not know this credential; it may be revoked, or not pushed here yet")
		return Principal{}, remote, false
	}
	return p, remote, true
}

func (a *API) read(w http.ResponseWriter, r *http.Request) {
	p, remote, ok := a.caller(w, r)
	if !ok {
		return
	}
	var req secretwire.ReadRequest
	if err := readJSON(r, maxBody, &req); err != nil {
		a.fail(w, err)
		return
	}
	if len(req.Purpose) > 200 {
		req.Purpose = req.Purpose[:200]
	}
	resp, err := a.readSealed(p, req, RouteDirect, remote)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// readSealed checks the rules, reads, and seals. A refusal by rule is logged
// and answered as not found, so a caller cannot learn which secrets exist.
func (a *API) readSealed(p Principal, req secretwire.ReadRequest, route, remote string) (secretwire.ReadResponse, error) {
	if err := ValidName(req.Name); err != nil {
		return secretwire.ReadResponse{}, InvalidError{err.Error()}
	}
	recipient, err := secretwire.ParseRecipient(req.Recipient)
	if err != nil {
		return secretwire.ReadResponse{}, InvalidError{err.Error()}
	}
	d, err := a.rules.Allow(p, CanRead, req.Name)
	if err != nil {
		return secretwire.ReadResponse{}, err
	}
	if !d.Allowed {
		detail := "no rule allows it"
		if d.NeedsApproval {
			detail = "needs an approval that was not given"
		}
		if err := a.store.Refused(Entry{Actor: p.Actor, Action: "read", Secret: req.Name, Field: req.Field,
			Reason: req.Purpose, Route: route, Remote: remote, Detail: detail}); err != nil {
			return secretwire.ReadResponse{}, err
		}
		if d.NeedsApproval {
			return secretwire.ReadResponse{}, ErrNeedsApproval
		}
		return secretwire.ReadResponse{}, ErrNotFound
	}
	value, ver, err := a.store.Read(ReadInput{Name: req.Name, Version: req.Version, Label: req.Label,
		Field: req.Field, Actor: p.Actor, Reason: req.Purpose, Route: route, Remote: remote})
	if err != nil {
		return secretwire.ReadResponse{}, err
	}
	sealed, err := secretwire.SealTo(recipient, value, secretwire.ReadAAD(req.Name, ver, req.Field))
	clear(value)
	if err != nil {
		return secretwire.ReadResponse{}, err
	}
	return secretwire.ReadResponse{Name: req.Name, Version: ver, Field: req.Field, Sealed: sealed}, nil
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	p, _, ok := a.caller(w, r)
	if !ok {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if err := ValidPrefix(prefix); err != nil {
		a.fail(w, InvalidError{err.Error()})
		return
	}
	out, err := a.listFor(p, prefix)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listFor(p Principal, prefix string) ([]secretwire.Listed, error) {
	secs, err := a.store.List(prefix, false)
	if err != nil {
		return nil, err
	}
	out := []secretwire.Listed{}
	for _, s := range secs {
		d, err := a.rules.Allow(p, CanList, s.Name)
		if err != nil {
			return nil, err
		}
		if !d.Allowed {
			continue
		}
		rd, _ := a.rules.Allow(p, CanRead, s.Name)
		l := secretwire.Listed{Name: s.Name, Type: s.Type, Current: s.Current(), UpdatedAt: s.UpdatedAt, CanRead: rd.Allowed}
		if v, ok := s.version(s.Current()); ok {
			l.Fields = v.Fields
		}
		out = append(out, l)
	}
	return out, nil
}

func (a *API) fail(w http.ResponseWriter, err error) {
	status, msg := statusFor(err)
	if status == http.StatusInternalServerError {
		a.log.Error("argus-secret request failed", "err", err)
	}
	writeErr(w, status, msg)
}
