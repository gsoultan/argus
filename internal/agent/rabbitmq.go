package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
RabbitMQ in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"rabbitmq" stream──▶ agent ──▶ management API on this server

The agent is RabbitMQ's administrator on its own server the way it is
Postgres's: rabbitmqctl talks to the node with the Erlang cookie, so there is
no admin password anywhere. It uses rabbitmqctl to give the person a user for
this session (tunnel.StoreUser) with their level's tag and permissions on the
entry's vhost, the password on standard input, never on the command line; and
to delete the user afterwards.

The person's operations go to the management API on loopback as that user.
RabbitMQ lets anyone who may read a queue purge it, so on top of the
permissions the agent offers only the operations of the level
(tunnel.RabbitOpAllowed), each one built here, never a pass-through of the
API.
*/

// RabbitMQService is the tunnel service name.
const RabbitMQService = tunnel.RabbitMQService

// RabbitConfig is the rabbitmq service.
type RabbitConfig struct {
	StoreConfig
	// Ctl runs rabbitmqctl with args and stdin; nil runs the one installed
	// on this server.
	Ctl func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// RabbitMQ is the rabbitmq service.
func RabbitMQ(cfg RabbitConfig) TunnelService {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Ctl == nil {
		cfg.Ctl = runRabbitmqctl
	}
	f := &rabbitFactory{cfg: cfg, uses: &userUse{}}
	return storeService(cfg.StoreConfig, tunnel.RabbitMQService, tunnel.EngineRabbitMQ, f.open)
}

type rabbitFactory struct {
	cfg  RabbitConfig
	uses *userUse
	mu   sync.Mutex
	// swept is set once users Argus left behind have been removed. Each
	// rabbitmqctl call starts an Erlang VM, so this happens once per agent
	// start rather than at every session.
	swept bool
}

type rabbitSession struct {
	f       *rabbitFactory
	g       tunnel.Grant
	db      tunnel.GrantDB
	vhost   string
	user    string
	pw      string
	key     string
	base    string
	http    *http.Client
	version string
	perms   tunnel.RabbitLevel
}

// rabbitmqctlPaths are where packages put it; PATH is the last resort.
var rabbitmqctlPaths = []string{"/usr/sbin/rabbitmqctl", "/usr/lib/rabbitmq/bin/rabbitmqctl",
	"/opt/rabbitmq/sbin/rabbitmqctl", "/usr/local/sbin/rabbitmqctl"}

func runRabbitmqctl(ctx context.Context, args []string, stdin string) ([]byte, error) {
	bin := ""
	for _, p := range rabbitmqctlPaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			bin = p
			break
		}
	}
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("rabbitmqctl"); err != nil {
			return nil, errors.New("rabbitmqctl is not installed on this server, so the agent cannot manage RabbitMQ users")
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return out.Bytes(), fmt.Errorf("rabbitmqctl %s: %v: %s", args[0], err, msg)
	}
	return out.Bytes(), nil
}

