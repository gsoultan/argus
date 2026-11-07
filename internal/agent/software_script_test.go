package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Install scripts on the agent: only a reviewed script runs, exactly as
reviewed, with its parameters in the environment and never in its text,
secrets masked in the log, and a report of what changed.
*/

// fakeServer is a server whose package database, services and ports change
// once the script has run.
type fakeServer struct {
	mu    sync.Mutex
	ran   bool
	calls []string
	exit  int
	check int
	out   string
}

func (f *fakeServer) run(_ context.Context, out io.Writer, _ []string, name string, args ...string) (int, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	write := func(s string) {
		if out != nil {
			_, _ = io.WriteString(out, s)
		}
	}
	switch {
	case strings.HasPrefix(cmd, "dpkg-query -W"):
		write("ii \tcurl\t8.5.0-2\tamd64\t400\tcurl\n")
		if f.ran {
			write("ii \tacme-agent\t2.1.0\tamd64\t9000\tAcme\n")
		}
	case strings.HasPrefix(cmd, "apt-mark showmanual"):
		write("curl\n")
	case strings.HasPrefix(cmd, "systemctl list-unit-files"):
		write("ssh.service enabled enabled\ngetty@.service enabled enabled\n")
		if f.ran {
			write("acme-agent.service enabled enabled\n")
		}
	case strings.HasPrefix(cmd, "ss -Htln"):
		write("LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*\n")
		if f.ran {
			write("LISTEN 0 4096 [::]:9100 [::]:*\n")
		}
	case strings.HasPrefix(cmd, "systemd-run") && strings.HasSuffix(cmd, "run.sh"):
		f.ran = true
		write(f.out)
		return f.exit, nil
	case strings.HasPrefix(cmd, "systemd-run") && strings.HasSuffix(cmd, "check.sh"):
		write("2.1.0\n")
		return f.check, nil
	}
	return 0, nil
}

func (f *fakeServer) called(sub string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return c
		}
	}
	return ""
}

const installSh = "#!/bin/bash\ncurl -fsSL \"https://acme.example/agent-$ARGUS_PARAM_VERSION.deb\" -o /tmp/a.deb\ndpkg -i /tmp/a.deb\n"
const checkSh = "dpkg-query -W -f='${Version}' acme-agent\n"

type scriptEnv struct {
	key  ed25519.PrivateKey
	dir  string
	appr tunnel.ScriptApproval
}

func newScriptEnv(t *testing.T) scriptEnv {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, GrantKeyFile), []byte(tunnel.EncodeGrantKey(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	_ = os.MkdirAll(run, 0o755)
	old, oldF := scriptRunDir, scriptFolders
	scriptRunDir, scriptFolders = run, []string{filepath.Join(dir, "opt")}
	t.Cleanup(func() { scriptRunDir, scriptFolders = old, oldF })
	return scriptEnv{key: key, dir: dir, appr: tunnel.ScriptApproval{
		VersionID: "0b9c1f3e-0000-4000-8000-000000000001", Script: "s1", Name: "Acme agent", Version: 3,
		Platform: tunnel.ScriptLinux, InstallSHA: tunnel.ScriptHash(installSh), CheckSHA: tunnel.ScriptHash(checkSh),
		TimeoutSec: 600, Params: []string{"VERSION", "TOKEN"}, ReviewedBy: "grace@example.com", ReviewedAt: time.Now(),
	}}
}

func (e scriptEnv) applier(t *testing.T, f *fakeServer, o *ScriptOrder) *PackageApplier {
	t.Helper()
	return &PackageApplier{
		Plan: PackagePlan{PackageRunRequest: PackageRunRequest{ID: "run-00000002", Mode: PackagesScript, Script: o},
			StateDir: e.dir, Unit: "argus-packages-test"},
		PackageChecker: PackageChecker{Run: f.run, Look: func(n string) (string, error) {
			switch n {
			case "apt-get", "dpkg-query", "bash":
				return "/usr/bin/" + n, nil
			}
			return "", os.ErrNotExist
		}, Now: time.Now},
		BootID: func() string { return "boot" },
	}
}

func (e scriptEnv) order(t *testing.T, a tunnel.ScriptApproval, text string) *ScriptOrder {
	t.Helper()
	tok, err := tunnel.SignScript(e.key, a)
	if err != nil {
		t.Fatal(err)
	}
	return &ScriptOrder{Action: tunnel.ScriptInstall, Text: text, Check: checkSh, Approval: tok, TimeoutSec: 600,
		Params: map[string]string{"VERSION": "2.1.0", "TOKEN": "s3cr3t-token-value"}, Secret: []string{"TOKEN"}}
}

func TestScriptRunsWhatWasReviewedAndReportsWhatChanged(t *testing.T) {
	e := newScriptEnv(t)
	f := &fakeServer{out: "Selecting acme-agent\nregistered with s3cr3t-token-value\n"}
	a := e.applier(t, f, e.order(t, e.appr, installSh))
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s %q\n%s", st.State, st.Message, strings.Join(f.calls, "\n"))
	}
	r := st.Script
	if r == nil || r.Exit != 0 || len(r.Packages) != 1 || r.Packages[0].Name != "acme-agent" ||
		r.Packages[0].Kind != tunnel.ChangeAdd || len(r.ServicesAdded) != 1 || r.ServicesAdded[0] != "acme-agent.service" ||
		len(r.PortsOpened) != 1 || r.PortsOpened[0] != 9100 || !r.Installed || r.CheckVersion != "2.1.0" {
		t.Fatalf("report = %+v", r)
	}
	if !strings.Contains(st.Message, "Acme agent v3") || !strings.Contains(st.Message, "1 package added") {
		t.Errorf("message = %q", st.Message)
	}
	// The secret never leaves the server in the log.
	if strings.Contains(st.Log, "s3cr3t-token-value") || !strings.Contains(st.Log, "registered with ••••••") {
		t.Errorf("log = %q", st.Log)
	}
	// Its own unit: no stdin, a timeout, Argus's state hidden, bash strict.
	cmd := f.called("run.sh")
	for _, want := range []string{"StandardInput=null", "RuntimeMaxSec=600", "InaccessiblePaths=-" + e.dir,
		"EnvironmentFile=", "/usr/bin/bash -euo pipefail"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the unit lacks %q: %s", want, cmd)
		}
	}
	// Parameters travel in the environment, not on the command line.
	if strings.Contains(strings.Join(f.calls, "\n"), "s3cr3t") {
		t.Error("a secret was put on a command line")
	}
	if _, err := os.Stat(filepath.Join(scriptRunDir, "argus-script-run-00000002")); !os.IsNotExist(err) {
		t.Error("the script and its environment file were left behind")
	}
}

