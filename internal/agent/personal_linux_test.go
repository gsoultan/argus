//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/*
The provisioner, against a passwd file and a command runner of the test's
own. What is held: it never touches an account it did not make, it locks an
account nobody wants rather than deleting it, a key is written only where
direct SSH is on, and .ssh turned into a link cannot make root write elsewhere.
*/

type fakeHost struct {
	t      *testing.T
	dir    string
	passwd string
	ran    []string
}

func newFakeHost(t *testing.T, lines ...string) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, dir: t.TempDir()}
	h.passwd = filepath.Join(h.dir, "passwd")
	h.write(lines...)
	return h
}

func (h *fakeHost) write(lines ...string) {
	if err := os.WriteFile(h.passwd, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) home(name string) string { return filepath.Join(h.dir, "home", name) }

// line is a passwd line for an account owned by whoever runs the test, so
// chown succeeds without root.
func (h *fakeHost) line(name, comment string) string {
	_ = os.MkdirAll(h.home(name), 0o755)
	return fmt.Sprintf("%s:x:%d:%d:%s:%s:/bin/bash", name, os.Getuid(), os.Getgid(), comment, h.home(name))
}

func (h *fakeHost) provisioner() *Provisioner {
	return &Provisioner{
		Passwd:  h.passwd,
		Sudoers: filepath.Join(h.dir, "sudoers.d", "argus-personal"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			h.ran = append(h.ran, name+" "+strings.Join(args, " "))
			if name == "useradd" {
				user := args[len(args)-1]
				b, _ := os.ReadFile(h.passwd)
				h.write(strings.TrimRight(string(b), "\n"), h.line(user, commentFor("x")))
			}
			return nil, nil
		},
	}
}

func (h *fakeHost) keys(name string) string {
	b, err := os.ReadFile(filepath.Join(h.home(name), ".ssh", "authorized_keys"))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (h *fakeHost) ranAny(prefix string) bool {
	for _, r := range h.ran {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func stateOf(states []PersonalAccountState, name string) string {
	for _, s := range states {
		if s.Name == name {
			return s.State
		}
	}
	return ""
}

const aKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ana@laptop"

func TestProvisionerCreatesAndWritesKeysOnlyForDirect(t *testing.T) {
	h := newFakeHost(t, "root:x:0:0:root:/root:/bin/bash")
	p := h.provisioner()
	states := p.Reconcile(context.Background(), []PersonalAccount{
		{Name: "ana", Owner: "ana@corp.example", Direct: DirectSSHAllow, Keys: []string{aKey}},
		{Name: "budi", Owner: "budi@corp.example", Keys: []string{aKey}},
	})
	if stateOf(states, "ana") != "ready" || stateOf(states, "budi") != "ready" {
		t.Fatalf("states %+v", states)
	}
	if !h.ranAny("useradd --create-home") {
		t.Fatalf("no account created: %v", h.ran)
	}
	if k := h.keys("ana"); !strings.Contains(k, aKey) {
		t.Errorf("ana's key not written: %q", k)
	}
	if k := h.keys("budi"); strings.Contains(k, "ssh-ed25519") {
		t.Errorf("a key written for an account with no direct SSH: %q", k)
	}

	// A key added by hand does not last past the next pass.
	path := filepath.Join(h.home("budi"), ".ssh", "authorized_keys")
	_ = os.WriteFile(path, []byte(aKey+"\n"), 0o600)
	p.Reconcile(context.Background(), []PersonalAccount{
		{Name: "ana", Owner: "ana@corp.example", Direct: DirectSSHAllow, Keys: []string{aKey}},
		{Name: "budi", Owner: "budi@corp.example"},
	})
	if k := h.keys("budi"); strings.Contains(k, "ssh-ed25519") {
		t.Errorf("a key added by hand survived: %q", k)
	}
}

func TestProvisionerLeavesAnAccountItDidNotMake(t *testing.T) {
	h := newFakeHost(t)
	h.write(h.line("deploy", "Deploy bot"))
	p := h.provisioner()
	states := p.Reconcile(context.Background(), []PersonalAccount{
		{Name: "deploy", Owner: "ana@corp.example", Direct: DirectSSHAllow, Keys: []string{aKey}},
	})
	if stateOf(states, "deploy") != "taken" {
		t.Fatalf("states %+v, want taken", states)
	}
	if len(h.ran) != 0 {
		t.Errorf("ran %v against an account Argus did not make", h.ran)
	}
	if k := h.keys("deploy"); k != "<missing>" {
		t.Errorf("wrote keys into an account Argus did not make: %q", k)
	}
}

func TestProvisionerLocksWhatNobodyWants(t *testing.T) {
	h := newFakeHost(t)
	h.write(h.line("ana", commentFor("ana@corp.example")), h.line("ops", "shared"))
	_ = os.MkdirAll(filepath.Join(h.home("ana"), ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(h.home("ana"), ".ssh", "authorized_keys"), []byte(aKey+"\n"), 0o600)
	p := h.provisioner()

	states := p.Reconcile(context.Background(), nil)
	if stateOf(states, "ana") != "locked" {
		t.Fatalf("states %+v, want ana locked", states)
	}
	if stateOf(states, "ops") != "" {
		t.Errorf("reported on an account Argus did not make: %+v", states)
	}
	if !h.ranAny("usermod --lock --expiredate 1 ana") || !h.ranAny("pkill -KILL -u ana") {
		t.Errorf("not locked and stopped: %v", h.ran)
	}
	if h.ranAny("userdel") {
		t.Error("an account was deleted")
	}
	if k := h.keys("ana"); strings.Contains(k, "ssh-ed25519") {
		t.Errorf("a locked account kept its key: %q", k)
	}
	if _, err := os.Stat(h.home("ana")); err != nil {
		t.Errorf("the home went: %v", err)
	}

	// Locked once, not on every pass; reopened when wanted again.
	h.ran = nil
	p.Reconcile(context.Background(), nil)
	if h.ranAny("usermod") {
		t.Errorf("locked again: %v", h.ran)
	}
	p.Reconcile(context.Background(), []PersonalAccount{{Name: "ana", Owner: "ana@corp.example"}})
	if !h.ranAny("usermod --expiredate  --password * ana") {
		t.Errorf("not reopened: %v", h.ran)
	}
}

func TestProvisionerRefusesALinkedSSHDirectory(t *testing.T) {
	h := newFakeHost(t)
	h.write(h.line("ana", commentFor("ana@corp.example")))
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(h.home("ana"), ".ssh")); err != nil {
		t.Fatal(err)
	}
	states := h.provisioner().Reconcile(context.Background(), []PersonalAccount{
		{Name: "ana", Owner: "ana@corp.example", Direct: DirectSSHAllow, Keys: []string{aKey}},
	})
	if stateOf(states, "ana") != "failed" {
		t.Fatalf("states %+v, want failed", states)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "authorized_keys")); err == nil {
		t.Fatal("wrote through a link")
	}
}

func TestProvisionerGivesSudoInItsOwnFile(t *testing.T) {
	h := newFakeHost(t)
	h.write(h.line("ana", commentFor("ana@corp.example")), h.line("budi", commentFor("budi@corp.example")))
	_ = os.MkdirAll(filepath.Join(h.dir, "sudoers.d"), 0o750)
	p := h.provisioner()
	p.Reconcile(context.Background(), []PersonalAccount{
		{Name: "ana", Owner: "ana@corp.example", Sudo: true},
		{Name: "budi", Owner: "budi@corp.example"},
	})
	b, err := os.ReadFile(p.Sudoers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ana ALL=(ALL:ALL) NOPASSWD: ALL") || strings.Contains(string(b), "budi") {
		t.Errorf("sudoers:\n%s", b)
	}
	if !h.ranAny("visudo -cf") {
		t.Error("not checked with visudo")
	}
	// Nobody with sudo: the file goes.
	p.Reconcile(context.Background(), []PersonalAccount{{Name: "ana", Owner: "ana@corp.example"}})
	if _, err := os.Stat(p.Sudoers); err == nil {
		t.Error("sudoers kept with nobody in it")
	}
}

// The agent builds its Provisioner with no Passwd, so "" has to read the
// server's own /etc/passwd. It read the (absent) account directory instead,
// and every account came back "the agent cannot read the server's account
// list" -- no personal account could be made, or locked when taken away.
//
// Checked through the list Reconcile reads rather than Reconcile itself, which
// on a real /etc/passwd would lock any Argus-made account it found.
func TestAProvisionerWithNoPasswdReadsTheServersAccounts(t *testing.T) {
	p := &Provisioner{}
	if got := p.passwd(); got != "/etc/passwd" {
		t.Fatalf("passwd() = %q, want /etc/passwd", got)
	}
	got, err := readPasswdEntries(p.passwd())
	if err != nil {
		t.Fatalf("cannot read the server's accounts: %v", err)
	}
	if _, ok := got["root"]; !ok {
		t.Errorf("root is not among the %d accounts read", len(got))
	}
}

// On a systemd host each pass crosses to `argus-agent personal-apply`, outside
// the agent's sandbox, where useradd and the sudoers drop-in can be written.
// The pass over there is the same one, and what it last did comes back so an
// account is not unlocked again on every pass.
func TestAPassOutsideTheSandboxIsTheSamePass(t *testing.T) {
	h := newFakeHost(t, "root:x:0:0:root:/root:/bin/bash")
	inner := h.provisioner()
	var requests []personalRequest
	p := &Provisioner{
		Passwd:  h.passwd,
		Sudoers: inner.Sudoers,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("a pass with Outside set ran a command inside the agent")
			return nil, nil
		},
		Outside: func(ctx context.Context, request []byte) ([]byte, []byte, error) {
			var req personalRequest
			if err := json.Unmarshal(request, &req); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, req)
			// What RunPersonalApply does, with the fake host's commands.
			helper := h.provisioner()
			helper.Passwd, helper.Sudoers, helper.state = req.Passwd, req.Sudoers, req.State
			states := helper.reconcile(ctx, req.Want)
			b, _ := json.Marshal(personalReply{States: states, State: helper.state})
			return b, []byte("level=WARN msg=\"personal account created\"\n"), nil
		},
	}
	want := []PersonalAccount{{Name: "ana", Owner: "ana@corp.example", Direct: DirectSSHAllow, Keys: []string{aKey}}}
	if st := p.Reconcile(context.Background(), want); stateOf(st, "ana") != "ready" {
		t.Fatalf("states %+v", st)
	}
	if k := h.keys("ana"); !strings.Contains(k, aKey) {
		t.Errorf("ana's key not written: %q", k)
	}
	h.ran = nil
	p.Reconcile(context.Background(), want)
	if h.ranAny("usermod") {
		t.Errorf("the second pass unlocked ana again: %v", h.ran)
	}
	if len(requests) != 2 || requests[1].State["ana"] != "ready" {
		t.Errorf("what the first pass did did not reach the second: %+v", requests)
	}
}

// A helper that cannot run fails every wanted account, saying why, rather
// than reporting them as they were.
func TestAnAccountHelperThatFailsFailsEveryAccount(t *testing.T) {
	p := &Provisioner{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Outside: func(context.Context, []byte) ([]byte, []byte, error) {
			return nil, []byte("Failed to start transient service unit\n"), fmt.Errorf("exit status 1")
		},
	}
	st := p.Reconcile(context.Background(), []PersonalAccount{{Name: "ana", Owner: "ana@corp.example"}})
	if len(st) != 1 || st[0].State != "failed" || !strings.Contains(st[0].Detail, "account helper") {
		t.Fatalf("states %+v", st)
	}
}

// The helper's own wire: a request on stdin, a reply on stdout. An account it
// did not make is reported taken and nothing is run for it.
func TestRunPersonalApplyAnswersOnStdout(t *testing.T) {
	h := newFakeHost(t)
	h.write(h.line("deploy", "Deploy bot"))
	req, _ := json.Marshal(personalRequest{
		Want:    []PersonalAccount{{Name: "deploy", Owner: "ana@corp.example"}},
		Passwd:  h.passwd,
		Sudoers: filepath.Join(h.dir, "sudoers.d", "argus-personal"),
	})
	var out, errs strings.Builder
	if code := RunPersonalApply(strings.NewReader(string(req)), &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var reply personalReply
	if err := json.Unmarshal([]byte(out.String()), &reply); err != nil {
		t.Fatalf("reply %q: %v", out.String(), err)
	}
	if stateOf(reply.States, "deploy") != "taken" {
		t.Errorf("states %+v, want deploy taken", reply.States)
	}
}
