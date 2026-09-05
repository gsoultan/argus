package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

/*
End-to-end forwarding.

The unit tests cover the relay, the listener bookkeeping and the policy
predicates in isolation. None of them proves that `ssh -L` through a running
argus-gateway actually reaches anything — and a protocol relay is exactly the
kind of code that passes its unit tests and then hangs on the wire.

So this stands up the real thing: an echo server, a real SSH target that can
open direct-tcpip channels, a real gateway built by NewServer, and a real
ssh.Client dialling it as `principal:target`. Nothing is stubbed on the path a
forwarded byte takes.
*/

/* ── Fixtures ────────────────────────────────────────────────────────────── */

func writeKeyPair(t *testing.T, dir, name, comment string) (privPath string, pub ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	privPath = filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(privPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	_ = comment
	return privPath, signer.PublicKey()
}

func signerFromFile(t *testing.T, path string) ssh.Signer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	s, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	return s
}

// echoServer is whatever sits behind the target — a database, an admin UI, the
// thing an operator is actually reaching for when they type -L.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// targetSSHServer is the host Argus brokers to. It supports exactly what this
// test needs from a real sshd: key auth, a session channel, and direct-tcpip.
func targetSSHServer(t *testing.T, authorizedFP string) string {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("target host signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if ssh.FingerprintSHA256(key) != authorizedFP {
				return nil, fmt.Errorf("unknown key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTarget(raw, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveTarget(raw net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		_ = raw.Close()
		return
	}
	defer conn.Close()
	go serveTargetGlobalRequests(conn, reqs)

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "direct-tcpip":
			// The behaviour under test: the target dials, not the gateway.
			var req directTCPIP
			if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
				_ = newChan.Reject(ssh.ConnectionFailed, "bad payload")
				continue
			}
			dest := net.JoinHostPort(req.DestHost, strconv.FormatUint(uint64(req.DestPort), 10))
			out, err := net.Dial("tcp", dest)
			if err != nil {
				_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, chReqs, err := newChan.Accept()
			if err != nil {
				_ = out.Close()
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go func() {
				defer out.Close()
				defer ch.Close()
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _, _ = io.Copy(out, ch); closeWrite(out) }()
				go func() { defer wg.Done(); _, _ = io.Copy(ch, out); _ = ch.CloseWrite() }()
				wg.Wait()
			}()
		case "session":
			ch, chReqs, err := newChan.Accept()
			if err != nil {
				continue
			}
			go func() {
				for r := range chReqs {
					if r.WantReply {
						_ = r.Reply(true, nil)
					}
				}
			}()
			go func() { _, _ = io.Copy(io.Discard, ch); _ = ch.Close() }()
		default:
			_ = newChan.Reject(ssh.Prohibited, "not supported")
		}
	}
}

// serveTargetGlobalRequests gives the test target real remote-forwarding.
//
// x/crypto/ssh does not implement the server half of tcpip-forward, so without
// this the target would refuse the request the gateway makes on a client's
// behalf and -R could not be tested end to end at all.
func serveTargetGlobalRequests(conn ssh.Conn, reqs <-chan *ssh.Request) {
	// A real sshd releases the port on cancel-tcpip-forward. Without that here
	// the cancel test would fail against a correct gateway, which would send
	// someone hunting a bug in the wrong place.
	var mu sync.Mutex
	listeners := map[string]net.Listener{}
	defer func() {
		mu.Lock()
		for _, l := range listeners {
			_ = l.Close()
		}
		mu.Unlock()
	}()

	for req := range reqs {
		if req.Type == "cancel-tcpip-forward" {
			var in tcpipForward
			ok := false
			if err := ssh.Unmarshal(req.Payload, &in); err == nil {
				key := net.JoinHostPort(in.BindAddr, strconv.FormatUint(uint64(in.BindPort), 10))
				mu.Lock()
				if l, exists := listeners[key]; exists {
					_ = l.Close()
					delete(listeners, key)
					ok = true
				}
				mu.Unlock()
			}
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
			continue
		}
		if req.Type != "tcpip-forward" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var in tcpipForward
		if err := ssh.Unmarshal(req.Payload, &in); err != nil {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		ln, err := net.Listen("tcp",
			net.JoinHostPort(in.BindAddr, strconv.FormatUint(uint64(in.BindPort), 10)))
		if err != nil {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		bound := uint32(ln.Addr().(*net.TCPAddr).Port)
		mu.Lock()
		listeners[net.JoinHostPort(in.BindAddr, strconv.FormatUint(uint64(bound), 10))] = ln
		mu.Unlock()
		if req.WantReply {
			// A caller that asked for port 0 learns its port from this reply
			// and nowhere else.
			var payload []byte
			if in.BindPort == 0 {
				payload = ssh.Marshal(struct{ Port uint32 }{bound})
			}
			_ = req.Reply(true, payload)
		}
		go func() {
			defer ln.Close()
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					originHost, originPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
					originPort, _ := strconv.ParseUint(originPortStr, 10, 32)
					ch, chReqs, err := conn.OpenChannel("forwarded-tcpip",
						ssh.Marshal(forwardedTCPIP{
							ConnectedHost: in.BindAddr,
							ConnectedPort: bound,
							OriginHost:    originHost,
							OriginPort:    uint32(originPort),
						}))
					if err != nil {
						return
					}
					go ssh.DiscardRequests(chReqs)
					defer ch.Close()
					var wg sync.WaitGroup
					wg.Add(2)
					go func() { defer wg.Done(); _, _ = io.Copy(ch, c); _ = ch.CloseWrite() }()
					go func() { defer wg.Done(); _, _ = io.Copy(c, ch); closeWrite(c) }()
					wg.Wait()
				}()
			}
		}()
	}
}

