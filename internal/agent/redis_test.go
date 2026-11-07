//go:build !windows

package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real Redis: the test starts redis-server on a unix socket in a temp
directory, with a requirepass in its redis.conf, when redis-server is on PATH,
and skips otherwise. ARGUS_TEST_REDIS_SERVER names another binary, such as
valkey-server, to run every Redis test against that instead.
*/

type redisServer struct {
	sock, conf string
}

func startRedis(t *testing.T) redisServer {
	t.Helper()
	name := os.Getenv("ARGUS_TEST_REDIS_SERVER")
	if name == "" {
		name = "redis-server"
	}
	return startRedisBin(t, name)
}

func startRedisBin(t *testing.T, name string) redisServer {
	t.Helper()
	bin, err := exec.LookPath(name)
	if err != nil {
		t.Skip(name + " is not on PATH")
	}
	dir, err := os.MkdirTemp("", "argus-redis-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "redis.sock")
	conf := filepath.Join(dir, "redis.conf")
	if err := os.WriteFile(conf, []byte("port 0\nunixsocket "+sock+"\nsave \"\"\nappendonly no\n"+
		"requirepass \"s3cret admin\"\ndir "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, conf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			return redisServer{sock: sock, conf: conf}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(name + " did not start")
	return redisServer{}
}

// admin runs a command as Redis's administrator, as the test's own check.
func (r redisServer) admin(t *testing.T, args ...string) tunnel.RedisValue {
	t.Helper()
	c, err := dialRedis(context.Background(), r.sock, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	lim := respLimit{items: 1000, budget: 1 << 20}
	if v, err := c.do(context.Background(), 5*time.Second, lim, "AUTH", "s3cret admin"); err != nil || v.T == "e" {
		t.Fatalf("admin auth: %v %+v", err, v)
	}
	v, err := c.do(context.Background(), 5*time.Second, lim, args...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r redisServer) users(t *testing.T) []string {
	var out []string
	for _, u := range r.admin(t, "ACL", "USERS").A {
		out = append(out, u.V)
	}
	return out
}

type storeHarness struct {
	t      *testing.T
	stream tunnel.Stream
	fr     *tunnel.FrameReader
	rec    []tunnel.PGRecord
	dir    string
	ready  tunnel.StoreAnswer
	close  func()
	nextID uint32
}

// openStoreSession opens a session on svc as the control plane would.
func openStoreSession(t *testing.T, svc TunnelService, service string, db tunnel.GrantDB, dir string) (*storeHarness, error) {
	t.Helper()
	key, _ := tunnel.GrantKeyFromSecret(strings.Repeat("s", 32))
	pub := key.Public().(ed25519.PublicKey)
	g := tunnel.NewGrant("ana.sari@corp.example", "cache-01", "ana_sari", "")
	g.Service = service
	g.DB = &db
	tok, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	closeOnce := sync.OnceFunc(func() { cancel(); _ = a.Close(); _ = b.Close(); <-done })
	t.Cleanup(closeOnce)
	go func() {
		defer close(done)
		svc(ctx, &Call{Header: tunnel.Header{Service: service, Grant: tok}, Stream: pipeStream{a}, GrantKey: pub})
	}()
	h := &storeHarness{t: t, stream: pipeStream{b}, fr: tunnel.NewFrameReader(b), dir: dir, close: closeOnce}
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(b, &ans); err != nil {
		t.Fatal(err)
	}
	if !ans.OK {
		closeOnce()
		return nil, &refusal{ans.Error}
	}
	if h.ready = h.answer(); h.ready.Type != "ready" || h.ready.Level != db.Level {
		t.Fatalf("expected ready at %s, got %+v", db.Level, h.ready)
	}
	return h, nil
}

type refusal struct{ why string }

func (r *refusal) Error() string { return r.why }

func (h *storeHarness) answer() tunnel.StoreAnswer {
	for {
		typ, p, err := h.fr.Next()
		if err != nil {
			h.t.Fatal(err)
		}
		switch typ {
		case tunnel.FramePGRecord:
			var r tunnel.PGRecord
			_ = json.Unmarshal(p, &r)
			h.rec = append(h.rec, r)
		case tunnel.FramePGAnswer:
			var a tunnel.StoreAnswer
			_ = json.Unmarshal(p, &a)
			return a
		default:
			h.t.Fatalf("unexpected frame %q", typ)
		}
	}
}

func (h *storeHarness) ask(req tunnel.StoreRequest) tunnel.StoreAnswer {
	h.t.Helper()
	h.nextID++
	req.ID = h.nextID
	b, _ := json.Marshal(req)
	if err := tunnel.WriteFrame(h.stream, tunnel.FramePGRequest, b); err != nil {
		h.t.Fatal(err)
	}
	a := h.answer()
	if a.ID != req.ID {
		h.t.Fatalf("answer for %d while asking %d: %+v", a.ID, req.ID, a)
	}
	return a
}

func (h *storeHarness) run(cmd string) tunnel.StoreAnswer {
	h.t.Helper()
	return h.ask(tunnel.StoreRequest{Op: "run", Command: cmd})
}

// recorded reports whether a stmt line with exactly text is in the recording.
func (h *storeHarness) recorded(kind, text string) bool {
	for _, r := range h.rec {
		if r.Kind == kind && r.SQL == text {
			return true
		}
	}
	return false
}

func openRedis(t *testing.T, srv redisServer, svc TunnelService, level string, dir string) *storeHarness {
	t.Helper()
	h, err := openStoreSession(t, svc, tunnel.RedisService, tunnel.GrantDB{ID: "r1", Engine: tunnel.EngineRedis,
		Name: "0", Host: srv.sock, Port: 6399, Level: level, ConfigPath: srv.conf, KeysLimit: 5}, dir)
	if err != nil {
		t.Fatalf("redis session refused: %v", err)
	}
	return h
}

func TestRedisLevelsAreRedisACL(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	srv.admin(t, "SET", "greeting", "hello")

	read := openRedis(t, srv, svc, tunnel.LevelRead, dir)
	if read.ready.User != "argus-ana_sari-read" || read.ready.Engine != "redis" {
		t.Fatalf("ready: %+v", read.ready)
	}
	if a := read.run("GET greeting"); a.Value == nil || a.Value.V != "hello" {
		t.Fatalf("GET at read: %+v", a)
	}
	// The refusal is Redis's own: the agent sent it, and the ACL said no.
	a := read.run("SET greeting bye")
	if a.Value == nil || a.Value.T != "e" || !strings.Contains(a.Value.V, "NOPERM") {
		t.Fatalf("SET at read should be refused by Redis's ACL, got %+v", a)
	}
	if !read.recorded("stmt", "SET greeting bye") {
		t.Fatal("the refused SET is not in the recording")
	}
	if v := srv.admin(t, "GET", "greeting"); v.V != "hello" {
		t.Fatalf("the value changed under a read session: %q", v.V)
	}
	// FLUSHALL is not a read command, so read cannot even be asked to
	// confirm it into existence: Redis refuses it after the confirm.
	if a := read.ask(tunnel.StoreRequest{Op: "run", Command: "FLUSHALL", Confirm: "cache-01"}); a.Value == nil || a.Value.T != "e" {
		t.Fatalf("FLUSHALL at read: %+v", a)
	}

	write := openRedis(t, srv, svc, tunnel.LevelWrite, dir)
	if a := write.run(`SET greeting "hi there"`); a.Value == nil || a.Value.V != "OK" {
		t.Fatalf("SET at write: %+v", a)
	}
	if a := write.run("KEYS *"); a.Value == nil || a.Value.T != "e" {
		t.Fatalf("KEYS is @dangerous, so not at write: %+v", a)
	}
	if a := write.ask(tunnel.StoreRequest{Op: "run", Command: "FLUSHDB", Confirm: "cache-01"}); a.Value == nil || a.Value.T != "e" {
		t.Fatalf("FLUSHDB at write: %+v", a)
	}

	owner := openRedis(t, srv, svc, tunnel.LevelOwner, dir)
	if a := owner.run("CONFIG GET maxmemory"); a.Value == nil || a.Value.T != "e" {
		t.Fatalf("CONFIG is @admin, so not at owner: %+v", a)
	}

	dba := openRedis(t, srv, svc, tunnel.LevelDBA, dir)
	if a := dba.run("CONFIG GET maxmemory"); a.Value == nil || a.Value.T != "a" {
		t.Fatalf("CONFIG GET at dba: %+v", a)
	}

	for _, h := range []*storeHarness{read, write, owner, dba} {
		h.close()
	}
	for _, u := range srv.users(t) {
		if tunnel.IsStoreUser(u) {
			t.Fatalf("user %s is still there after its session closed", u)
		}
	}
}

func TestRedisDangerousNeedsConfirm(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	srv.admin(t, "SET", "a", "1")
	dba := openRedis(t, srv, svc, tunnel.LevelDBA, dir)

	a := dba.run("FLUSHALL")
	if a.Type != "error" || !a.NeedsConfirm {
		t.Fatalf("FLUSHALL without confirm: %+v", a)
	}
	if v := srv.admin(t, "EXISTS", "a"); v.I != 1 {
		t.Fatal("FLUSHALL ran without being confirmed")
	}
	if a := dba.ask(tunnel.StoreRequest{Op: "run", Command: "FLUSHALL", Confirm: "cache-02"}); !a.NeedsConfirm {
		t.Fatalf("FLUSHALL confirmed with the wrong name: %+v", a)
	}
	if a := dba.ask(tunnel.StoreRequest{Op: "run", Command: "flushall", Confirm: "cache-01"}); a.Value == nil || a.Value.V != "OK" {
		t.Fatalf("confirmed FLUSHALL: %+v", a)
	}
	if v := srv.admin(t, "EXISTS", "a"); v.I != 0 {
		t.Fatal("FLUSHALL did not run")
	}
	// AUTH would make the session someone else.
	if a := dba.run("AUTH default x"); a.Type != "error" {
		t.Fatalf("AUTH: %+v", a)
	}
	// KEYS on a db past the entry's limit (5) points at the browser.
	for i := 0; i < 10; i++ {
		srv.admin(t, "SET", "k"+string(rune('a'+i)), "v")
	}
	if a := dba.run("KEYS *"); a.Type != "error" || !strings.Contains(a.Error, "SCAN") {
		t.Fatalf("KEYS on a big db: %+v", a)
	}
	dba.close()

	var refused, ran int
	for _, r := range dba.rec {
		if r.Kind == "error" && strings.HasPrefix(r.Error, "refused:") {
			refused++
		}
		if r.Kind == "stmt" && r.SQL == "flushall" {
			ran++
		}
	}
	if refused < 4 || ran != 1 {
		t.Fatalf("recording: %d refusals and %d FLUSHALLs run: %+v", refused, ran, dba.rec)
	}
	f, err := os.Open(filepath.Join(dir, dba.ready.Session+".argussql"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The file also holds the close line, which the stream had gone before.
	v, err := tunnel.VerifyPGRecording(f, "")
	if err != nil || !v.OK || v.Lines != len(dba.rec)+1 {
		t.Fatalf("the recording does not verify: %+v %v", v, err)
	}
}

func TestRedisBrowserAndSharedUser(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	srv.admin(t, "HSET", "user:1", "name", "Ana", "team", "db")
	srv.admin(t, "RPUSH", "jobs", "a", "b", "c")
	srv.admin(t, "SET", "bin", "\x00\xff")
	// An application's own user, and one an earlier agent left behind.
	srv.admin(t, "ACL", "SETUSER", "app", "on", ">app", "~*", "+@all")
	srv.admin(t, "ACL", "SETUSER", "argus-ghost-read", "on", ">x", "~*", "+@all")

	one := openRedis(t, srv, svc, tunnel.LevelRead, dir)
	users := strings.Join(srv.users(t), " ")
	if strings.Contains(users, "argus-ghost-read") || !strings.Contains(users, "app") {
		t.Fatalf("users after open: %s", users)
	}
	a := one.ask(tunnel.StoreRequest{Op: "scan", Pattern: "*", Count: 100})
	if a.Type != "result" || a.Total != 3 || len(a.Keys) != 3 {
		t.Fatalf("scan: %+v", a)
	}
	types := map[string]string{}
	for _, k := range a.Keys {
		types[k.Name] = k.Type
	}
	if types["user:1"] != "hash" || types["jobs"] != "list" {
		t.Fatalf("types: %+v", types)
	}
	v := one.ask(tunnel.StoreRequest{Op: "value", Key: "user:1"})
	if v.KeyType != "hash" || v.Total != 2 || v.Value == nil || len(v.Value.A) != 4 {
		t.Fatalf("hash value: %+v", v)
	}
	if !one.recorded("stmt", "HSCAN user:1 0 COUNT 200") {
		t.Fatalf("reading a value is not recorded: %+v", one.rec)
	}
	if v := one.ask(tunnel.StoreRequest{Op: "value", Key: "jobs", Count: 2}); !v.Truncated || len(v.Value.A) != 2 || v.Total != 3 {
		t.Fatalf("list value cut at 2: %+v", v)
	}
	if v := one.ask(tunnel.StoreRequest{Op: "value", Key: "bin"}); !v.Value.Q || v.Value.V != `"\x00\xff"` {
		t.Fatalf("binary value: %+v", v.Value)
	}

	// A second read session of the same person shares the user; closing it
	// leaves the first working.
	two := openRedis(t, srv, svc, tunnel.LevelRead, dir)
	two.close()
	if a := one.run("GET bin"); a.Value == nil || a.Value.T != "b" {
		t.Fatalf("the first session stopped working when the second closed: %+v", a)
	}
	one.close()
	if strings.Contains(strings.Join(srv.users(t), " "), "argus-") {
		t.Fatal("the shared user outlived both sessions")
	}
}

func TestRedisRefusesARemoteHost(t *testing.T) {
	dir := t.TempDir()
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	_, err := openStoreSession(t, svc, tunnel.RedisService, tunnel.GrantDB{ID: "r1", Engine: tunnel.EngineRedis,
		Name: "0", Host: "10.0.0.5", Port: 6379, Level: tunnel.LevelRead}, dir)
	if err == nil || !strings.Contains(err.Error(), "on this server") {
		t.Fatalf("a remote Redis was not refused: %v", err)
	}
	// And a Postgres grant cannot be spent on Redis.
	_, err = openStoreSession(t, svc, tunnel.RedisService, tunnel.GrantDB{ID: "r1",
		Name: "0", Host: "127.0.0.1", Port: 6379, Level: tunnel.LevelRead}, dir)
	if err == nil || !strings.Contains(err.Error(), "not for redis") {
		t.Fatalf("a postgres grant opened redis: %v", err)
	}
}

func TestValkeyNamesItself(t *testing.T) {
	srv := startRedisBin(t, "valkey-server")
	dir := t.TempDir()
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	srv.admin(t, "SET", "greeting", "hello")

	h := openRedis(t, srv, svc, tunnel.LevelRead, dir)
	if h.ready.Server != "valkey" || h.ready.Engine != "redis" || h.ready.Version == "" {
		t.Fatalf("ready: %+v", h.ready)
	}
	if a := h.run("GET greeting"); a.Value == nil || a.Value.V != "hello" {
		t.Fatalf("GET at read: %+v", a)
	}
	if a := h.run("SET greeting bye"); a.Value == nil || a.Value.T != "e" || !strings.Contains(a.Value.V, "NOPERM") {
		t.Fatalf("SET at read should be refused by Valkey's ACL, got %+v", a)
	}
	h.close()
	for _, u := range srv.users(t) {
		if tunnel.IsStoreUser(u) {
			t.Fatalf("user %s is still there after its session closed", u)
		}
	}

	res := discoverRedis(context.Background(), tunnel.GrantDiscover{Engine: tunnel.EngineRedis, Host: srv.sock, Port: 6399, ConfigPath: srv.conf})
	if !res.OK || res.Server != "valkey" {
		t.Fatalf("discover: %+v", res)
	}
}

func TestRedisServerOf(t *testing.T) {
	for _, c := range []struct{ info, server, version, compat string }{
		{"# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\n", "", "7.2.4", "7.2.4"},
		{"# Server\r\nredis_version:7.2.4\r\nserver_name:valkey\r\nvalkey_version:8.1.1\r\n", "valkey", "8.1.1", "7.2.4"},
		{"# Server\r\nredis_version:7.2.13\r\nserver_name:valkey\r\n", "valkey", "7.2.13", "7.2.13"},
	} {
		s, v, compat := redisServerOf(c.info)
		if s != c.server || v != c.version || compat != c.compat {
			t.Errorf("%q: got %q %q %q", c.info, s, v, compat)
		}
	}
	if got := redisConf("/srv/cache/redis.conf"); got != "/srv/cache/redis.conf" {
		t.Errorf("a named file is read as named, got %s", got)
	}
}
