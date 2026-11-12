package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Redis in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"redis" stream──▶ agent ──▶ redis on this server

The agent signs in as Redis's administrator on its own server -- with the
requirepass of the server's redis.conf, or with none when the default user has
no password -- and uses that connection for one thing: giving the person an
ACL user for this session (tunnel.StoreUser) with the rules of their level and
a one-time password, and taking it away afterwards. The person's commands run
on a connection signed in as that user, so what they may do is what Redis's
ACL says, whatever the console or this agent thought of the command.
*/

// RedisService is the tunnel service name.
const RedisService = tunnel.RedisService

// defaultRedisConf is where the admin password is looked for when the entry
// names no file.
const defaultRedisConf = "/etc/redis/redis.conf"

// defaultValkeyConf is Valkey's own place for it. When the entry has the
// default and there is no redis.conf, the agent reads this one instead, so a
// Valkey added with defaults works as it is installed.
const defaultValkeyConf = "/etc/valkey/valkey.conf"

// redisConf is the file the agent reads for an entry's config path: the path
// itself, or Valkey's when the path is Redis's default and absent.
func redisConf(path string) string {
	if path == "" {
		path = defaultRedisConf
	}
	if path == defaultRedisConf {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(defaultValkeyConf); err == nil {
				return defaultValkeyConf
			}
		}
	}
	return path
}

// redisServerOf reads who answered from INFO server: Valkey reports
// server_name:valkey and its own valkey_version next to the redis_version it
// stays compatible with. It returns the server's name ("" for Redis), the
// version to show, and the Redis version it speaks, for the ACL check.
func redisServerOf(info string) (server, version, compat string) {
	compat = infoField(info, "redis_version")
	if v := infoField(info, "valkey_version"); v != "" || infoField(info, "server_name") == "valkey" {
		if v == "" {
			v = compat
		}
		return "valkey", v, compat
	}
	return "", compat, compat
}

// Redis is the redis service.
func Redis(cfg StoreConfig) TunnelService {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	undo := cfg.UndoDir
	if undo == "" && cfg.RecordDir != "" {
		undo = filepath.Join(filepath.Dir(filepath.Clean(cfg.RecordDir)), "redis-undo")
	}
	f := &redisFactory{cfg: cfg, uses: &userUse{}, undoDir: undo}
	return storeService(cfg, tunnel.RedisService, tunnel.EngineRedis, f.open)
}

type redisFactory struct {
	cfg  StoreConfig
	uses *userUse
	// undoDir holds undo copies of changed keys; see redis_manage.go.
	undoDir string
	// mu serializes giving and taking users, so two sessions of one person
	// opening together cannot interleave a reset with an added password.
	mu sync.Mutex
}

type redisSession struct {
	f    *redisFactory
	g    tunnel.Grant
	db   tunnel.GrantDB
	addr string // where Redis was reached: a socket path or host:port
	host string
	port int
	conf string

	user, pw string
	key      string
	conn     *respConn
	index    int
	version  string
	server   string
	timeout  time.Duration
	maxItems int
}

