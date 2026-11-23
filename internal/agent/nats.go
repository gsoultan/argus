package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
NATS in the browser, through the tunnel, as a broker.

	browser ──wss──▶ argus-control ──"nats" stream──▶ agent ──▶ nats-server on this server

One entry is one account of a nats-server in operator mode. The agent holds a
signing key of that account in a root-only file (tunnel.NATSAdminFile) and,
for each session, makes a fresh user key and signs a user JWT for it: the
person's level as publish and subscribe permissions (tunnel.NATSPermissions),
an inbox only this session may read, and an expiry a little past the
session's longest allowed length. Everything the person does runs as that
user, so NATS itself holds them to their level. Nothing is written to the
server and the user's key never leaves this process, so closing the session
leaves nothing behind.

A server that lets anyone connect without credentials is refused: a level
would mean nothing on it. Numbers that only the server knows as a whole --
connections, subscriptions, messages in and out -- come from its monitoring
port on this server, filtered to the entry's account.
*/

// NATSService is the tunnel service name.
const NATSService = tunnel.NATSService

// NATS is the nats service.
func NATS(cfg StoreConfig) TunnelService {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	f := &natsFactory{cfg: cfg}
	return storeService(cfg, tunnel.NATSService, tunnel.EngineNATS, f.open)
}

type natsFactory struct {
	cfg StoreConfig
	// httpGet stands in for reading the monitoring port, in tests.
	httpGet func(ctx context.Context, u string) ([]byte, error)
}

// natsAccountKey is what the admin file says about one account: its public
// key (A…) and the seed of one of its signing keys (SA…).
type natsAccountKey struct {
	ID   string
	Seed string
}

// natsAdmin is the admin file: the accounts the agent may open, and where
// the server's client and monitoring ports are when they are not the
// entry's.
type natsAdmin struct {
	Accounts    map[string]natsAccountKey
	MonitorPort int
}

// readNATSAdmin reads the admin file: "account NAME = ACCOUNTKEY SIGNINGSEED"
// lines, an optional "monitor = PORT", and # comments. The file must be
// readable by its owner only, as a key file is.
func readNATSAdmin(path string) (natsAdmin, error) {
	if !tunnel.ValidNATSAdminFile(path) {
		return natsAdmin{}, fmt.Errorf("%s is not a key file the agent will read", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return natsAdmin{}, fmt.Errorf("NATS needs an account's signing key to give people users, and %s does not exist: "+
				"put a line \"account NAME = ACCOUNT_PUBLIC_KEY SIGNING_KEY_SEED\" in it for each account, readable by root only", path)
		}
		return natsAdmin{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return natsAdmin{}, fmt.Errorf("%s can be read by others; chmod 600 it, since it holds signing keys", path)
	}
	b, err := readFileLimited(path, 64<<10)
	if err != nil {
		return natsAdmin{}, err
	}
	return parseNATSAdmin(path, b)
}

func parseNATSAdmin(path string, b []byte) (natsAdmin, error) {
	a := natsAdmin{Accounts: map[string]natsAccountKey{}, MonitorPort: tunnel.NATSDefaultMonitorPort}
	sc := bufio.NewScanner(bytes.NewReader(b))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		at := strings.IndexByte(line, '=')
		if at < 0 {
			return natsAdmin{}, fmt.Errorf("%s line %d has no =", path, n)
		}
		k, v := strings.TrimSpace(line[:at]), strings.TrimSpace(line[at+1:])
		f := strings.Fields(k)
		switch {
		case len(f) == 2 && strings.EqualFold(f[0], "account"):
			name := f[1]
			parts := strings.Fields(v)
			if !tunnel.ValidNATSAccount(name) || len(parts) != 2 {
				return natsAdmin{}, fmt.Errorf("%s line %d: write account NAME = ACCOUNT_PUBLIC_KEY SIGNING_KEY_SEED", path, n)
			}
			if !nkeys.IsValidPublicAccountKey(parts[0]) {
				return natsAdmin{}, fmt.Errorf("%s line %d: %s is not an account's public key (it starts with A)", path, n, name)
			}
			kp, err := nkeys.FromSeed([]byte(parts[1]))
			if err == nil {
				var pub string
				if pub, err = kp.PublicKey(); err == nil && !nkeys.IsValidPublicAccountKey(pub) {
					err = errors.New("not an account key")
				}
				kp.Wipe()
			}
			if err != nil {
				return natsAdmin{}, fmt.Errorf("%s line %d: the second key of %s is not an account signing key's seed (it starts with SA)", path, n, name)
			}
			a.Accounts[name] = natsAccountKey{ID: parts[0], Seed: parts[1]}
		case strings.EqualFold(k, "monitor") || strings.EqualFold(k, "monitor_port"):
			p, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(v, "http://127.0.0.1:"), "127.0.0.1:"))
			if err != nil || p < 1 || p > 65535 {
				return natsAdmin{}, fmt.Errorf("%s line %d: the monitoring port is a number", path, n)
			}
			a.MonitorPort = p
		}
	}
	if len(a.Accounts) == 0 {
		return natsAdmin{}, fmt.Errorf("%s names no account", path)
	}
	return a, nil
}

type natsSession struct {
	f        *natsFactory
	g        tunnel.Grant
	db       tunnel.GrantDB
	url      string
	file     string
	account  natsAccountKey
	monitor  int
	user     string
	inbox    string
	nc       *nats.Conn
	js       jetstream.JetStream
	timeout  time.Duration
	maxItems int
	version  string
	noJS     bool
}

