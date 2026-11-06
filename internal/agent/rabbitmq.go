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
	cmd := rabbitmqctlCmd(cctx, bin, args)
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
	return s.callAs(ctx, method, path, body, out, "", tunnel.MaxStoreAnswer)
}

// callAs is call with the reason RabbitMQ is given (empty: who asked) and
// the largest answer accepted.
func (s *rabbitSession) callAs(ctx context.Context, method, path string, body any, out any, reason string, limit int) (int, error) {
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
	if reason == "" {
		reason = "from Argus by " + s.g.Email
	}
	req.Header.Set("X-Reason", reason)
	resp, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, errCancelled
		}
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(b) > limit {
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

// rabbitBrowse is what a person may look at without it being recorded, as
// the Postgres object tree is not: names, counts and rates.
var rabbitBrowse = map[string]bool{"overview": true, "queues": true, "exchanges": true, "exchange": true,
	"bindings": true, "queue": true, "consumers": true, "connections": true, "channels": true}

// rabbitNamed are the operations that act on one named thing.
var rabbitNamed = map[string]bool{"exchange": true, "queue": true, "peek": true, "purge": true, "move": true,
	"declare_queue": true, "delete_queue": true, "declare_exchange": true, "bind": true, "close_connection": true}

// rabbitRates asks for ten minutes of samples, one every 30 seconds.
const rabbitRates = "msg_rates_age=600&msg_rates_incr=30"

// maxMove bounds the messages one move takes; the console asks again for more.
const maxMove = 10_000

func (s *rabbitSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.RabbitOpAllowed(req.Op, s.db.Level) {
		if tunnel.RabbitOpAllowed(req.Op, tunnel.LevelDBA) {
			why := req.Op + " needs a higher level than " + s.db.Level
			if err := rec.refused(req.ID, rabbitText(req), why); err != nil {
				return tunnel.StoreAnswer{}, err
			}
			return tunnel.StoreAnswer{Type: "error", Error: why}, nil
		}
		return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
	}
	if rabbitNamed[req.Op] && req.Name == "" {
		return tunnel.StoreAnswer{Type: "error", Error: "no name given"}, nil
	}
	if len(req.Name) > 255 || len(req.Exchange) > 255 || len(req.RoutingKey) > 255 || len(req.Target) > 255 || len(req.Reason) > 255 {
		return tunnel.StoreAnswer{Type: "error", Error: "a name, routing key or reason is longer than RabbitMQ allows"}, nil
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

	if rabbitBrowse[req.Op] {
		data, err := s.browse(ctx, req, count)
		if err != nil {
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
		props := map[string]any{}
		if len(req.Headers) > 0 {
			if len(req.Headers) > 64 {
				return tunnel.StoreAnswer{Type: "error", Error: "too many headers"}, nil
			}
			props["headers"] = req.Headers
		}
		for k, v := range req.Properties {
			if rabbitPublishProps[k] && v != "" {
				props[k] = v
			}
		}
		// Persistent unless the person chose transient.
		if req.Properties["delivery_mode"] == "1" {
			props["delivery_mode"] = 1
		} else {
			props["delivery_mode"] = 2
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
	case "move":
		var res moveResult
		res, err = s.move(ctx, req.Name, req.Target, req.Count)
		data, items, tag = res, int64(res.Moved), "MOVE "+strconv.Itoa(res.Moved)
	case "declare_queue":
		_, err = s.call(ctx, http.MethodPut, "/api/queues/"+s.v()+"/"+esc(req.Name),
			map[string]any{"durable": req.Durable, "auto_delete": false, "arguments": map[string]any{}}, nil)
		tag = "DECLARE QUEUE"
	case "delete_queue":
		_, err = s.call(ctx, http.MethodDelete, "/api/queues/"+s.v()+"/"+esc(req.Name), nil, nil)
		tag = "DELETE QUEUE"
	case "declare_exchange":
		if !tunnel.RabbitExchangeKinds[req.Kind] {
			err = errors.New("an exchange is direct, topic, fanout or headers")
			break
		}
		_, err = s.call(ctx, http.MethodPut, "/api/exchanges/"+s.v()+"/"+esc(req.Name),
			map[string]any{"type": req.Kind, "durable": req.Durable, "auto_delete": false, "internal": false,
				"arguments": map[string]any{}}, nil)
		tag = "DECLARE EXCHANGE"
	case "bind":
		if req.Exchange == "" {
			err = errors.New("bind needs an exchange; the default exchange is bound to every queue already")
			break
		}
		_, err = s.call(ctx, http.MethodPost, "/api/bindings/"+s.v()+"/e/"+esc(req.Exchange)+"/q/"+esc(req.Name),
			map[string]any{"routing_key": req.RoutingKey, "arguments": map[string]any{}}, nil)
		tag = "BIND"
	case "close_connection":
		// RabbitMQ sends X-Reason to the client in the close frame.
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = "closed"
		}
		_, err = s.callAs(ctx, http.MethodDelete, "/api/connections/"+esc(req.Name), nil, nil,
			reason+" (from Argus by "+s.g.Email+")", tunnel.MaxStoreAnswer)
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

// rabbitPublishProps are the message properties a person may set besides
// headers and delivery mode.
var rabbitPublishProps = map[string]bool{"content_type": true, "content_encoding": true, "message_id": true,
	"correlation_id": true, "reply_to": true, "type": true, "app_id": true, "expiration": true}

// browse answers the read-only operations: names, counts and rates.
func (s *rabbitSession) browse(ctx context.Context, req tunnel.StoreRequest, count int) (any, error) {
	get := func(path string) (any, error) {
		var data any
		_, err := s.call(ctx, http.MethodGet, path, nil, &data)
		return data, err
	}
	switch req.Op {
	case "overview":
		vhost, err := get("/api/vhosts/" + s.v() + "?" + rabbitRates)
		if err != nil {
			return nil, err
		}
		// The nodes need RabbitMQ's monitoring tag, which a read level does
		// not have: without it the console shows no node panel, nothing else.
		var nodes any
		if _, err := s.call(ctx, http.MethodGet, "/api/nodes?columns=name,running,uptime,mem_used,mem_limit,mem_alarm,"+
			"disk_free,disk_free_limit,disk_free_alarm,fd_used,fd_total,sockets_used,sockets_total", nil, &nodes); err != nil {
			var re errRabbit
			if !errors.As(err, &re) {
				return nil, err
			}
			nodes = nil
		}
		return map[string]any{"vhost": vhost, "nodes": nodes}, nil
	case "queues", "exchanges":
		q := url.Values{"page": {"1"}, "page_size": {strconv.Itoa(min(count, 500))}}
		if p := strings.TrimSpace(req.Pattern); p != "" {
			q.Set("name", p)
			q.Set("use_regex", "false")
		}
		if req.Op == "queues" {
			q.Set("columns", "name,type,durable,auto_delete,exclusive,arguments,messages,messages_ready,"+
				"messages_unacknowledged,consumers,state,idle_since,policy,"+
				"message_stats.publish_details.rate,message_stats.deliver_get_details.rate,message_stats.ack_details.rate")
		} else {
			q.Set("columns", "name,type,durable,auto_delete,internal,arguments,"+
				"message_stats.publish_in_details.rate,message_stats.publish_out_details.rate")
		}
		return get("/api/" + req.Op + "/" + s.v() + "?" + q.Encode())
	case "queue":
		queue, err := get("/api/queues/" + s.v() + "/" + esc(req.Name) + "?lengths_age=600&lengths_incr=30&" + rabbitRates)
		if err != nil {
			return nil, err
		}
		bindings, err := get("/api/queues/" + s.v() + "/" + esc(req.Name) + "/bindings")
		if err != nil {
			return nil, err
		}
		return map[string]any{"queue": queue, "bindings": bindings}, nil
	case "exchange":
		name := req.Name
		if name == "(default)" {
			name = "amq.default"
		}
		ex, err := get("/api/exchanges/" + s.v() + "/" + esc(name) + "?" + rabbitRates)
		if err != nil {
			return nil, err
		}
		bindings, err := get("/api/exchanges/" + s.v() + "/" + esc(name) + "/bindings/source")
		if err != nil {
			return nil, err
		}
		return map[string]any{"exchange": ex, "bindings": bindings}, nil
	case "bindings":
		return get("/api/bindings/" + s.v())
	case "consumers":
		return get("/api/consumers/" + s.v())
	case "connections":
		return get("/api/vhosts/" + s.v() + "/connections")
	case "channels":
		return get("/api/vhosts/" + s.v() + "/channels")
	}
	return nil, errors.New("unknown operation " + req.Op)
}

type moveResult struct {
	Moved int `json:"moved"`
	// Stopped says why the move ended before it ran out of messages.
	Stopped string `json:"stopped,omitempty"`
}

// maxMoveMessage bounds one message a move carries. A message is taken off
// the queue before the agent sees it, so the bound is far above anything
// the console carries: losing a message to a size check is not an option.
const maxMoveMessage = 256 << 20

// move takes messages off one queue and publishes each to another through
// the default exchange, one at a time. The management API acknowledges a
// message as it hands it over, so a message that cannot be published to the
// target is published back to the queue it came from, and the move stops.
func (s *rabbitSession) move(ctx context.Context, from, to string, n int) (moveResult, error) {
	var res moveResult
	if to == "" || to == from {
		return res, errors.New("move needs another queue to move the messages to")
	}
	var src, dst struct {
		Ready int `json:"messages_ready"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(from)+"?columns=messages_ready", nil, &src); err != nil {
		return res, err
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(to)+"?columns=messages_ready", nil, &dst); err != nil {
		return res, fmt.Errorf("the queue %s: %w", to, err)
	}
	if n <= 0 || n > src.Ready {
		n = src.Ready
	}
	n = min(n, maxMove)
	type msg struct {
		Payload    string         `json:"payload"`
		Encoding   string         `json:"payload_encoding"`
		Properties map[string]any `json:"properties"`
	}
	publish := func(queue string, m msg) (bool, error) {
		props := m.Properties
		if props == nil {
			props = map[string]any{}
		}
		var out struct {
			Routed bool `json:"routed"`
		}
		_, err := s.call(context.WithoutCancel(ctx), http.MethodPost, "/api/exchanges/"+s.v()+"/amq.default/publish",
			map[string]any{"properties": props, "routing_key": queue, "payload": m.Payload,
				"payload_encoding": m.Encoding}, &out)
		return out.Routed, err
	}
	for res.Moved < n {
		if ctx.Err() != nil {
			res.Stopped = "cancelled"
			return res, nil
		}
		var got []msg
		if _, err := s.callAs(ctx, http.MethodPost, "/api/queues/"+s.v()+"/"+esc(from)+"/get",
			map[string]any{"count": 1, "ackmode": "ack_requeue_false", "encoding": "auto"}, &got,
			"", maxMoveMessage); err != nil {
			if res.Moved == 0 {
				return res, err
			}
			res.Stopped = err.Error()
			return res, nil
		}
		if len(got) == 0 {
			res.Stopped = "no more ready messages"
			return res, nil
		}
		m := got[0]
		if m.Encoding == "" {
			m.Encoding = "string"
		}
		routed, err := publish(to, m)
		if err == nil && routed {
			res.Moved++
			continue
		}
		why := "the queue " + to + " did not take it"
		if err != nil {
			why = err.Error()
		}
		if back, berr := publish(from, m); berr != nil || !back {
			// The only copy left is this one: it goes into the recording with
			// the error, so it can be published again by hand.
			return res, fmt.Errorf("a message could not be moved (%s) nor put back on %s; it was %s payload %q properties %v",
				why, from, m.Encoding, m.Payload, m.Properties)
		}
		res.Stopped = "a message could not be moved, so it was put back and the move stopped: " + why
		return res, nil
	}
	return res, nil
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
		pkeys := make([]string, 0, len(req.Properties))
		for k := range req.Properties {
			pkeys = append(pkeys, k)
		}
		sort.Strings(pkeys)
		for _, k := range pkeys {
			args = append(args, "property", k+"="+req.Properties[k])
		}
		args = append(args, "payload", req.Payload)
	case "bind":
		args = append(args, "exchange", req.Exchange, "queue", req.Name, "routing_key", req.RoutingKey)
	case "declare_queue":
		args = append(args, req.Name, "durable", strconv.FormatBool(req.Durable))
	case "declare_exchange":
		args = append(args, req.Name, "type", req.Kind, "durable", strconv.FormatBool(req.Durable))
	case "move":
		args = append(args, "from", req.Name, "to", req.Target, "count", strconv.Itoa(max(req.Count, 0)))
	case "close_connection":
		args = append(args, req.Name, "reason", req.Reason)
	default:
		args = append(args, req.Name)
	}
	return tunnel.JoinRedisCommand(args)
}
