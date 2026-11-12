//go:build !windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

func openRedisWith(t *testing.T, srv redisServer, svc TunnelService, db tunnel.GrantDB, dir string) (*storeHarness, error) {
	t.Helper()
	db.ID, db.Engine, db.Name, db.Host, db.Port, db.ConfigPath = "r1", tunnel.EngineRedis, "0", srv.sock, 6399, srv.conf
	return openStoreSession(t, svc, tunnel.RedisService, db, dir)
}

// data reads an answer's Data into v.
func data(t *testing.T, a tunnel.StoreAnswer, v any) {
	t.Helper()
	b, _ := json.Marshal(a.Data)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("answer data: %v (%s)", err, b)
	}
}

func redisSvc(dir string) TunnelService {
	return Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir + "/rec",
		UndoDir: dir + "/undo"})
}

func TestRedisEditKeepsAnUndoCopy(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	srv.admin(t, "HSET", "user:1", "plan", "team", "seats", "12")
	srv.admin(t, "EXPIRE", "user:1", "3600")

	w := openRedis(t, srv, svc, tunnel.LevelWrite, dir+"/rec")
	a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "user:1", Undo: true,
		Edits: [][]string{{"HSET", "user:1", "plan", "pro"}, {"HDEL", "user:1", "seats"}}})
	if a.Type != "result" {
		t.Fatalf("edit: %+v", a)
	}
	var res RedisEditResult
	data(t, a, &res)
	if res.Ran != 2 || res.UndoID == "" {
		t.Fatalf("edit result: %+v", res)
	}
	if !w.recorded("stmt", "HSET user:1 plan pro") || !w.recorded("stmt", "HDEL user:1 seats") {
		t.Fatal("each edit command is recorded before it runs")
	}
	if v := srv.admin(t, "HGET", "user:1", "plan"); v.V != "pro" {
		t.Fatalf("plan is %+v", v)
	}
	// An edit is only commands on its own key.
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "user:1",
		Edits: [][]string{{"DEL", "user:2"}}}); a.Type != "error" {
		t.Fatalf("an edit on another key ran: %+v", a)
	}
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "user:1",
		Edits: [][]string{{"FLUSHDB", "user:1"}}}); a.Type != "error" {
		t.Fatalf("FLUSHDB ran as an edit: %+v", a)
	}

	// The copy is listed for its maker and puts the key back, TTL and all.
	var list []RedisUndo
	data(t, w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpUndoList}), &list)
	if len(list) != 1 || list[0].ID != res.UndoID || list[0].Keys != 1 {
		t.Fatalf("undo list: %+v", list)
	}
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpUndo, UndoID: res.UndoID}); a.Type != "result" {
		t.Fatalf("undo: %+v", a)
	}
	if v := srv.admin(t, "HGET", "user:1", "plan"); v.V != "team" {
		t.Fatalf("after undo plan is %+v", v)
	}
	if v := srv.admin(t, "TTL", "user:1"); v.I <= 3500 {
		t.Fatalf("after undo TTL is %d", v.I)
	}
	if !w.recorded("stmt", "UNDO "+res.UndoID+" (EDIT user:1)") {
		t.Fatal("putting a copy back is recorded")
	}
	// Read cannot edit.
	r := openRedis(t, srv, svc, tunnel.LevelRead, dir+"/rec")
	if a := r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "user:1",
		Edits: [][]string{{"HSET", "user:1", "plan", "x"}}}); a.Type != "error" || !strings.Contains(a.Error, "write") {
		t.Fatalf("edit at read: %+v", a)
	}
}