func natsURL(host string, port int) string {
	if port == 0 {
		port = tunnel.NATSDefaultPort
	}
	return "nats://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func (f *natsFactory) open(ctx context.Context, g tunnel.Grant, id string) (storeEngine, error) {
	db := *g.DB
	if !tunnel.LocalLoopback(db.Host) {
		return nil, errors.New("NATS must be on this server; " + db.Host + " is not")
	}
	if !tunnel.ValidNATSAccount(db.Name) {
		return nil, errors.New("the grant names no NATS account")
	}
	if tunnel.NATSStoreOps(db.Level) == nil {
		return nil, errors.New("unknown level " + db.Level)
	}
	s := &natsSession{f: f, g: g, db: db, url: natsURL(db.Host, db.Port), file: db.ConfigPath,
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout), maxItems: db.MaxRows}
	if s.file == "" {
		s.file = tunnel.NATSAdminFile
	}
	if s.maxItems <= 0 || s.maxItems > 1000 {
		s.maxItems = 200
	}
	admin, err := readNATSAdmin(s.file)
	if err != nil {
		return nil, err
	}
	acc, ok := admin.Accounts[db.Name]
	if !ok {
		return nil, fmt.Errorf("%s has no signing key for the account %s", s.file, db.Name)
	}
	s.account, s.monitor = acc, admin.MonitorPort
	s.user = tunnel.StoreUser(g.Principal, db.Level)
	tok := make([]byte, 8)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s.inbox = tunnel.NATSInboxPrefix + hex.EncodeToString(tok)
	if err := refuseOpenNATS(ctx, s.url); err != nil {
		return nil, err
	}
	life := secs(db.MaxSessionS, defaultMaxSession) + 5*time.Minute
	if err := s.connect(ctx, life); err != nil {
		return nil, err
	}
	return s, nil
}

// refuseOpenNATS refuses a server that lets a client in without
// credentials: whatever level Argus signed, anyone could do more.
func refuseOpenNATS(ctx context.Context, u string) error {
	nc, err := nats.Connect(u, nats.Name("argus-agent check"), nats.Timeout(5*time.Second), nats.NoReconnect())
	if err == nil {
		nc.Close()
		return errors.New("NATS on this server lets anyone connect without credentials, so a person's level would mean nothing; " +
			"run it in operator mode (accounts and users made with nsc) before adding it to Argus")
	}
	if errors.Is(err, nats.ErrAuthorization) || strings.Contains(strings.ToLower(err.Error()), "authorization") ||
		strings.Contains(strings.ToLower(err.Error()), "authentication") {
		return nil
	}
	return fmt.Errorf("could not reach NATS at %s: %w", u, err)
}

// natsUserJWT signs a user for the session: a fresh key, the level's
// permissions, issued by the account's signing key, expiring after life.
func natsUserJWT(acc natsAccountKey, name, level, inbox string, life time.Duration) (string, nkeys.KeyPair, error) {
	perms, ok := tunnel.NATSPermissions(level, inbox)
	if !ok {
		return "", nil, errors.New("unknown level " + level)
	}
	ukp, err := nkeys.CreateUser()
	if err != nil {
		return "", nil, err
	}
	upub, err := ukp.PublicKey()
	if err != nil {
		return "", nil, err
	}
	skp, err := nkeys.FromSeed([]byte(acc.Seed))
	if err != nil {
		return "", nil, err
	}
	defer skp.Wipe()
	uc := jwt.NewUserClaims(upub)
	uc.Name = name
	uc.IssuerAccount = acc.ID
	uc.Expires = time.Now().Add(life).Unix()
	uc.Pub.Allow.Add(perms.PubAllow...)
	uc.Pub.Deny.Add(perms.PubDeny...)
	uc.Sub.Allow.Add(append(perms.SubAllow, inbox+".>")...)
	uc.Sub.Deny.Add(perms.SubDeny...)
	tok, err := uc.Encode(skp)
	if err != nil {
		return "", nil, err
	}
	return tok, ukp, nil
}

func (s *natsSession) connect(ctx context.Context, life time.Duration) error {
	tok, ukp, err := natsUserJWT(s.account, s.user, s.db.Level, s.inbox, life)
	if err != nil {
		return fmt.Errorf("could not sign your NATS user: %w", err)
	}
	nc, err := nats.Connect(s.url,
		nats.UserJWT(func() (string, error) { return tok, nil }, func(nonce []byte) ([]byte, error) { return ukp.Sign(nonce) }),
		nats.CustomInboxPrefix(s.inbox),
		nats.Name("argus · "+s.g.Email+" · "+s.db.Level),
		nats.Timeout(10*time.Second),
		nats.MaxReconnects(5),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "authorization") {
			return fmt.Errorf("NATS would not take the user Argus signed for %s: check that the signing key in %s is one of the account's signing keys "+
				"(nsc edit account %s --sk) and that the server has the account's JWT", s.db.Name, s.file, s.db.Name)
		}
		return fmt.Errorf("could not connect to NATS at %s: %w", s.url, err)
	}
	s.nc = nc
	s.version = nc.ConnectedServerVersion()
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return err
	}
	s.js = js
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := js.AccountInfo(cctx); err != nil {
		if errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount) || errors.Is(err, jetstream.ErrJetStreamNotEnabled) {
			s.noJS = true
		} else {
			nc.Close()
			return fmt.Errorf("NATS answered, but not JetStream's account info: %w", err)
		}
	}
	return nil
}

func (s *natsSession) ready() tunnel.StoreAnswer {
	return tunnel.StoreAnswer{Engine: tunnel.EngineNATS, User: s.user, Ops: tunnel.NATSStoreOps(s.db.Level),
		Version: s.version, ChangesNeedApproval: s.db.ChangesNeedApproval}
}

func (s *natsSession) close(context.Context) {
	if s.nc != nil {
		s.nc.Close()
	}
}

func natsErrText(err error) string {
	var je jetstream.JetStreamError
	if errors.As(err, &je) && je.APIError() != nil {
		return je.APIError().Description
	}
	if errors.Is(err, nats.ErrPermissionViolation) || strings.Contains(err.Error(), "Permissions Violation") {
		return "NATS refused it: your level does not allow that here"
	}
	return err.Error()
}

