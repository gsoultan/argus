package gateway

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

/*
The RDP listener shuts down the way the SSH one does.

Every fault fixed on the SSH side had a twin here, and one of them was worse:
main closes this listener first, so a single open desktop held the entire
shutdown -- including the SSH drain that seals SSH recordings -- until systemd
sent SIGKILL.
*/

func rdpDrainServer(t *testing.T) *RDPServer {
	t.Helper()
	return &RDPServer{
		srv:      testServer(t, t.TempDir()),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]*rdp.Session{},
		targets:  map[string]net.Conn{},
		clients:  map[string]net.Conn{},
	}
}

// heldRDPSession occupies the wait group until its connections are closed,
// which is what a real relay does.
func heldRDPSession(t *testing.T, s *RDPServer, id string) (*rdp.Session, net.Conn, net.Conn) {
	t.Helper()
	sess := &rdp.Session{
		ID: id, User: "lin@northwind.id", Principal: "administrator",
		Target: "win-01", StartedAt: time.Now().UTC(),
	}
	// Real sockets over loopback, not net.Pipe. A pipe write blocks until the
	// far end reads, so a deadline error there is indistinguishable from a
	// closed connection -- which made the first version of this test pass
	// against the very bug it was written to catch.
	clientA, clientB := tcpPair(t)
	targetA, targetB := tcpPair(t)
	t.Cleanup(func() { _ = clientB.Close(); _ = targetB.Close() })

	s.track(sess)
	s.setClient(id, clientA)
	s.setTarget(id, targetA)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.untrack(sess)
		// Blocked on the operator's connection, exactly like handleConn.
		buf := make([]byte, 1)
		_, _ = clientA.Read(buf)
	}()
	return sess, clientB, targetB
}

// tcpPair returns two ends of a real connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return a, r.c
}

// A desktop that will not close on its own is ended, so the drain completes.
func TestRDPShutdownEndsDesktopsThatOutlastTheDrain(t *testing.T) {
	s := rdpDrainServer(t)
	sess, _, _ := heldRDPSession(t, s, "rdp-held")

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.CloseWithin(50 * time.Millisecond) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("rdp shutdown never returned; one open desktop would hold the " +
			"whole gateway until SIGKILL")
	}
	// Promptly, not eventually. Returning only after the full seal grace means
	// the sessions never actually unwound -- the drain gave up rather than
	// working, and on a real gateway the recordings would be unsealed.
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown took %s; the sessions did not unwind, the grace "+
			"period merely expired", d)
	}
	if _, _, killed := sess.Killed(); !killed {
		t.Error("the session was left running by a shutdown")
	}
}

// Closing only the target leaves the operator connected and handleConn blocked.
// Both ends have to go.
func TestRDPDisconnectClosesBothEnds(t *testing.T) {
	s := rdpDrainServer(t)
	_, client, target := heldRDPSession(t, s, "rdp-both")

	s.disconnect("rdp-both")

	// Read from the far ends. A closed connection returns EOF or a reset;
	// an open one blocks until the deadline, and the two are distinguishable.
	for name, c := range map[string]net.Conn{"client": client, "target": target} {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err := c.Read(make([]byte, 1))
		if err == nil {
			t.Errorf("the %s connection is still open after disconnect", name)
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Errorf("the %s connection was never closed by disconnect", name)
		}
	}
}

// An idle listener stops immediately rather than sitting out the window.
func TestRDPShutdownIsImmediateWhenIdle(t *testing.T) {
	s := rdpDrainServer(t)
	start := time.Now()
	if err := s.CloseWithin(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("an idle rdp shutdown took %s", d)
	}
}

// disconnect on an unknown id must not panic: a terminate can race the session
// ending on its own.
func TestRDPDisconnectOnAnUnknownSessionIsHarmless(t *testing.T) {
	s := rdpDrainServer(t)
	s.disconnect("never-existed")
	s.clearClient("never-existed")
}
