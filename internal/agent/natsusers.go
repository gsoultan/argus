package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A NATS entry's Users tab (tunnel/natsusers.go has the rules): the users the
account's apps sign in with.

The server stores no users in operator mode, so the agent signs one: the
browser sends the user's public key, the agent puts the rules and an expiry in
a user JWT signed with the account's signing key, and hands the JWT back for
the browser to put in a .creds file beside the seed it kept. The agent then
writes what it signed to a list on this server (natsLedger), root-only,
holding only public facts, because nothing else remembers it.

The tab lists those users, with how many connections each has now (from the
monitoring port), and the users connected now that Argus did not sign, so a
person sees every app, not only Argus's.
*/

// NATSAppUserInfo is one user on the tab.
type NATSAppUserInfo struct {
	Name      string             `json:"name"`
	PublicKey string             `json:"publicKey"`
	IssuedAt  time.Time          `json:"issuedAt,omitzero"`
	Expires   time.Time          `json:"expires,omitzero"`
	By        string             `json:"by,omitempty"`
	Spec      tunnel.NATSAppUser `json:"spec"`
	// InArgus: Argus signed it. A user connected now that Argus did not sign
	// is shown with its name and key only.
	InArgus     bool `json:"inArgus"`
	Connections int  `json:"connections"`
}

// NATSUsersData is the users answer.
type NATSUsersData struct {
	Users     []NATSAppUserInfo `json:"users"`
	Account   string            `json:"account"`
	AccountID string            `json:"accountId"`
	// ConnsError is why connections could not be counted, when they could not.
	ConnsError string `json:"connsError,omitempty"`
}

// NATSIssued answers a new user: the user and its JWT, which the browser
// puts in the .creds file with the seed it kept.
type NATSIssued struct {
	User NATSAppUserInfo `json:"user"`
	JWT  string          `json:"jwt"`
}

// natsLedgerMu keeps two sessions from writing the list at once.
var natsLedgerMu sync.Mutex

// natsLedgerKeep is how long an expired user stays on the list, so a person
// sees what stopped working and why.
const natsLedgerKeep = 30 * 24 * time.Hour

// natsLedgerMax bounds the list of one account.
const natsLedgerMax = 5000

func (s *natsSession) ledgerPath() string {
	dir := s.f.cfg.StateDir
	if dir == "" {
		dir = DefaultStateDir
	}
	return filepath.Join(dir, "nats-users", s.account.ID+".json")
}

func readNATSLedger(path string) ([]NATSAppUserInfo, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []NATSAppUserInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l []NATSAppUserInfo
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, errors.New("the list of NATS users at " + path + " is not readable: " + err.Error())
	}
	return l, nil
}

