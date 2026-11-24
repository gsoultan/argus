package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// silentPipe is a net.Pipe end as a tunnel stream.
type silentPipe struct{ net.Conn }

func (p silentPipe) SetDeadline(t time.Time) error { return p.Conn.SetDeadline(t) }

// fakeAgent is a tunnel whose agent end runs serve on every stream.
type fakeAgent struct {
	serve  func(tunnel.Stream)
	done   chan struct{}
	closed sync.Once
}

func newFakeAgent(serve func(tunnel.Stream)) *fakeAgent {
	return &fakeAgent{serve: serve, done: make(chan struct{})}
}

func (f *fakeAgent) OpenStream(context.Context) (tunnel.Stream, error) {
	select {
	case <-f.done:
		return nil, errors.New("closed")
	default:
	}
	a, b := net.Pipe()
	go f.serve(silentPipe{b})
	return silentPipe{a}, nil
}
func (f *fakeAgent) AcceptStream(ctx context.Context) (tunnel.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeAgent) Done() <-chan struct{}       { return f.done }
func (f *fakeAgent) Close() error                { f.closed.Do(func() { close(f.done) }); return nil }
func (f *fakeAgent) Transport() tunnel.Transport { return tunnel.TransportUpgrade }
func (f *fakeAgent) RemoteAddr() net.Addr        { return &net.TCPAddr{} }
func (f *fakeAgent) GrantKey() string            { return "" }

func hubWith(host string, sess tunnel.Session) *TunnelHub {
	h := NewTunnelHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	t := &liveTunnel{tenant: DefaultTenantID, host: host, sess: sess}
	h.byHost[tunnelKey(DefaultTenantID, host)] = t
	return h
}

// readHeader reads what the console sent and returns the service asked for.
func readHeader(st tunnel.Stream) string {
	var hd tunnel.Header
	if tunnel.ReadJSON(st, &hd) != nil {
		return ""
	}
	return hd.Service
}

// An agent whose tunnel has stalled answers nothing, ping included: the
// person is told so, and the tunnel is closed so the agent reconnects.
func TestAStalledTunnelIsClosedAndSaidSo(t *testing.T) {
	agent := newFakeAgent(func(st tunnel.Stream) {
		readHeader(st)
		_, _ = io.Copy(io.Discard, st)
	})
	h := hubWith("ip-10-0-2-147", agent)
	ctx, cancel := context.WithTimeout(WithTenant(context.Background(), &Tenant{ID: DefaultTenantID}), 200*time.Millisecond)
	defer cancel()
	_, _, err := h.OpenWith(ctx, "ip-10-0-2-147", tunnel.Header{Service: "postgres"})
	var s *AgentSilentError
	if !errors.As(err, &s) || s.Alive {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-agent.Done():
	default:
		t.Error("the stalled tunnel was left open")
	}
	msg := agentUnreachable("db-01", err)
	if !strings.Contains(msg, "the agent on db-01 stopped answering") || strings.Contains(msg, "i/o deadline") {
		t.Errorf("message = %q", msg)
	}
}

// An agent that answers a ping but is slow to start this one thing is up:
// the tunnel stays, and the person is told it is the step that is slow.
func TestASlowStepOnALiveAgentKeepsTheTunnel(t *testing.T) {
	agent := newFakeAgent(func(st tunnel.Stream) {
		if readHeader(st) != "ping" {
			_, _ = io.Copy(io.Discard, st)
			return
		}
		_ = tunnel.WriteJSON(st, tunnel.Answer{OK: true})
		buf := make([]byte, 64)
		n, _ := st.Read(buf)
		_, _ = st.Write(buf[:n])
	})
	h := hubWith("db-01", agent)
	ctx, cancel := context.WithTimeout(WithTenant(context.Background(), &Tenant{ID: DefaultTenantID}), 200*time.Millisecond)
	defer cancel()
	_, _, err := h.OpenWith(ctx, "db-01", tunnel.Header{Service: "postgres"})
	var s *AgentSilentError
	if !errors.As(err, &s) || !s.Alive {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-agent.Done():
		t.Error("a live agent's tunnel was closed")
	default:
	}
	if msg := agentUnreachable("db-01", err); !strings.Contains(msg, "connected but did not answer within") {
		t.Errorf("message = %q", msg)
	}
}

// Anything but a timeout reads as before.
func TestOtherOpenFailuresReadAsBefore(t *testing.T) {
	if got := agentUnreachable("db-01", io.ErrUnexpectedEOF); got != "could not reach the agent on db-01: unexpected EOF" {
		t.Errorf("got %q", got)
	}
}