func TestRedisBulkCountsFirstAndStopsAtItsCap(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	for i := 0; i < 150; i++ {
		srv.admin(t, "SET", "s:"+strconv.Itoa(i), "x")
	}
	srv.admin(t, "SET", "keep:1", "y")

	h := openRedis(t, srv, svc, tunnel.LevelWrite, dir+"/rec")
	var n RedisCount
	data(t, h.ask(tunnel.StoreRequest{Op: tunnel.RedisOpCount, Filter: tunnel.RedisFilter{Pattern: "s:*"}}), &n)
	if n.Count != 150 || len(n.Sample) != 20 || n.Types["string"] != 150 || n.Digest == "" {
		t.Fatalf("count: %+v", n)
	}
	// Every key, with no narrowing at all, is FLUSHDB's job.
	if a := h.ask(tunnel.StoreRequest{Op: tunnel.RedisOpCount, Filter: tunnel.RedisFilter{Pattern: "*"}}); a.Type != "error" {
		t.Fatalf("a bulk on every key: %+v", a)
	}
	// More match than it may change: nothing changes.
	a := h.ask(tunnel.StoreRequest{Op: tunnel.RedisOpBulk, Action: "delete", Max: 149,
		Filter: tunnel.RedisFilter{Pattern: "s:*"}})
	if a.Type != "error" || !strings.Contains(a.Error, "nothing changed") {
		t.Fatalf("over the cap: %+v", a)
	}
	if v := srv.admin(t, "DBSIZE"); v.I != 151 {
		t.Fatalf("keys after a refused bulk: %d", v.I)
	}
	a = h.ask(tunnel.StoreRequest{Op: tunnel.RedisOpBulk, Action: "delete", Max: 150, Undo: true,
		Filter: tunnel.RedisFilter{Pattern: "s:*"}})
	var res RedisBulkResult
	data(t, a, &res)
	if a.Type != "result" || res.Changed != 150 || res.UndoID == "" {
		t.Fatalf("bulk: %+v %+v", a, res)
	}
	if v := srv.admin(t, "DBSIZE"); v.I != 1 {
		t.Fatalf("keys after the bulk: %d", v.I)
	}
	if !h.recorded("stmt", "BULK DELETE MATCH s:* KEEPCOPY") {
		t.Fatalf("the bulk is recorded as one line: %+v", h.rec)
	}
	if a := h.ask(tunnel.StoreRequest{Op: tunnel.RedisOpUndo, UndoID: res.UndoID}); a.Type != "result" {
		t.Fatalf("restore all: %+v", a)
	}
	if v := srv.admin(t, "DBSIZE"); v.I != 151 {
		t.Fatalf("keys after restore: %d", v.I)
	}
}

func TestRedisChangesWaitForApproval(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	for i := 0; i < 120; i++ {
		srv.admin(t, "SET", "s:"+strconv.Itoa(i), "x")
	}
	h, err := openRedisWith(t, srv, svc, tunnel.GrantDB{Level: tunnel.LevelDBA, ChangesNeedApproval: true}, dir+"/rec")
	if err != nil {
		t.Fatal(err)
	}
	if !h.ready.ChangesNeedApproval {
		t.Fatal("ready says changes wait")
	}
	bulk := tunnel.StoreRequest{Op: tunnel.RedisOpBulk, Action: "delete", Max: 120, Filter: tunnel.RedisFilter{Pattern: "s:*"}}
	a := h.ask(bulk)
	if a.NeedsApproval == nil || a.NeedsApproval.Text != "BULK DELETE MATCH s:*" || a.NeedsApproval.Cap != 120 {
		t.Fatalf("bulk without approval: %+v", a)
	}
	if v := srv.admin(t, "DBSIZE"); v.I != 120 {
		t.Fatal("nothing changes before approval")
	}
	// A small bulk does not wait, but still stops at its cap.
	small := bulk
	small.Max = 50
	if a := h.ask(small); a.NeedsApproval != nil || a.Type != "error" || !strings.Contains(a.Error, "nothing changed") {
		t.Fatalf("small bulk: %+v", a)
	}
	cfg := tunnel.StoreRequest{Op: tunnel.RedisOpConfigSet, Config: []tunnel.RedisConfigItem{{Name: "maxmemory", Value: "10mb"}}}
	if a := h.ask(cfg); a.NeedsApproval == nil || a.NeedsApproval.Text != "CONFIG SET maxmemory 10mb" {
		t.Fatalf("config without approval: %+v", a)
	}
	// With an approval (the control plane checked it), it runs and the
	// recording names it.
	bulk.Approval = "8d0f6a1e-0000-4000-8000-000000000001"
	if a := h.ask(bulk); a.Type != "result" {
		t.Fatalf("approved bulk: %+v", a)
	}
	found := false
	for _, r := range h.rec {
		found = found || (r.Kind == "stmt" && r.Approval == bulk.Approval)
	}
	if !found {
		t.Fatal("the recording names the approval")
	}
}