func (s *natsSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.NATSOpAllowed(req.Op, s.db.Level) {
		return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op + " for NATS at " + s.db.Level}, nil
	}
	n := req.NATS
	if n == nil {
		n = &tunnel.NATSRequest{}
	}
	if n.Stream != "" && !tunnel.ValidNATSName(n.Stream) {
		return tunnel.StoreAnswer{Type: "error", Error: "no stream named"}, nil
	}
	if n.Consumer != "" && !tunnel.ValidNATSName(n.Consumer) {
		return tunnel.StoreAnswer{Type: "error", Error: "no consumer named"}, nil
	}
	if s.noJS {
		switch req.Op {
		case "overview", "subjects", "connections", "tail", "publish":
		default:
			return tunnel.StoreAnswer{Type: "error", Error: "JetStream is not on for the account " + s.db.Name}, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	switch req.Op {
	case "overview":
		return s.overview(cctx)
	case "streams":
		return s.streams(cctx)
	case "stream":
		return s.stream(cctx, n)
	case "consumers":
		return s.consumers(cctx, n.Stream)
	case "consumer":
		return s.consumer(cctx, n)
	case "subjects":
		return s.subjects(cctx)
	case "connections":
		return s.connections(cctx)
	case "get":
		return s.get(cctx, req, n, rec)
	case "tail":
		return s.tail(cctx, req, n, rec)
	case "publish":
		return s.publish(cctx, req, n, rec)
	case "stream_create", "stream_update":
		return s.streamSave(cctx, req, n, rec)
	case "stream_purge":
		return s.streamPurge(cctx, req, n, rec)
	case "stream_delete":
		return s.streamDelete(cctx, req, n, rec)
	case "msg_delete":
		return s.msgDelete(cctx, req, n, rec)
	case "consumer_create":
		return s.consumerCreate(cctx, req, n, rec)
	case "consumer_delete":
		return s.consumerDelete(cctx, req, n, rec)
	case "consumer_pause":
		return s.consumerPause(cctx, req, n, rec)
	case "consumer_move":
		return s.consumerMove(cctx, req, n, rec)
	}
	return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
}

// run records text, runs fn, and records how it went: the shape of every
// change and every read of messages.
func (s *natsSession) run(id uint32, rec *stmtRecorder, text, tag string, fn func() (any, int64, bool, error)) (tunnel.StoreAnswer, error) {
	if err := rec.stmt(id, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	data, items, truncated, err := fn()
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(id, natsErrText(err), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err), MS: elapsed}, nil
	}
	if tag == "" {
		tag = strings.ToUpper(strings.Fields(text)[0])
	}
	if err := rec.done(id, tag, items, elapsed, data, truncated); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: data, Total: items, MS: elapsed, Truncated: truncated, Tag: tag}, nil
}

// refuse records an operation the agent would not send.
func refuseNATS(id uint32, rec *stmtRecorder, text, why string, confirm bool) (tunnel.StoreAnswer, error) {
	if err := rec.refused(id, text, why); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "error", NeedsConfirm: confirm, Error: why}, nil
}

// ---------------------------------------------------------------- monitoring

