package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/recorder"
)

// selfSigned returns a certificate and its Argus fingerprint.
func selfSigned(t *testing.T, cn string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		rdp.CertFingerprint(der)
}

// windowsHost stands in for a Windows target: it completes the RDP negotiation,
// speaks TLS, and echoes whatever arrives.
func windowsHost(t *testing.T, protocol uint32) (addr, fingerprint string) {
	t.Helper()
	cert, fp := selfSigned(t, "win-01")

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
				_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
				if _, err := rdp.ReadPDU(raw); err != nil {
					return
				}
				if _, err := raw.Write(rdp.BuildConnectionConfirm(protocol)); err != nil {
					return
				}
				srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := srv.Handshake(); err != nil {
					return
				}
				_, _ = io.Copy(srv, srv)
			}()
		}
	}()
	return ln.Addr().String(), fp
}

// tpktData builds a TPKT-framed data PDU carrying payload.
func tpktData(payload string) []byte {
	body := append([]byte{0x02, 0xF0, 0x80}, payload...)
	out := make([]byte, 4+len(body))
	out[0] = 3
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[4:], body)
	return out
}

// clientCR builds a Connection Request the way mstsc does.
func clientCR(cookie string, protocols uint32) []byte {
	var v []byte
	v = append(v, ("Cookie: mstshash=" + cookie + "\r\n")...)
	neg := make([]byte, 8)
	neg[0] = 0x01
	binary.LittleEndian.PutUint16(neg[2:4], 8)
	binary.LittleEndian.PutUint32(neg[4:8], protocols)
	v = append(v, neg...)

	x := make([]byte, 7+len(v))
	x[0] = byte(len(x) - 1)
	x[1] = 0xE0
	copy(x[7:], v)

	out := make([]byte, 4+len(x))
	out[0] = 3
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[4:], x)
	return out
}

