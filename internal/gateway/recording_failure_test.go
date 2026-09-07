package gateway

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/live"
	"github.com/gsoultan/argus/internal/recorder"
)

/*
What happens when the recording stops working.

"Fail closed when recording is unavailable" is the console's phrasing of the
claim an auditor actually cares about: that every privileged session is
recorded. Until now it was a switch that was stored, displayed, audited -- and
read by nothing. A session whose recorder died carried on, unrecorded, and was
reported afterwards as an ordinary PTY recording.
*/

// brokenWriter starts working and stops, standing in for a disk that fills or a
// volume that goes read-only part-way through a session. It has to accept the
// header, because a recorder that could never be created is the start-up case,
// not the mid-session one under test here.
type brokenWriter struct {
	broken bool
	used   int
}

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.broken {
		return 0, errors.New("no space left on device")
	}
	b.used += len(p)
	return len(p), nil
}

// testServer is the smallest Server these tests can run against. It carries a
// real host key store because riskFlags consults one, and New() -- which these
// tests deliberately bypass -- is what normally guarantees it is present.
func testServer(t *testing.T, dir string) *Server {
	t.Helper()
	keys, err := hostkey.Open(filepath.Join(t.TempDir(), "known_hosts"), false)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg:      Config{RecordingDir: dir, HostKeys: keys},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: map[string]*Session{},
	}
}