func (s *natsSession) monitorGet(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := "http://" + net.JoinHostPort(s.db.Host, strconv.Itoa(s.monitor)) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if s.f.httpGet != nil {
		return s.f.httpGet(ctx, u)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(r)
	if err != nil {
		return nil, fmt.Errorf("NATS's monitoring port %d on this server did not answer; set http_port (or monitor = in %s): %w", s.monitor, s.file, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("NATS's monitoring port answered %s", resp.Status)
	}
	return b, nil
}

// NATSAccountStat is the account's traffic as the server counts it.
type NATSAccountStat struct {
	Conns         int   `json:"conns"`
	Subs          int64 `json:"subscriptions"`
	MsgsIn        int64 `json:"msgsIn"`
	MsgsOut       int64 `json:"msgsOut"`
	BytesIn       int64 `json:"bytesIn"`
	BytesOut      int64 `json:"bytesOut"`
	SlowConsumers int64 `json:"slowConsumers"`
}

func (s *natsSession) accountStat(ctx context.Context) (*NATSAccountStat, error) {
	b, err := s.monitorGet(ctx, "/accstatz", url.Values{"unused": {"1"}})
	if err != nil {
		return nil, err
	}
	var z struct {
		Accounts []struct {
			Account  string `json:"acc"`
			Conns    int    `json:"conns"`
			NumSubs  int64  `json:"num_subscriptions"`
			Sent     struct{ Msgs, Bytes int64 }
			Received struct{ Msgs, Bytes int64 }
			Slow     int64 `json:"slow_consumers"`
		} `json:"account_statz"`
	}
	if err := json.Unmarshal(b, &z); err != nil {
		return nil, err
	}
	for _, a := range z.Accounts {
		if a.Account == s.account.ID {
			// "Received" is what the server took in from the account's
			// clients; "sent" what it delivered to them.
			return &NATSAccountStat{Conns: a.Conns, Subs: a.NumSubs, MsgsIn: a.Received.Msgs, MsgsOut: a.Sent.Msgs,
				BytesIn: a.Received.Bytes, BytesOut: a.Sent.Bytes, SlowConsumers: a.Slow}, nil
		}
	}
	return &NATSAccountStat{}, nil
}

// NATSConn is one client connection of the account, on this server.
type NATSConn struct {
	CID      uint64          `json:"cid"`
	Name     string          `json:"name,omitempty"`
	Lang     string          `json:"lang,omitempty"`
	Version  string          `json:"version,omitempty"`
	IP       string          `json:"ip"`
	Port     int             `json:"port"`
	Start    time.Time       `json:"start"`
	Last     time.Time       `json:"lastActivity"`
	RTT      string          `json:"rtt,omitempty"`
	Pending  int             `json:"pendingBytes"`
	InMsgs   int64           `json:"inMsgs"`
	OutMsgs  int64           `json:"outMsgs"`
	InBytes  int64           `json:"inBytes"`
	OutBytes int64           `json:"outBytes"`
	Subs     uint32          `json:"subscriptions"`
	User     string          `json:"user,omitempty"`
	Argus    bool            `json:"argus,omitempty"`
	SubList  []NATSSubDetail `json:"subs,omitempty"`
}

// NATSSubDetail is one subscription.
type NATSSubDetail struct {
	Subject string `json:"subject"`
	Queue   string `json:"queue,omitempty"`
	Msgs    int64  `json:"msgs"`
}

func (s *natsSession) conns(ctx context.Context) ([]NATSConn, bool, error) {
	limit := 1000
	b, err := s.monitorGet(ctx, "/connz", url.Values{"acc": {s.account.ID}, "subs": {"detail"},
		"limit": {strconv.Itoa(limit)}, "sort": {"msgs_to"}})
	if err != nil {
		return nil, false, err
	}
	var z struct {
		Total int `json:"total"`
		Conns []struct {
			CID      uint64    `json:"cid"`
			Name     string    `json:"name"`
			Lang     string    `json:"lang"`
			Version  string    `json:"version"`
			IP       string    `json:"ip"`
			Port     int       `json:"port"`
			Start    time.Time `json:"start"`
			Last     time.Time `json:"last_activity"`
			RTT      string    `json:"rtt"`
			Pending  int       `json:"pending_bytes"`
			InMsgs   int64     `json:"in_msgs"`
			OutMsgs  int64     `json:"out_msgs"`
			InBytes  int64     `json:"in_bytes"`
			OutBytes int64     `json:"out_bytes"`
			NumSubs  uint32    `json:"subscriptions"`
			Account  string    `json:"account"`
			User     string    `json:"authorized_user"`
			JWTName  string    `json:"name_tag"`
			Subs     []struct {
				Subject string `json:"subject"`
				Queue   string `json:"qgroup"`
				Msgs    int64  `json:"msgs"`
			} `json:"subscriptions_list_detail"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(b, &z); err != nil {
		return nil, false, err
	}
	out := []NATSConn{}
	for _, c := range z.Conns {
		if c.Account != "" && c.Account != s.account.ID {
			continue
		}
		nc := NATSConn{CID: c.CID, Name: c.Name, Lang: c.Lang, Version: c.Version, IP: c.IP, Port: c.Port, Start: c.Start,
			Last: c.Last, RTT: c.RTT, Pending: c.Pending, InMsgs: c.InMsgs, OutMsgs: c.OutMsgs, InBytes: c.InBytes,
			OutBytes: c.OutBytes, Subs: c.NumSubs, User: c.JWTName, Argus: strings.HasPrefix(c.Name, "argus · ")}
		for _, sd := range c.Subs {
			if strings.HasPrefix(sd.Subject, "_INBOX") {
				continue
			}
			nc.SubList = append(nc.SubList, NATSSubDetail{Subject: sd.Subject, Queue: sd.Queue, Msgs: sd.Msgs})
		}
		out = append(out, nc)
	}
	return out, z.Total > limit, nil
}

// ---------------------------------------------------------------- reads (not recorded)

// NATSOverview is the overview tab.
type NATSOverview struct {
	Account   string                 `json:"account"`
	AccountID string                 `json:"accountId"`
	Server    string                 `json:"server,omitempty"`
	Version   string                 `json:"version,omitempty"`
	Cluster   string                 `json:"cluster,omitempty"`
	JetStream bool                   `json:"jetstream"`
	Info      *jetstream.AccountInfo `json:"info,omitempty"`
	Stat      *NATSAccountStat       `json:"stat,omitempty"`
	StatError string                 `json:"statError,omitempty"`
}

func (s *natsSession) overview(ctx context.Context) (tunnel.StoreAnswer, error) {
	si := s.nc.ConnectedServerName()
	o := NATSOverview{Account: s.db.Name, AccountID: s.account.ID, Server: si, Version: s.version,
		Cluster: s.nc.ConnectedClusterName(), JetStream: !s.noJS}
	if !s.noJS {
		ai, err := s.js.AccountInfo(ctx)
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
		}
		o.Info = ai
	}
	st, err := s.accountStat(ctx)
	if err != nil {
		o.StatError = err.Error()
	} else {
		o.Stat = st
	}
	return tunnel.StoreAnswer{Type: "result", Data: o}, nil
}

func (s *natsSession) streams(ctx context.Context) (tunnel.StoreAnswer, error) {
	l := s.js.ListStreams(ctx)
	out := []*jetstream.StreamInfo{}
	truncated := false
	for si := range l.Info() {
		if len(out) >= 1000 {
			truncated = true
			continue
		}
		out = append(out, si)
	}
	if err := l.Err(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Config.Name < out[j].Config.Name })
	return tunnel.StoreAnswer{Type: "result", Data: out, Total: int64(len(out)), Truncated: truncated}, nil
}

// stream is one stream with how many messages each subject holds, at most
// 1000 subjects.
func (s *natsSession) stream(ctx context.Context, n *tunnel.NATSRequest) (tunnel.StoreAnswer, error) {
	if n.Stream == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "no stream named"}, nil
	}
	st, err := s.js.Stream(ctx, n.Stream)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(">"))
	if err != nil {
		info = st.CachedInfo()
	}
	subjects := info.State.Subjects
	truncated := false
	if len(subjects) > 1000 {
		keep := make(map[string]uint64, 1000)
		type kv struct {
			k string
			v uint64
		}
		all := make([]kv, 0, len(subjects))
		for k, v := range subjects {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
		for _, x := range all[:1000] {
			keep[x.k] = x.v
		}
		subjects, truncated = keep, true
	}
	info.State.Subjects = nil
	return tunnel.StoreAnswer{Type: "result", Truncated: truncated,
		Data: map[string]any{"info": info, "subjects": subjects}}, nil
}

func (s *natsSession) consumers(ctx context.Context, stream string) (tunnel.StoreAnswer, error) {
	var names []string
	if stream != "" {
		names = []string{stream}
	} else {
		l := s.js.StreamNames(ctx)
		for n := range l.Name() {
			names = append(names, n)
		}
		if err := l.Err(); err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
		}
	}
	sort.Strings(names)
	out := []*jetstream.ConsumerInfo{}
	truncated := false
	for _, name := range names {
		st, err := s.js.Stream(ctx, name)
		if err != nil {
			if stream != "" {
				return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
			}
			continue
		}
		l := st.ListConsumers(ctx)
		for ci := range l.Info() {
			if len(out) >= 2000 {
				truncated = true
				continue
			}
			out = append(out, ci)
		}
		if err := l.Err(); err != nil && stream != "" {
			return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
		}
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, Total: int64(len(out)), Truncated: truncated}, nil
}

func (s *natsSession) consumer(ctx context.Context, n *tunnel.NATSRequest) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.Consumer == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the consumer"}, nil
	}
	c, err := s.js.Consumer(ctx, n.Stream, n.Consumer)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
	}
	return tunnel.StoreAnswer{Type: "result", Data: c.CachedInfo()}, nil
}

// NATSSubject is one subject in use: who subscribes to it, and which
// streams store it.
type NATSSubject struct {
	Subject string   `json:"subject"`
	Subs    int      `json:"subs"`
	Queues  []string `json:"queues,omitempty"`
	Msgs    int64    `json:"msgs"`
	Apps    []string `json:"apps,omitempty"`
	Streams []string `json:"streams,omitempty"`
}

// subjects joins what is subscribed to on this server with what the
// streams take, so the console can say where a subject goes.
func (s *natsSession) subjects(ctx context.Context) (tunnel.StoreAnswer, error) {
	conns, truncated, err := s.conns(ctx)
	by := map[string]*NATSSubject{}
	get := func(sub string) *NATSSubject {
		x := by[sub]
		if x == nil {
			x = &NATSSubject{Subject: sub}
			by[sub] = x
		}
		return x
	}
	monitorErr := ""
	if err != nil {
		monitorErr = err.Error()
	}
	for _, c := range conns {
		if c.Argus {
			continue
		}
		for _, sd := range c.SubList {
			x := get(sd.Subject)
			x.Subs++
			x.Msgs += sd.Msgs
			if sd.Queue != "" && !slices.Contains(x.Queues, sd.Queue) {
				x.Queues = append(x.Queues, sd.Queue)
			}
			app := c.Name
			if app == "" {
				app = c.IP
			}
			if !slices.Contains(x.Apps, app) && len(x.Apps) < 20 {
				x.Apps = append(x.Apps, app)
			}
		}
	}
	if !s.noJS {
		l := s.js.ListStreams(ctx)
		for si := range l.Info() {
			for _, sub := range si.Config.Subjects {
				x := get(sub)
				x.Streams = append(x.Streams, si.Config.Name)
			}
		}
	}
	out := make([]NATSSubject, 0, len(by))
	for _, x := range by {
		if strings.HasPrefix(x.Subject, "$JS.") || strings.HasPrefix(x.Subject, "$SYS.") {
			continue
		}
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	if len(out) > 2000 {
		out, truncated = out[:2000], true
	}
	return tunnel.StoreAnswer{Type: "result", Truncated: truncated, Total: int64(len(out)),
		Data: map[string]any{"subjects": out, "monitorError": monitorErr}}, nil
}

func (s *natsSession) connections(ctx context.Context) (tunnel.StoreAnswer, error) {
	conns, truncated, err := s.conns(ctx)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	return tunnel.StoreAnswer{Type: "result", Data: conns, Total: int64(len(conns)), Truncated: truncated}, nil
}

// ---------------------------------------------------------------- reading messages (recorded)

// NATSMessage is one message read from a stream or a subject.
type NATSMessage struct {
	Seq     uint64              `json:"seq,omitempty"`
	Subject string              `json:"subject"`
	Time    string              `json:"time,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Data    string              `json:"data"`
	B64     bool                `json:"b64,omitempty"`
	Cut     bool                `json:"cut,omitempty"`
	Size    int                 `json:"size"`
}

const natsShowBytes = 64 << 10

func natsMessage(seq uint64, subject string, t time.Time, h nats.Header, data []byte) NATSMessage {
	text, b64, cut := kafkaText(data, natsShowBytes)
	m := NATSMessage{Seq: seq, Subject: subject, Data: text, B64: b64, Cut: cut, Size: len(data)}
	if !t.IsZero() {
		m.Time = t.UTC().Format(time.RFC3339Nano)
	}
	if len(h) > 0 {
		m.Headers = map[string][]string(h)
	}
	return m
}

// get reads a stream's messages by sequence with the stream's own get: no
// consumer is made and nothing is acked, so no app's place moves.
func (s *natsSession) get(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "no stream named"}, nil
	}
	if n.Subject != "" && !tunnel.ValidNATSSubject(n.Subject) {
		return tunnel.StoreAnswer{Type: "error", Error: "the subject filter is not a subject"}, nil
	}
	count := req.Count
	if count <= 0 {
		count = 20
	}
	count = min(count, s.maxItems, 200)
	var at time.Time
	text := "GET " + n.Stream
	switch n.From {
	case "", "newest":
		text += " newest"
	case "seq":
		if n.Seq == 0 {
			return tunnel.StoreAnswer{Type: "error", Error: "say the sequence to start at"}, nil
		}
		text += " from seq " + strconv.FormatUint(n.Seq, 10)
	case "time":
		t, err := time.Parse(time.RFC3339, n.Time)
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: "say the time to start at"}, nil
		}
		at = t
		text += " from time " + t.UTC().Format(time.RFC3339)
	default:
		return tunnel.StoreAnswer{Type: "error", Error: "a get starts at the newest, a sequence or a time"}, nil
	}
	if n.Subject != "" {
		text += " subject " + n.Subject
	}
	text += " count " + strconv.Itoa(count)
	return s.run(req.ID, rec, text, "", func() (any, int64, bool, error) {
		st, err := s.js.Stream(ctx, n.Stream)
		if err != nil {
			return nil, 0, false, err
		}
		state := st.CachedInfo().State
		msgs := []NATSMessage{}
		if state.Msgs == 0 {
			return msgs, 0, false, nil
		}
		switch n.From {
		case "seq", "time":
			seq := n.Seq
			if n.From == "time" {
				seq = s.seqAt(ctx, st, state.FirstSeq, state.LastSeq, at)
			}
			for len(msgs) < count && seq <= state.LastSeq && ctx.Err() == nil {
				var opts []jetstream.GetMsgOpt
				if n.Subject != "" {
					opts = append(opts, jetstream.WithGetMsgSubject(n.Subject))
				}
				m, err := st.GetMsg(ctx, seq, opts...)
				if errors.Is(err, jetstream.ErrMsgNotFound) {
					if n.Subject != "" {
						break
					}
					seq++
					continue
				}
				if err != nil {
					return nil, 0, false, err
				}
				msgs = append(msgs, natsMessage(m.Sequence, m.Subject, m.Time, m.Header, m.Data))
				seq = m.Sequence + 1
			}
			return msgs, int64(len(msgs)), seq <= state.LastSeq && len(msgs) == count, nil
		}
		// Newest first, walking back from the last sequence; a filter
		// looks at no more than 5000 messages for its matches.
		looked := 0
		for seq := state.LastSeq; seq >= state.FirstSeq && seq > 0 && len(msgs) < count && looked < 5000 && ctx.Err() == nil; seq-- {
			looked++
			m, err := st.GetMsg(ctx, seq)
			if errors.Is(err, jetstream.ErrMsgNotFound) {
				continue
			}
			if err != nil {
				return nil, 0, false, err
			}
			if n.Subject != "" && !tunnel.NATSSubjectMatches(n.Subject, m.Subject) {
				continue
			}
			msgs = append(msgs, natsMessage(m.Sequence, m.Subject, m.Time, m.Header, m.Data))
		}
		return msgs, int64(len(msgs)), len(msgs) == count || looked >= 5000, nil
	})
}