func (f *redisFactory) open(ctx context.Context, g tunnel.Grant, _ string) (storeEngine, error) {
	db := *g.DB
	if !tunnel.LocalPGHost(db.Host) {
		return nil, errors.New("Redis must be on this server; " + db.Host + " is not")
	}
	if !tunnel.ValidRedisDB(db.Name) {
		return nil, errors.New("the grant names no Redis db number")
	}
	if db.RedisRules != nil {
		if why := tunnel.ValidRedisRules(db.RedisRules, db.Level); why != "" {
			return nil, errors.New("the rules set for " + db.Level + " on this entry are not ones Argus writes: " + why)
		}
	}
	idx, _ := strconv.Atoi(db.Name)
	s := &redisSession{f: f, g: g, db: db, host: db.Host, port: db.Port, conf: db.ConfigPath, index: idx,
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout), maxItems: db.MaxRows}
	s.conf = redisConf(s.conf)
	if s.maxItems <= 0 || s.maxItems > tunnel.MaxStoreItems {
		s.maxItems = tunnel.MaxStoreItems
	}
	s.user = tunnel.StoreUser(g.Principal, db.Level)

	admin, err := s.adminConn(ctx)
	if err != nil {
		return nil, err
	}
	defer admin.close()
	info, err := admin.do(ctx, 10*time.Second, respLimit{items: 1, budget: 1 << 20}, "INFO", "server")
	if err != nil {
		return nil, fmt.Errorf("Redis did not answer: %w", err)
	}
	var compat string
	s.server, s.version, compat = redisServerOf(info.V)
	if major, _ := strconv.Atoi(strings.SplitN(compat, ".", 2)[0]); major > 0 && major < 6 {
		return nil, fmt.Errorf("Redis %s has no ACL users; Argus needs Redis 6 or later", s.version)
	}
	s.key = redisUseKey(db.Port, s.user)

	f.mu.Lock()
	err = s.giveUser(ctx, admin)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.connect(ctx); err != nil {
		s.takeUser(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// adminConn signs in as Redis's administrator: with the redis.conf's
// requirepass when it has one, else with none. The password is read from
// this server's file for this one connection and goes nowhere else.
func (s *redisSession) adminConn(ctx context.Context) (*respConn, error) {
	pw, socket, err := readRedisConf(s.conf)
	if err != nil {
		return nil, err
	}
	type way struct {
		host string
		port int
	}
	ways := []way{{s.host, s.port}}
	if socket != "" && socket != s.host {
		ways = append(ways, way{socket, 0})
	}
	var last error
	for _, w := range ways {
		c, err := dialRedis(ctx, w.host, w.port)
		if err != nil {
			last = err
			continue
		}
		lim := respLimit{items: 16, budget: 1 << 16}
		if pw != "" {
			v, err := c.do(ctx, 10*time.Second, lim, "AUTH", pw)
			if err != nil {
				c.close()
				last = err
				continue
			}
			if v.T == "e" && !strings.Contains(v.V, "without any password configured") {
				c.close()
				return nil, fmt.Errorf("Redis refused the requirepass in %s: %s", s.conf, v.V)
			}
		}
		v, err := c.do(ctx, 10*time.Second, lim, "ACL", "WHOAMI")
		if err != nil {
			c.close()
			last = err
			continue
		}
		if v.T == "e" {
			c.close()
			if strings.Contains(v.V, "unknown command") {
				return nil, errors.New("this Redis has no ACL users; Argus needs Redis 6 or later")
			}
			if strings.HasPrefix(v.V, "NOAUTH") {
				return nil, fmt.Errorf("Redis needs a password and %s has no requirepass the agent can use; "+
					"set the entry's config file to the redis.conf that has it", s.conf)
			}
			return nil, fmt.Errorf("Redis refused the agent: %s", v.V)
		}
		if strings.HasPrefix(w.host, "/") {
			s.addr = w.host
		} else {
			s.addr = w.host + ":" + strconv.Itoa(w.port)
		}
		s.host, s.port = w.host, w.port
		return c, nil
	}
	return nil, fmt.Errorf("could not reach Redis at %s:%d: %w", s.db.Host, s.db.Port, last)
}

// giveUser makes the person's user for this level, or adds this session's
// password to it when another session already holds it. Users Argus left
// behind -- an agent that stopped mid-session -- are removed first.
func (s *redisSession) giveUser(ctx context.Context, admin *respConn) error {
	lim := respLimit{items: 10000, budget: 4 << 20}
	if v, err := admin.do(ctx, 10*time.Second, lim, "ACL", "USERS"); err == nil && v.T == "a" {
		for _, u := range v.A {
			if tunnel.IsStoreUser(u.V) && !s.f.uses.live(redisUseKey(s.db.Port, u.V)) {
				_, _ = admin.do(ctx, 10*time.Second, lim, "ACL", "DELUSER", u.V)
			}
		}
	}
	pw, err := sessionPassword()
	if err != nil {
		return err
	}
	s.pw = pw
	args := []string{"ACL", "SETUSER", s.user}
	if s.f.uses.add(s.key, 1) == 1 {
		args = append(args, "reset", "on", ">"+pw)
		args = append(args, tunnel.RedisLevelRules(s.db.Level, s.db.RedisRules)...)
	} else {
		args = append(args, "on", ">"+pw)
	}
	v, err := admin.do(ctx, 10*time.Second, lim, args...)
	if err == nil && v.T == "e" {
		err = errors.New(v.V)
	}
	if err != nil {
		s.f.uses.add(s.key, -1)
		return fmt.Errorf("could not prepare your Redis user: %w", err)
	}
	return nil
}

// takeUser removes this session's password, and the user with it when this
// was its last session. Redis ends the user's connections when it goes.
func (s *redisSession) takeUser(ctx context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	left := s.f.uses.add(s.key, -1)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	admin, err := s.adminConn(cctx)
	if err != nil {
		s.f.cfg.Log.Error("redis: could not remove a user after its session", "user", s.user, "error", err,
			"detail", "it is removed at the next session on this server")
		return
	}
	defer admin.close()
	args := []string{"ACL", "DELUSER", s.user}
	if left > 0 {
		args = []string{"ACL", "SETUSER", s.user, "<" + s.pw}
	}
	if v, err := admin.do(cctx, 10*time.Second, respLimit{items: 4, budget: 1 << 12}, args...); err != nil || v.T == "e" {
		s.f.cfg.Log.Error("redis: could not remove a user after its session", "user", s.user, "error", err,
			"reply", v.V)
	}
}

// connect signs in as the person's user and selects the db.
func (s *redisSession) connect(ctx context.Context) error {
	c, err := dialRedis(ctx, s.host, s.port)
	if err != nil {
		return fmt.Errorf("could not reach Redis: %w", err)
	}
	lim := respLimit{items: 4, budget: 1 << 12}
	v, err := c.do(ctx, 10*time.Second, lim, "AUTH", s.user, s.pw)
	if err == nil && v.T == "e" {
		err = errors.New(v.V)
	}
	if err == nil && s.index != 0 {
		v, err = c.do(ctx, 10*time.Second, lim, "SELECT", strconv.Itoa(s.index))
		if err == nil && v.T == "e" {
			err = errors.New(v.V)
		}
	}
	if err != nil {
		c.close()
		return fmt.Errorf("could not sign in to Redis as %s: %w", s.user, err)
	}
	if s.conn != nil {
		s.conn.close()
	}
	s.conn = c
	return nil
}

func (s *redisSession) ready() tunnel.StoreAnswer {
	ops := []string{"run", "scan", "value"}
	for _, op := range []string{tunnel.RedisOpInfo, tunnel.RedisOpUndoList, tunnel.RedisOpEdit, tunnel.RedisOpCount,
		tunnel.RedisOpBulk, tunnel.RedisOpUndo, tunnel.RedisOpConfigGet, tunnel.RedisOpACLList, tunnel.RedisOpACLDryRun,
		tunnel.RedisOpConfigSet, tunnel.RedisOpSave, tunnel.RedisOpACLSet, tunnel.RedisOpACLDel} {
		if tunnel.LevelRank(s.db.Level) >= tunnel.LevelRank(tunnel.RedisOpLevel(op)) {
			ops = append(ops, op)
		}
	}
	return tunnel.StoreAnswer{Engine: tunnel.EngineRedis, User: s.user, Version: s.version, Server: s.server,
		Ops: ops, ChangesNeedApproval: s.db.ChangesNeedApproval}
}

func (s *redisSession) close(ctx context.Context) {
	if s.conn != nil {
		s.conn.close()
	}
	s.takeUser(ctx)
}

func (s *redisSession) lim() respLimit {
	return respLimit{items: s.maxItems, budget: tunnel.MaxStoreAnswer}
}

func (s *redisSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	switch req.Op {
	case "run":
		return s.run(ctx, req, rec)
	case tunnel.RedisOpInfo, tunnel.RedisOpEdit, tunnel.RedisOpCount, tunnel.RedisOpBulk, tunnel.RedisOpUndoList,
		tunnel.RedisOpUndo, tunnel.RedisOpConfigGet, tunnel.RedisOpConfigSet, tunnel.RedisOpSave,
		tunnel.RedisOpACLList, tunnel.RedisOpACLSet, tunnel.RedisOpACLDel, tunnel.RedisOpACLDryRun:
		return s.manage(ctx, req, rec)
	case "scan":
		return s.scan(ctx, req)
	case "value":
		return s.value(ctx, req, rec)
	}
	return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
}

// exec runs one command on the person's connection. A timeout or a cancel
// leaves the connection mid-reply, so it is replaced; a connection that
// cannot be replaced ends the session.
func (s *redisSession) exec(ctx context.Context, lim respLimit, args ...string) (tunnel.RedisValue, error, error) {
	v, err := s.conn.do(ctx, s.timeout, lim, args...)
	if err == nil {
		return v, nil, nil
	}
	why := err
	switch {
	case errors.Is(err, errTimedOut):
		why = fmt.Errorf("Redis did not answer within %s; the command may still finish on the server", s.timeout)
	case errors.Is(err, errCancelled):
		why = errors.New("cancelled; the command may still finish on the server")
	}
	if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
		return v, why, fmt.Errorf("the connection to Redis ended: %w", cerr)
	}
	return v, why, nil
}

func (s *redisSession) run(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if len(req.Command) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the command is too long"}, nil
	}
	args, err := tunnel.SplitRedisCommand(strings.TrimSpace(req.Command))
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	text := tunnel.JoinRedisCommand(args)
	if why := tunnel.RedisRefusal(args); why != "" {
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	if tunnel.RedisNeedsConfirm(args) && req.Confirm != s.g.Host {
		why := strings.ToUpper(args[0]) + " needs you to type the server's name, " + s.g.Host + ", to confirm"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", NeedsConfirm: true, Error: why}, nil
	}
	name := strings.ToUpper(args[0])
	if name == "KEYS" {
		limit := s.db.KeysLimit
		if limit <= 0 {
			limit = tunnel.DefaultKeysLimit
		}
		v, why, fatal := s.exec(ctx, respLimit{items: 1, budget: 64}, "DBSIZE")
		if fatal != nil {
			return tunnel.StoreAnswer{}, fatal
		}
		if why == nil && v.T == "i" && v.I > int64(limit) {
			msg := fmt.Sprintf("this db holds %d keys; KEYS would stop Redis while it walks them all, "+
				"so it runs only on dbs of up to %d. Use the key browser, which uses SCAN.", v.I, limit)
			if err := rec.refused(req.ID, text, msg); err != nil {
				return tunnel.StoreAnswer{}, err
			}
			return tunnel.StoreAnswer{Type: "error", Error: msg}, nil
		}
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	v, why, fatal := s.exec(ctx, s.lim(), args...)
	elapsed := time.Since(start).Milliseconds()
	if why != nil {
		if err := rec.fail(req.ID, why.Error(), elapsed); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		if fatal != nil {
			return tunnel.StoreAnswer{}, fatal
		}
		return tunnel.StoreAnswer{Type: "error", Error: why.Error(), MS: elapsed}, nil
	}
	if v.T == "e" {
		if err := rec.fail(req.ID, v.V, elapsed); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "result", Value: &v, MS: elapsed}, nil
	}
	if name == "SELECT" && len(args) == 2 {
		s.index, _ = strconv.Atoi(args[1])
	}
	if err := rec.done(req.ID, redisTag(v), redisItems(v), elapsed, v, v.Cut); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Value: &v, MS: elapsed, Truncated: v.Cut}, nil
}

