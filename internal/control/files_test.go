package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
File transfer through an agent's tunnel. The refusals first: the grant opens
files only, never a shell, and a terminal grant never opens files. Then that
it works -- upload, list, download, the bytes intact -- and that each transfer
is in the audit chain with who, where, as whom, the path, the size and hash.

file-op runs as whoever runs the test: this binary is the helper, re-entered
through TestMain.
*/

func TestMain(m *testing.M) {
	if os.Getenv("ARGUS_TEST_FILE_OP") == "1" && len(os.Args) > 1 && os.Args[1] == "file-op" {
		os.Exit(agent.RunFileOp(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func (e *tunnelEnv) runFilesAgent(t *testing.T, cred, host string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	services := agent.DefaultTunnelServices()
	services[agent.TerminalService] = agent.Terminal(agent.TerminalConfig{
		Collector: agent.NewCollector(t.TempDir(), log),
		Verifier:  &tunnel.GrantVerifier{Host: host}, Hostname: host, Log: log,
		Spawn: agent.SpawnDirect,
	})
	services[agent.FilesService] = agent.Files(agent.FilesConfig{
		Verifier: &tunnel.GrantVerifier{Host: host}, Log: log,
		Spawn: agent.SpawnDirect, Helper: exe, HelperEnv: []string{"ARGUS_TEST_FILE_OP=1"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.RunTunnel(ctx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
}

func (e *tunnelEnv) openFiles(t *testing.T, target, principal, ticket string) *websocket.Conn {
	t.Helper()
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "wss"
	u.Path = "/api/v1/files"
	u.RawQuery = url.Values{"target": {target}, "principal": {principal}, "ticket": {ticket}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: e.srv.Client(),
		HTTPHeader: http.Header{"Origin": {e.srv.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func writeMsg(t *testing.T, c *websocket.Conn, m filesMessage) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// readMsg reads the next control message, gathering any binary data before it.
func readMsg(t *testing.T, c *websocket.Conn) (filesMessage, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var data []byte
	for {
		typ, p, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageBinary {
			data = append(data, p...)
			continue
		}
		var m filesMessage
		_ = json.Unmarshal(p, &m)
		if m.Type == "download-start" {
			continue
		}
		return m, data
	}
}

func TestAGrantOpensOnlyTheServiceItWasSignedFor(t *testing.T) {
	e := newTunnelEnv(t)
	e.withTerminal(t)
	host := unique("files-grant")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runFilesAgent(t, cred, host)

	open := func(service string, g tunnel.Grant) error {
		tok, _ := tunnel.SignGrant(e.api.Tunnels.GrantKey, g)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, _, err := e.api.Tunnels.OpenWith(ctx, host, tunnel.Header{Service: service, Grant: tok})
		if st != nil {
			_ = st.Close()
		}
		return err
	}
	terminalGrant := tunnel.NewGrant("ana@x", host, me(t), "")
	if err := open(agent.FilesService, terminalGrant); !tunnel.IsRefused(err) {
		t.Errorf("a terminal grant opened file transfer: %v", err)
	}
	filesGrant := tunnel.NewGrant("ana@x", host, me(t), "")
	filesGrant.Service = agent.FilesService
	if err := open(agent.TerminalService, filesGrant); !tunnel.IsRefused(err) {
		t.Errorf("a files grant opened a shell: %v", err)
	}
	ok := tunnel.NewGrant("ana@x", host, me(t), "")
	ok.Service = agent.FilesService
	if err := open(agent.FilesService, ok); err != nil {
		t.Errorf("the genuine files grant was refused: %v", err)
	}
}

func TestFilesMoveThroughTheTunnelAndAreAudited(t *testing.T) {
	e := newTunnelEnv(t)
	signer := e.withTerminal(t)
	host := unique("files-ok")
	cred, _ := enrolled(t, e.store, e.h, host)
	e.runFilesAgent(t, cred, host)

	dir := t.TempDir()
	ticket, _ := signer.IssueTicket("ana@example.com", host, me(t), time.Minute)
	c := e.openFiles(t, host, me(t), ticket)
	if m, _ := readMsg(t, c); m.Type != "ready" {
		t.Fatalf("not ready: %+v", m)
	}

	// Upload, in more than one chunk.
	body := []byte(strings.Repeat("argus file transfer\n", 20000))
	want := sha256.Sum256(body)
	target := filepath.Join(dir, "report.csv")
	writeMsg(t, c, filesMessage{Type: "upload", Path: target})
	if m, _ := readMsg(t, c); m.Type != "upload-ready" || m.Path != target {
		t.Fatalf("upload not ready: %+v", m)
	}
	for p := body; len(p) > 0; {
		n := min(len(p), tunnel.FileChunk)
		if err := c.Write(context.Background(), websocket.MessageBinary, p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	writeMsg(t, c, filesMessage{Type: "upload-end"})
	done, _ := readMsg(t, c)
	if done.Type != "upload-done" || done.Size != int64(len(body)) || done.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("upload ended as %+v", done)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(body) {
		t.Fatalf("the file on the host is not what was sent: %v", err)
	}

	// The same name again is refused without overwrite, and the operation
	// failing does not end the file browser.
	writeMsg(t, c, filesMessage{Type: "upload", Path: target})
	if m, _ := readMsg(t, c); m.Type != "upload-ready" {
		t.Fatalf("second upload: %+v", m)
	}
	_ = c.Write(context.Background(), websocket.MessageBinary, []byte("clobber"))
	writeMsg(t, c, filesMessage{Type: "upload-end"})
	if m, _ := readMsg(t, c); m.Type != "error" || !strings.Contains(m.Data, "already exists") {
		t.Fatalf("an existing file was replaced without overwrite: %+v", m)
	}
	if got, _ := os.ReadFile(target); string(got) != string(body) {
		t.Fatal("the refused upload changed the file")
	}

	// List.
	writeMsg(t, c, filesMessage{Type: "list", Path: dir})
	list, _ := readMsg(t, c)
	if list.Type != "listing" || len(list.Entries) != 1 || list.Entries[0].Name != "report.csv" {
		t.Fatalf("listing = %+v", list)
	}

	// Download.
	writeMsg(t, c, filesMessage{Type: "download", Path: target})
	end, data := readMsg(t, c)
	if end.Type != "download-end" || string(data) != string(body) || end.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("download ended as %+v with %d bytes", end, len(data))
	}
	writeMsg(t, c, filesMessage{Type: "download", Path: filepath.Join(dir, "missing")})
	if m, _ := readMsg(t, c); m.Type != "error" {
		t.Fatalf("a missing file downloaded: %+v", m)
	}

	// The chain names who, where, as whom, what and its hash.
	events, err := e.store.AuditEvents(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var up, down bool
	for _, ev := range events {
		if ev.Target != host || ev.ActorEmail != "ana@example.com" {
			continue
		}
		has := strings.Contains(ev.Detail, target) && strings.Contains(ev.Detail, me(t)) &&
			strings.Contains(ev.Detail, hex.EncodeToString(want[:]))
		switch {
		case ev.Action == "file.upload" && ev.Severity == "notice":
			up = up || has
		case ev.Action == "file.download" && ev.Severity == "notice":
			down = down || has
		}
	}
	if !up || !down {
		t.Errorf("audit chain: upload %v, download %v", up, down)
	}
}