// seqAt finds the first sequence stored at or after t, by halving: a
// stream's sequences are in time order.
func (s *natsSession) seqAt(ctx context.Context, st jetstream.Stream, lo, hi uint64, t time.Time) uint64 {
	for lo < hi && ctx.Err() == nil {
		mid := lo + (hi-lo)/2
		m, err := st.GetMsg(ctx, mid, jetstream.WithGetMsgSubject(">"))
		if err != nil {
			return lo
		}
		if m.Time.Before(t) {
			lo = m.Sequence + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// tail listens on a subject for a few seconds and returns what arrived:
// live messages, whether a stream keeps them or not.
func (s *natsSession) tail(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidNATSSubject(n.Subject) || strings.HasPrefix(n.Subject, "$") || strings.HasPrefix(n.Subject, "_INBOX") {
		return tunnel.StoreAnswer{Type: "error", Error: "say an app's subject to listen on"}, nil
	}
	wait := n.Seconds
	if wait <= 0 || wait > 30 {
		wait = 10
	}
	count := req.Count
	if count <= 0 || count > 200 {
		count = 100
	}
	text := fmt.Sprintf("TAIL %s %ds max %d", n.Subject, wait, count)
	return s.run(req.ID, rec, text, "", func() (any, int64, bool, error) {
		sub, err := s.nc.SubscribeSync(n.Subject)
		if err != nil {
			return nil, 0, false, err
		}
		defer func() { _ = sub.Unsubscribe() }()
		_ = sub.SetPendingLimits(count*2, 64<<20)
		if err := s.nc.FlushWithContext(ctx); err != nil {
			return nil, 0, false, err
		}
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		msgs := []NATSMessage{}
		for len(msgs) < count {
			left := time.Until(deadline)
			if left <= 0 {
				break
			}
			m, err := sub.NextMsg(left)
			if errors.Is(err, nats.ErrTimeout) {
				break
			}
			if err != nil {
				return nil, 0, false, err
			}
			msgs = append(msgs, natsMessage(0, m.Subject, time.Now(), m.Header, m.Data))
		}
		return msgs, int64(len(msgs)), len(msgs) == count, nil
	})
}

// ---------------------------------------------------------------- writes (recorded)

func (s *natsSession) publish(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.NATSAppSubject(n.Subject) {
		return tunnel.StoreAnswer{Type: "error", Error: "publish to an app's subject: no wildcards, nothing starting with $ or _INBOX"}, nil
	}
	if len(req.Payload) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the message is too long"}, nil
	}
	if len(req.Headers) > 64 {
		return tunnel.StoreAnswer{Type: "error", Error: "too many headers"}, nil
	}
	mode := n.Mode
	if mode == "" {
		mode = "core"
	}
	if mode != "core" && mode != "stream" && mode != "request" {
		return tunnel.StoreAnswer{Type: "error", Error: "publish, publish to a stream, or request"}, nil
	}
	msg := nats.NewMsg(n.Subject)
	msg.Data = []byte(req.Payload)
	keys := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		if k == "" || strings.ContainsAny(k, ": \r\n") {
			return tunnel.StoreAnswer{Type: "error", Error: "a header's name has no spaces or colons"}, nil
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	text := "PUBLISH " + n.Subject
	if mode != "core" {
		text = strings.ToUpper(mode) + " " + n.Subject
	}
	for _, k := range keys {
		msg.Header.Set(k, req.Headers[k])
		text += " header " + strconv.Quote(k) + "=" + strconv.Quote(req.Headers[k])
	}
	text += " " + req.Payload
	return s.run(req.ID, rec, text, "PUBLISH 1", func() (any, int64, bool, error) {
		switch mode {
		case "stream":
			ack, err := s.js.PublishMsg(ctx, msg)
			if err != nil {
				return nil, 0, false, err
			}
			return map[string]any{"stream": ack.Stream, "seq": ack.Sequence, "duplicate": ack.Duplicate}, 1, false, nil
		case "request":
			wait := n.Seconds
			if wait <= 0 || wait > 30 {
				wait = 5
			}
			rctx, cancel := context.WithTimeout(ctx, time.Duration(wait)*time.Second)
			defer cancel()
			r, err := s.nc.RequestMsgWithContext(rctx, msg)
			if err != nil {
				if errors.Is(err, nats.ErrNoResponders) {
					return nil, 0, false, errors.New("nobody answers on " + n.Subject)
				}
				return nil, 0, false, err
			}
			return map[string]any{"reply": natsMessage(0, r.Subject, time.Now(), r.Header, r.Data)}, 1, false, nil
		}
		if err := s.nc.PublishMsg(msg); err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"sent": true}, 1, false, s.nc.FlushWithContext(ctx)
	})
}

func natsStreamConfig(sp tunnel.NATSStreamSpec) jetstream.StreamConfig {
	c := jetstream.StreamConfig{Name: sp.Name, Subjects: sp.Subjects, Description: sp.Description,
		Replicas: max(sp.Replicas, 1), MaxAge: time.Duration(sp.MaxAgeS) * time.Second,
		MaxBytes: -1, MaxMsgs: -1, MaxMsgsPerSubject: -1, AllowDirect: true,
		DenyDelete: !sp.AllowDelete, DenyPurge: sp.DenyPurge,
		Duplicates: time.Duration(sp.DuplicatesS) * time.Second}
	if sp.MaxBytes > 0 {
		c.MaxBytes = sp.MaxBytes
	}
	if sp.MaxMsgs > 0 {
		c.MaxMsgs = sp.MaxMsgs
	}
	if sp.MaxPerSub > 0 {
		c.MaxMsgsPerSubject = sp.MaxPerSub
	}
	switch sp.Retention {
	case "interest":
		c.Retention = jetstream.InterestPolicy
	case "workqueue":
		c.Retention = jetstream.WorkQueuePolicy
	}
	if sp.Storage == "memory" {
		c.Storage = jetstream.MemoryStorage
	}
	if sp.Discard == "new" {
		c.Discard = jetstream.DiscardNew
	}
	return c
}

func (s *natsSession) streamSave(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.StreamSpec == nil {
		return tunnel.StoreAnswer{Type: "error", Error: "say what the stream is"}, nil
	}
	if err := n.StreamSpec.Validate(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	cfg := natsStreamConfig(*n.StreamSpec)
	update := req.Op == "stream_update"
	if update {
		// Keep what the console does not set: an update changes only the
		// fields the form shows.
		st, err := s.js.Stream(ctx, cfg.Name)
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: natsErrText(err)}, nil
		}
		old := st.CachedInfo().Config
		old.Subjects, old.Description, old.MaxAge, old.MaxBytes, old.MaxMsgs = cfg.Subjects, cfg.Description, cfg.MaxAge, cfg.MaxBytes, cfg.MaxMsgs
		old.MaxMsgsPerSubject, old.Discard, old.Duplicates, old.Replicas = cfg.MaxMsgsPerSubject, cfg.Discard, cfg.Duplicates, cfg.Replicas
		old.DenyDelete, old.DenyPurge = cfg.DenyDelete, cfg.DenyPurge
		cfg = old
	}
	b, _ := json.Marshal(cfg)
	verb := "STREAM CREATE "
	if update {
		verb = "STREAM UPDATE "
	}
	return s.run(req.ID, rec, verb+string(b), strings.TrimSpace(verb), func() (any, int64, bool, error) {
		var st jetstream.Stream
		var err error
		if update {
			st, err = s.js.UpdateStream(ctx, cfg)
		} else {
			st, err = s.js.CreateStream(ctx, cfg)
		}
		if err != nil {
			return nil, 0, false, err
		}
		return st.CachedInfo(), 1, false, nil
	})
}

func (s *natsSession) streamPurge(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "no stream named"}, nil
	}
	if n.Subject != "" && !tunnel.ValidNATSSubject(n.Subject) {
		return tunnel.StoreAnswer{Type: "error", Error: "the subject is not a subject"}, nil
	}
	if n.Seq > 0 && n.Keep > 0 {
		return tunnel.StoreAnswer{Type: "error", Error: "purge up to a sequence, or keep the newest, not both"}, nil
	}
	text := "STREAM PURGE " + n.Stream
	var opts []jetstream.StreamPurgeOpt
	if n.Subject != "" {
		text += " subject " + n.Subject
		opts = append(opts, jetstream.WithPurgeSubject(n.Subject))
	}
	if n.Seq > 0 {
		text += " below seq " + strconv.FormatUint(n.Seq, 10)
		opts = append(opts, jetstream.WithPurgeSequence(n.Seq))
	}
	if n.Keep > 0 {
		text += " keep " + strconv.FormatUint(n.Keep, 10)
		opts = append(opts, jetstream.WithPurgeKeep(n.Keep))
	}
	if tunnel.NATSNeedsConfirm(req.Op, n) && req.Confirm != n.Stream {
		return refuseNATS(req.ID, rec, text, "purging every message of "+n.Stream+" needs you to type the stream's name to confirm", true)
	}
	return s.run(req.ID, rec, text, "STREAM PURGE", func() (any, int64, bool, error) {
		st, err := s.js.Stream(ctx, n.Stream)
		if err != nil {
			return nil, 0, false, err
		}
		before := st.CachedInfo().State.Msgs
		if err := st.Purge(ctx, opts...); err != nil {
			return nil, 0, false, err
		}
		after := before
		if info, err := st.Info(ctx); err == nil {
			after = info.State.Msgs
		}
		gone := int64(before) - int64(after)
		return map[string]any{"purged": max(gone, 0)}, max(gone, 0), false, nil
	})
}

