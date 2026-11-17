package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

const debianMongodConf = `# mongod.conf

# Where and how to store data.
storage:
  dbPath: /var/lib/mongodb

# network interfaces
net:
  port: 27017
  bindIp: 127.0.0.1   # the loopback only


security:
  authorization: enabled
`

func TestSetMongoBindChangesOnlyBindIp(t *testing.T) {
	b, err := setMongoBind([]byte(debianMongodConf), "127.0.0.1,10.0.0.183")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(debianMongodConf, "net:\n  port: 27017\n  bindIp: 127.0.0.1   # the loopback only\n",
		"net:\n  bindIp: 127.0.0.1,10.0.0.183\n  port: 27017\n", 1)
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	// Anywhere is bindIpAll, and bindIp goes.
	all, err := setMongoBind(b, "*")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(all), "net:\n  bindIpAll: true\n  port: 27017\n") || strings.Contains(string(all), "bindIp:") {
		t.Fatalf("anywhere:\n%s", all)
	}
	if got, _ := mongoConfBind(all); got != "*" {
		t.Fatalf("reads back as %q", got)
	}
	// And back to the loopback, the rest of the file as it was.
	back, err := setMongoBind(all, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "security:\n  authorization: enabled\n") || !strings.HasPrefix(string(back), "# mongod.conf\n") {
		t.Fatalf("the rest changed:\n%s", back)
	}
}

func TestSetMongoBindEdges(t *testing.T) {
	// No net: block at all: one is added, and mongod's default was the loopback.
	if got, _ := mongoConfBind([]byte("storage:\n  dbPath: /data\n")); got != "127.0.0.1" {
		t.Fatalf("default %q", got)
	}
	b, err := setMongoBind([]byte("storage:\n  dbPath: /data\n"), "127.0.0.1,10.0.0.5")
	if err != nil || !strings.HasSuffix(string(b), "net:\n  bindIp: 127.0.0.1,10.0.0.5\n") {
		t.Fatalf("%v\n%s", err, b)
	}
	// Four-space indentation is kept.
	b, err = setMongoBind([]byte("net:\n    port: 27017\n    bindIp: 0.0.0.0\n"), "127.0.0.1")
	if err != nil || string(b) != "net:\n    bindIp: 127.0.0.1\n    port: 27017\n" {
		t.Fatalf("%v\n%q", err, b)
	}
	// A deeper bindIp (not net's own) is left alone... and the result is checked.
	b, err = setMongoBind([]byte("net:\n  port: 27017\n  tls:\n    mode: requireTLS\n"), "*")
	if err != nil || !strings.Contains(string(b), "    mode: requireTLS") {
		t.Fatalf("%v\n%s", err, b)
	}
	// Quoted values, comments, and keys deeper than net's own are read as mongod reads them.
	if got, _ := mongoConfBind([]byte("net:\n  bindIp: \"127.0.0.1, 10.0.0.5\"  # app\n  other:\n    bindIpAll: true\nstorage:\n  bindIpAll: true\n")); got != "127.0.0.1,10.0.0.5" {
		t.Fatalf("read %q", got)
	}
	// The flow form is refused rather than rewritten.
	if _, err := setMongoBind([]byte("net: {port: 27017, bindIp: 127.0.0.1}\n"), "*"); err == nil {
		t.Fatal("the flow form was rewritten")
	}
	if !cmdLineBind([]string{"mongod", "--config", "/etc/mongod.conf", "--bind_ip_all"}) ||
		!cmdLineBind([]string{"mongod", "--bind_ip=0.0.0.0"}) || cmdLineBind([]string{"mongod", "--config", "/etc/mongod.conf"}) {
		t.Fatal("cmdLineBind")
	}
}