// scan answers the key browser: one SCAN step, with each key's type and
// TTL. Not recorded -- it reads key names, as the Postgres tree reads table
// names -- and it runs as the person's user, so it shows what they may see.
func (s *redisSession) scan(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	cursor := req.Cursor
	if cursor == "" {
		cursor = "0"
	}
	if _, err := strconv.ParseUint(cursor, 10, 64); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: "the cursor is not a number"}, nil
	}
	count := req.Count
	if count <= 0 {
		count = tunnel.DefaultStoreItems
	}
	count = min(count, 1000)
	args := []string{"SCAN", cursor, "COUNT", strconv.Itoa(count)}
	if p := strings.TrimSpace(req.Pattern); p != "" && p != "*" {
		args = append(args, "MATCH", p)
	}
	if req.Type != "" {
		args = append(args, "TYPE", req.Type)
	}
	v, why, fatal := s.exec(ctx, respLimit{items: 10 * count, budget: 8 << 20}, args...)
	if fatal != nil {
		return tunnel.StoreAnswer{}, fatal
	}
	if why != nil {
		return tunnel.StoreAnswer{Type: "error", Error: why.Error()}, nil
	}
	if v.T == "e" {
		return tunnel.StoreAnswer{Type: "error", Error: v.V}, nil
	}
	if v.T != "a" || len(v.A) != 2 || v.A[1].T != "a" {
		return tunnel.StoreAnswer{Type: "error", Error: "Redis answered SCAN in a way the agent does not know"}, nil
	}
	ans := tunnel.StoreAnswer{Type: "result", Cursor: v.A[0].V, Keys: []tunnel.RedisKey{}}
	names := v.A[1].A
	if len(names) > 0 {
		cmds := make([][]string, 0, 2*len(names))
		for _, k := range names {
			raw := redisRaw(k)
			cmds = append(cmds, []string{"TYPE", raw}, []string{"TTL", raw})
		}
		vs, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: 1 << 12}, cmds)
		if err != nil {
			if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
				return tunnel.StoreAnswer{}, fmt.Errorf("the connection to Redis ended: %w", cerr)
			}
			return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
		}
		for i, k := range names {
			ans.Keys = append(ans.Keys, tunnel.RedisKey{Name: k.V, Q: k.Q, Type: vs[2*i].V, TTL: vs[2*i+1].I})
		}
	}
	if cursor == "0" {
		if n, why, fatal := s.exec(ctx, respLimit{items: 1, budget: 64}, "DBSIZE"); fatal != nil {
			return tunnel.StoreAnswer{}, fatal
		} else if why == nil && n.T == "i" {
			ans.Total = n.I
		}
	}
	return ans, nil
}