func (s *natsSession) streamDelete(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "no stream named"}, nil
	}
	text := "STREAM DELETE " + n.Stream
	if req.Confirm != n.Stream {
		return refuseNATS(req.ID, rec, text, "deleting "+n.Stream+" and every message in it needs you to type the stream's name to confirm", true)
	}
	return s.run(req.ID, rec, text, "STREAM DELETE", func() (any, int64, bool, error) {
		return map[string]any{"deleted": n.Stream}, 1, false, s.js.DeleteStream(ctx, n.Stream)
	})
}

func (s *natsSession) msgDelete(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.Seq == 0 {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the message's sequence"}, nil
	}
	text := fmt.Sprintf("MSG DELETE %s seq %d", n.Stream, n.Seq)
	return s.run(req.ID, rec, text, "MSG DELETE", func() (any, int64, bool, error) {
		st, err := s.js.Stream(ctx, n.Stream)
		if err != nil {
			return nil, 0, false, err
		}
		return map[string]any{"deleted": n.Seq}, 1, false, st.DeleteMsg(ctx, n.Seq)
	})
}

func natsConsumerConfig(c tunnel.NATSConsumerSpec) jetstream.ConsumerConfig {
	cfg := jetstream.ConsumerConfig{Durable: c.Name, Name: c.Name, Description: c.Description,
		AckWait: time.Duration(c.AckWaitS) * time.Second, MaxDeliver: c.MaxDeliver, MaxAckPending: c.MaxAckPending,
		DeliverSubject: c.DeliverSubject, DeliverGroup: c.DeliverGroup, OptStartSeq: c.StartSeq}
	switch len(c.FilterSubjects) {
	case 0:
	case 1:
		cfg.FilterSubject = c.FilterSubjects[0]
	default:
		cfg.FilterSubjects = c.FilterSubjects
	}
	switch c.Deliver {
	case "new":
		cfg.DeliverPolicy = jetstream.DeliverNewPolicy
	case "last":
		cfg.DeliverPolicy = jetstream.DeliverLastPolicy
	case "last_per_subject":
		cfg.DeliverPolicy = jetstream.DeliverLastPerSubjectPolicy
	case "by_start_sequence":
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
	case "by_start_time":
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		if t, err := time.Parse(time.RFC3339, c.StartTime); err == nil {
			cfg.OptStartTime = &t
		}
	}
	switch c.Ack {
	case "all":
		cfg.AckPolicy = jetstream.AckAllPolicy
	case "none":
		cfg.AckPolicy = jetstream.AckNonePolicy
	default:
		cfg.AckPolicy = jetstream.AckExplicitPolicy
	}
	for _, b := range c.BackoffS {
		cfg.BackOff = append(cfg.BackOff, time.Duration(b)*time.Second)
	}
	if len(cfg.BackOff) > 0 && cfg.MaxDeliver <= len(cfg.BackOff) {
		// NATS wants more tries than back-off steps.
		cfg.MaxDeliver = len(cfg.BackOff) + 1
	}
	return cfg
}

