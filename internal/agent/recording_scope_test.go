package agent

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"
)

/*
The recording scope: "every login" or "Argus sessions only".

What these hold is the refusal side. A direct login goes unrecorded only when
the console asked for it, never by default or because a value could not be
read; a brokered session is recorded whatever the scope; and a login that is
let through is always reported.
*/

func startAs(t *testing.T, sock string, origin Origin) (net.Conn, *json.Decoder) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(SessionStart{
		Type: "start", Principal: "ops", Hostname: "db-01", Origin: origin,
		ClientAddr: "10.0.0.9", StartedAt: time.Now().UTC(), Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	return conn, json.NewDecoder(bufio.NewReader(conn))
}

func readReply(t *testing.T, conn net.Conn, dec *json.Decoder) Reply {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var r Reply
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("no answer to the start: %v", err)
	}
	return r
}

func recordings(t *testing.T, c *Collector) int {
	t.Helper()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// An agent that has not heard from the console records a direct login.
func TestADirectLoginIsRecordedUntilTheConsoleSaysOtherwise(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.OnUnrecorded = func(SessionStart) { t.Error("a login was let through with no scope set") }

	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if r.Type != "decision" || !r.Record || r.SessionID == "" {
		t.Fatalf("decision = %+v, want record with a session id", r)
	}
	if recordings(t, c) != 1 {
		t.Error("no recording was opened for a direct login under the default scope")
	}
}

func TestArgusOnlyLetsADirectLoginThroughAndReportsIt(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetScope(ScopeArgusOnly)
	reported := make(chan SessionStart, 1)
	c.OnUnrecorded = func(m SessionStart) { reported <- m }

	conn, dec := startAs(t, sock, Direct)
	r := readReply(t, conn, dec)
	if r.Type != "decision" || r.Record {
		t.Fatalf("decision = %+v, want record:false", r)
	}
	if r.Reason == "" {
		t.Error("the user is not told why the login is not recorded")
	}
	select {
	case m := <-reported:
		if m.Principal != "ops" || m.ClientAddr != "10.0.0.9" {
			t.Errorf("report lost who and where: %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a login let through unrecorded was not reported")
	}
	if n := recordings(t, c); n != 0 {
		t.Errorf("%d recordings opened for a login the scope skips", n)
	}
}

// "Argus sessions only" still records the Argus sessions.
func TestArgusOnlyStillRecordsABrokeredSession(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetScope(ScopeArgusOnly)
	c.OnUnrecorded = func(SessionStart) { t.Error("a brokered session was let through unrecorded") }

	conn, dec := startAs(t, sock, Brokered)
	if r := readReply(t, conn, dec); !r.Record {
		t.Fatalf("decision = %+v, want record", r)
	}
	if recordings(t, c) != 1 {
		t.Error("no recording was opened for a brokered session")
	}
}

// A value this agent cannot read must not be what turns recording off.
func TestAnUnknownScopeRecordsEverything(t *testing.T) {
	for _, s := range []string{"", "ARGUS_ONLY", "argus-only", "none", "off"} {
		if got := ParseScope(s); got != ScopeEveryLogin {
			t.Errorf("ParseScope(%q) = %q, want every_login", s, got)
		}
	}
	c := NewCollector(t.TempDir(), nil)
	c.SetScope("nonsense")
	if c.Scope() != ScopeEveryLogin {
		t.Errorf("Scope() = %q after an unknown value", c.Scope())
	}
}

// The shim's side: what each answer, or the lack of one, leads to.
func TestTheShimReadsTheDecision(t *testing.T) {
	answer := func(lines ...string) decision {
		t.Helper()
		a, b := net.Pipe()
		defer a.Close()
		go func() {
			for _, l := range lines {
				_, _ = b.Write([]byte(l + "\n"))
			}
			if len(lines) > 0 {
				_ = b.Close()
			}
		}()
		return awaitDecision(a, json.NewDecoder(bufio.NewReader(a)), 200*time.Millisecond)
	}

	if d := answer(`{"type":"decision","record":true,"session_id":"x"}`); d.refused || d.skip {
		t.Errorf("record answer gave %+v", d)
	}
	if d := answer(`{"type":"decision","record":false,"reason":"why"}`); !d.skip || d.reason != "why" {
		t.Errorf("skip answer gave %+v", d)
	}
	// A daemon that could not open the recording: the fail-closed path.
	if d := answer(`{"error":"disk full"}`); !d.refused {
		t.Errorf("an open failure gave %+v, want refused", d)
	}
	// The daemon hung up without answering: also fail-closed.
	a, b := net.Pipe()
	_ = b.Close()
	if d := awaitDecision(a, json.NewDecoder(a), 200*time.Millisecond); !d.refused {
		t.Errorf("a closed connection gave %+v, want refused", d)
	}
	// An older daemon says nothing until the end and is recording: stream.
	a, b = net.Pipe()
	defer b.Close()
	if d := awaitDecision(a, json.NewDecoder(a), 100*time.Millisecond); d.refused || d.skip {
		t.Errorf("silence gave %+v, want record", d)
	}
}

// Console-only refuses a direct login outright, whatever the recording scope,
// and reports it. The reason travels as an error too, so a shim older than
// the refusal still fails closed instead of letting the login through.
func TestConsoleOnlyRefusesADirectLoginAndReportsIt(t *testing.T) {
	for _, scope := range []RecordingScope{ScopeEveryLogin, ScopeArgusOnly} {
		c, sock := testCollector(t)
		c.SetScope(scope)
		c.SetConsoleOnly(true)
		refused := make(chan SessionStart, 1)
		c.OnRefused = func(m SessionStart) { refused <- m }
		c.OnUnrecorded = func(SessionStart) { t.Error("a refused login was reported as let through") }

		conn, dec := startAs(t, sock, Direct)
		r := readReply(t, conn, dec)
		if !r.Refused || r.Record {
			t.Fatalf("%s: decision = %+v, want refused", scope, r)
		}
		if r.Error == "" {
			t.Errorf("%s: no error for a shim that predates the refusal", scope)
		}
		select {
		case m := <-refused:
			if m.Principal != "ops" || m.ClientAddr != "10.0.0.9" {
				t.Errorf("report lost who and where: %+v", m)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: a refused login was not reported", scope)
		}
		if n := recordings(t, c); n != 0 {
			t.Errorf("%s: %d recordings opened for a refused login", scope, n)
		}
		c.CloseWithin(time.Second)
	}
}

// A session the gateway's browser terminal opened is an Argus session.
func TestConsoleOnlyStillAdmitsABrokeredSession(t *testing.T) {
	c, sock := testCollector(t)
	defer c.CloseWithin(time.Second)
	c.SetConsoleOnly(true)
	c.OnRefused = func(SessionStart) { t.Error("a brokered session was refused") }

	conn, dec := startAs(t, sock, Brokered)
	if r := readReply(t, conn, dec); !r.Record || r.Refused {
		t.Fatalf("decision = %+v, want record", r)
	}
}

// The shim reads a refusal as "not allowed", not as a recording failure that
// FailOpen could wave through.
func TestShimReadsARefusalAsConsoleOnly(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_ = json.NewEncoder(b).Encode(Decision{Type: "decision", Refused: true,
			Reason: ConsoleOnlyReason, Error: ConsoleOnlyReason})
	}()
	d := awaitDecision(a, json.NewDecoder(a), 3*time.Second)
	if !d.consoleOnly || d.refused || d.skip {
		t.Fatalf("decision = %+v, want consoleOnly", d)
	}
	if d.reason == "" {
		t.Error("the user is not told to use the console")
	}
}
