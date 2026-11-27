package secretclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/vault"
)

const cred = "argus-agent-test.0123456789abcdef0123456789abcdef"

type rig struct {
	client  *Client
	store   *vault.Store
	control *httptest.Server
	asked   *atomic.Int32
	down    *atomic.Bool
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newRig is a real argus-secret holding prod/db, whose rules let this host
// read prod/ and nothing else, and a stand-in argus-control saying where it is.
func newRig(t *testing.T) *rig {
	t.Helper()
	root := make([]byte, vault.KeySize)
	_, _ = rand.Read(root)
	store, err := vault.Open(t.TempDir(), root, "default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	rules := vault.NewRules(time.Hour)
	if err := rules.Replace(vault.Bundle{Org: "default", Serial: 1, Folders: []string{"prod", "dev"},
		Servers: []vault.ServerIdentity{{ID: "h1", Hostname: "db-01", CredentialHash: hash(cred)}},
		Rules:   []vault.Rule{{Path: "prod/", Subjects: []vault.Subject{{Kind: vault.SubjectServer, ID: "h1"}}, Can: []string{vault.CanRead}}}},
		"default"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(vault.PutInput{Name: "prod/db", Type: vault.TypeKV, Value: []byte(`{"user":"app","password":"hunter2"}`),
		Actor: vault.Actor{Kind: vault.ActorPerson, ID: "a@x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(vault.PutInput{Name: "dev/db", Type: vault.TypeText, Value: []byte("devpass"),
		Actor: vault.Actor{Kind: vault.ActorPerson, ID: "a@x"}}); err != nil {
		t.Fatal(err)
	}
	api, err := vault.NewAPI(vault.APIOptions{Store: store, Rules: rules, ControlKey: pub,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	vs := httptest.NewTLSServer(api.Handler())
	t.Cleanup(vs.Close)
	pin := tlsconfig.Pin(vs.Certificate())

	asked := &atomic.Int32{}
	down := &atomic.Bool{}
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/api/v1/report/secrets-servers" || r.Header.Get("Authorization") != "Bearer "+cred {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		asked.Add(1)
		_ = json.NewEncoder(w).Encode([]Server{{Name: "Vault", URL: vs.URL, TLSPin: pin, Folders: []string{"prod", "dev"}}})
	}))
	t.Cleanup(control.Close)
	now := time.Now()
	return &rig{client: &Client{ControlURL: control.URL, Credential: cred, StateDir: t.TempDir(),
		Now: func() time.Time { return now }}, store: store, control: control, asked: asked, down: down}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"prod/db":              {Name: "prod/db"},
		"prod/db#password":     {Name: "prod/db", Field: "password"},
		"prod/db#password@3":   {Name: "prod/db", Field: "password", Version: 3},
		"prod/db@previous":     {Name: "prod/db", Label: "previous"},
		" prod/payments/db@1 ": {Name: "prod/payments/db", Version: 1},
	} {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "#field", "/etc/passwd", "a b"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestReadsDirectlyAndOnlyWhatItMay(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	v, err := r.client.Read(ctx, Ref{Name: "prod/db", Field: "password"}, "test")
	if err != nil || string(v.Bytes) != "hunter2" || v.Version != 1 {
		t.Fatalf("read = %+v, %v", v, err)
	}
	// No rule for dev/: refused, and said the same way as missing.
	if _, err := r.client.Read(ctx, Ref{Name: "dev/db"}, "test"); err == nil || !strings.Contains(err.Error(), "may not read") {
		t.Errorf("dev/db = %v, want a refusal", err)
	}
	if _, err := r.client.Read(ctx, Ref{Name: "elsewhere/db"}, "test"); err == nil || !strings.Contains(err.Error(), "no secrets server") {
		t.Errorf("a folder no server keeps = %v", err)
	}
	// The read is in the server's log, with its purpose, and no value.
	items, err := r.store.LogAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ok, refused bool
	for _, it := range items {
		line := string(it.Line)
		if strings.Contains(line, "hunter2") {
			t.Fatal("a value is in the read log")
		}
		ok = ok || (strings.Contains(line, `"secret":"prod/db"`) && strings.Contains(line, `"outcome":"ok"`) && strings.Contains(line, `"reason":"test"`))
		refused = refused || (strings.Contains(line, `"secret":"dev/db"`) && strings.Contains(line, `"outcome":"refused"`))
	}
	if !ok || !refused {
		t.Errorf("log: read %v, refusal %v", ok, refused)
	}
}

func TestReadsWhileControlIsDown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.client.Read(ctx, Ref{Name: "prod/db", Field: "user"}, "test"); err != nil {
		t.Fatal(err)
	}
	// Past the refresh, with argus-control down: the kept list still works.
	later := time.Now().Add(time.Hour)
	r.client.Now = func() time.Time { return later }
	r.down.Store(true)
	v, err := r.client.Read(ctx, Ref{Name: "prod/db", Field: "user"}, "test")
	if err != nil || string(v.Bytes) != "app" {
		t.Fatalf("read with control down = %q, %v", v.Bytes, err)
	}
	// Never enrolled and never told: a clear error, not a guess.
	fresh := &Client{ControlURL: r.control.URL, Credential: cred, StateDir: t.TempDir()}
	if _, err := fresh.Read(ctx, Ref{Name: "prod/db"}, "test"); err == nil {
		t.Error("read with no list and control down succeeded")
	}
}

func TestRefusesAnAnswerForAnotherSecret(t *testing.T) {
	r := newRig(t)
	// A server (or something in the way) answering for prod/other.
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(secretwire.ReadResponse{Name: "prod/other", Version: 1, Sealed: []byte("x")})
	}))
	defer evil.Close()
	srvs := []Server{{Name: "Evil", URL: evil.URL, Folders: []string{"prod"}}}
	raw, _ := json.Marshal(keptServers{At: time.Now(), Servers: srvs})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, serversFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{Credential: cred, StateDir: dir, HTTP: func(string) (*http.Client, error) { return evil.Client(), nil }}
	if _, err := c.Read(context.Background(), Ref{Name: "prod/db"}, "test"); err == nil || !strings.Contains(err.Error(), "answered for") {
		t.Errorf("answer for another secret = %v", err)
	}
	_ = r
}

func TestSocketNamesItsCaller(t *testing.T) {
	r := newRig(t)
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := Listen(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	known := &atomic.Bool{}
	known.Store(true)
	s := &Socket{Client: r.client, PeerOf: func(net.Conn) (Peer, error) {
		return Peer{UID: 1001, PID: 4242, Known: known.Load()}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	c := SocketClient{Path: path}
	v, err := c.Read(context.Background(), Ref{Name: "prod/db", Field: "password"}, "deploy")
	if err != nil || string(v.Bytes) != "hunter2" {
		t.Fatalf("socket read = %q, %v", v.Bytes, err)
	}
	items, _ := r.store.LogAfter(0, 100)
	var named bool
	for _, it := range items {
		named = named || strings.Contains(string(it.Line), "socket uid=1001 pid=4242")
	}
	if !named {
		t.Error("the socket read does not name its caller in the log")
	}
	known.Store(false)
	if _, err := c.Read(context.Background(), Ref{Name: "prod/db"}, ""); err == nil {
		t.Error("a caller the kernel cannot name was answered")
	}
}