func (s *natsSession) consumerCreate(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.ConsumerSpec == nil {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the consumer"}, nil
	}
	if err := n.ConsumerSpec.Validate(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	cfg := natsConsumerConfig(*n.ConsumerSpec)
	b, _ := json.Marshal(cfg)
	return s.run(req.ID, rec, "CONSUMER CREATE "+n.Stream+" "+string(b), "CONSUMER CREATE", func() (any, int64, bool, error) {
		c, err := s.js.CreateConsumer(ctx, n.Stream, cfg)
		if err != nil {
			return nil, 0, false, err
		}
		return c.CachedInfo(), 1, false, nil
	})
}

func (s *natsSession) consumerDelete(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.Consumer == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the consumer"}, nil
	}
	return s.run(req.ID, rec, "CONSUMER DELETE "+n.Stream+" "+n.Consumer, "CONSUMER DELETE", func() (any, int64, bool, error) {
		return map[string]any{"deleted": n.Consumer}, 1, false, s.js.DeleteConsumer(ctx, n.Stream, n.Consumer)
	})
}

// consumerPause pauses a consumer until a time, or resumes it when no time
// is given.
func (s *natsSession) consumerPause(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.Consumer == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the consumer"}, nil
	}
	if n.Time == "" {
		return s.run(req.ID, rec, "CONSUMER RESUME "+n.Stream+" "+n.Consumer, "CONSUMER RESUME", func() (any, int64, bool, error) {
			r, err := s.js.ResumeConsumer(ctx, n.Stream, n.Consumer)
			return r, 1, false, err
		})
	}
	until, err := time.Parse(time.RFC3339, n.Time)
	if err != nil || !until.After(time.Now()) {
		return tunnel.StoreAnswer{Type: "error", Error: "pause until a time still to come"}, nil
	}
	text := "CONSUMER PAUSE " + n.Stream + " " + n.Consumer + " until " + until.UTC().Format(time.RFC3339)
	return s.run(req.ID, rec, text, "CONSUMER PAUSE", func() (any, int64, bool, error) {
		r, err := s.js.PauseConsumer(ctx, n.Stream, n.Consumer, until)
		return r, 1, false, err
	})
}

