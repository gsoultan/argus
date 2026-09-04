package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

/* ── Relay ───────────────────────────────────────────────────────────────── */

// sshChannelPair returns two ends of one real SSH channel.
//
// Real channels rather than net.Pipe: the behaviour under test is
// CloseWrite/EOF propagation, and ssh.Channel's half-close semantics are the
// specific thing a hand-rolled pipe would fail to reproduce.
//
// Over TCP loopback, not net.Pipe. net.Pipe is unbuffered and synchronous, and
// both ends of an SSH handshake write their version banner before reading
// either — so both block in Write, neither ever reads, and the handshake
// deadlocks before the test starts.
func sshChannelPair(t *testing.T) (client, server ssh.Channel) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srvCfg := &ssh.ServerConfig{NoClientAuth: true}
	srvCfg.AddHostKey(signer)

	type result struct {
		ch   ssh.Channel
		conn ssh.Conn
		err  error
	}
	srvOut := make(chan result, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			srvOut <- result{err: err}
			return
		}
		conn, chans, reqs, err := ssh.NewServerConn(raw, srvCfg)
		if err != nil {
			srvOut <- result{err: err}
			return
		}
		go ssh.DiscardRequests(reqs)
		newChan, ok := <-chans
		if !ok {
			srvOut <- result{conn: conn, err: errors.New("no channel opened")}
			return
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			srvOut <- result{conn: conn, err: err}
			return
		}
		go ssh.DiscardRequests(chReqs)
		srvOut <- result{ch: ch, conn: conn}
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cConn, cChans, cReqs, err := ssh.NewClientConn(raw, ln.Addr().String(), &ssh.ClientConfig{
		User:            "test",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("client conn: %v", err)
	}
	cli := ssh.NewClient(cConn, cChans, cReqs)
	t.Cleanup(func() { _ = cli.Close() })

	clientCh, clientReqs, err := cli.OpenChannel("argus-test", nil)
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	go ssh.DiscardRequests(clientReqs)

	select {
	case r := <-srvOut:
		if r.conn != nil {
			t.Cleanup(func() { _ = r.conn.Close() })
		}
		if r.err != nil {
			t.Fatalf("server side: %v", r.err)
		}
		return clientCh, r.ch
	case <-time.After(10 * time.Second):
		t.Fatal("server never accepted the channel")
		return nil, nil
	}
}

// The relay has to move bytes both ways and count each direction separately —
// the byte totals are what the audit event reports about a tunnel whose
// contents are deliberately not recorded.
func TestRelayChannelsCopiesBothWaysAndCounts(t *testing.T) {
	a1, a2 := sshChannelPair(t)
	b1, b2 := sshChannelPair(t)

	// a2 <-> b2 are joined by the relay; a1 and b1 are the two "outside" ends.
	var n forwardedBytes
	done := make(chan struct{})
	go func() { relayChannels(a2, b2, &n); close(done) }()

	out := []byte("GET /health HTTP/1.1\r\n\r\n")
	back := []byte("HTTP/1.1 200 OK\r\n\r\nfine")

	var wg sync.WaitGroup
	wg.Add(2)
	var gotB, gotA []byte
	go func() {
		defer wg.Done()
		if _, err := a1.Write(out); err != nil {
			t.Errorf("write a1: %v", err)
		}
		_ = a1.CloseWrite()
		gotA, _ = io.ReadAll(a1)
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, len(out))
		if _, err := io.ReadFull(b1, buf); err != nil {
			t.Errorf("read b1: %v", err)
			return
		}
		gotB = buf
		if _, err := b1.Write(back); err != nil {
			t.Errorf("write b1: %v", err)
		}
		_ = b1.CloseWrite()
	}()
	wg.Wait()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// The failure this guards against: a relay that never propagates EOF
		// leaves the tunnel open until the whole session is torn down.
		t.Fatal("relay did not finish after both sides half-closed")
	}

	if string(gotB) != string(out) {
		t.Errorf("forward direction: got %q want %q", gotB, out)
	}
	if string(gotA) != string(back) {
		t.Errorf("return direction: got %q want %q", gotA, back)
	}
	if n.up.Load() != int64(len(out)) {
		t.Errorf("up counted %d, want %d", n.up.Load(), len(out))
	}
	if n.down.Load() != int64(len(back)) {
		t.Errorf("down counted %d, want %d", n.down.Load(), len(back))
	}
}

func TestRelayChannelsToleratesNilCounter(t *testing.T) {
	a1, a2 := sshChannelPair(t)
	b1, b2 := sshChannelPair(t)
	done := make(chan struct{})
	go func() { relayChannels(a2, b2, nil); close(done) }()
	_ = a1.CloseWrite()
	_ = b1.CloseWrite()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay hung with a nil counter")
	}
}

/* ── Remote forward bookkeeping ──────────────────────────────────────────── */