func TestScriptRefusedUnlessExactlyReviewed(t *testing.T) {
	e := newScriptEnv(t)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	_ = other
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)

	cases := map[string]func() *ScriptOrder{
		"a changed byte": func() *ScriptOrder { return e.order(t, e.appr, installSh+" ") },
		"another signer": func() *ScriptOrder {
			tok, _ := tunnel.SignScript(otherKey, e.appr)
			o := e.order(t, e.appr, installSh)
			o.Approval = tok
			return o
		},
		"a parameter it does not take": func() *ScriptOrder {
			o := e.order(t, e.appr, installSh)
			o.Params["PATH"] = "/tmp"
			return o
		},
		"a Windows script": func() *ScriptOrder {
			a := e.appr
			a.Platform = tunnel.ScriptWindows
			return e.order(t, a, installSh)
		},
		"a changed check": func() *ScriptOrder {
			o := e.order(t, e.appr, installSh)
			o.Check = "true"
			return o
		},
		"no uninstall reviewed": func() *ScriptOrder {
			o := e.order(t, e.appr, installSh)
			o.Action = tunnel.ScriptUninstall
			return o
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeServer{}
			st := e.applier(t, f, mk()).Apply(context.Background())
			if st.State != RunFailed || !strings.Contains(st.Message, "Nothing was run") {
				t.Fatalf("state = %s %q", st.State, st.Message)
			}
			if f.called("systemd-run") != "" {
				t.Fatal("a script ran that was not the one reviewed")
			}
		})
	}
}

func TestScriptFailureAndCheck(t *testing.T) {
	e := newScriptEnv(t)
	f := &fakeServer{exit: 7}
	st := e.applier(t, f, e.order(t, e.appr, installSh)).Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "exit code 7") || st.Script == nil || st.Script.Exit != 7 {
		t.Fatalf("state = %s %q %+v", st.State, st.Message, st.Script)
	}
	// Exit 0 but the check says it is not there: failed, said plainly.
	f = &fakeServer{check: 1}
	st = e.applier(t, f, e.order(t, e.appr, installSh)).Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "check says it is not installed") {
		t.Fatalf("state = %s %q", st.State, st.Message)
	}
}

func TestScriptOrderValidate(t *testing.T) {
	for _, o := range []*ScriptOrder{
		nil,
		{Action: "rm", Text: "x", Approval: "a"},
		{Action: tunnel.ScriptInstall, Text: "", Approval: "a"},
		{Action: tunnel.ScriptInstall, Text: "x"},
		{Action: tunnel.ScriptInstall, Text: "x", Approval: "a", Params: map[string]string{"lower": "x"}},
		{Action: tunnel.ScriptInstall, Text: "x", Approval: "a", Params: map[string]string{"A": "x\ny=1"}},
	} {
		if o.validate() == nil {
			t.Errorf("%+v was accepted", o)
		}
	}
}

func TestParseListenPortsAndUnits(t *testing.T) {
	ports := parseListenPorts([]byte("LISTEN 0 128 127.0.0.53%lo:53 0.0.0.0:*\nLISTEN 0 4096 [::]:22 [::]:*\nLISTEN 0 4096 0.0.0.0:22 0.0.0.0:*\n"))
	if len(ports) != 2 || ports[0] != 22 || ports[1] != 53 {
		t.Errorf("ports = %v", ports)
	}
	units := parseUnitFiles([]byte("ssh.service enabled enabled\ngetty@.service enabled enabled\ncron.timer enabled enabled\n"))
	if len(units) != 1 || units[0] != "ssh.service" {
		t.Errorf("units = %v", units)
	}
}