func (f *rabbitFactory) open(ctx context.Context, g tunnel.Grant, _ string) (storeEngine, error) {
	db := *g.DB
	if !tunnel.LocalLoopback(db.Host) {
		return nil, errors.New("the RabbitMQ management API must be on this server; " + db.Host + " is not")
	}
	if !tunnel.ValidVhost(db.Name) {
		return nil, errors.New("the grant names no vhost")
	}
	perms, ok := tunnel.RabbitPermissions(db.Level)
	if !ok {
		return nil, errors.New("unknown access level " + db.Level)
	}
	scheme := "http"
	if db.Port == 15671 {
		scheme = "https"
	}
	s := &rabbitSession{f: f, g: g, db: db, vhost: db.Name, perms: perms,
		user: tunnel.StoreUser(g.Principal, db.Level),
		base: scheme + "://" + net.JoinHostPort(db.Host, strconv.Itoa(db.Port))}
	s.key = strconv.Itoa(db.Port) + "\x00" + s.user
	s.http = &http.Client{
		Timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout),
		Transport: &http.Transport{
			// Loopback only: never through a proxy from the environment.
			Proxy: nil,
			// A loopback listener has no certificate worth checking.
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback only
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	f.mu.Lock()
	err := s.giveUser(ctx)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var ov struct {
		Version string `json:"rabbitmq_version"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/overview", nil, &ov); err != nil {
		s.close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("the RabbitMQ management API at %s did not let your user in: %w; "+
			"is the management plugin enabled (rabbitmq-plugins enable rabbitmq_management)?", s.base, err)
	}
	s.version = ov.Version
	return s, nil
}

// giveUser makes the person's user for this level, or reuses it while
// another session of theirs holds it: RabbitMQ keeps one password per user.
func (s *rabbitSession) giveUser(ctx context.Context) error {
	ctl := s.f.cfg.Ctl
	if !s.f.swept {
		if out, err := ctl(ctx, []string{"list_users", "--silent", "--formatter", "json"}, ""); err == nil {
			var users []struct {
				User string `json:"user"`
			}
			if json.Unmarshal(out, &users) == nil {
				for _, u := range users {
					if tunnel.IsStoreUser(u.User) && !s.f.uses.live(strconv.Itoa(s.db.Port)+"\x00"+u.User) {
						_, _ = ctl(ctx, []string{"delete_user", u.User}, "")
					}
				}
				s.f.swept = true
			}
		}
	}
	s.f.uses.add(s.key, 1)
	pw, fresh, err := s.f.uses.password(s.key, sessionPassword)
	if err != nil {
		s.f.uses.add(s.key, -1)
		return err
	}
	s.pw = pw
	if !fresh {
		return nil
	}
	fail := func(err error) error {
		_, _ = ctl(context.WithoutCancel(ctx), []string{"delete_user", s.user}, "")
		s.f.uses.add(s.key, -1)
		return fmt.Errorf("could not prepare your RabbitMQ user: %w", err)
	}
	if _, err := ctl(ctx, []string{"add_user", s.user}, pw+"\n"); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fail(err)
		}
		if _, err := ctl(ctx, []string{"change_password", s.user}, pw+"\n"); err != nil {
			return fail(err)
		}
	}
	if _, err := ctl(ctx, []string{"set_user_tags", s.user, s.perms.Tag}, ""); err != nil {
		return fail(err)
	}
	if _, err := ctl(ctx, []string{"set_permissions", "-p", s.vhost, s.user,
		s.perms.Configure, s.perms.Write, s.perms.Read}, ""); err != nil {
		return fail(err)
	}
	return nil
}

func (s *rabbitSession) ready() tunnel.StoreAnswer {
	return tunnel.StoreAnswer{Engine: tunnel.EngineRabbitMQ, User: s.user, Version: s.version,
		Ops: tunnel.RabbitOps(s.db.Level)}
}

// close deletes the user when this was its last session; RabbitMQ closes
// the user's connections with it.
func (s *rabbitSession) close(ctx context.Context) {
	s.http.CloseIdleConnections()
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.uses.add(s.key, -1) > 0 {
		return
	}
	if _, err := s.f.cfg.Ctl(ctx, []string{"delete_user", s.user}, ""); err != nil {
		s.f.cfg.Log.Error("rabbitmq: could not delete a user after its session", "user", s.user, "error", err,
			"detail", "it is removed the next time the agent starts a RabbitMQ session")
	}
}

// errRabbit is an answer from the management API that was not a success.
type errRabbit struct {
	status int
	reason string
}

func (e errRabbit) Error() string {
	if e.reason == "" {
		return "RabbitMQ answered " + strconv.Itoa(e.status)
	}
	return e.reason
}

// call makes one management API request as the person's user. path is
// already escaped.
func (s *rabbitSession) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(s.user, s.pw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Reason", "from Argus by "+s.g.Email)
	resp, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, errCancelled
		}
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, tunnel.MaxStoreAnswer+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(b) > tunnel.MaxStoreAnswer {
		return resp.StatusCode, errors.New("RabbitMQ's answer is larger than Argus will carry; ask for fewer")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(b, &e)
		if resp.StatusCode == http.StatusUnauthorized && e.Reason == "" {
			e.Reason = "RabbitMQ did not accept your session's user"
		}
		return resp.StatusCode, errRabbit{resp.StatusCode, e.Reason}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("RabbitMQ answered something that is not JSON: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (s *rabbitSession) v() string { return url.PathEscape(s.vhost) }

func esc(name string) string { return url.PathEscape(name) }

func (s *rabbitSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.RabbitOpAllowed(req.Op, s.db.Level) {
		if _, known := map[string]bool{"queues": true, "exchanges": true, "queue": true, "peek": true,
			"publish": true, "purge": true, "declare_queue": true, "delete_queue": true, "bind": true,
			"connections": true, "close_connection": true}[req.Op]; known {
			why := req.Op + " needs a higher level than " + s.db.Level
			if err := rec.refused(req.ID, rabbitText(req), why); err != nil {
				return tunnel.StoreAnswer{}, err
			}
			return tunnel.StoreAnswer{Type: "error", Error: why}, nil
		}
		return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
	}
	needsName := req.Op != "queues" && req.Op != "exchanges" && req.Op != "connections"
	if needsName && (req.Name == "" && req.Op != "publish") {
		return tunnel.StoreAnswer{Type: "error", Error: "no name given"}, nil
	}
	if len(req.Name) > 255 || len(req.Exchange) > 255 || len(req.RoutingKey) > 255 {
		return tunnel.StoreAnswer{Type: "error", Error: "a name or routing key is longer than RabbitMQ allows"}, nil
	}
	if len(req.Payload) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the message is too long"}, nil
	}
	count := req.Count
	if count <= 0 {
		count = tunnel.DefaultStoreItems
	}
	if s.db.MaxRows > 0 {
		count = min(count, s.db.MaxRows)
	}
	count = min(count, tunnel.MaxStoreItems)

	// Browsing: not recorded, as the Postgres object tree is not.
	switch req.Op {
	case "queues", "exchanges":
		q := url.Values{"page": {"1"}, "page_size": {strconv.Itoa(min(count, 500))}}
		if p := strings.TrimSpace(req.Pattern); p != "" {
			q.Set("name", p)
			q.Set("use_regex", "false")
		}
		if req.Op == "queues" {
			q.Set("columns", "name,type,durable,auto_delete,messages,messages_ready,messages_unacknowledged,consumers,state")
		} else {
			q.Set("columns", "name,type,durable,auto_delete,internal")
		}
		var data any
		if _, err := s.call(ctx, http.MethodGet, "/api/"+req.Op+"/"+s.v()+"?"+q.Encode(), nil, &data); err != nil {
			return s.failed(ctx, err)
		}
		return tunnel.StoreAnswer{Type: "result", Data: data}, nil
	case "queue":
		var queue, bindings any
		if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(req.Name), nil, &queue); err != nil {
			return s.failed(ctx, err)
		}
		if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(req.Name)+"/bindings", nil, &bindings); err != nil {
			return s.failed(ctx, err)
		}
		return tunnel.StoreAnswer{Type: "result", Data: map[string]any{"queue": queue, "bindings": bindings}}, nil
	case "connections":
		var data any
		if _, err := s.call(ctx, http.MethodGet, "/api/vhosts/"+s.v()+"/connections", nil, &data); err != nil {
			return s.failed(ctx, err)
		}
		return tunnel.StoreAnswer{Type: "result", Data: data}, nil
	}

	// Everything else reads messages or changes something: confirmed where
	// it destroys, recorded before it is sent.
	text := rabbitText(req)
	if tunnel.RabbitNeedsConfirm(req.Op) && req.Confirm != s.g.Host {
		why := strings.ReplaceAll(req.Op, "_", " ") + " needs you to type the server's name, " + s.g.Host + ", to confirm"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", NeedsConfirm: true, Error: why}, nil
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	var (
		data  any
		err   error
		tag   string
		items int64
	)
	switch req.Op {
	case "peek":
		// Put back, not taken: RabbitMQ requeues each message, which marks
		// it redelivered for whoever consumes it next. The console says so.
		var msgs []any
		_, err = s.call(ctx, http.MethodPost, "/api/queues/"+s.v()+"/"+esc(req.Name)+"/get",
			map[string]any{"count": min(count, 100), "ackmode": "reject_requeue_true", "encoding": "auto",
				"truncate": tunnel.MaxCell}, &msgs)
		data, items, tag = msgs, int64(len(msgs)), "PEEK "+strconv.Itoa(len(msgs))
	case "publish":
		ex := req.Exchange
		if ex == "" {
			ex = "amq.default"
		}
		props := map[string]any{"delivery_mode": 2}
		if len(req.Headers) > 0 {
			if len(req.Headers) > 64 {
				return tunnel.StoreAnswer{Type: "error", Error: "too many headers"}, nil
			}
			props["headers"] = req.Headers
		}
		var out struct {
			Routed bool `json:"routed"`
		}
		_, err = s.call(ctx, http.MethodPost, "/api/exchanges/"+s.v()+"/"+esc(ex)+"/publish",
			map[string]any{"properties": props, "routing_key": req.RoutingKey, "payload": req.Payload,
				"payload_encoding": "string"}, &out)
		data, tag = out, map[bool]string{true: "PUBLISH routed", false: "PUBLISH not routed"}[out.Routed]
	case "purge":
		_, err = s.call(ctx, http.MethodDelete, "/api/queues/"+s.v()+"/"+esc(req.Name)+"/contents", nil, nil)
		tag = "PURGE"
	case "declare_queue":
		_, err = s.call(ctx, http.MethodPut, "/api/queues/"+s.v()+"/"+esc(req.Name),
			map[string]any{"durable": req.Durable, "auto_delete": false, "arguments": map[string]any{}}, nil)
		tag = "DECLARE QUEUE"
	case "delete_queue":
		_, err = s.call(ctx, http.MethodDelete, "/api/queues/"+s.v()+"/"+esc(req.Name), nil, nil)
		tag = "DELETE QUEUE"
	case "bind":
		if req.Exchange == "" {
			err = errors.New("bind needs an exchange; the default exchange is bound to every queue already")
			break
		}
		_, err = s.call(ctx, http.MethodPost, "/api/bindings/"+s.v()+"/e/"+esc(req.Exchange)+"/q/"+esc(req.Name),
			map[string]any{"routing_key": req.RoutingKey, "arguments": map[string]any{}}, nil)
		tag = "BIND"
	case "close_connection":
		_, err = s.call(ctx, http.MethodDelete, "/api/connections/"+esc(req.Name), nil, nil)
		tag = "CLOSE CONNECTION"
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(req.ID, err.Error(), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: err.Error(), MS: elapsed}, nil
	}
	if err := rec.done(req.ID, tag, items, elapsed, data, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: data, MS: elapsed}, nil
}

// failed answers a browse that did not work. A user RabbitMQ no longer
// knows -- deleted by an administrator mid-session -- ends the session.
func (s *rabbitSession) failed(_ context.Context, err error) (tunnel.StoreAnswer, error) {
	var re errRabbit
	if errors.As(err, &re) && re.status == http.StatusUnauthorized {
		return tunnel.StoreAnswer{}, errors.New("RabbitMQ no longer accepts this session's user")
	}
	return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
}

// rabbitText is an operation as the recording shows it, quoted so it reads
// back unambiguously.
func rabbitText(req tunnel.StoreRequest) string {
	args := []string{strings.ToUpper(req.Op)}
	switch req.Op {
	case "peek":
		args = append(args, "queue", req.Name, "count", strconv.Itoa(max(req.Count, 0)))
	case "publish":
		ex := req.Exchange
		if ex == "" {
			ex = "amq.default"
		}
		args = append(args, "exchange", ex, "routing_key", req.RoutingKey)
		keys := make([]string, 0, len(req.Headers))
		for k := range req.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "header", k+"="+req.Headers[k])
		}
		args = append(args, "payload", req.Payload)
	case "bind":
		args = append(args, "exchange", req.Exchange, "queue", req.Name, "routing_key", req.RoutingKey)
	case "declare_queue":
		args = append(args, req.Name, "durable", strconv.FormatBool(req.Durable))
	default:
		args = append(args, req.Name)
	}
	return tunnel.JoinRedisCommand(args)
}