// startGateway builds a real gateway in front of a real target.
func startGateway(t *testing.T, policy *PolicyHolder) (addr string, clientSigner ssh.Signer) {
	t.Helper()
	dir := t.TempDir()

	// The gateway's own host key, and the key it injects to reach the target.
	hostKeyPath, _ := writeKeyPair(t, dir, "gateway_host_key", "")
	injectedPath, injectedPub := writeKeyPair(t, dir, "injected", "")
	userKeyPath, userPub := writeKeyPair(t, dir, "user", "")

	// authorized_keys entries must carry a comment: it is the identity every
	// audit event is attributed to, so an unlabelled key is refused.
	authorized := filepath.Join(dir, "authorized_keys")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(userPub))) + " dewi.p@northwind.id\n"
	if err := os.WriteFile(authorized, []byte(line), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	targetAddr := targetSSHServer(t, ssh.FingerprintSHA256(injectedPub))
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	srv, err := NewServer(Config{
		Listen:             "127.0.0.1:0",
		HostKeyPath:        hostKeyPath,
		AuthorizedKeysPath: authorized,
		RecordingDir:       dir,
		HostKeys:           mustHostKeys(t),
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		Policy:             policy,
		Inventory: &Inventory{assets: map[string]Asset{
			"pay-01": {
				Hostname:   "pay-01",
				Address:    host,
				Port:       port,
				Principals: []string{"ops"},
				KeyPath:    injectedPath,
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Listen binds cfg.Listen itself and blocks, so the bound address is only
	// knowable afterwards. Same package, so the listener is readable directly
	// rather than widening the production API for a test's benefit.
	go func() { _ = srv.Listen() }()
	t.Cleanup(func() { _ = srv.Close() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		if addr := srv.Addr(); addr != nil {
			return addr.String(), signerFromFile(t, userKeyPath)
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never bound a listener")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialGateway(t *testing.T, addr string, signer ssh.Signer) *ssh.Client {
	t.Helper()
	cli, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		// The target is encoded in the username, which is how Argus avoids
		// requiring a wrapper script.
		User:            "ops:pay-01",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

/* ── Tests ───────────────────────────────────────────────────────────────── */

// The feature, end to end: bytes typed into a -L tunnel come back from a
// service only the target can see.
func TestLocalForwardReachesTheTargetsNetwork(t *testing.T) {
	echo := echoServer(t)

	policy := NewPolicyHolder()
	policy.Set(Policy{AllowLocalForward: true, ProxySftpSubsystem: true})

	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	conn, err := cli.Dial("tcp", echo)
	if err != nil {
		t.Fatalf("open forward: %v", err)
	}
	defer conn.Close()

	msg := []byte("SELECT 1;\n")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read back through tunnel: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("tunnel returned %q, want %q", buf, msg)
	}
}

// The default. A gateway whose policy nobody has loosened must refuse -L, and
// this is the assertion that would fail if the default ever inverted.
func TestLocalForwardIsRefusedByDefault(t *testing.T) {
	echo := echoServer(t)
	addr, signer := startGateway(t, NewPolicyHolder()) // closed
	cli := dialGateway(t, addr, signer)

	_, err := cli.Dial("tcp", echo)
	if err == nil {
		t.Fatal("a closed policy must refuse local port forwarding")
	}
	// The refusal has to say why. "administratively prohibited" sends an
	// operator to read the policy; a bare failure sends them to debug their
	// own network.
	if !strings.Contains(strings.ToLower(err.Error()), "policy") &&
		!strings.Contains(strings.ToLower(err.Error()), "prohibit") {
		t.Errorf("refusal should name policy, got: %v", err)
	}
}

// A gateway with no policy configured at all behaves like a closed one. This is
// the path a deployment takes before its control plane is reachable.
func TestNilPolicyRefusesForwarding(t *testing.T) {
	echo := echoServer(t)
	addr, signer := startGateway(t, nil)
	cli := dialGateway(t, addr, signer)

	if _, err := cli.Dial("tcp", echo); err == nil {
		t.Fatal("a gateway with no policy must refuse forwarding")
	}
}

// Forwarding is per-connection, so opening several must not interfere — the
// bug this guards against is shared relay state between tunnels.
func TestConcurrentLocalForwardsAreIndependent(t *testing.T) {
	echo := echoServer(t)
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowLocalForward: true})
	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := cli.Dial("tcp", echo)
			if err != nil {
				errs <- fmt.Errorf("dial %d: %w", i, err)
				return
			}
			defer conn.Close()
			want := fmt.Sprintf("tunnel-%d\n", i)
			if _, err := conn.Write([]byte(want)); err != nil {
				errs <- fmt.Errorf("write %d: %w", i, err)
				return
			}
			buf := make([]byte, len(want))
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- fmt.Errorf("read %d: %w", i, err)
				return
			}
			// Each tunnel must get its own bytes back, not another's.
			if string(buf) != want {
				errs <- fmt.Errorf("tunnel %d got %q, want %q", i, buf, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Closing the client end must release the tunnel rather than leaving it open
// until the session ends.
func TestForwardClosesWhenTheClientHangsUp(t *testing.T) {
	echo := echoServer(t)
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowLocalForward: true})
	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	conn, err := cli.Dial("tcp", echo)
	if err != nil {
		t.Fatalf("open forward: %v", err)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 6)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	// The session survives its tunnel; a forward ending is not a session ending.
	if _, err := cli.Dial("tcp", echo); err != nil {
		t.Errorf("session should still serve new forwards after one closed: %v", err)
	}
}

// The bound, exercised through a real client.
//
// An authorised session must not be able to exhaust the gateway from the
// inside: every -L channel costs a goroutine pair here and a socket on the
// target. The limit is on what is held at once, so closing one frees budget.
func TestLocalForwardsAreBoundedPerSession(t *testing.T) {
	echo := echoServer(t)
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowLocalForward: true})
	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	// Hold the limit open. Each needs a byte in flight so the relay goroutines
	// are actually running rather than the channel merely being accepted.
	open := make([]net.Conn, 0, MaxLocalForwards)
	t.Cleanup(func() {
		for _, c := range open {
			_ = c.Close()
		}
	})
	for i := range MaxLocalForwards {
		c, err := cli.Dial("tcp", echo)
		if err != nil {
			t.Fatalf("forward %d should be within the limit: %v", i, err)
		}
		open = append(open, c)
	}

	if _, err := cli.Dial("tcp", echo); err == nil {
		t.Fatalf("the %dth forward should be refused", MaxLocalForwards+1)
	} else if !strings.Contains(err.Error(), strconv.Itoa(MaxLocalForwards)) {
		// The refusal names the limit, so an operator who needs more knows what
		// to ask for instead of guessing at a transient fault.
		t.Errorf("refusal should state the limit, got: %v", err)
	}

	// Closing one returns budget.
	last := open[len(open)-1]
	open = open[:len(open)-1]
	if err := last.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var reopened net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := cli.Dial("tcp", echo)
		if err == nil {
			reopened = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reopened == nil {
		t.Fatal("closing a forward should free a slot")
	}
	open = append(open, reopened)
}

/* ── Remote forwarding (-R) end to end ───────────────────────────────────── */

// `ssh -R` through a running gateway: the target binds a port, a connection to
// it arrives at the operator's machine, and bytes flow both ways.
//
// This is the harder direction — the listener lives on the target, the channel
// is opened *towards* the client, and three separate SSH connections have to
// agree about which port was bound. It was covered only at the bookkeeping
// level before.
func TestRemoteForwardDeliversConnectionsToTheClient(t *testing.T) {
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowRemoteForward: true})
	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	// Port 0: the bound port has to travel target → gateway → client, and a
	// fixed port would hide a break in that chain.
	ln, err := cli.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("request remote forward: %v", err)
	}
	defer ln.Close()

	bound := ln.Addr().String()
	if _, portStr, _ := net.SplitHostPort(bound); portStr == "0" {
		t.Fatal("the client was never told which port the target bound")
	}

	// Serve on the client side, as an operator exposing a local service.
	served := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			served <- err
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			served <- err
			return
		}
		if string(buf) != "ping\n" {
			served <- fmt.Errorf("client received %q", buf)
			return
		}
		_, err = c.Write([]byte("pong\n"))
		served <- err
	}()

	// Something on the target's network connects to the forwarded port.
	conn, err := net.DialTimeout("tcp", bound, 10*time.Second)
	if err != nil {
		t.Fatalf("connect to the forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	back := make([]byte, 5)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(back) != "pong\n" {
		t.Errorf("got %q back through the remote forward, want %q", back, "pong\n")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("client side of the forward: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the client never served the forwarded connection")
	}
}

func TestRemoteForwardIsRefusedByDefault(t *testing.T) {
	addr, signer := startGateway(t, NewPolicyHolder()) // closed
	cli := dialGateway(t, addr, signer)

	ln, err := cli.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = ln.Close()
		t.Fatal("a closed policy must refuse remote port forwarding")
	}
}