// startRDPGateway wires a gateway in front of a fake Windows host.
func startRDPGateway(t *testing.T, asset Asset, targetFingerprint string) (addr, recordings string) {
	t.Helper()

	pins, err := hostkey.Open(filepath.Join(t.TempDir(), "known_hosts"), false)
	if err != nil {
		t.Fatal(err)
	}
	// Pinned up front rather than trusted on first use, so the test exercises
	// verification rather than the TOFU path that accepts anything once.
	if targetFingerprint != "" {
		if err := pins.VerifyFingerprint(asset.Hostname, targetFingerprint, "x509"); err == nil {
			t.Fatal("a strict store accepted an unknown host")
		}
		pins.TOFU = true
		if err := pins.VerifyFingerprint(asset.Hostname, targetFingerprint, "x509"); err != nil {
			t.Fatalf("pinning the target: %v", err)
		}
		pins.TOFU = false
	}

	srv := &Server{
		cfg: Config{
			Inventory: &Inventory{assets: map[string]Asset{asset.Hostname: asset}},
			HostKeys:  pins,
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]*Session{},
	}

	gwCert, _ := selfSigned(t, "argus")
	recordings = t.TempDir()
	rs, err := NewRDPServer(srv, RDPConfig{
		Listen:       "127.0.0.1:0",
		TLS:          &tls.Config{Certificates: []tls.Certificate{gwCert}},
		RecordingDir: recordings,
		DialTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rs.listener = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go rs.handleConn(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), recordings
}

// The whole point: a stock client reaches a Windows host through Argus, the
// session is proxied, and what crossed the wire is on disk and verifiable.
func TestRDPSessionIsBrokeredAndRecorded(t *testing.T) {
	targetAddr, fingerprint := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	asset := Asset{
		Hostname:   "win-01",
		Address:    host,
		Port:       port,
		Protocol:   ProtocolRDP,
		Principals: []string{"ops"},
	}
	gwAddr, recordings := startRDPGateway(t, asset, fingerprint)

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if _, err := conn.Write(clientCR("ops:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	confirm, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no connection confirm: %v", err)
	}
	protocol, failure, err := rdp.ParseConnectionConfirm(confirm)
	if err != nil || failure != 0 {
		t.Fatalf("gateway refused: failure=%#x err=%v", failure, err)
	}
	if protocol != rdp.ProtocolSSL {
		t.Fatalf("protocol = %s", rdp.ProtocolName(protocol))
	}

	// The client now speaks TLS to Argus, which speaks TLS to the target.
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "argus"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client tls handshake: %v", err)
	}

	sent := tpktData("hello-from-client")
	if _, err := tlsConn.Write(sent); err != nil {
		t.Fatal(err)
	}
	// The fake host echoes, so this proves the full path in both directions.
	echoed, err := rdp.ReadPDU(tlsConn)
	if err != nil {
		t.Fatalf("nothing came back through the gateway: %v", err)
	}
	if string(echoed) != string(sent) {
		t.Errorf("echo differed:\n got %x\nwant %x", echoed, sent)
	}
	tlsConn.Close()

	// Give the gateway a moment to seal the recording.
	var files []os.DirEntry
	for range 50 {
		files, _ = os.ReadDir(recordings)
		if len(files) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(files) != 1 {
		t.Fatalf("expected one recording, found %d", len(files))
	}
	if !strings.HasSuffix(files[0].Name(), rdp.Extension) {
		t.Errorf("recording is named %q", files[0].Name())
	}

	body, err := os.ReadFile(filepath.Join(recordings, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	// The same verifier the SSH recordings use, with no knowledge of RDP.
	v, err := recorder.Verify(strings.NewReader(string(body)), "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.Lines < 3 {
		t.Errorf("recording holds %d lines; the session was not captured", v.Lines)
	}
	if !strings.Contains(string(body), `"c"`) {
		t.Error("no client-input frame was recorded")
	}
	if !strings.Contains(string(body), `"s"`) {
		t.Error("no server-output frame was recorded")
	}
}

// The inventory is the authority on who may be assumed where, not the target's
// own account database.
func TestRDPRefusesAPrincipalTheInventoryDoesNotAllow(t *testing.T) {
	targetAddr, fingerprint := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	asset := Asset{
		Hostname: "win-01", Address: host, Port: port,
		Protocol: ProtocolRDP, Principals: []string{"ops"},
	}
	gwAddr, recordings := startRDPGateway(t, asset, fingerprint)

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Administrator is not in the allowlist.
	if _, err := conn.Write(clientCR("Administrator:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	reply, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no refusal was sent: %v", err)
	}
	if _, failure, _ := rdp.ParseConnectionConfirm(reply); failure == 0 {
		t.Error("the gateway confirmed a principal the inventory forbids")
	}

	if files, _ := os.ReadDir(recordings); len(files) != 0 {
		t.Errorf("a refused connection produced %d recordings", len(files))
	}
}

// A target whose certificate does not match its pin is an interception. The
// session must not proceed, and the client must be told.
func TestRDPRefusesAnUnpinnedTarget(t *testing.T) {
	targetAddr, _ := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	asset := Asset{
		Hostname: "win-01", Address: host, Port: port,
		Protocol: ProtocolRDP, Principals: []string{"ops"},
	}
	// Pin a fingerprint the target will never present.
	gwAddr, _ := startRDPGateway(t, asset,
		"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(clientCR("ops:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	reply, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no refusal was sent: %v", err)
	}
	if _, failure, _ := rdp.ParseConnectionConfirm(reply); failure == 0 {
		t.Error("the gateway brokered a session to a target whose certificate changed")
	}
}

// An RDP request for an SSH asset would fail confusingly at the target.
func TestRDPRefusesANonRDPAsset(t *testing.T) {
	asset := Asset{
		Hostname: "pay-01", Address: "127.0.0.1", Port: 22,
		Protocol: ProtocolSSH, Principals: []string{"ops"},
	}
	gwAddr, _ := startRDPGateway(t, asset, "")

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(clientCR("ops:pay-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	reply, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no refusal was sent: %v", err)
	}
	if _, failure, _ := rdp.ParseConnectionConfirm(reply); failure == 0 {
		t.Error("an SSH asset was brokered over RDP")
	}
}

func TestAssetPortDefaultsFollowTheProtocol(t *testing.T) {
	ssh := Asset{Hostname: "pay-01", Address: "10.0.0.1"}
	if got := ssh.Addr(); got != "10.0.0.1:22" {
		t.Errorf("ssh default = %q", got)
	}
	win := Asset{Hostname: "win-01", Address: "10.0.0.2", Protocol: ProtocolRDP}
	if got := win.Addr(); got != "10.0.0.2:3389" {
		t.Errorf("rdp default = %q; an RDP session sent to port 22 fails with an "+
			"error that says nothing about the real mistake", got)
	}
	explicit := Asset{Hostname: "win-02", Address: "10.0.0.3", Port: 13389, Protocol: ProtocolRDP}
	if got := explicit.Addr(); got != "10.0.0.3:13389" {
		t.Errorf("explicit port = %q", got)
	}
}

// Recording is not optional. A session Argus cannot record is a session it must
// not broker: otherwise "every privileged session is recorded" quietly stops
// being true on the one host where the disk filled up.
func TestRDPRefusesToBrokerWhatItCannotRecord(t *testing.T) {
	targetAddr, fingerprint := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	asset := Asset{
		Hostname: "win-01", Address: host, Port: port,
		Protocol: ProtocolRDP, Principals: []string{"ops"},
	}

	pins, err := hostkey.Open(filepath.Join(t.TempDir(), "known_hosts"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := pins.VerifyFingerprint(asset.Hostname, fingerprint, "x509"); err != nil {
		t.Fatal(err)
	}
	pins.TOFU = false

	srv := &Server{
		cfg: Config{
			Inventory: &Inventory{assets: map[string]Asset{asset.Hostname: asset}},
			HostKeys:  pins,
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]*Session{},
	}

	// A regular file where the recording directory should be, so MkdirAll fails
	// the way a full or read-only disk would.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	gwCert, _ := selfSigned(t, "argus")
	rs, err := NewRDPServer(srv, RDPConfig{
		Listen:       "127.0.0.1:0",
		TLS:          &tls.Config{Certificates: []tls.Certificate{gwCert}},
		RecordingDir: filepath.Join(blocked, "recordings"),
		DialTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rs.listener = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go rs.handleConn(c)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(clientCR("ops:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	if _, err := rdp.ReadPDU(conn); err != nil {
		t.Fatalf("no connection confirm: %v", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "argus"})
	if err := tlsConn.Handshake(); err != nil {
		// Refusing during the handshake is also an acceptable refusal.
		return
	}

	// The gateway must drop the session rather than proxy it unrecorded, so a
	// write either fails or is never answered.
	_ = tlsConn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := tlsConn.Write(tpktData("should-not-be-proxied")); err == nil {
		if _, err := rdp.ReadPDU(tlsConn); err == nil {
			t.Error("the gateway proxied a session it could not record")
		}
	}

	if len(rs.ActiveSessions()) != 0 {
		t.Error("an unrecordable session was tracked as active")
	}
}
