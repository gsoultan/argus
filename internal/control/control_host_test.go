package control

import (
	"context"

	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/*
argus-control looking after its own server, through the helper its package
ships (control_host.go).

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run ControlHost
*/

// controlHostFixture is a control plane asking the helper on host to enroll.
func controlHostFixture(t *testing.T) (*API, *Store, http.Handler, ControlHostOptions) {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	host := unique("ctl-host") + ".example"
	cleanEnrollment(t, s, host)
	opts := ControlHostOptions{
		Dir: t.TempDir(), Hostname: host, ControlURL: "https://127.0.0.1:8080",
		Pin: func() (string, error) { return "sha256//pin", nil },
	}
	return a, s, h, opts
}

func readRequest(t *testing.T, opts ControlHostOptions) ControlHostRequest {
	t.Helper()
	path := filepath.Join(opts.Dir, ControlHostRequestFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no request for the helper: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the request holds a token and is %v; want 0600", st.Mode().Perm())
	}
	r, err := readControlHostRequest(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The whole round: asked, enrolled, in the inventory and marked, audited
// with argus-control as the actor, and the token gone from disk afterwards.
func TestTheControlHostEnrollsItselfAndJoinsTheInventory(t *testing.T) {
	a, s, h, opts := controlHostFixture(t)
	ctx := context.Background()

	if err := a.controlHostTick(ctx, opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	req := readRequest(t, opts)
	if req.Hostname != opts.Hostname || req.ControlURL != opts.ControlURL || req.Pin != "sha256//pin" ||
		!strings.HasPrefix(req.Token, enrollTokenPrefix) {
		t.Fatalf("request: %+v", req)
	}
	if e := lastAuditOn(t, s, opts.Hostname); e.Action != "agent.enroll_token.created" ||
		e.ActorEmail != controlHostCreator {
		t.Errorf("minting the token should be audited as argus-control's: %+v", e)
	}

	// A second look keeps the same token: one a day reaches the audit chain.
	if err := a.controlHostTick(ctx, opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	if again := readRequest(t, opts); again.Token != req.Token {
		t.Errorf("a still-good request was replaced")
	}

	w := enroll(t, h, req.Token, opts.Hostname)
	if w.Code != http.StatusOK {
		t.Fatalf("the helper's enrollment was refused: %d %s", w.Code, w.Body)
	}
	e := lastAuditOn(t, s, opts.Hostname)
	if e.Action != "agent.enrolled" || e.ActorEmail != controlHostCreator ||
		!strings.Contains(e.Detail, "argus-control's own server") ||
		!strings.Contains(e.Detail, "Plain SSH to it is not gated") ||
		!strings.Contains(e.Detail, "added to the inventory") {
		t.Errorf("enrollment entry: %s %s %s", e.Action, e.ActorEmail, e.Detail)
	}
	asset := assetNamed(t, s, opts.Hostname)
	if !asset.ControlHost {
		t.Errorf("the control server is in the inventory but not marked as such: %+v", asset)
	}

	if err := a.controlHostTick(ctx, opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, ControlHostRequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the token stayed on disk after the helper enrolled: %v", err)
	}
}

// A token made for the helper is only for this host's name.
func TestTheControlHostTokenIsForItsOwnHostOnly(t *testing.T) {
	a, s, h, opts := controlHostFixture(t)
	if err := a.controlHostTick(context.Background(), opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	other := unique("ctl-other")
	cleanEnrollment(t, s, other)
	if w := enroll(t, h, readRequest(t, opts).Token, other); w.Code != http.StatusUnauthorized {
		t.Fatalf("another host enrolled with the control server's token: %d %s", w.Code, w.Body)
	}
}

// A host already enrolled -- the argus-agent package installed by hand --
// is never handed a token, and an expiring request is replaced.
func TestTheControlHostIsNotAskedWhenAlreadyEnrolledAndRenewsAnOldRequest(t *testing.T) {
	a, s, h, opts := controlHostFixture(t)
	ctx := context.Background()

	if err := a.controlHostTick(ctx, opts, time.Now().Add(controlHostTokenTTL-time.Minute)); err != nil {
		t.Fatal(err)
	}
	first := readRequest(t, opts)
	if err := a.controlHostTick(ctx, opts, time.Now().Add(controlHostTokenTTL-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if readRequest(t, opts).Token == first.Token {
		t.Errorf("a request about to expire was kept")
	}

	_ = os.Remove(filepath.Join(opts.Dir, ControlHostRequestFile))
	enrolled(t, s, h, opts.Hostname)
	if err := a.controlHostTick(ctx, opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(opts.Dir, ControlHostRequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a token was minted for a host that already has an agent")
	}
}

// The control server cannot be removed, nor its helper updated or restarted
// from the console: it moves with argus-control.
func TestTheControlHostIsNotRemovedOrUpdatedFromTheConsole(t *testing.T) {
	a, s, h, opts := controlHostFixture(t)
	if err := a.controlHostTick(context.Background(), opts, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := enroll(t, h, readRequest(t, opts).Token, opts.Hostname); w.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", w.Code, w.Body)
	}
	asset := assetNamed(t, s, opts.Hostname)

	w := do(t, h, http.MethodDelete, "/api/v1/assets/"+asset.ID, "admin-token", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "control_host: false") {
		t.Fatalf("removing the control server: %d %s", w.Code, w.Body)
	}
	if back := assetNamed(t, s, opts.Hostname); back.ID != asset.ID {
		t.Fatalf("the control server left the inventory")
	}

	w = do(t, h, http.MethodPost, "/api/v1/assets/"+asset.ID+"/agent/update", "admin-token",
		`{"version":"99.0.0"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "updates and restarts with argus-control") {
		t.Fatalf("updating the control server's helper: %d %s", w.Code, w.Body)
	}
	if _, err := s.RequestAgentUpdate(context.Background(), opts.Hostname, "99.0.0", false, "x"); !errors.Is(err, ErrControlHostAgent) {
		t.Fatalf("the store asked the helper to update: %v", err)
	}
	if _, err := s.RequestAgentRestart(context.Background(), opts.Hostname, "1.0.0", false, "x"); !errors.Is(err, ErrControlHostAgent) {
		t.Fatalf("the store asked the helper to restart: %v", err)
	}
	v, err := s.AgentUpdateView(context.Background(), opts.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ControlHost || v.Status == nil || v.Status.Supported || v.Status.Restartable {
		t.Fatalf("the agent card should say the helper does not update here: %+v", v)
	}
}

// An agent someone enrolled the ordinary way is none of this.
func TestAnOrdinaryEnrollmentIsNotTheControlHost(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("ctl-plain")
	enrolled(t, s, h, host)
	var marked bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT control_host FROM agent_credentials WHERE hostname = $1 AND revoked_at IS NULL`, host).
		Scan(&marked); err != nil || marked {
		t.Fatalf("an ordinary agent is marked as the control server's helper: %v %v", marked, err)
	}
}

func TestControlHostURL(t *testing.T) {
	for _, c := range []struct{ listen, want string }{
		{":8080", "https://127.0.0.1:8080"},
		{"0.0.0.0:8443", "https://127.0.0.1:8443"},
		{"[::]:8443", "https://[::1]:8443"},
		{"10.0.0.5:8080", "https://10.0.0.5:8080"},
	} {
		got, err := ControlHostURL(c.listen, true)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.listen, got, err, c.want)
		}
	}
	if _, err := ControlHostURL(":8080", false); err == nil {
		t.Errorf("a plaintext listener was offered to the helper, which sends its credential on every report")
	}
}

func assetNamed(t *testing.T, s *Store, hostname string) Asset {
	t.Helper()
	list, err := s.Assets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if strings.EqualFold(a.Hostname, hostname) {
			return a
		}
	}
	t.Fatalf("%s is not in the inventory", hostname)
	return Asset{}
}

// The loop runs from main with a context that names no organization. A
// production store refuses those, so the loop must scope itself.
func TestTheControlHostLoopWorksOnAStrictStore(t *testing.T) {
	a, s, _, opts := controlHostFixture(t)
	s.Strict = true
	t.Cleanup(func() { s.Strict = false })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.RunControlHost(ctx, opts); close(done) }()
	path := filepath.Join(opts.Dir, ControlHostRequestFile)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the loop wrote no request on a strict store")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