// sessionWithRecorder builds a Session whose recorder writes to w.
func sessionWithRecorder(t *testing.T, w *brokenWriter, policy Policy) *Session {
	t.Helper()
	srv := testServer(t, t.TempDir())
	sess := &Session{
		ID: "sess-recording", User: "lin@northwind.id", Principal: "ops",
		Target: Asset{Hostname: "db-01"}, StartedAt: time.Now().UTC(),
		srv: srv, hub: live.NewHub(), log: srv.log, pol: policy,
	}
	rec, err := recorder.New(w, recorder.Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	sess.rec = rec
	// Everything above succeeded; from here the disk is full.
	w.broken = true
	return sess
}

// The default. A recorder that stops working ends the session.
func TestRecordingLossTerminatesTheSessionWhenPolicyFailsClosed(t *testing.T) {
	sess := sessionWithRecorder(t, &brokenWriter{}, DefaultPolicy())

	if err := sess.record(recorder.Output, []byte("some output")); err == nil {
		t.Fatal("the broken writer should have failed the record")
	}
	sess.recordingLost(errors.New("no space left on device"))

	by, reason, killed := sess.Killed()
	if !killed {
		t.Fatal("a session that cannot be recorded must not be left running")
	}
	if by != "argus" {
		t.Errorf("terminated by %q, want argus", by)
	}
	if !strings.Contains(reason, "recording") {
		t.Errorf("the reason should say why: %q", reason)
	}
}

// Policy can choose availability over evidence. The session survives, but
// nothing pretends its recording is usable.
func TestRecordingLossCanBeSurvivedWhenPolicySaysSo(t *testing.T) {
	open := DefaultPolicy()
	open.FailClosedOnRecordingLoss = false
	sess := sessionWithRecorder(t, &brokenWriter{}, open)

	sess.recordingLost(errors.New("no space left on device"))

	if _, _, killed := sess.Killed(); killed {
		t.Error("policy does not fail closed, so the session should continue")
	}
	if !sess.recordingIsBroken() {
		t.Error("the session must still be marked as having lost its recording")
	}
}

// Whatever policy decides, the artefact must not be described as a working
// recording afterwards.
func TestABrokenRecordingIsNotReportedAsPTY(t *testing.T) {
	sess := sessionWithRecorder(t, &brokenWriter{}, DefaultPolicy())
	if got := sess.reportedFidelity(); got != "pty" {
		t.Fatalf("a healthy session reports %q, want pty", got)
	}

	sess.recordingLost(errors.New("disk full"))
	if got := sess.reportedFidelity(); got != "none" {
		t.Errorf("fidelity = %q, want none -- there is no usable replay", got)
	}
	flags := strings.Join(sess.riskFlags(), ",")
	if !strings.Contains(flags, "recording-incomplete") {
		t.Errorf("risk flags should mark the gap: %s", flags)
	}
}

// Both relay directions can hit the failure at once; it must be handled once.
func TestRecordingLossIsHandledOnlyOnce(t *testing.T) {
	sess := sessionWithRecorder(t, &brokenWriter{}, DefaultPolicy())
	sess.recordingLost(errors.New("first"))
	if _, _, killed := sess.Killed(); !killed {
		t.Fatal("precondition: the first failure terminates")
	}
	// Terminate returns false on a session already ended; the second call must
	// not panic, re-audit, or overwrite the recorded reason.
	_, firstReason, _ := sess.Killed()
	sess.recordingLost(errors.New("second"))
	_, secondReason, _ := sess.Killed()
	if firstReason != secondReason {
		t.Errorf("the termination reason changed on a second report: %q -> %q",
			firstReason, secondReason)
	}
}

// A session that runs to the end with a working recorder leaves a real file.
func TestAHealthyRecordingIsUntouchedByAnyOfThis(t *testing.T) {
	dir := t.TempDir()
	srv := testServer(t, dir)
	sess := &Session{
		ID: "sess-ok", User: "lin@northwind.id", Principal: "ops",
		Target: Asset{Hostname: "db-01"}, StartedAt: time.Now().UTC(),
		srv: srv, hub: live.NewHub(), log: srv.log, pol: DefaultPolicy(),
	}
	if err := sess.openRecording(dir); err != nil {
		t.Fatal(err)
	}
	if err := sess.record(recorder.Output, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, _, killed := sess.Killed(); killed {
		t.Error("a working recorder must not terminate anything")
	}
	if sess.recordingIsBroken() {
		t.Error("a working recorder must not be marked broken")
	}
	if _, err := os.Stat(filepath.Join(dir, "sess-ok.cast")); err != nil {
		t.Errorf("no recording was written: %v", err)
	}
	if _, err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// The relay loop is where a real failure arrives. Recording each chunk happens
// before it is forwarded, so a session that cannot be recorded also stops
// moving data -- the ordering matters and this pins it.
func TestRelayEndsTheSessionWhenItCannotRecord(t *testing.T) {
	w := &brokenWriter{}
	sess := sessionWithRecorder(t, w, DefaultPolicy())

	src := io.NopCloser(strings.NewReader("uname -a\nrm -rf /var\n"))
	var forwarded strings.Builder
	sess.relay(src, &forwarded, recorder.Output)

	if _, _, killed := sess.Killed(); !killed {
		t.Error("relay saw the recorder fail and let the session live")
	}
	if forwarded.Len() != 0 {
		t.Errorf("unrecorded bytes were forwarded anyway: %q", forwarded.String())
	}
}

// The same relay, with a recorder that works, must be transparent.
func TestRelayForwardsEverythingWhenRecordingWorks(t *testing.T) {
	var sink strings.Builder
	sess := sessionWithRecorder(t, &brokenWriter{}, DefaultPolicy())
	// Un-break the writer: this case is the healthy one.
	rec, err := recorder.New(&sink, recorder.Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	sess.rec = rec

	const payload = "uname -a\n"
	var forwarded strings.Builder
	sess.relay(io.NopCloser(strings.NewReader(payload)), &forwarded, recorder.Output)

	if forwarded.String() != payload {
		t.Errorf("relay forwarded %q, want %q", forwarded.String(), payload)
	}
	if _, _, killed := sess.Killed(); killed {
		t.Error("a working recording must not end the session")
	}
	if !strings.Contains(sink.String(), "uname -a") {
		t.Error("the recording does not contain what passed through")
	}
}
