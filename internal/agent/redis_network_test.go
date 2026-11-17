//go:build !windows

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// startRedisTCP runs a Redis as it is installed: listening on TCP on the
// loopback only, protected mode on, the default user with no password, and a
// config file it can rewrite. The agent signs in over its unix socket.
func startRedisTCP(t *testing.T) (redisServer, int) {
	t.Helper()
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is not on PATH")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dir := t.TempDir()
	sock := filepath.Join(dir, "redis.sock")
	conf := filepath.Join(dir, "redis.conf")
	if err := os.WriteFile(conf, []byte("port "+strconv.Itoa(port)+"\nbind 127.0.0.1 -::1\nprotected-mode yes\n"+
		"unixsocket "+sock+"\nsave \"\"\nappendonly no\ndir "+dir+"\n"), 0o600); err != nil {
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
			return redisServer{sock: sock, conf: conf}, port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("redis-server did not start")
	return redisServer{}, 0
}

// lanIP is a non-loopback address of this machine, standing in for its LAN
// address, or "" when it has none.
func lanIP() string {
	ifs, _ := net.InterfaceAddrs()
	for _, a := range ifs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func TestRedisUserSignsInFromTheLANOnceOpened(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("this machine has no address besides the loopback")
	}
	// Whatever kind of address this machine has, it stands in for a LAN one.
	oldAddrs, oldLAN := hostAddresses, lanAddress
	hostAddresses = func() []RedisAddress {
		return []RedisAddress{{IP: ip, Iface: "eth0", Prefix: ip + "/24", Private: true}}
	}
	lanAddress = func(a string) string {
		if a == ip {
			return ""
		}
		return tunnel.ValidRedisLANAddress(a)
	}
	t.Cleanup(func() { hostAddresses, lanAddress = oldAddrs, oldLAN })

	srv, port := startRedisTCP(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	d := openRedis(t, srv, svc, tunnel.LevelDBA, dir+"/rec")

	const pw = "app-pass-7741-lan"
	sum := sha256.Sum256([]byte(pw))
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "checkout-api", Create: true, State: "on",
		AddHashes: []string{hex.EncodeToString(sum[:])}, Rules: []string{"~cart:*", "+@read", "+@write", "+@connection"}}); a.Type != "result" {
		t.Fatalf("make user: %+v", a)
	}

	login := func(password string) RedisLoginResult {
		t.Helper()
		var r RedisLoginResult
		a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLLogin, User: "checkout-api", Password: password})
		if a.Type != "result" {
			t.Fatalf("login check: %+v", a)
		}
		data(t, a, &r)
		if len(r.Logins) != 2 || r.Port != port {
			t.Fatalf("login check covers the loopback and the LAN address: %+v", r)
		}
		return r
	}

	// As installed: the user works on this machine, and nothing answers on the LAN.
	r := login(pw)
	if !r.Logins[0].OK || r.Logins[1].OK || !strings.Contains(r.Logins[1].Detail, "does not listen") {
		t.Fatalf("before opening: %+v", r.Logins)
	}
	var n RedisNetwork
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetwork}), &n)
	if n.Port != port || !n.DefaultOpen || !n.ProtectedMode || !n.AgentConf || n.Addresses[0].State != "closed" {
		t.Fatalf("network as installed: %+v", n)
	}

	// Opening an open default to the LAN is refused, and so is a public address.
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Addresses: []string{ip}}); a.Type != "error" ||
		!strings.Contains(a.Error, "default user has no password") {
		t.Fatalf("opened with the default user open: %+v", a)
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Addresses: []string{"8.8.8.8"}}); a.Type != "error" {
		t.Fatalf("a public address was accepted: %+v", a)
	}

	// The default user gets a password made here, in redis.conf, and the agent still signs in.
	var sec RedisSecureResult
	a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpDefaultSecure})
	if a.Type != "result" {
		t.Fatalf("secure default: %+v", a)
	}
	data(t, a, &sec)
	if sec.Network.DefaultOpen || sec.SavedTo != srv.conf {
		t.Fatalf("after securing: %+v", sec)
	}
	if got, _, _ := readRedisConf(srv.conf); len(got) < 32 {
		t.Fatalf("redis.conf has no requirepass: %q", got)
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Addresses: []string{ip}}); a.Type != "result" {
		t.Fatalf("open to the LAN: %+v", a)
	}
	b, _ := os.ReadFile(srv.conf)
	if !strings.Contains(string(b), "-"+ip) || !strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("redis.conf bind after opening:\n%s", b)
	}
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetwork}), &n)
	if n.Addresses[0].State != "signin" {
		t.Fatalf("the LAN address asks for a sign-in: %+v", n.Addresses)
	}

	// Now the user signs in on the LAN address too; a wrong password does not.
	r = login(pw)
	if !r.Logins[0].OK || !r.Logins[1].OK {
		t.Fatalf("after opening: %+v", r.Logins)
	}
	r = login("not-the-password")
	if r.Logins[1].OK || !strings.Contains(r.Logins[1].Detail, "wrong password") {
		t.Fatalf("wrong password: %+v", r.Logins)
	}

	// A real client on the LAN address, as an application would connect.
	c, err := dialRedis(context.Background(), ip, port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	lim := respLimit{items: 4, budget: 1 << 12}
	if v, err := c.do(context.Background(), 5*time.Second, lim, "AUTH", "checkout-api", pw); err != nil || v.T == "e" {
		t.Fatalf("auth from the LAN: %v %+v", err, v)
	}
	if v, _ := c.do(context.Background(), 5*time.Second, lim, "SET", "cart:1", "x"); v.T == "e" {
		t.Fatalf("its rules apply: %+v", v)
	}
	if v, _ := c.do(context.Background(), 5*time.Second, lim, "SET", "other:1", "x"); v.T != "e" {
		t.Fatalf("a key outside its rules was written: %+v", v)
	}

	// The checks are recorded by user, never with the password.
	if !d.recorded("stmt", "ACL LOGIN CHECK checkout-api") {
		t.Fatal("the sign-in check is recorded")
	}
	d.close()
	all, _ := json.Marshal(d.rec)
	if strings.Contains(string(all), pw) {
		t.Fatal("the password reached the recording")
	}
}

