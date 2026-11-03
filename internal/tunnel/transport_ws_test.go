package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

/*
The WebSocket transport is what gets an agent through a proxy that forwards
browser traffic only. These hold that it carries a session both ways, that the
grant key rides the handshake as it does on the plain upgrade, and that a
refusal reads as one, so the agent backs off rather than hammering.
*/

func wsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, DialOptions) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, DialOptions{ControlURL: srv.URL, Credential: "c", Hostname: "h",
		TLS: &tls.Config{RootCAs: pool}}
}

func TestAWebSocketTunnelCarriesStreamsBothWays(t *testing.T) {
	got := make(chan Session, 1)
	_, o := wsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !IsWebSocketUpgrade(r) || r.URL.Path != UpgradePath {
			http.Error(w, "not a websocket", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer c" || r.Header.Get("Argus-Hostname") != "h" {
			http.Error(w, "who", http.StatusUnauthorized)
			return
		}
		w.Header().Set(GrantKeyHeader, "grant-key")
		s, err := AcceptWebSocket(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		got <- s
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent, err := DialWebSocket(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	ctl := <-got
	defer ctl.Close()

	if agent.Transport() != TransportWebSocket || ctl.Transport() != TransportWebSocket {
		t.Fatalf("transports %s and %s", agent.Transport(), ctl.Transport())
	}
	if agent.GrantKey() != "grant-key" {
		t.Fatalf("grant key %q did not ride the handshake", agent.GrantKey())
	}

	// The control plane opens a stream toward the agent, as every session
	// does, and bytes go both ways; then a large write, which crosses many
	// WebSocket messages.
	go func() {
		st, err := agent.AcceptStream(ctx)
		if err != nil {
			return
		}
		_, _ = io.Copy(st, st)
		_ = st.Close()
	}()
	st, err := ctl.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 3<<20)
	for i := range want {
		want[i] = byte(i * 7)
	}
	go func() { _, _ = st.Write(want) }()
	back := make([]byte, len(want))
	if _, err := io.ReadFull(st, back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, want) {
		t.Fatal("the echo came back different")
	}
}

func TestARefusedWebSocketIsARefusal(t *testing.T) {
	_, o := wsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "revoked", http.StatusUnauthorized)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := DialWebSocket(ctx, o); !IsRefused(err) {
		t.Fatalf("got %v, want a refusal", err)
	}
}

// What a Cloudflare Tunnel does to the plain upgrade: the request arrives
// without it. DialUpgrade must fail with something other than a refusal, so
// the agent goes on to try a WebSocket.
func TestADroppedUpgradeIsNotARefusal(t *testing.T) {
	_, o := wsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expected Upgrade", http.StatusBadRequest)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := DialUpgrade(ctx, o)
	if err == nil || IsRefused(err) {
		t.Fatalf("got %v, want a failure that is not a refusal", err)
	}
}