// consumerMove moves where a consumer starts: NATS cannot change a
// consumer's start, so it is made again with the same name and settings and
// the new start. Apps pulling from it see the change on their next pull.
func (s *natsSession) consumerMove(ctx context.Context, req tunnel.StoreRequest, n *tunnel.NATSRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if n.Stream == "" || n.Consumer == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "say the stream and the consumer"}, nil
	}
	var to string
	switch n.From {
	case "seq":
		if n.Seq == 0 {
			return tunnel.StoreAnswer{Type: "error", Error: "say the sequence"}, nil
		}
		to = "seq " + strconv.FormatUint(n.Seq, 10)
	case "time":
		t, err := time.Parse(time.RFC3339, n.Time)
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: "say the time"}, nil
		}
		to = "time " + t.UTC().Format(time.RFC3339)
	case "newest":
		to = "new"
	case "first":
		to = "all"
	default:
		return tunnel.StoreAnswer{Type: "error", Error: "move to a sequence, a time, the first or the newest"}, nil
	}
	text := "CONSUMER MOVE " + n.Stream + " " + n.Consumer + " to " + to
	return s.run(req.ID, rec, text, "CONSUMER MOVE", func() (any, int64, bool, error) {
		c, err := s.js.Consumer(ctx, n.Stream, n.Consumer)
		if err != nil {
			return nil, 0, false, err
		}
		cfg := c.CachedInfo().Config
		if cfg.Durable == "" {
			return nil, 0, false, errors.New("only a named consumer can be moved; an ephemeral one goes with its app")
		}
		cfg.OptStartSeq, cfg.OptStartTime = 0, nil
		switch n.From {
		case "seq":
			cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, n.Seq
		case "time":
			t, _ := time.Parse(time.RFC3339, n.Time)
			cfg.DeliverPolicy, cfg.OptStartTime = jetstream.DeliverByStartTimePolicy, &t
		case "newest":
			cfg.DeliverPolicy = jetstream.DeliverNewPolicy
		case "first":
			cfg.DeliverPolicy = jetstream.DeliverAllPolicy
		}
		if err := s.js.DeleteConsumer(ctx, n.Stream, n.Consumer); err != nil {
			return nil, 0, false, err
		}
		nc, err := s.js.CreateConsumer(ctx, n.Stream, cfg)
		if err != nil {
			return nil, 0, false, fmt.Errorf("the consumer was removed but could not be made again (%s); make it again with these settings: %w",
				natsErrText(err), err)
		}
		return nc.CachedInfo(), 1, false, nil
	})
}

// ---------------------------------------------------------------- discovery

// discoverNATS lists the accounts the admin file has keys for, with what
// the server says about each when its monitoring port answers.
func discoverNATS(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	file := d.ConfigPath
	if file == "" {
		file = tunnel.NATSAdminFile
	}
	admin, err := readNATSAdmin(file)
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	if err := refuseOpenNATS(ctx, natsURL(d.Host, d.Port)); err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	names := make([]string, 0, len(admin.Accounts))
	for n := range admin.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	res := tunnel.DiscoverResult{OK: true}
	for _, n := range names {
		res.Databases = append(res.Databases, tunnel.Discovered{Name: n})
	}
	return res
}