// fakeRedisFirewall is the server's firewall as the Redis session sees it.
func TestRedisReachSelectedMachinesWritesBindAndFirewall(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("this machine has no address besides the loopback")
	}
	oldAddrs, oldLAN := hostAddresses, lanAddress
	hostAddresses = func() []RedisAddress {
		return []RedisAddress{{IP: ip, Iface: "eth0", Prefix: ip + "/24", Private: true}}
	}
	lanAddress = func(a string) string {
		if a == ip {
			return ""
		}
		return tunnel.ValidRedisLANAddress(a)
	}
	t.Cleanup(func() { hostAddresses, lanAddress = oldAddrs, oldLAN })

	srv, port := startRedisTCP(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	fw := &fakeRedisFirewall{}
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir + "/rec",
		UndoDir: dir + "/undo", Firewall: fw.do})
	d := openRedis(t, srv, svc, tunnel.LevelDBA, dir+"/rec")
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpDefaultSecure}); a.Type != "result" {
		t.Fatalf("secure default: %+v", a)
	}
	var n RedisNetwork
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetwork}), &n)
	if n.Reach != tunnel.ReachLocal || n.Host != "cache-01" {
		t.Fatalf("as installed: reach %q host %q", n.Reach, n.Host)
	}

	sel := tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Reach: tunnel.ReachSelected,
		Addresses: []string{ip}, Sources: []string{"10.0.0.61", "10.0.0.50"}}

	// No firewall on: selected machines cannot be enforced, so nothing changes.
	if a := d.ask(sel); a.Type != "error" || !strings.Contains(a.Error, "no firewall is on") {
		t.Fatalf("selected with no firewall: %+v", a)
	}
	if b, _ := os.ReadFile(srv.conf); strings.Contains(string(b), "-"+ip) {
		t.Fatal("bind changed although the change was refused")
	}

	// A rule Argus did not add already lets everyone in: refused too.
	fw.active, fw.others = true, []string{"6379/tcp ALLOW IN Anywhere"}
	if a := d.ask(sel); a.Type != "error" || !strings.Contains(a.Error, "did not add") {
		t.Fatalf("selected under an open rule: %+v", a)
	}
	fw.others = nil

	a := d.ask(sel)
	if a.Type != "result" {
		t.Fatalf("selected: %+v", a)
	}
	var res RedisNetworkResult
	data(t, a, &res)
	if res.Network.Reach != tunnel.ReachSelected || len(res.Network.FirewallSources) != 2 {
		t.Fatalf("after: %+v", res.Network)
	}
	last := fw.asked[len(fw.asked)-2] // the change; the view's check follows it
	if last.Check || last.Port != port || last.Reach != tunnel.ReachSelected || strings.Join(last.Sources, " ") != "10.0.0.61 10.0.0.50" {
		t.Fatalf("firewall asked %+v", last)
	}
	if b, _ := os.ReadFile(srv.conf); !strings.Contains(string(b), "-"+ip) {
		t.Fatalf("redis.conf bind:\n%s", b)
	}
	if !d.recordedHas("stmt", "CONFIG REWRITE\nFIREWALL ALLOW 10.0.0.61 10.0.0.50\nFIREWALL DENY OTHERS") {
		t.Fatal("the firewall change is recorded with the bind")
	}

	// Back to this server only: bind first, then Argus's rules go.
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Reach: tunnel.ReachLocal}); a.Type != "result" {
		t.Fatalf("local: %+v", a)
	}
	data(t, d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpNetwork}), &n)
	if n.Reach != tunnel.ReachLocal || len(fw.sources) != 0 {
		t.Fatalf("after closing: %+v firewall %v", n, fw.sources)
	}
	d.close()
}

