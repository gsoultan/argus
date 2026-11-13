package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Reaching a Redis from other machines on the LAN, and proving a user's
password works there.

Two things stop a client on the LAN even when its user and password are
right, and both are Redis's defaults:

  - bind 127.0.0.1 -::1: Redis listens on the loopback only, so a client
    elsewhere is refused before it can say anything;
  - protected mode: while the default user has no password, Redis answers
    DENIED to every connection not from the loopback, whatever user it
    signs in as.

network says which of these applies, address by address, by connecting to
each of this server's addresses the way a client would. network_set adds
LAN addresses to bind (private ones only, loopback always kept, each one
optional so Redis still starts if it goes) and refuses while the default
user is open. default_secure closes that: it gives the default user a
password made here, written to redis.conf where the agent reads its own
sign-in from, so the password never leaves this server.

acl_login is the proof: it signs in as a user with the password the person
typed, on the loopback and on each LAN address, and says which worked. The
password is used for that and dropped; the recording says only who was
checked.

A firewall on this server is the one thing a connection from here cannot
see past, since it comes from this machine; the console says so and gives
the command for the firewall it finds.
*/

// RedisAddress is one of this server's addresses, and what a client there
// gets from Redis.
type RedisAddress struct {
	IP    string `json:"ip"`
	Iface string `json:"iface"`
	// Prefix is the address's network, as CIDR: the LAN a firewall rule opens to.
	Prefix  string `json:"prefix"`
	Private bool   `json:"private"`
	// State: "signin" (Redis answers and asks for a sign-in), "open" (it
	// answers with no sign-in at all), "protected" (protected mode refuses
	// it), "closed" (nothing listens there).
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// RedisNetwork is how Redis can be reached from the network.
type RedisNetwork struct {
	Port          int      `json:"port"`
	TLSPort       int      `json:"tlsPort,omitempty"`
	Bind          []string `json:"bind"`
	ProtectedMode bool     `json:"protectedMode"`
	DefaultOpen   bool     `json:"defaultOpen"`
	// DefaultClients are connections signed in as default now: what giving
	// it a password would cut off at their next connection.
	DefaultClients int    `json:"defaultClients"`
	ConfigFile     string `json:"configFile"`
	// AgentConf says the agent reads its sign-in from the file Redis
	// rewrites, which default_secure needs.
	AgentConf bool           `json:"agentConf"`
	Addresses []RedisAddress `json:"addresses"`
	// Firewall is the firewall found on this server: "ufw", "firewalld" or "".
	Firewall string `json:"firewall"`
}

// RedisLogin is how signing in went on one address.
type RedisLogin struct {
	Address string `json:"address"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// RedisLoginResult is a sign-in check.
type RedisLoginResult struct {
	User   string       `json:"user"`
	Port   int          `json:"port"`
	Logins []RedisLogin `json:"logins"`
}

// hostAddresses lists this server's addresses, without the loopback and
// link-local ones nobody on a LAN uses.
var hostAddresses = func() []RedisAddress {
	out := []RedisAddress{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsMulticast() {
				continue
			}
			pre := &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}
			out = append(out, RedisAddress{IP: ipn.IP.String(), Iface: ifc.Name, Prefix: pre.String(), Private: ipn.IP.IsPrivate()})
		}
	}
	return out
}

// lanAddress says why an address may not be opened to the LAN; a variable
// so a test on a machine with no private address can stand one in.
var lanAddress = tunnel.ValidRedisLANAddress

// probe connects to ip:port as a client with no password would, and says
// what Redis answers.
func probeRedis(ctx context.Context, ip string, port int) (state, detail string) {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := dialRedis(pctx, ip, port)
	if err != nil {
		return "closed", "nothing answers on " + net.JoinHostPort(ip, strconv.Itoa(port))
	}
	defer c.close()
	v, err := c.do(pctx, 3*time.Second, respLimit{items: 4, budget: 1 << 12}, "PING")
	switch {
	case err != nil && strings.Contains(strings.ToUpper(err.Error()), "DENIED"):
		return "protected", "protected mode: Redis refuses clients off this machine while the default user has no password"
	case err != nil:
		return "closed", err.Error()
	case v.T == "e" && strings.HasPrefix(v.V, "DENIED"):
		return "protected", "protected mode: Redis refuses clients off this machine while the default user has no password"
	case v.T == "e" && strings.HasPrefix(v.V, "NOAUTH"):
		return "signin", ""
	case v.T == "e":
		return "closed", v.V
	}
	return "open", "answers with no sign-in: anyone who reaches this address can use Redis as the default user"
}

func configInt(ctx context.Context, c *respConn, name string) int {
	v, err := adminDo(ctx, c, "CONFIG", "GET", name)
	if err != nil || len(v.A) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(v.A[1].V)
	return n
}

func configStr(ctx context.Context, c *respConn, name string) string {
	v, err := adminDo(ctx, c, "CONFIG", "GET", name)
	if err != nil || len(v.A) != 2 {
		return ""
	}
	return v.A[1].V
}

func defaultOpen(ctx context.Context, c *respConn) bool {
	u, err := adminDo(ctx, c, "ACL", "GETUSER", "default")
	if err != nil {
		return false
	}
	on, nopass := false, false
	for _, x := range aclField(u, "flags").A {
		on = on || x.V == "on"
		nopass = nopass || x.V == "nopass"
	}
	return on && nopass
}

// firewallFound is the firewall that is on, from its own files.
var firewallFound = func() string {
	if b, err := os.ReadFile("/etc/ufw/ufw.conf"); err == nil && strings.Contains(string(b), "ENABLED=yes") {
		return "ufw"
	}
	if _, err := os.Stat("/var/run/firewalld/firewalld.pid"); err == nil {
		return "firewalld"
	}
	return ""
}

func (s *redisSession) network(ctx context.Context) (tunnel.StoreAnswer, error) {
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	n, err := s.networkView(ctx, c)
	if err != nil {
		return errAnswer("%s", err)
	}
	return tunnel.StoreAnswer{Type: "result", Data: n}, nil
}

func (s *redisSession) networkView(ctx context.Context, c *respConn) (RedisNetwork, error) {
	n := RedisNetwork{Bind: strings.Fields(configStr(ctx, c, "bind")), Addresses: hostAddresses(), Firewall: firewallFound()}
	n.Port = configInt(ctx, c, "port")
	n.TLSPort = configInt(ctx, c, "tls-port")
	n.ProtectedMode = configStr(ctx, c, "protected-mode") == "yes"
	n.DefaultOpen = defaultOpen(ctx, c)
	if v, err := adminDo(ctx, c, "INFO", "server"); err == nil {
		n.ConfigFile = parseInfo(v.V)["config_file"]
	}
	n.AgentConf = sameFile(n.ConfigFile, s.conf)
	if v, err := adminDo(ctx, c, "CLIENT", "LIST"); err == nil {
		for _, line := range strings.Split(v.V, "\n") {
			if strings.Contains(" "+line+" ", " user=default ") {
				n.DefaultClients++
			}
		}
	}
	if n.Port == 0 {
		return n, errors.New("Redis listens on no TCP port (port 0), so nothing on the network can reach it")
	}
	for i := range n.Addresses {
		n.Addresses[i].State, n.Addresses[i].Detail = probeRedis(ctx, n.Addresses[i].IP, n.Port)
	}
	return n, nil
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// RedisNetworkResult is how a network change went, and the network after it.
type RedisNetworkResult struct {
	Network      RedisNetwork `json:"network"`
	Rewritten    bool         `json:"rewritten"`
	RewriteError string       `json:"rewriteError,omitempty"`
}

func (s *redisSession) networkSet(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if len(req.Addresses) > 8 {
		return errAnswer("at most 8 addresses")
	}
	mine := map[string]bool{}
	for _, a := range hostAddresses() {
		mine[a.IP] = true
	}
	seen := map[string]bool{}
	for _, a := range req.Addresses {
		if why := lanAddress(a); why != "" {
			return errAnswer("%s", why)
		}
		if !mine[a] {
			return errAnswer("%s is not an address of this server", a)
		}
		if seen[a] {
			return errAnswer("%s is listed twice", a)
		}
		seen[a] = true
	}
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	if len(req.Addresses) > 0 && defaultOpen(ctx, c) {
		return errAnswer("the default user has no password, so opening Redis to the LAN would let anyone there use it " +
			"(and protected mode refuses them anyway); give the default user a password first")
	}
	a, _ := tunnel.RedisApproval(req)
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: a.Text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	if _, err := adminDo(ctx, c, "CONFIG", "SET", "bind", tunnel.RedisBind(req.Addresses)); err != nil {
		if rerr := rec.fail(req.ID, err.Error(), time.Since(start).Milliseconds()); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("Redis refused the new bind: %s", err)
	}
	res := RedisNetworkResult{Rewritten: true}
	if _, err := adminDo(ctx, c, "CONFIG", "REWRITE"); err != nil {
		res.Rewritten, res.RewriteError = false, err.Error()
	}
	if err := rec.done(req.ID, "config set bind", 1, time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	res.Network, _ = s.networkView(ctx, c)
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

// RedisSecureResult is how giving the default user a password went.
type RedisSecureResult struct {
	Network RedisNetwork `json:"network"`
	// SavedTo is the file the password is in, for whoever needs it on the server.
	SavedTo string `json:"savedTo"`
}

func (s *redisSession) defaultSecure(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	defer c.close()
	if !defaultOpen(ctx, c) {
		return errAnswer("the default user already needs a password, or is off")
	}
	cf := ""
	if v, err := adminDo(ctx, c, "INFO", "server"); err == nil {
		cf = parseInfo(v.V)["config_file"]
	}
	if cf == "" {
		return errAnswer("Redis was started without a config file, so a password set now would be lost at a restart; " +
			"set requirepass in its config file on the server")
	}
	if !sameFile(cf, s.conf) {
		return errAnswer("Redis rewrites %s but the agent signs in with %s; set requirepass on the server, "+
			"or point this database's config file at %s", cf, s.conf, cf)
	}
	pw, err := sessionPassword()
	if err != nil {
		return errAnswer("%s", err)
	}
	a, _ := tunnel.RedisApproval(req)
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: a.Text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	fail := func(why string) (tunnel.StoreAnswer, error) {
		if rerr := rec.fail(req.ID, why, time.Since(start).Milliseconds()); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return errAnswer("%s", why)
	}
	// Put back no password, so the agent can still sign in, when the file
	// does not end up holding the new one.
	undo := func() {
		uctx := context.WithoutCancel(ctx)
		_, _ = adminDo(uctx, c, "CONFIG", "SET", "requirepass", "")
		_, _ = adminDo(uctx, c, "ACL", "SETUSER", "default", "nopass")
		_, _ = adminDo(uctx, c, "CONFIG", "REWRITE")
	}
	if _, err := adminDo(ctx, c, "CONFIG", "SET", "requirepass", pw); err != nil {
		return fail("Redis refused the password: " + err.Error())
	}
	if _, err := adminDo(ctx, c, "CONFIG", "REWRITE"); err != nil {
		undo()
		return fail("writing " + cf + " failed, so nothing was changed: " + err.Error())
	}
	if acl := configStr(ctx, c, "aclfile"); acl != "" {
		if _, err := adminDo(ctx, c, "ACL", "SAVE"); err != nil {
			undo()
			return fail("saving the ACL file failed, so nothing was changed: " + err.Error())
		}
	}
	if got, _, err := readRedisConf(s.conf); err != nil || got != pw {
		undo()
		return fail("the agent could not read the new password back from " + s.conf + ", so nothing was changed")
	}
	if err := rec.done(req.ID, "requirepass set", 1, time.Since(start).Milliseconds(), nil, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	res := RedisSecureResult{SavedTo: cf}
	res.Network, _ = s.networkView(ctx, c)
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

// aclLogin signs in as a user on the loopback and on each of this server's
// addresses Redis answers on, the way an application there would.
func (s *redisSession) aclLogin(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	user, pw := req.User, req.Password
	if user != "default" && !tunnel.ValidRedisUserName(user) {
		return errAnswer("not a user Argus checks: %s", user)
	}
	if pw == "" || len(pw) > tunnel.MaxRedisPassword {
		return errAnswer("type the password to check, up to %d characters", tunnel.MaxRedisPassword)
	}
	c, err := s.admin(ctx)
	if err != nil {
		return errAnswer("%s", err)
	}
	port := configInt(ctx, c, "port")
	c.close()
	if port == 0 {
		return errAnswer("Redis listens on no TCP port")
	}
	// The recording names who was checked, never the password.
	if err := rec.stmt(req.ID, "ACL LOGIN CHECK "+user); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	targets := []string{"127.0.0.1"}
	for _, a := range hostAddresses() {
		targets = append(targets, a.IP)
	}
	res := RedisLoginResult{User: user, Port: port, Logins: []RedisLogin{}}
	for _, ip := range targets {
		res.Logins = append(res.Logins, tryLogin(ctx, ip, port, user, pw))
	}
	ok := slices.ContainsFunc(res.Logins, func(l RedisLogin) bool { return l.OK })
	tag := "login refused"
	if ok {
		tag = "login ok"
	}
	if err := rec.done(req.ID, tag, int64(len(res.Logins)), time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

func tryLogin(ctx context.Context, ip string, port int, user, pw string) RedisLogin {
	out := RedisLogin{Address: net.JoinHostPort(ip, strconv.Itoa(port))}
	lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := dialRedis(lctx, ip, port)
	if err != nil {
		out.Detail = "nothing answers here: Redis does not listen on this address"
		return out
	}
	defer c.close()
	lim := respLimit{items: 4, budget: 1 << 12}
	v, err := c.do(lctx, 4*time.Second, lim, "AUTH", user, pw)
	switch {
	case err != nil:
		out.Detail = loginWhy(err.Error())
		return out
	case v.T == "e":
		out.Detail = loginWhy(v.V)
		return out
	}
	if v, err := c.do(lctx, 4*time.Second, lim, "PING"); err != nil || v.T == "e" {
		why := v.V
		if err != nil {
			why = err.Error()
		}
		out.Detail = "signed in, but PING was refused: " + why
		return out
	}
	out.OK = true
	return out
}

func loginWhy(e string) string {
	switch {
	case strings.Contains(e, "DENIED"):
		return "protected mode refuses clients off this machine while the default user has no password"
	case strings.Contains(e, "WRONGPASS"):
		return "wrong password, or the user is off or does not exist"
	}
	return e
}