// value reads one key whole, or its first items: the fetch is recorded,
// because reading a value is reading the data.
func (s *redisSession) value(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	key := req.Key
	if req.KeyQuoted {
		key = redisRaw(tunnel.RedisValue{V: key, Q: true})
	}
	if key == "" || len(key) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "no key to read"}, nil
	}
	head, err := s.conn.pipeline(ctx, s.timeout, respLimit{items: 1, budget: 1 << 12},
		[][]string{{"TYPE", key}, {"TTL", key}})
	if err != nil {
		if cerr := s.connect(context.WithoutCancel(ctx)); cerr != nil {
			return tunnel.StoreAnswer{}, fmt.Errorf("the connection to Redis ended: %w", cerr)
		}
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	if head[0].T == "e" {
		return tunnel.StoreAnswer{Type: "error", Error: head[0].V}, nil
	}
	typ, ttl := head[0].V, head[1].I
	if typ == "none" {
		return tunnel.StoreAnswer{Type: "error", Error: "the key no longer exists"}, nil
	}
	limit := req.Count
	if limit <= 0 {
		limit = tunnel.DefaultStoreItems
	}
	limit = min(limit, s.maxItems)
	n := strconv.Itoa(limit)
	var sizeCmd, fetch []string
	switch typ {
	case "string":
		sizeCmd, fetch = []string{"STRLEN", key}, []string{"GET", key}
	case "hash":
		sizeCmd, fetch = []string{"HLEN", key}, []string{"HSCAN", key, "0", "COUNT", n}
	case "list":
		sizeCmd, fetch = []string{"LLEN", key}, []string{"LRANGE", key, "0", strconv.Itoa(limit - 1)}
	case "set":
		sizeCmd, fetch = []string{"SCARD", key}, []string{"SSCAN", key, "0", "COUNT", n}
	case "zset":
		sizeCmd, fetch = []string{"ZCARD", key}, []string{"ZRANGE", key, "0", strconv.Itoa(limit - 1), "WITHSCORES"}
	case "stream":
		sizeCmd, fetch = []string{"XLEN", key}, []string{"XRANGE", key, "-", "+", "COUNT", n}
	default:
		return tunnel.StoreAnswer{Type: "result", KeyType: typ, TTL: ttl,
			Error: "the key browser cannot show a " + typ + "; use a command"}, nil
	}
	size, why, fatal := s.exec(ctx, respLimit{items: 1, budget: 64}, sizeCmd...)
	if fatal != nil {
		return tunnel.StoreAnswer{}, fatal
	}
	if why == nil && size.T == "e" {
		return tunnel.StoreAnswer{Type: "error", Error: size.V}, nil
	}
	if err := rec.stmt(req.ID, tunnel.JoinRedisCommand(fetch)); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	v, why, fatal := s.exec(ctx, s.lim(), fetch...)
	elapsed := time.Since(start).Milliseconds()
	if why != nil || v.T == "e" {
		msg := v.V
		if why != nil {
			msg = why.Error()
		}
		if err := rec.fail(req.ID, msg, elapsed); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		if fatal != nil {
			return tunnel.StoreAnswer{}, fatal
		}
		return tunnel.StoreAnswer{Type: "error", Error: msg, MS: elapsed}, nil
	}
	// SCAN-shaped answers carry a cursor and the items; the viewer wants
	// the items.
	if (typ == "hash" || typ == "set") && v.T == "a" && len(v.A) == 2 {
		v = v.A[1]
	}
	total := size.I
	truncated := v.Cut
	if typ != "string" {
		got := int64(len(v.A))
		if typ == "hash" || typ == "zset" {
			got /= 2
		}
		truncated = truncated || got < total
	}
	if err := rec.done(req.ID, redisTag(v), redisItems(v), elapsed, v, truncated); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Value: &v, KeyType: typ, TTL: ttl, Total: total,
		Truncated: truncated, MS: elapsed}, nil
}