func TestMongoNetworkText(t *testing.T) {
	req := tunnel.StoreRequest{Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	want := "SET net.bindIp = '127.0.0.1,10.0.0.183' (from the next start)\nFIREWALL ALLOW 10.0.0.61\nFIREWALL DENY OTHERS\nRESTART MONGOD"
	if got := tunnel.MongoNetworkText(req); got != want {
		t.Fatalf("got %q", got)
	}
	any := tunnel.StoreRequest{Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachAny}
	if got := tunnel.MongoNetworkText(any); !strings.HasPrefix(got, "SET net.bindIpAll = true") || !tunnel.OpensToAnywhere(got) {
		t.Fatalf("anywhere %q", got)
	}
}

// mongoNetFake is mongod as the session reads it: it runs with what its
// configuration file said when it last started.
type mongoNetFake struct {
	conf    string
	running string
	cmdLine bool
}

func (f *mongoNetFake) state(context.Context) (MongoNetwork, error) {
	b, err := os.ReadFile(f.conf)
	if err != nil {
		return MongoNetwork{}, err
	}
	next, err := mongoConfBind(b)
	if err != nil {
		return MongoNetwork{}, err
	}
	return MongoNetwork{Bind: f.running, Next: next, PendingRestart: next != f.running, Port: 27017, Version: "7.0.14",
		ConfigFile: f.conf, CmdLine: f.cmdLine, Host: "db-01", pid: 42}, nil
}

func mongoNetSession(t *testing.T, level string, approval bool, fw *fakeRedisFirewall, log *[]string) (*mongoSession, *mongoNetFake, *stmtRecorder, string) {
	t.Helper()
	dir := t.TempDir()
	conf := filepath.Join(dir, "mongod.conf")
	if err := os.WriteFile(conf, []byte(debianMongodConf), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &mongoNetFake{conf: conf, running: "127.0.0.1"}
	cfg := StoreConfig{
		Firewall: func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
			if !r.Check {
				*log = append(*log, "firewall "+r.Reach)
			}
			return fw.do(ctx, r)
		},
		Restart: func(_ context.Context, unit string) error {
			b, _ := os.ReadFile(conf)
			fake.running, _ = mongoConfBind(b)
			*log = append(*log, "restart "+unit+" "+fake.running)
			return nil
		},
	}
	f := &mongoFactory{cfg: cfg, uses: &userUse{}, netState: fake.state}
	rec, err := newStmtRecorder(filepath.Join(dir, "rec"), "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.f.Close() })
	s := &mongoSession{f: f, g: tunnel.Grant{Host: "db-01", Email: "ana@corp.example"}, host: "127.0.0.1", port: 27017,
		db: tunnel.GrantDB{Name: "app", Level: level, ChangesNeedApproval: approval}}
	return s, fake, rec, filepath.Join(dir, "rec", "s.argussql")
}

func mongoRecords(t *testing.T, path string) []tunnel.PGRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []tunnel.PGRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r tunnel.PGRecord
		_ = json.Unmarshal(sc.Bytes(), &r)
		out = append(out, r)
	}
	return out
}

func TestMongoNetworkSelectedNarrowsTheFirewallFirstAndRestarts(t *testing.T) {
	oldAddrs, oldSvc := hostAddresses, serviceOf
	hostAddresses = func() []RedisAddress { return nil }
	serviceOf = func(int, ...string) string { return "mongod.service" }
	t.Cleanup(func() { hostAddresses, serviceOf = oldAddrs, oldSvc })

	var log []string
	fw := &fakeRedisFirewall{active: true}
	s, fake, rec, recPath := mongoNetSession(t, tunnel.LevelDBA, false, fw, &log)
	ctx := context.Background()

	a, err := s.do(ctx, tunnel.StoreRequest{ID: 1, Op: tunnel.MongoOpNetwork}, rec)
	if err != nil || a.Type != "result" || a.Data.(MongoNetwork).Reach != tunnel.ReachLocal {
		t.Fatalf("view: %v %+v", err, a)
	}
	req := tunnel.StoreRequest{ID: 2, Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}, Restart: true}
	a, err = s.do(ctx, req, rec)
	if err != nil || a.Type != "result" {
		t.Fatalf("set: %v %+v", err, a)
	}
	if !slices.Equal(log, []string{"firewall selected", "restart mongod.service 127.0.0.1,10.0.0.183"}) {
		t.Fatalf("ran %v", log)
	}
	res := a.Data.(MongoNetworkResult)
	if !res.Restarted || res.Network.Reach != tunnel.ReachSelected || res.Network.PendingRestart {
		t.Fatalf("result %+v", res)
	}
	if fake.running != "127.0.0.1,10.0.0.183" {
		t.Fatalf("running %q", fake.running)
	}
	recs := mongoRecords(t, recPath)
	if len(recs) != 2 || recs[0].Kind != "stmt" || recs[0].SQL != tunnel.MongoNetworkText(req) || recs[1].Kind != "done" {
		t.Fatalf("records %+v", recs)
	}

	// Closing stops mongod listening before the firewall's rules go, and
	// without a restart it waits for one.
	log = nil
	a, _ = s.do(ctx, tunnel.StoreRequest{ID: 3, Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachLocal}, rec)
	if a.Type != "result" || !slices.Equal(log, []string{"firewall local"}) {
		t.Fatalf("close: %v %+v", log, a)
	}
	if n := a.Data.(MongoNetworkResult).Network; !n.PendingRestart || n.Next != "127.0.0.1" {
		t.Fatalf("after closing %+v", n)
	}
	b, _ := os.ReadFile(fake.conf)
	if string(b) != strings.Replace(debianMongodConf, "  port: 27017\n  bindIp: 127.0.0.1   # the loopback only\n", "  bindIp: 127.0.0.1\n  port: 27017\n", 1) {
		t.Fatalf("the file:\n%s", b)
	}
}

