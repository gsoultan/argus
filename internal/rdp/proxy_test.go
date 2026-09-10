package rdp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCookie(t *testing.T) {
	cases := []struct {
		in        string
		principal string
		target    string
		wantErr   bool
	}{
		{"ops:win-01", "ops", "win-01", false},
		{"ops+win-01", "ops", "win-01", false},
		{"ops/win-01", "ops", "win-01", false},
		{"Administrator:dc-01.corp.northwind.id", "Administrator", "dc-01.corp.northwind.id", false},
		{"  ops:win-01  ", "ops", "win-01", false},
		{"", "", "", true},
		{"justausername", "", "", true},
		{":win-01", "", "", true},
		{"ops:", "", "", true},
		{"ops:win-01:extra", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseCookie(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCookie(%q) succeeded, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCookie(%q): %v", tc.in, err)
			}
			if got.Principal != tc.principal || got.Target != tc.target {
				t.Errorf("got %s@%s, want %s@%s",
					got.Principal, got.Target, tc.principal, tc.target)
			}
		})
	}
}

// A username with no target must not be guessed at: defaulting the principal
// would let a typo silently open a session as the wrong account.
func TestCookieErrorsExplainTheSyntax(t *testing.T) {
	_, err := ParseCookie("justausername")
	if err == nil || !strings.Contains(err.Error(), "principal:host") {
		t.Errorf("error does not tell the user what to type: %v", err)
	}
}

/* ── Handshake over real sockets ─────────────────────────────────────────── */

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pipeConns returns a connected pair over a real TCP listener.
//
// net.Pipe is synchronous and unbuffered, so a handshake that writes a refusal
// and then returns deadlocks against a peer that is not already reading. A real
// socket behaves the way the gateway will.
func pipeConns(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		done <- c
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-done
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	_ = server.SetDeadline(time.Now().Add(10 * time.Second))
	return client, server
}

func TestHandshakeAcceptsAWellFormedClient(t *testing.T) {
	client, server := pipeConns(t)

	go func() {
		_, _ = client.Write(connectionRequest("ops:win-01", ProtocolSSL|ProtocolHybrid, true))
	}()

	req, protocol, err := Handshake(server, discardLog())
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if req.Principal != "ops" || req.Target != "win-01" {
		t.Errorf("request = %s@%s", req.Principal, req.Target)
	}
	// CredSSP over TLS: it authenticates the user before a desktop exists.
	if protocol != ProtocolHybrid {
		t.Errorf("protocol = %s, want credssp", ProtocolName(protocol))
	}
}

// The control that matters most in the connection sequence. Standard RDP
// security cannot authenticate the server, so brokering it would leave Argus
// unable to prove a session reached the host it claims.
func TestHandshakeRefusesWeakSecurity(t *testing.T) {
	cases := []struct {
		name  string
		frame []byte
	}{
		{"standard RDP security only", connectionRequest("ops:win-01", ProtocolRDP, true)},
		{"no negotiation structure at all", connectionRequest("ops:win-01", 0, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := pipeConns(t)
			go func() { _, _ = client.Write(tc.frame) }()

			if _, _, err := Handshake(server, discardLog()); err == nil {
				t.Fatal("Handshake accepted a connection Argus must not broker")
			}

			// The client must be told why, in the protocol's own vocabulary,
			// rather than seeing a dropped socket.
			reply, err := ReadPDU(client)
			if err != nil {
				t.Fatalf("no refusal was sent: %v", err)
			}
			_, failure, err := ParseConnectionConfirm(reply)
			if err != nil {
				t.Fatalf("refusal did not parse: %v", err)
			}
			if failure != FailSSLRequiredByServer {
				t.Errorf("failure = %#x, want SSL_REQUIRED_BY_SERVER", failure)
			}
		})
	}
}

func TestHandshakeRefusesAConnectionWithNoTarget(t *testing.T) {
	client, server := pipeConns(t)
	go func() {
		_, _ = client.Write(connectionRequest("justausername", ProtocolHybrid, true))
	}()

	if _, _, err := Handshake(server, discardLog()); err == nil {
		t.Fatal("Handshake accepted a cookie with no target")
	}
	if _, err := ReadPDU(client); err != nil {
		t.Errorf("no refusal was sent: %v", err)
	}
}

func TestHandshakeRejectsANonRDPClient(t *testing.T) {
	client, server := pipeConns(t)
	go func() {
		_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	}()
	if _, _, err := Handshake(server, discardLog()); err == nil {
		t.Fatal("Handshake accepted an HTTP request")
	}
}

/* ── Target dialling and certificate pinning ─────────────────────────────── */