// Cancelling releases the port on the target. A listener that survived its
// cancel would outlive the authorisation that opened it.
func TestRemoteForwardCancelReleasesThePort(t *testing.T) {
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowRemoteForward: true})
	addr, signer := startGateway(t, policy)
	cli := dialGateway(t, addr, signer)

	ln, err := cli.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("request remote forward: %v", err)
	}
	bound := ln.Addr().String()
	if err := ln.Close(); err != nil { // sends cancel-tcpip-forward
		t.Fatalf("close listener: %v", err)
	}

	// The port on the target should stop accepting. Polled rather than asserted
	// once: the cancel travels client → gateway → target.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", bound, time.Second)
		if err != nil {
			return // refused, which is the outcome under test
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s still accepts connections after the forward was cancelled", bound)
}

/* ── Policy stability ────────────────────────────────────────────────────── */

// A session keeps the policy it started under, in both directions.
//
// This is a stated guarantee that had no assertion behind it, and writing the
// test found the two halves of the gateway disagreeing: channel opens used a
// connection-scoped copy while agent and X11 requests re-read the live policy.
// A session could be told it could open a channel and then refused a related
// request moments later, for no reason visible to the person using it.
func TestPolicyChangesDoNotAffectSessionsAlreadyOpen(t *testing.T) {
	echo := echoServer(t)
	policy := NewPolicyHolder()
	policy.Set(Policy{AllowLocalForward: true})
	addr, signer := startGateway(t, policy)

	// A session opened while forwarding was permitted.
	early := dialGateway(t, addr, signer)
	first, err := early.Dial("tcp", echo)
	if err != nil {
		t.Fatalf("forward should be permitted at open: %v", err)
	}
	_ = first.Close()

	// Policy is tightened underneath it.
	policy.Set(DefaultPolicy())

	// The session in flight keeps what it had. Tightening policy is not how a
	// session in progress is stopped — terminating it is, and that is a
	// separate deliberate act with its own audit entry.
	second, err := early.Dial("tcp", echo)
	if err != nil {
		t.Errorf("an open session must keep the policy it started under: %v", err)
	} else {
		_ = second.Close()
	}

	// A session opened after the change gets the new policy.
	late := dialGateway(t, addr, signer)
	if c, err := late.Dial("tcp", echo); err == nil {
		_ = c.Close()
		t.Error("a session opened after the tightening must be refused")
	}
}

// The same in the loosening direction, so the policy a session ran under is
// the one recorded against it rather than whatever happened to be current.
func TestLooseningDoesNotReachSessionsAlreadyOpen(t *testing.T) {
	echo := echoServer(t)
	policy := NewPolicyHolder() // closed
	addr, signer := startGateway(t, policy)

	early := dialGateway(t, addr, signer)
	if c, err := early.Dial("tcp", echo); err == nil {
		_ = c.Close()
		t.Fatal("forwarding should be refused while policy is closed")
	}

	policy.Set(Policy{AllowLocalForward: true})

	if c, err := early.Dial("tcp", echo); err == nil {
		_ = c.Close()
		t.Error("a loosening must not reach a session that opened before it")
	}
	// But a new session picks it up.
	late := dialGateway(t, addr, signer)
	c, err := late.Dial("tcp", echo)
	if err != nil {
		t.Errorf("a session opened after the loosening should be permitted: %v", err)
	} else {
		_ = c.Close()
	}
}