func TestMongoNetworkRefusals(t *testing.T) {
	oldAddrs := hostAddresses
	hostAddresses = func() []RedisAddress { return nil }
	t.Cleanup(func() { hostAddresses = oldAddrs })
	ctx := context.Background()
	var log []string
	sel := tunnel.StoreRequest{ID: 1, Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{"10.0.0.183"}, Sources: []string{"10.0.0.61"}}

	refused := func(s *mongoSession, rec *stmtRecorder, req tunnel.StoreRequest, want string) tunnel.StoreAnswer {
		t.Helper()
		a, err := s.do(ctx, req, rec)
		if err != nil || a.Type != "error" || !strings.Contains(a.Error, want) {
			t.Fatalf("want a refusal with %q, got %v %+v", want, err, a)
		}
		return a
	}
	// Below DBA, even reading.
	s, _, rec, _ := mongoNetSession(t, tunnel.LevelWrite, false, &fakeRedisFirewall{active: true}, &log)
	refused(s, rec, tunnel.StoreRequest{Op: tunnel.MongoOpNetwork}, "DBA work")
	// Another server's MongoDB.
	s, _, rec, _ = mongoNetSession(t, tunnel.LevelDBA, false, &fakeRedisFirewall{active: true}, &log)
	s.host = "10.0.0.9"
	refused(s, rec, sel, "another server")
	// No firewall, for selected machines.
	s, fake, rec, recPath := mongoNetSession(t, tunnel.LevelDBA, false, &fakeRedisFirewall{}, &log)
	refused(s, rec, sel, "no firewall is on")
	// Started with --bind_ip.
	s, fake, rec, recPath = mongoNetSession(t, tunnel.LevelDBA, false, &fakeRedisFirewall{active: true}, &log)
	fake.cmdLine = true
	refused(s, rec, sel, "--bind_ip")
	// Anywhere waits for approval, recorded as refused, and then needs the name.
	s, _, rec, recPath = mongoNetSession(t, tunnel.LevelDBA, false, &fakeRedisFirewall{active: true}, &log)
	anyReq := tunnel.StoreRequest{ID: 4, Op: tunnel.MongoOpNetworkSet, Reach: tunnel.ReachAny}
	a := refused(s, rec, anyReq, "approves")
	if a.NeedsApproval == nil || a.NeedsApproval.Text != tunnel.MongoNetworkText(anyReq) {
		t.Fatalf("needs approval %+v", a.NeedsApproval)
	}
	anyReq.Approval = "11111111-1111-1111-1111-111111111111"
	refused(s, rec, anyReq, "type the server's name")
	// A database whose changes need approval waits for selected machines too.
	s, _, rec, _ = mongoNetSession(t, tunnel.LevelDBA, true, &fakeRedisFirewall{active: true}, &log)
	refused(s, rec, sel, "approves")
	if len(log) != 0 {
		t.Fatalf("a refusal ran %v", log)
	}
	for _, r := range mongoRecords(t, recPath) {
		if r.Kind != "error" {
			t.Fatalf("a refusal recorded a change: %+v", r)
		}
	}
}