func TestRedisReachAnywhereNeedsNameApprovalAndPasswords(t *testing.T) {
	srv, _ := startRedisTCP(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	fw := &fakeRedisFirewall{active: true}
	svc := Redis(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir + "/rec",
		UndoDir: dir + "/undo", Firewall: fw.do})
	// A user anyone could sign in as, made on the server.
	if c, err := dialRedis(context.Background(), srv.sock, 0); err != nil {
		t.Fatal(err)
	} else {
		if v, err := c.do(context.Background(), 5*time.Second, respLimit{items: 4, budget: 1 << 12},
			"ACL", "SETUSER", "reports", "on", "nopass", "~report:*", "+@read"); err != nil || v.T == "e" {
			t.Fatalf("make reports: %v %+v", err, v)
		}
		c.close()
	}
	d := openRedis(t, srv, svc, tunnel.LevelDBA, dir+"/rec")
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpDefaultSecure}); a.Type != "result" {
		t.Fatalf("secure default: %+v", a)
	}
	anyReq := tunnel.StoreRequest{Op: tunnel.RedisOpNetworkSet, Reach: tunnel.ReachAny}

	// Always a second person, even on an entry whose changes need no approval.
	a := d.ask(anyReq)
	if a.Type != "error" || a.NeedsApproval == nil || !strings.Contains(a.NeedsApproval.Text, `CONFIG SET bind "* -::*"`) ||
		!strings.Contains(a.NeedsApproval.Text, "FIREWALL ALLOW ANY") {
		t.Fatalf("anywhere without approval: %+v", a)
	}
	anyReq.Approval = "ap-1"
	// Then the server's name, typed.
	if a := d.ask(anyReq); a.Type != "error" || !a.NeedsConfirm {
		t.Fatalf("anywhere without the name: %+v", a)
	}
	anyReq.Confirm = "CACHE-01"
	// Then no user may be without a password.
	if a := d.ask(anyReq); a.Type != "error" || !strings.Contains(a.Error, "reports needs no password") {
		t.Fatalf("anywhere with a user that has no password: %+v", a)
	}
	if b, _ := os.ReadFile(srv.conf); strings.Contains(string(b), "bind *") {
		t.Fatal("bind changed although the change was refused")
	}
	if a := d.ask(tunnel.StoreRequest{Op: tunnel.RedisOpACLSet, User: "reports", State: "off"}); a.Type != "result" {
		t.Fatalf("turn reports off: %+v", a)
	}
	a = d.ask(anyReq)
	if a.Type != "result" {
		t.Fatalf("anywhere: %+v", a)
	}
	var res RedisNetworkResult
	data(t, a, &res)
	if res.Network.Reach != tunnel.ReachAny || !fw.all || !res.Network.ProtectedMode {
		t.Fatalf("after: reach %q firewall all %v protected %v", res.Network.Reach, fw.all, res.Network.ProtectedMode)
	}
	d.close()
}

// recordedHas says a record of kind holds text.
func (h *storeHarness) recordedHas(kind, text string) bool {
	for _, r := range h.rec {
		if r.Kind == kind && strings.Contains(r.SQL, text) {
			return true
		}
	}
	return false
}

func TestReachCheckSaysWhatAClientGets(t *testing.T) {
	srv, port := startRedisTCP(t)
	_ = srv
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	// Default user with no password: protected mode lets the loopback in.
	r := reach(context.Background(), dial, tunnel.GrantReach{Host: "127.0.0.1", Port: port, Engine: tunnel.EngineRedis})
	if !r.Connected || r.Answer != "open" {
		t.Fatalf("loopback: %+v", r)
	}
	// Nothing listening: the error Hermod saw, named.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	r = reach(context.Background(), dial, tunnel.GrantReach{Host: "127.0.0.1", Port: closed, Engine: tunnel.EngineRedis})
	if r.Connected || !strings.Contains(r.Detail, "connection refused") {
		t.Fatalf("closed port: %+v", r)
	}
	for _, g := range []tunnel.GrantReach{{Host: "cache-01", Port: 6379}, {Host: "0.0.0.0", Port: 6379}, {Host: "10.0.0.1", Port: 0}} {
		if g.Validate() == nil {
			t.Errorf("%+v was accepted", g)
		}
	}
}
