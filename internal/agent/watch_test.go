package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Watching a session from the agent's own recording: the console's Watch live
for a login through sshd, which the control plane never relays.
*/

// watchCall runs the watch service on one end of a pipe and returns the
// other, after the agent's answer.
func watchCall(t *testing.T, c *Collector, key ed25519.PrivateKey, g tunnel.Grant) (net.Conn, tunnel.Answer) {
	t.Helper()
	grant, err := tunnel.SignGrant(key, g)
	if err != nil {
		t.Fatal(err)
	}
	agentEnd, cpEnd := net.Pipe()
	t.Cleanup(func() { _ = cpEnd.Close() })
	svc := Watch(WatchConfig{Collector: c, Verifier: &tunnel.GrantVerifier{Host: "db-01"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go svc(context.Background(), &Call{
		Header:   tunnel.Header{Service: WatchService, Grant: grant},
		Stream:   agentEnd,
		GrantKey: key.Public().(ed25519.PublicKey),
	})
	_ = cpEnd.SetDeadline(time.Now().Add(5 * time.Second))
	var ans tunnel.Answer
	if err := tunnel.ReadJSON(cpEnd, &ans); err != nil {
		t.Fatalf("no answer: %v", err)
	}
	return cpEnd, ans
}

func watchGrant(session, principal string) tunnel.Grant {
	g := tunnel.NewGrant("auditor@example.com", "db-01", principal, "10.0.0.1")
	g.Service, g.Session = WatchService, session
	return g
}

// A direct login is reported as running when it is let in, and a viewer
// sees its output -- what came before they arrived and what follows -- at
// its size, then its end.
func TestADirectLoginCanBeWatchedWhileItRuns(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetDirectSSH(DirectSSHAllow)
	active := make(chan SessionRecord, 4)
	c.OnActive = func(r SessionRecord) { active <- r }
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }
	_, key, _ := ed25519.GenerateKey(rand.Reader)

	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if !r.Record || r.SessionID == "" {
		t.Fatalf("not recorded: %+v", r)
	}
	select {
	case rec := <-active:
		if rec.ID != r.SessionID || rec.State != "active" || rec.Origin != Direct ||
			rec.DirectApproval != "allowed" {
			t.Fatalf("active report = %+v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a direct login was not reported as running")
	}
	if got := c.ActiveTunnelRecords(); len(got) != 1 || got[0].ID != r.SessionID {
		t.Fatalf("not repeated as running: %+v", got)
	}

	enc := json.NewEncoder(conn)
	_ = enc.Encode(Frame{Type: "o", Data: "before-the-viewer\r\n"})
	_ = enc.Encode(Frame{Type: "r", Data: "132x40"})
	_ = enc.Encode(Frame{Type: "i", Data: "typed-secret"})

	// A grant for another account, or another service, opens nothing.
	if _, ans := watchCall(t, c, key, watchGrant(r.SessionID, "root")); ans.OK {
		t.Error("a grant for another account opened the view")
	}
	other := watchGrant(r.SessionID, "ops")
	other.Service = FilesService
	if _, ans := watchCall(t, c, key, other); ans.OK {
		t.Error("a files grant opened the view")
	}

	// The writes above reach the recording asynchronously; wait for the
	// backlog to hold them before the viewer arrives.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, _ := c.watching(r.SessionID)
		if cols, _ := s.size(); cols == 132 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	v, ans := watchCall(t, c, key, watchGrant(r.SessionID, "ops"))
	if !ans.OK || ans.Session != r.SessionID {
		t.Fatalf("watch refused: %+v", ans)
	}
	_ = enc.Encode(Frame{Type: "o", Data: "after-the-viewer\r\n"})

	var out strings.Builder
	var sizes []string
	fr := tunnel.NewFrameReader(v)
	ended := false
	for !ended {
		typ, p, err := fr.Next()
		if err != nil {
			t.Fatalf("stream ended without the session's end: %v (saw %q)", err, out.String())
		}
		switch typ {
		case tunnel.FrameOutput:
			out.Write(p)
			if strings.Contains(out.String(), "after-the-viewer") {
				_ = enc.Encode(SessionEnd{Type: "end"})
			}
		case tunnel.FrameResize:
			cols, rows, _ := tunnel.ParseResize(p)
			sizes = append(sizes, sizeOf(cols, rows))
		case tunnel.FrameExit:
			ended = true
		}
	}
	got := out.String()
	if !strings.Contains(got, "before-the-viewer") || !strings.Contains(got, "after-the-viewer") {
		t.Errorf("the viewer saw %q", got)
	}
	if strings.Contains(got, "typed-secret") {
		t.Error("the viewer was sent keystrokes")
	}
	if len(sizes) == 0 || sizes[0] != "132x40" {
		t.Errorf("sizes = %v, want 132x40 first", sizes)
	}
	select {
	case <-sealed:
	case <-time.After(3 * time.Second):
		t.Fatal("not sealed")
	}
	if got := c.ActiveTunnelRecords(); len(got) != 0 {
		t.Errorf("an ended login is still reported as running: %+v", got)
	}

	// Once it has ended there is nothing to watch.
	if _, ans := watchCall(t, c, key, watchGrant(r.SessionID, "ops")); ans.OK {
		t.Error("an ended session opened a view")
	}
}

func sizeOf(cols, rows int) string {
	b, _ := json.Marshal(cols)
	c, _ := json.Marshal(rows)
	return string(b) + "x" + string(c)
}

func TestParseEvent(t *testing.T) {
	for _, tc := range []struct {
		line   string
		stream byte
		data   string
		ok     bool
	}{
		{`[0.5,"o","hi\r\n\u001b[0m"]`, 'o', "hi\r\n\x1b[0m", true},
		{`[1.25,"r","120x30"]`, 'r', "120x30", true},
		{`[2,"i","secret"]`, 'i', "", true},
		{`[3,"x",{"pid":1}]`, 'x', "", true},
		{`{"version":2}`, 0, "", false},
		{`[1,"o",`, 0, "", false},
	} {
		stream, data, ok := parseEvent([]byte(tc.line))
		if ok != tc.ok || stream != tc.stream || string(data) != tc.data {
			t.Errorf("%s: got %q %q %v", tc.line, stream, data, ok)
		}
	}
}