// redisUseKey names a user on one Redis server: the entry's port, which
// stays the same however the agent reached it.
func redisUseKey(port int, user string) string { return strconv.Itoa(port) + "\x00" + user }

// redisRaw is the key's bytes again from how it was shown.
func redisRaw(v tunnel.RedisValue) string {
	if !v.Q {
		return v.V
	}
	args, err := tunnel.SplitRedisCommand(v.V)
	if err != nil || len(args) != 1 {
		return v.V
	}
	return args[0]
}

// redisTag is how a reply reads in the recording, as redis-cli prints it.
func redisTag(v tunnel.RedisValue) string {
	switch v.T {
	case "s":
		if len(v.V) > 64 {
			return v.V[:64]
		}
		return v.V
	case "i":
		return "(integer) " + strconv.FormatInt(v.I, 10)
	case "n":
		return "(nil)"
	case "b":
		return "(string)"
	case "a":
		n := int64(len(v.A))
		if v.N > 0 {
			n = v.N
		}
		return "(array " + strconv.FormatInt(n, 10) + ")"
	}
	return ""
}

func redisItems(v tunnel.RedisValue) int64 {
	if v.T == "a" {
		if v.N > 0 {
			return v.N
		}
		return int64(len(v.A))
	}
	return 0
}

// infoField reads one field from an INFO answer.
func infoField(info, field string) string {
	for _, l := range strings.Split(info, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ":"); ok && k == field {
			return v
		}
	}
	return ""
}

var errNotExist = fs.ErrNotExist

// readFileLimited reads a file of at most n bytes.
func readFileLimited(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > n {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, n)
	}
	return b, nil
}