func TestRedisConfiguration(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)

	w := openRedis(t, srv, svc, tunnel.LevelWrite, dir+"/rec")
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpConfigGet}); a.Type != "error" {
		t.Fatalf("config at write: %+v", a)
	}
	o := openRedis(t, srv, svc, tunnel.LevelOwner, dir+"/rec")
	var cfg RedisConfig
	data(t, o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpConfigGet}), &cfg)
	if !cfg.FileRead || len(cfg.Settings) < 50 {
		t.Fatalf("config: %d settings, file read %v", len(cfg.Settings), cfg.FileRead)
	}
	by := map[string]RedisSetting{}
	for _, s := range cfg.Settings {
		by[s.Name] = s
	}
	if rp := by["requirepass"]; !rp.Secret || !rp.Set || rp.Value != "" || rp.File != "" {
		t.Fatalf("requirepass is shown: %+v", rp)
	}
	if d := by["dir"]; d.Group != "locked" || d.Settable {
		t.Fatalf("dir: %+v", d)
	}
	if m := by["maxmemory"]; !m.Settable || m.Group != "memory" {
		t.Fatalf("maxmemory: %+v", m)
	}
	if a := o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpConfigSet,
		Config: []tunnel.RedisConfigItem{{Name: "maxmemory", Value: "1mb"}}}); a.Type != "error" {
		t.Fatalf("config set at owner: %+v", a)
	}

	d := openRedis(t, srv, svc, tunnel.LevelDBA, dir+"/rec")
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpConfigSet,
		Config: []tunnel.RedisConfigItem{{Name: "dir", Value: "/etc"}}}); a.Type != "error" {
		t.Fatalf("dir changed: %+v", a)
	}
	// Changed on the running server only: the overview sees the drift.
	srv.admin(t, "CONFIG", "SET", "maxmemory-policy", "allkeys-lru")
	var info RedisInfo
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpInfo}), &info)
	if info.Info["redis_version"] == "" || !hasItem(info.Drift, "maxmemory-policy") {
		t.Fatalf("info: version %q drift %v", info.Info["redis_version"], info.Drift)
	}
	a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpConfigSet, Rewrite: true,
		Config: []tunnel.RedisConfigItem{{Name: "maxmemory", Value: "2mb"}, {Name: "maxmemory-policy", Value: "allkeys-lru"}}})
	var res RedisConfigResult
	data(t, a, &res)
	if a.Type != "result" || !res.Rewritten || res.Before["maxmemory"] != "0" || res.After["maxmemory"] != "2097152" {
		t.Fatalf("config set: %+v %+v", a, res)
	}
	b, _ := os.ReadFile(srv.conf)
	if !strings.Contains(string(b), "maxmemory 2mb") || !strings.Contains(string(b), "maxmemory-policy allkeys-lru") {
		t.Fatalf("redis.conf after rewrite:\n%s", b)
	}
	if !d.recorded("stmt", "CONFIG SET maxmemory 2mb maxmemory-policy allkeys-lru\nCONFIG REWRITE") {
		t.Fatal("the change is recorded as it was asked")
	}
}