// fakeTarget is an RDP server that completes the negotiation and then speaks
// TLS, which is what a Windows host does.
func fakeTarget(t *testing.T, protocol uint32) (addr string, fingerprint string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "win-01"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"win-01"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := ReadPDU(raw); err != nil {
					return
				}
				if _, err := raw.Write(BuildConnectionConfirm(protocol)); err != nil {
					return
				}
				srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := srv.Handshake(); err != nil {
					return
				}
				// Echo whatever arrives, so a relay test has something to see.
				_, _ = io.Copy(srv, srv)
			}()
		}
	}()

	return ln.Addr().String(), CertFingerprint(der)
}

// stubPinner records what it was asked and answers with a fixed verdict.
type stubPinner struct {
	expect string
	err    error
	seen   string
}

func (p *stubPinner) VerifyFingerprint(host, presented, keyType string) error {
	p.seen = presented
	if p.err != nil {
		return p.err
	}
	if p.expect != "" && presented != p.expect {
		return errors.New("fingerprint mismatch")
	}
	return nil
}

func TestDialTargetPinsTheCertificate(t *testing.T) {
	addr, fingerprint := fakeTarget(t, ProtocolSSL)

	pins := &stubPinner{expect: fingerprint}
	conn, err := DialTarget(addr, "win-01", ProtocolSSL, pins, 5*time.Second)
	if err != nil {
		t.Fatalf("DialTarget: %v", err)
	}
	defer conn.Close()

	if pins.seen != fingerprint {
		t.Errorf("pinned %q, target presented %q", pins.seen, fingerprint)
	}
	if !strings.HasPrefix(pins.seen, "SHA256:") {
		t.Errorf("fingerprint %q is not in the form the trust store uses", pins.seen)
	}
}

// A changed certificate is the RDP equivalent of a changed host key: it is the
// signature of an interception, and the connection must not proceed.
func TestDialTargetRefusesAnUnpinnedCertificate(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolSSL)

	pins := &stubPinner{err: errors.New("host key mismatch")}
	conn, err := DialTarget(addr, "win-01", ProtocolSSL, pins, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("DialTarget proceeded despite a refused pin")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

// A target that answers with standard RDP security must be refused even though
// the client asked for something stronger — otherwise the far half of the
// session is unauthenticated while the near half looks fine.
func TestDialTargetRefusesAWeakTarget(t *testing.T) {
	addr, _ := fakeTarget(t, ProtocolRDP)

	conn, err := DialTarget(addr, "win-01", ProtocolSSL, &stubPinner{}, 5*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("DialTarget accepted a target that selected standard RDP security")
	}
	if !strings.Contains(err.Error(), "standard RDP security") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

/* ── Relay and recording ─────────────────────────────────────────────────── */

func TestRelayRecordsBothDirections(t *testing.T) {
	clientSide, gatewayClient := pipeConns(t)
	gatewayTarget, targetSide := pipeConns(t)

	var buf syncBuffer
	rec, err := NewRecorder(&buf, Header{Title: "ops@win-01"})
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{ID: "s1", Principal: "ops", Target: "win-01"}
	sess.SetRecorder(rec)

	fromClient := tpkt([]byte{0x02, tpduData, 0x80, 'i', 'n'})
	fromTarget := tpkt([]byte{0x02, tpduData, 0x80, 'o', 'u', 't'})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = sess.Relay(gatewayClient, gatewayTarget, discardLog())
	}()

	if _, err := clientSide.Write(fromClient); err != nil {
		t.Fatal(err)
	}
	if _, err := targetSide.Write(fromTarget); err != nil {
		t.Fatal(err)
	}

	// Let both directions carry their frame, then close to end the relay.
	got := make([]byte, len(fromTarget))
	if _, err := io.ReadFull(clientSide, got); err != nil {
		t.Fatalf("client never received the target's frame: %v", err)
	}
	relayed := make([]byte, len(fromClient))
	if _, err := io.ReadFull(targetSide, relayed); err != nil {
		t.Fatalf("target never received the client's frame: %v", err)
	}
	clientSide.Close()
	targetSide.Close()
	wg.Wait()

	if _, err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, `"s"`) {
		t.Error("no server-output frame was recorded")
	}
	if !strings.Contains(body, `"c"`) {
		t.Error("no client-input frame was recorded")
	}
}

// syncBuffer is a bytes.Buffer safe for the two relay goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

/* ── Shadowing a proxied session ─────────────────────────────────────────── */

// Decoding every update of every proxied session whether or not anyone is
// looking would spend the gateway's CPU on pixels nobody sees. A privileged
// access gateway must not get slower because a feature exists.
func TestProxiedSessionDoesNotDecodeWithoutViewers(t *testing.T) {
	sess := &Session{ID: "s1"}
	// The hub exists — a session always has one — but nobody has subscribed.
	// Testing with no hub at all would pass whether or not the viewer count is
	// checked, which is a test that cannot fail for the reason it claims.
	hub := sess.Hub()
	if hub.Viewers() != 0 {
		t.Fatal("a fresh hub already has viewers")
	}

	update := bitmapUpdateData(bitmapRect(t, 0, 0, 2, 1, 24, false,
		[]byte{1, 2, 3, 4, 5, 6}))
	sess.shadow(fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, update))

	sess.mu.Lock()
	rea := sess.rea
	sess.mu.Unlock()
	if rea != nil {
		t.Error("an update was decoded with nobody watching")
	}
}

