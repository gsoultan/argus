// Command rdpclient drives the RDP connection sequence against a target and
// reports what came back.
//
// Development only. It exists because the sequence is a dozen dependent steps
// and the interesting failures are all "the server stopped answering at step
// N" — which a log of each step makes obvious and a session that simply does
// not appear does not.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

func main() {
	addr := flag.String("addr", "", "target host:port")
	user := flag.String("user", "", "username for auto-logon")
	pass := flag.String("pass", "", "password")
	domain := flag.String("domain", "", "domain")
	width := flag.Int("width", 1024, "desktop width")
	height := flag.Int("height", 768, "desktop height")
	depth := flag.Int("depth", 16, "colour depth")
	frames := flag.Int("frames", 20, "how many update PDUs to read")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: rdpclient -addr host:port [-user u -pass p]")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	raw, err := net.DialTimeout("tcp", *addr, 10*time.Second)
	if err != nil {
		die("dial", err)
	}
	defer raw.Close()

	// Negotiation.
	if _, err := raw.Write(negotiationRequest()); err != nil {
		die("send negotiation", err)
	}
	frame, err := rdp.ReadPDU(raw)
	if err != nil {
		die("read connection confirm", err)
	}
	protocol, failure, err := rdp.ParseConnectionConfirm(frame)
	if err != nil {
		die("parse connection confirm", err)
	}
	if failure != 0 {
		die("target refused", fmt.Errorf("%s", rdp.FailureName(failure)))
	}
	fmt.Printf("negotiated  %s\n", rdp.ProtocolName(protocol))

	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err != nil {
		die("tls handshake", err)
	}
	cs := conn.ConnectionState()
	fmt.Printf("tls         %s, cert %s\n", tlsVersion(cs.Version),
		rdp.CertFingerprint(cs.PeerCertificates[0].Raw))

	client, err := rdp.Connect(conn, rdp.ClientConfig{
		Width: *width, Height: *height, Depth: *depth,
		SelectedProtocol: protocol,
		Logon: rdp.LogonInfo{
			Domain: *domain, User: *user, Password: *pass,
			ClientAddress: "127.0.0.1",
		},
		Log:         log,
		ReadTimeout: 8 * time.Second,
	})
	if err != nil {
		die("connection sequence", err)
	}
	defer client.Close()

	w, h := client.Size()
	fmt.Printf("session     %dx%d\n", w, h)

	fmt.Println("reading updates...")
	var rects, pixels int
	deadline := time.Now().Add(15 * time.Second)
	for i := 0; i < *frames && time.Now().Before(deadline); i++ {
		batch, err := client.Next()
		if err != nil {
			fmt.Printf("update %d: %v\n", i, err)
			break
		}
		for _, r := range batch {
			rects++
			pixels += r.Width * r.Height
		}
	}
	fmt.Printf("decoded     %d rectangles, %d pixels\n", rects, pixels)
	if rects == 0 {
		fmt.Println("NO GRAPHICS RECEIVED")
		os.Exit(1)
	}
	fmt.Println("OK")
}

// negotiationRequest builds a Connection Request asking for TLS.
func negotiationRequest() []byte {
	neg := []byte{0x01, 0x00, 0x08, 0x00, 0x01, 0x00, 0x00, 0x00}
	x := make([]byte, 7+len(neg))
	x[0] = byte(len(x) - 1)
	x[1] = 0xE0
	copy(x[7:], neg)
	out := make([]byte, 4+len(x))
	out[0] = 3
	out[2] = byte(len(out) >> 8)
	out[3] = byte(len(out))
	copy(out[4:], x)
	return out
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func die(what string, err error) {
	fmt.Fprintf(os.Stderr, "FAILED at %s: %v\n", what, err)
	os.Exit(1)
}