type fakeListener struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeListener) Accept() (net.Conn, error) { return nil, errors.New("closed") }
func (f *fakeListener) Addr() net.Addr            { return &net.TCPAddr{Port: 1} }
func (f *fakeListener) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakeListener) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// A port opened on the target by -R must not outlive the session that
// authorised it. This is the bookkeeping that guarantees it.
func TestRemoteForwardsCloseAllReleasesEveryListener(t *testing.T) {
	var r remoteForwards
	a, b := &fakeListener{}, &fakeListener{}
	r.put("127.0.0.1:8080", a)
	r.put("127.0.0.1:9090", b)

	r.closeAll()
	if !a.isClosed() || !b.isClosed() {
		t.Error("closeAll must close every listener it holds")
	}
	// And forget them, so a second close is not attempted on a dead listener.
	if r.take("127.0.0.1:8080") != nil {
		t.Error("closeAll should also drop its references")
	}
}

func TestRemoteForwardsTakeIsOnceOnly(t *testing.T) {
	var r remoteForwards
	l := &fakeListener{}
	r.put("127.0.0.1:8080", l)

	if got := r.take("127.0.0.1:8080"); got != l {
		t.Fatal("take should return the listener that was put")
	}
	// cancel-tcpip-forward arriving twice must not close someone else's
	// listener or panic on a nil.
	if got := r.take("127.0.0.1:8080"); got != nil {
		t.Error("a second take must return nil")
	}
	if got := r.take("never-registered"); got != nil {
		t.Error("taking an unknown key must return nil")
	}
}

func TestRemoteForwardsIsSafeUnderConcurrency(t *testing.T) {
	var r remoteForwards
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "127.0.0.1:" + strings.Repeat("1", i%5+1)
			r.put(key, &fakeListener{})
			r.take(key)
		}()
	}
	wg.Wait()
	r.closeAll()
}

/* ── Reporting ───────────────────────────────────────────────────────────── */

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {1024, "1.0 KB"},
		{1536, "1.5 KB"}, {1024 * 1024, "1.0 MB"}, {3 * 1024 * 1024 * 1024, "3.0 GB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The close event is the only statement Argus makes about a tunnel's volume,
// so it names both directions rather than a single total.
func TestDescribeVolumeNamesBothDirections(t *testing.T) {
	var n forwardedBytes
	n.up.Add(2048)
	n.down.Add(1024 * 1024)
	got := describeVolume(&n)
	if !strings.Contains(got, "2.0 KB out") || !strings.Contains(got, "1.0 MB back") {
		t.Errorf("describeVolume = %q, want both directions named", got)
	}
}

/* ── closeWrite ──────────────────────────────────────────────────────────── */

type plainConn struct {
	net.Conn
	closed bool
}

func (p *plainConn) Close() error { p.closed = true; return nil }

// A connection that cannot half-close must be fully closed instead, or the far
// end waits for an EOF that never arrives.
func TestCloseWriteFallsBackToCloseOnPlainConns(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	p := &plainConn{Conn: c1}
	closeWrite(p)
	if !p.closed {
		t.Error("a conn without CloseWrite must be closed outright")
	}
}

func TestCloseWriteUsesHalfCloseWhenAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	server := <-accepted
	defer server.Close()

	closeWrite(c) // TCPConn implements CloseWrite

	// The peer sees EOF, and the connection is still readable in the other
	// direction — which is what half-close means and why a full Close here
	// would break a tunnel mid-response.
	if _, err := io.ReadAll(server); err != nil {
		t.Fatalf("peer should read EOF cleanly: %v", err)
	}
	if _, err := server.Write([]byte("still open")); err != nil {
		t.Errorf("return direction should survive a half-close: %v", err)
	}
}

/* ── Bounds ──────────────────────────────────────────────────────────────── */

// The map is keyed by an address the client chose, so it needs a ceiling.
func TestRemoteForwardsRefusesPastTheLimit(t *testing.T) {
	var r remoteForwards
	for i := range MaxRemoteForwards {
		key := "127.0.0.1:" + strconv.Itoa(9000+i)
		if err := r.put(key, &fakeListener{}); err != nil {
			t.Fatalf("put %d should be accepted: %v", i, err)
		}
	}
	if err := r.put("127.0.0.1:9999", &fakeListener{}); !errors.Is(err, ErrTooManyForwards) {
		t.Errorf("put past the limit = %v, want ErrTooManyForwards", err)
	}

	// Releasing one frees budget, so a session that closes a forward can open
	// another — the limit is on what is held, not on what has ever been asked.
	if l := r.take("127.0.0.1:9000"); l == nil {
		t.Fatal("take should return a registered listener")
	}
	if err := r.put("127.0.0.1:9999", &fakeListener{}); err != nil {
		t.Errorf("put after a release should be accepted: %v", err)
	}
	r.closeAll()
}

// Re-binding the same address replaces the listener rather than consuming
// another slot, or a client re-requesting one port would exhaust its budget.
func TestRemoteForwardsRebindDoesNotConsumeBudget(t *testing.T) {
	var r remoteForwards
	first := &fakeListener{}
	if err := r.put("127.0.0.1:8080", first); err != nil {
		t.Fatalf("put: %v", err)
	}
	for range MaxRemoteForwards * 2 {
		if err := r.put("127.0.0.1:8080", &fakeListener{}); err != nil {
			t.Fatalf("re-binding the same address should always be accepted: %v", err)
		}
	}
	// The displaced listener must be closed, not leaked on the target.
	if !first.isClosed() {
		t.Error("a replaced listener must be closed")
	}
	r.closeAll()
}