func TestProxiedSessionBroadcastsToViewers(t *testing.T) {
	sess := &Session{ID: "s1"}
	_, frames, cancel := sess.Hub().Subscribe()
	defer cancel()

	update := bitmapUpdateData(bitmapRect(t, 4, 8, 2, 1, 24, false,
		[]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}))
	sess.shadow(fastPathUpdate(t, fastPathUpdateBitmap, fragSingle, update))

	select {
	case got := <-frames:
		if len(got) < FrameHeaderSize {
			t.Fatalf("broadcast %d bytes", len(got))
		}
		if got[0] != FrameBitmap {
			t.Errorf("frame type = %d", got[0])
		}
		if x := int(got[2]) | int(got[3])<<8; x != 4 {
			t.Errorf("x = %d, want 4", x)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a viewer received nothing")
	}
}

// The share and user ids are the only two values Argus reads out of a proxied
// session, and it reads them because without them it cannot ask the target to
// redraw for a viewer who arrives after the screen was last painted.
func TestProxiedSessionObservesTheConnectionSequence(t *testing.T) {
	sess := &Session{ID: "s1", Width: 1024, Height: 768}

	// Attach user confirm carries the user id.
	sess.observe(x224Data([]byte{mcsAttachUserConfirm<<2 | 0x02, 0x00, 0x00, 0x07}))

	// Demand active carries the share id.
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], 0xDEADBEEF)
	inner := make([]byte, 6, 6+len(body))
	binary.LittleEndian.PutUint16(inner[0:2], uint16(6+len(body)))
	binary.LittleEndian.PutUint16(inner[2:4], pduTypeDemandActive|pduVersion<<4)
	inner = append(inner, body...)
	sess.observe(x224Data(sendDataIndication(1004, ChannelGlobal, inner)))

	sess.mu.Lock()
	shareID, userID := sess.shareID, sess.userID
	sess.mu.Unlock()

	if userID != 7+userChannelBase {
		t.Errorf("user id = %d", userID)
	}
	if shareID != 0xDEADBEEF {
		t.Errorf("share id = %#x", shareID)
	}

	// With both known, a redraw can be requested.
	var out syncBuffer
	if err := sess.RefreshFor(&out); err != nil {
		t.Fatalf("RefreshFor: %v", err)
	}
	if out.String() == "" {
		t.Error("no refresh request was written")
	}
}

// Without the sequence observed there is nothing to address the request to, and
// guessing would send a PDU the server rejects mid-session.
func TestRefreshRequiresTheObservedIDs(t *testing.T) {
	sess := &Session{ID: "s1"}
	var out syncBuffer
	if err := sess.RefreshFor(&out); err == nil {
		t.Error("a refresh was sent before the connection sequence was seen")
	}
}

// sendDataIndication builds the MCS wrapper a server uses.
func sendDataIndication(userID, channel uint16, data []byte) []byte {
	head := make([]byte, 0, 8+len(data))
	head = append(head, mcsSendDataIndication<<2)
	head = binary.BigEndian.AppendUint16(head, userID-userChannelBase)
	head = binary.BigEndian.AppendUint16(head, channel)
	head = append(head, 0x70)
	if len(data) < 0x80 {
		head = append(head, byte(len(data)))
	} else {
		head = append(head, byte(0x80|len(data)>>8), byte(len(data)))
	}
	return append(head, data...)
}

// A frame the session saw and the file did not has to be visible afterwards.
//
// The relay stops on a failed write, so the session ends rather than carrying
// on unrecorded. What was missing is any mark on the artefact: a replay that
// cuts off part-way looks exactly like a session that ended there, and an
// auditor cannot tell the two apart without being told.
func TestABrokenRecordingIsMarkedOnTheSession(t *testing.T) {
	var sink brokenFrameWriter
	rec, err := NewRecorder(&sink, Header{Width: 1024, Height: 768})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{ID: "rdp-1", rec: rec}

	if err := s.record(ClientInput, []byte("first frame")); err != nil {
		t.Fatalf("the first frame should record: %v", err)
	}
	if s.RecordingBroken() {
		t.Fatal("a healthy recording must not be marked broken")
	}

	sink.broken = true
	if err := s.record(ServerOutput, []byte("second frame")); err == nil {
		t.Fatal("the broken writer should have failed the record")
	}
	if !s.RecordingBroken() {
		t.Error("a frame that could not be written left no mark on the session")
	}
}

type brokenFrameWriter struct{ broken bool }

func (b *brokenFrameWriter) Write(p []byte) (int, error) {
	if b.broken {
		return 0, errors.New("no space left on device")
	}
	return len(p), nil
}