func hasItem(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func sha(pw string) string {
	s := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(s[:])
}

func TestRedisUsers(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	d := openRedis(t, srv, svc, tunnel.LevelDBA, dir+"/rec")

	a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, Create: true, User: "billing-svc", State: "on",
		AddHashes: []string{sha("first pass")}, Rules: []string{"~billing:*", "+@read", "+@write", "+@connection"}})
	if a.Type != "result" {
		t.Fatalf("create: %+v", a)
	}
	var saved RedisUserResult
	data(t, a, &saved)
	if saved.Saved != "redis.conf" {
		t.Fatalf("kept for a restart: %+v", saved)
	}
	// The password is never in the recording, only its hash.
	for _, r := range d.rec {
		if strings.Contains(r.SQL, "first pass") {
			t.Fatal("a password reached the recording")
		}
	}
	signIn := func(user, pw string) bool {
		c, err := dialRedis(context.Background(), srv.sock, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer c.close()
		v, err := c.do(context.Background(), 5*time.Second, respLimit{items: 1, budget: 1 << 10}, "AUTH", user, pw)
		return err == nil && v.T != "e"
	}
	if !signIn("billing-svc", "first pass") {
		t.Fatal("the new user cannot sign in")
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, Create: true, User: "billing-svc",
		Rules: []string{"+@read"}}); a.Type != "error" {
		t.Fatalf("created twice: %+v", a)
	}
	// Rotation: add a second password, remove the first by its short hash.
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "billing-svc",
		AddHashes: []string{sha("second pass")}}); a.Type != "result" {
		t.Fatalf("add password: %+v", a)
	}
	var users RedisUsers
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLList}), &users)
	var bill, def, mine *RedisUser
	for i := range users.Users {
		switch u := &users.Users[i]; {
		case u.Name == "billing-svc":
			bill = u
		case u.Name == "default":
			def = u
		case u.Argus:
			mine = u
		}
	}
	if bill == nil || len(bill.Passwords) != 2 || !bill.On || !hasItem(bill.Keys, "~billing:*") {
		t.Fatalf("billing-svc listed as %+v", bill)
	}
	if def == nil || !def.Self || mine == nil {
		t.Fatalf("default %+v, argus user %+v", def, mine)
	}
	first := tunnel.RedisHashRef(sha("first pass"))
	if !hasItem(bill.Passwords, first) {
		t.Fatalf("passwords %v, want %s", bill.Passwords, first)
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "billing-svc",
		RemoveHashes: []string{first}}); a.Type != "result" {
		t.Fatalf("remove password: %+v", a)
	}
	if signIn("billing-svc", "first pass") || !signIn("billing-svc", "second pass") {
		t.Fatal("the first password should be gone and the second still work")
	}
	// What Argus never changes from here.
	for _, u := range []string{"default", mine.Name} {
		if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLDel, User: u}); a.Type != "error" {
			t.Fatalf("deleted %s: %+v", u, a)
		}
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "default", State: "off"}); a.Type != "error" {
		t.Fatalf("turned off the agent's own user: %+v", a)
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "billing-svc",
		Rules: []string{">plaintext"}}); a.Type != "error" {
		t.Fatalf("a plaintext password rule: %+v", a)
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLDel, User: "billing-svc"}); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}
	if signIn("billing-svc", "second pass") {
		t.Fatal("a deleted user signed in")
	}
	// Owner sees users but cannot change them.
	o := openRedis(t, srv, svc, tunnel.LevelOwner, dir+"/rec")
	if a := o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLList}); a.Type != "result" {
		t.Fatalf("list at owner: %+v", a)
	}
	if a := o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, Create: true, User: "x",
		Rules: []string{"+@read"}}); a.Type != "error" {
		t.Fatalf("create at owner: %+v", a)
	}
}

func TestRedisLevelRulesAndDryRun(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	rules := []string{"%R~*", "~cart:*", "-@all", "+@read", "+@write", "+@connection", "-@dangerous"}
	h, err := openRedisWith(t, srv, svc, tunnel.GrantDB{Level: tunnel.LevelWrite, RedisRules: rules}, dir+"/rec")
	if err != nil {
		t.Fatal(err)
	}
	if a := h.run("SET cart:1 x"); a.Value == nil || a.Value.V != "OK" {
		t.Fatalf("SET cart:1: %+v", a)
	}
	if a := h.run("SET user:1 x"); a.Value == nil || !strings.Contains(a.Value.V, "NOPERM") {
		t.Fatalf("SET user:1 should be refused by Redis: %+v", a)
	}
	if a := h.run("GET user:1"); a.Value == nil || a.Value.T == "e" {
		t.Fatalf("GET user:1 is read only, not refused: %+v", a)
	}
	// Rules that reach the server's configuration are refused below DBA.
	if _, err := openRedisWith(t, srv, svc, tunnel.GrantDB{Level: tunnel.LevelWrite,
		RedisRules: []string{"~*", "+@admin"}}, dir+"/rec"); err == nil {
		t.Fatal("+@admin at write opened")
	}

	o := openRedis(t, srv, svc, tunnel.LevelOwner, dir+"/rec")
	var dr RedisDryRun
	data(t, o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLDryRun, Rules: rules, Command: "SET cart:9 1"}), &dr)
	if !dr.Allowed {
		t.Fatalf("dry run SET cart:9: %+v", dr)
	}
	data(t, o.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLDryRun, Rules: rules, Command: "DEL user:1"}), &dr)
	if dr.Allowed || dr.Reason == "" || strings.Contains(dr.Reason, "dryrun") {
		t.Fatalf("dry run DEL user:1: %+v", dr)
	}
	for _, u := range srv.users(t) {
		if strings.Contains(u, "dryrun") {
			t.Fatalf("the dry run's user was left behind: %s", u)
		}
	}
}
