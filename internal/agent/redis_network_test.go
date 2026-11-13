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