// changeNATSLedger reads the list, lets fn change it, drops users long
// expired and writes it back, root-only.
func changeNATSLedger(path string, fn func([]NATSAppUserInfo) ([]NATSAppUserInfo, error)) error {
	natsLedgerMu.Lock()
	defer natsLedgerMu.Unlock()
	l, err := readNATSLedger(path)
	if err != nil {
		return err
	}
	if l, err = fn(l); err != nil {
		return err
	}
	cut := time.Now().Add(-natsLedgerKeep)
	kept := l[:0]
	for _, u := range l {
		if u.Expires.IsZero() || u.Expires.After(cut) {
			kept = append(kept, u)
		}
	}
	if len(kept) > natsLedgerMax {
		return errors.New("Argus has signed " + strconv.Itoa(natsLedgerMax) + " users for this account; take some off the list first")
	}
	b, err := json.MarshalIndent(kept, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

// userConns counts the account's connections by user key, and names the
// users Argus did not sign. Argus's own sessions are left out.
func (s *natsSession) userConns(ctx context.Context) (map[string]int, map[string]string, error) {
	b, err := s.monitorGet(ctx, "/connz", url.Values{"acc": {s.account.ID}, "auth": {"true"}, "limit": {"10000"}})
	if err != nil {
		return nil, nil, err
	}
	var z struct {
		Conns []struct {
			Name    string `json:"name"`
			Account string `json:"account"`
			User    string `json:"authorized_user"`
			JWT     string `json:"jwt"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(b, &z); err != nil {
		return nil, nil, err
	}
	n, names := map[string]int{}, map[string]string{}
	for _, c := range z.Conns {
		if c.Account != "" && c.Account != s.account.ID || strings.HasPrefix(c.Name, "argus · ") || c.User == "" {
			continue
		}
		n[c.User]++
		if name := natsJWTName(c.JWT); name != "" {
			names[c.User] = name
		}
	}
	return n, names, nil
}

// natsJWTName is the name in a connection's user JWT, for showing only: the
// server has checked the JWT already.
func natsJWTName(tok string) string {
	if tok == "" {
		return ""
	}
	uc, err := jwt.DecodeUserClaims(tok)
	if err != nil {
		return ""
	}
	return uc.Name
}

func (s *natsSession) users(ctx context.Context) (tunnel.StoreAnswer, error) {
	l, err := readNATSLedger(s.ledgerPath())
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	out := NATSUsersData{Account: s.db.Name, AccountID: s.account.ID, Users: []NATSAppUserInfo{}}
	conns, names, cerr := s.userConns(ctx)
	if cerr != nil {
		out.ConnsError = "connections not counted: " + cerr.Error()
	}
	signed := map[string]bool{}
	for _, u := range l {
		u.InArgus = true
		u.Connections = conns[u.PublicKey]
		signed[u.PublicKey] = true
		out.Users = append(out.Users, u)
	}
	for k, c := range conns {
		if signed[k] || !nkeys.IsValidPublicUserKey(k) {
			continue
		}
		out.Users = append(out.Users, NATSAppUserInfo{Name: names[k], PublicKey: k, Connections: c})
	}
	sort.SliceStable(out.Users, func(i, j int) bool {
		a, b := out.Users[i], out.Users[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.IssuedAt.After(b.IssuedAt)
	})
	return tunnel.StoreAnswer{Type: "result", Data: out, Total: int64(len(out.Users))}, nil
}

// signNATSAppUser signs the user's JWT with the account's signing key.
func signNATSAppUser(acc natsAccountKey, name, key string, spec tunnel.NATSAppUser, now time.Time) (string, time.Time, error) {
	skp, err := nkeys.FromSeed([]byte(acc.Seed))
	if err != nil {
		return "", time.Time{}, err
	}
	defer skp.Wipe()
	exp := now.Add(time.Duration(spec.Days) * 24 * time.Hour).Truncate(time.Second)
	uc := jwt.NewUserClaims(key)
	uc.Name = name
	uc.IssuerAccount = acc.ID
	uc.IssuedAt = now.Unix()
	uc.Expires = exp.Unix()
	uc.Tags.Add("argus")
	p := tunnel.NATSAppUserPerms(spec)
	uc.Pub.Allow.Add(p.PubAllow...)
	uc.Pub.Deny.Add(p.PubDeny...)
	uc.Sub.Allow.Add(p.SubAllow...)
	uc.Sub.Deny.Add(p.SubDeny...)
	if spec.Replies {
		uc.Resp = &jwt.ResponsePermission{MaxMsgs: 1, Expires: time.Minute}
	}
	for _, src := range spec.Sources {
		if !strings.Contains(src, "/") {
			if strings.Contains(src, ":") {
				src += "/128"
			} else {
				src += "/32"
			}
		}
		uc.Limits.Src.Add(src)
	}
	tok, err := uc.Encode(skp)
	return tok, exp, err
}

func (s *natsSession) userSet(req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	text := tunnel.NATSUserText(s.db.Name, req)
	if why := tunnel.ValidNATSUserChange(req); why != "" {
		return refuseNATS(req.ID, rec, text, why, false)
	}
	u := req.BrokerUser
	if !nkeys.IsValidPublicUserKey(u.PublicKey) {
		return refuseNATS(req.ID, rec, text, "the user's public key is not a valid NATS user key", false)
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	fail := func(why string) (tunnel.StoreAnswer, error) {
		ms := time.Since(start).Milliseconds()
		if err := rec.fail(req.ID, why, ms); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, MS: ms}, nil
	}
	tok, exp, err := signNATSAppUser(s.account, u.Name, u.PublicKey, *u.NATS, start)
	if err != nil {
		return fail("could not sign the user: " + err.Error())
	}
	info := NATSAppUserInfo{Name: u.Name, PublicKey: u.PublicKey, IssuedAt: start.UTC().Truncate(time.Second), Expires: exp.UTC(),
		By: s.g.Email, Spec: *u.NATS, InArgus: true}
	err = changeNATSLedger(s.ledgerPath(), func(l []NATSAppUserInfo) ([]NATSAppUserInfo, error) {
		for _, x := range l {
			if x.PublicKey == u.PublicKey {
				return nil, errors.New("that key is already a user's; make a new key")
			}
		}
		return append(l, info), nil
	})
	if err != nil {
		// Not on the list, so not handed over: a user nobody can see is
		// one nobody would know to revoke.
		return fail(err.Error())
	}
	ms := time.Since(start).Milliseconds()
	if err := rec.done(req.ID, "CREATE USER", 1, ms, info, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: NATSIssued{User: info, JWT: tok}, Total: 1, MS: ms, Tag: "CREATE USER"}, nil
}

func (s *natsSession) userDel(req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	text := tunnel.NATSUserText(s.db.Name, req)
	if why := tunnel.ValidNATSUserChange(req); why != "" {
		return refuseNATS(req.ID, rec, text, why, false)
	}
	u := req.BrokerUser
	return s.run(req.ID, rec, text, "FORGET USER", func() (any, int64, bool, error) {
		err := changeNATSLedger(s.ledgerPath(), func(l []NATSAppUserInfo) ([]NATSAppUserInfo, error) {
			for i, x := range l {
				if x.PublicKey == u.PublicKey && x.Name == u.Name {
					return append(l[:i], l[i+1:]...), nil
				}
			}
			return nil, errors.New("Argus did not sign a user " + u.Name + " with that key")
		})
		if err != nil {
			return nil, 0, false, err
		}
		return map[string]string{"revoke": tunnel.NATSRevokeCommand(s.db.Name, u.PublicKey)}, 1, false, nil
	})
}
