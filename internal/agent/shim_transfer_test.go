//go:build !windows

package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gsoultan/argus/internal/recorder"
)

// transferSession runs command through the shim's no-terminal path with
// input on stdin, against a real collector, and returns the sealed record
// and what the recording holds.
func transferSession(t *testing.T, command string, input []byte) (SessionRecord, string, []byte) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "argus-xfer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")

	c := NewCollector(filepath.Join(dir, "rec"), nil)
	sealed := make(chan SessionRecord, 1)
	c.OnSession = func(r SessionRecord) { sealed <- r }
	if err := os.MkdirAll(filepath.Join(dir, "rec"), 0o700); err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Listen(sock) }()
	t.Cleanup(func() { _ = c.CloseWithin(time.Second) })
	waitSocket(t, sock)

	start := SessionStart{Type: "start", Principal: "deploy", Hostname: "h",
		ClientAddr: "10.0.0.167", Command: command, StartedAt: time.Now()}
	rc, why := openRecording(sock, start, restartWait, false)
	if rc == nil {
		t.Fatalf("no recording: %s", why)
	}
	st := newStream(rc, rc.d.sessionID)
	var out, errOut bytes.Buffer
	code := runWithoutPTY(exec.Command("/bin/sh", "-c", command), st, command,
		bytes.NewReader(input), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if conn, _ := st.close(code); conn != nil {
		defer conn.Close()
	}

	var rec SessionRecord
	select {
	case rec = <-sealed:
	case <-time.After(10 * time.Second):
		t.Fatal("the recording was never sealed")
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := recorder.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Verify(bytes.NewReader(body), rec.ChainHead); err != nil {
		t.Errorf("the recording does not verify against its chain head: %v", err)
	}
	return rec, string(body), out.Bytes()
}

// A deploy that copies a build to the server is recorded by what it was --
// the command, the text it printed, how many bytes went in -- and not by a
// copy of the build. Before, the payload went into the recording as JSON
// text, where binary data grows to about two and a half times its size: a
// 100 MB upload made a 257 MB recording.
func TestAnUploadIsCountedNotCopiedIntoTheRecording(t *testing.T) {
	payload := make([]byte, 8<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	rec, body, out := transferSession(t, "cat > /dev/null; echo deployed", payload)

	// The runner's login went straight to sshd, not through Argus. It is
	// still recorded, still marked as a bypass, and its commands still count.
	if rec.Origin != Direct {
		t.Errorf("origin %q; a login through sshd is a bypass", rec.Origin)
	}
	if flags := riskFlags(rec); !slices.Contains(flags, "bypassed-gateway") {
		t.Errorf("risk flags %v; the console shows this as bypassed-argus", flags)
	}
	if rec.ChainHead == "" || rec.Path == "" {
		t.Errorf("the bypassed session was not sealed as a recording: %+v", rec)
	}
	if rec.Bytes > 64<<10 {
		t.Errorf("an 8 MB upload left a %d byte recording; it should hold the "+
			"command and the counts, not the payload", rec.Bytes)
	}
	for _, want := range []string{
		`cat \u003e /dev/null; echo deployed`, // the command, as JSON writes it
		`"deployed\n"`,                        // what it printed
		"binary data sent to the server from here on is counted, not kept",
		"8388608 bytes sent to the server (8.0 MB)", // random bytes may open with a few that read as text
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the recording does not say %q:\n%s", want, body)
		}
	}
	if string(out) != "deployed\n" {
		t.Errorf("the client got %q; the session's own output must be untouched", out)
	}
}

// Binary output is counted the same way, and the client still gets every
// byte of it.
func TestADownloadIsCountedAndStillDelivered(t *testing.T) {
	_, body, out := transferSession(t, "head -c 3000000 /dev/urandom", nil)
	if len(out) != 3000000 {
		t.Fatalf("the client got %d bytes of 3000000", len(out))
	}
	if !strings.Contains(body, "3000000 bytes sent back by the server") {
		t.Errorf("the recording does not count the download:\n%.2000s", body)
	}
	if len(body) > 64<<10 {
		t.Errorf("a 3 MB download left %d bytes of recording", len(body))
	}
}

// Text -- a script piped to `bash -s`, a command's output -- is what an
// investigator reads, and is kept whole up to the limit.
func TestTextInASessionWithoutATerminalIsKept(t *testing.T) {
	script := []byte("echo one\necho two ✓\n")
	_, body, out := transferSession(t, "sh", script)
	if string(out) != "one\ntwo ✓\n" {
		t.Fatalf("output %q", out)
	}
	// The shell may print its two lines in one write or two; what matters is
	// that every byte of each direction is in the recording, in order.
	if got := castText(t, body, "i"); !strings.Contains(got, "echo one\necho two ✓\n") {
		t.Errorf("the recording lost the script: %q\n%s", got, body)
	}
	if got := castText(t, body, "o"); got != "one\ntwo ✓\n" {
		t.Errorf("the recording lost the output: %q\n%s", got, body)
	}
	if strings.Contains(body, "counted, not kept") {
		t.Errorf("text was treated as a transfer:\n%s", body)
	}
}

// scp names the file before it sends it. The name is kept; the file is not.
func TestTheTextBeforeABinaryTransferIsKept(t *testing.T) {
	_, body, _ := transferSession(t, `printf 'C0644 5 build.tar.gz\n'; printf '\037\213\010\000\000'`, nil)
	if !strings.Contains(body, `C0644 5 build.tar.gz\n`) {
		t.Errorf("the file name was lost:\n%s", body)
	}
	if !strings.Contains(body, "binary data sent back by the server") {
		t.Errorf("the binary part was not marked:\n%s", body)
	}
}

// castText joins the data of every event of one kind in a recording.
func castText(t *testing.T, body, kind string) string {
	t.Helper()
	var b strings.Builder
	for i, line := range strings.Split(body, "\n") {
		if i == 0 || line == "" {
			continue
		}
		var ev []any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || len(ev) != 3 {
			continue
		}
		if k, _ := ev[1].(string); k == kind {
			s, _ := ev[2].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestSplitTextWaitsForACharacterAReadCutInTwo(t *testing.T) {
	check := []byte("✓")
	text, rest, binary := splitText(append([]byte("ok "), check[:2]...))
	if binary || string(text) != "ok " || !bytes.Equal(rest, check[:2]) {
		t.Errorf("got %q %q %v", text, rest, binary)
	}
	if _, _, binary := splitText([]byte{'a', 0xff, 'b'}); !binary {
		t.Error("an invalid byte is binary")
	}
	if _, _, binary := splitText([]byte{'a', 0, 'b'}); !binary {
		t.Error("a NUL is binary")
	}
}

func TestRuneStartNeverCutsACharacter(t *testing.T) {
	p := []byte("aé✓")
	for n := 0; n <= len(p); n++ {
		if got := p[:runeStart(p, n)]; !utf8.Valid(got) {
			t.Errorf("cut at %d gave %q", n, got)
		}
	}
}
