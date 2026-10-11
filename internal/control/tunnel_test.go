package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Agent tunnels.

The refusals first: the shared reporter token opens no tunnel, a credential
connects only as its own host, a revoked credential's tunnel closes, and an
agent refuses a service it does not offer. Then that a tunnel works at all, on
both transports, and that the console is told the truth about it.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Tunnel
*/

type tunnelEnv struct {
	api   *API
	store *Store
	h     http.Handler
	srv   *httptest.Server
	tls   *tls.Config // what an agent trusts
}

func newTunnelEnv(t *testing.T) *tunnelEnv {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	a.Tunnels = NewTunnelHub(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "test-replica")
	t.Cleanup(a.Tunnels.Close)
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tunnelEnv{api: a, store: s, h: h, srv: srv,
		tls: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

// withQUIC starts a QUIC listener with the test server's certificate.
func (e *tunnelEnv) withQUIC(t *testing.T) {
	t.Helper()
	qtls := e.srv.TLS.Clone()
	qtls.NextProtos = []string{tunnel.ALPN}
	ln, err := quic.ListenAddr("127.0.0.1:0", qtls, tunnel.ServerQUICConfig())
	if err != nil {
		t.Fatalf("quic listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ln.Close() })
	e.api.Tunnels.QUICPort = ln.Addr().(*net.UDPAddr).Port
	go e.api.ServeQUIC(ctx, ln)
}

func (e *tunnelEnv) dial(cred, host string) tunnel.DialOptions {
	return tunnel.DialOptions{ControlURL: e.srv.URL, Credential: cred, Hostname: host,
		Version: "test", TLS: e.tls}
}

// runAgent runs a real agent tunnel loop until the test ends.
func (e *tunnelEnv) runAgent(t *testing.T, cred, host string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{
		Dial:     e.dial(cred, host),
		Services: agent.DefaultTunnelServices(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// waitTunnel waits until the hub can ping host, and returns the transport.
func (e *tunnelEnv) waitTunnel(t *testing.T, host string, want tunnel.Transport) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.api.Tunnels.mu.Lock()
		lt := e.api.Tunnels.byHost[strings.ToLower(host)]
		e.api.Tunnels.mu.Unlock()
		if lt != nil && (want == "" || lt.sess.Transport() == want) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := tunnel.Ping(ctx, lt.sess)
			cancel()
			if err == nil {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no working %s tunnel from %s", want, host)
}

func (e *tunnelEnv) listed(t *testing.T, host string) AgentCredential {
	t.Helper()
	creds, err := e.store.AgentCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		if strings.EqualFold(c.Hostname, host) && c.RevokedAt == nil {
			return c
		}
	}
	for _, c := range creds {
		if strings.EqualFold(c.Hostname, host) {
			return c
		}
	}
	t.Fatalf("%s is not listed", host)
	return AgentCredential{}
}

/* ── Refusals ────────────────────────────────────────────────────────────── */

func TestTheSharedReporterTokenOpensNoTunnel(t *testing.T) {
	e := newTunnelEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := tunnel.DialUpgrade(ctx, e.dial(testReporterToken, "any-host"))
	if !tunnel.IsRefused(err) {
		t.Fatalf("the fleet-wide token was not refused: %v", err)
	}
}

func TestATunnelConnectsOnlyAsItsOwnHost(t *testing.T) {
	e := newTunnelEnv(t)
	e.withQUIC(t)
	host := unique("tun-own")
	cred, _ := enrolled(t, e.store, e.h, host)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := tunnel.DialUpgrade(ctx, e.dial(cred, "someone-else")); !tunnel.IsRefused(err) {
		t.Errorf("HTTPS: a credential connected as another host: %v", err)
	}
	if _, err := tunnel.DialQUIC(ctx, e.dial(cred, "someone-else"), e.api.Tunnels.QUICPort); !tunnel.IsRefused(err) {
		t.Errorf("QUIC: a credential connected as another host: %v", err)
	}
	if _, err := tunnel.DialQUIC(ctx, e.dial("argus-agent-not-a-real-one", host),
		e.api.Tunnels.QUICPort); !tunnel.IsRefused(err) {
		t.Errorf("QUIC: an unknown credential connected: %v", err)
	}
}

func TestRevokingAnAgentClosesItsTunnel(t *testing.T) {
	e := newTunnelEnv(t)
	host := unique("tun-revoke")
	cred, id := enrolled(t, e.store, e.h, host)
	e.runAgent(t, cred, host)
	e.waitTunnel(t, host, "")

	if w := do(t, e.h, http.MethodPost, "/api/v1/agents/"+id+"/revoke", "admin-token", ""); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := e.api.Tunnels.Open(ctx, host, "ping")
		cancel()
		if err != nil {
			if got := e.listed(t, host); got.Tunnel.Connected {
				t.Error("the console still lists a revoked agent's tunnel as connected")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a revoked agent's tunnel stayed open")
}

// A revocation on another replica is caught at the next ping.
func TestAPingNoticesARevocationMadeElsewhere(t *testing.T) {
	e := newTunnelEnv(t)
	host := unique("tun-elsewhere")
	cred, id := enrolled(t, e.store, e.h, host)
	e.runAgent(t, cred, host)
	e.waitTunnel(t, host, "")

	// Straight to the database: this replica is not told.
	if _, err := e.store.RevokeAgentCredential(context.Background(), id, "other-replica"); err != nil {
		t.Fatal(err)
	}
	e.api.Tunnels.mu.Lock()
	lt := e.api.Tunnels.byHost[strings.ToLower(host)]
	e.api.Tunnels.mu.Unlock()
	e.api.Tunnels.ping(lt)
	select {
	case <-lt.sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel outlived a revocation made on another replica")
	}
}

func TestAnAgentRefusesAServiceItDoesNotOffer(t *testing.T) {
	e := newTunnelEnv(t)
	host := unique("tun-svc")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runAgent(t, cred, host)
	e.waitTunnel(t, host, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, svc := range []string{"shell", "exec", "127.0.0.1:22", "../ping", ""} {
		if _, err := e.api.Tunnels.Open(ctx, host, svc); !tunnel.IsRefused(err) {
			t.Errorf("service %q was not refused: %v", svc, err)
		}
	}
}

/* ── It works, and the console is told the truth ─────────────────────────── */

func TestATunnelWorksOverHTTPS(t *testing.T) {
	e := newTunnelEnv(t)
	host := unique("tun-https")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runAgent(t, cred, host)
	e.waitTunnel(t, host, tunnel.TransportUpgrade)

	e.api.Tunnels.mu.Lock()
	lt := e.api.Tunnels.byHost[strings.ToLower(host)]
	e.api.Tunnels.mu.Unlock()
	e.api.Tunnels.ping(lt)

	got := e.listed(t, host).Tunnel
	if !got.Connected || got.Transport != "tcp" || got.Replica != "test-replica" {
		t.Fatalf("listed as %+v", got)
	}
	if got.RTTMillis == nil {
		t.Error("no round trip was recorded")
	}
}

// Offered QUIC on the HTTPS fallback, the agent moves to it.
func TestAnAgentMovesToQUICWhenOffered(t *testing.T) {
	e := newTunnelEnv(t)
	e.withQUIC(t)
	host := unique("tun-quic")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runAgent(t, cred, host)
	e.waitTunnel(t, host, tunnel.TransportQUIC)

	if got := e.listed(t, host).Tunnel; !got.Connected || got.Transport != "quic" {
		t.Fatalf("listed as %+v", got)
	}
}

// A replica that stopped pinging is not believed.
func TestAStaleTunnelIsListedAsDown(t *testing.T) {
	tr, rep := "quic", "gone-replica"
	now := time.Now()
	old := now.Add(-tunnelFreshFor - time.Second)
	if st := tunnelStateFrom(&tr, &rep, &old, &old, nil, now); st.Connected {
		t.Error("a tunnel nobody has pinged for three intervals is listed as connected")
	}
	fresh := now.Add(-time.Second)
	if st := tunnelStateFrom(&tr, &rep, &old, &fresh, nil, now); !st.Connected {
		t.Error("a freshly pinged tunnel is listed as down")
	}
}
