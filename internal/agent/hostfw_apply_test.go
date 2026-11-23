package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A firewall change is saved first, checked after, and put back by itself
when no heartbeat gets through, when nobody keeps a firewall turned on, or
when it fails part way; one planned against a firewall that has changed
since is refused before anything is touched.
*/

type fakeFW struct {
	fw        tunnel.HostFirewall
	applied   int
	restored  int
	failApply error
	snapErr   error
}

func (f *fakeFW) read(context.Context) tunnel.HostFirewall { return f.fw }
func (f *fakeFW) snapshot(context.Context) (fwSnapshot, error) {
	return fwSnapshot{Kind: f.fw.Kind, Active: f.fw.Active}, f.snapErr
}
func (f *fakeFW) restore(context.Context, fwSnapshot) error {
	f.restored++
	f.fw.Rules = nil
	return nil
}
func (f *fakeFW) apply(_ context.Context, _ tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, always []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error {
	f.applied++
	if f.failApply != nil {
		return f.failApply
	}
	for _, l := range append(spec.Lines, always...) {
		for _, s := range lineSourcesOrNone(l) {
			r := tunnel.FirewallRule{Direction: l.Direction, Action: l.Action, Protocol: l.Protocol, Ports: l.Ports, Name: tunnel.FirewallLineMark(l.Key)}
			if s != "" {
				r.Sources = []string{s}
			}
			f.fw.Rules = append(f.fw.Rules, r)
		}
	}
	if spec.TurnOn {
		f.fw.Active = true
	}
	*ran = append(*ran, tunnel.FirewallRan{Command: "fake", OK: true})
	return nil
}

func lineSourcesOrNone(l tunnel.FirewallLine) []string {
	if len(l.Sources) == 0 {
		return []string{""}
	}
	return l.Sources
}

func waiter(confirm, keep bool) (fwWaiter, *[]tunnel.FirewallRun) {
	var states []tunnel.FirewallRun
	return fwWaiter{
		confirmed: func(context.Context, time.Duration) bool { return confirm },
		kept:      func(context.Context, time.Duration) bool { return keep },
		write:     func(r tunnel.FirewallRun) { states = append(states, r) },
	}, &states
}

func fwTestPlan(t *testing.T, spec tunnel.FirewallChangeSpec) FirewallPlan {
	spec.ID = "0b9e6c5e-1111"
	return FirewallPlan{Run: tunnel.FirewallRun{ID: spec.ID}, Spec: spec, StateDir: t.TempDir()}
}

var sshLine = tunnel.FirewallLine{Key: "ssh", Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Sources: []string{"10.8.0.0/24"}}

func TestFirewallChangeKeptOnceAHeartbeatGetsThrough(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}}
	w, states := waiter(true, false)
	plan := fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}})
	run := runFirewall(context.Background(), be, plan, w)
	if run.State != tunnel.FirewallRunApplied || be.restored != 0 || run.After == nil {
		t.Fatalf("run: %+v restored %d", run, be.restored)
	}
	var seen []string
	for _, s := range *states {
		seen = append(seen, s.State)
	}
	if !strings.Contains(strings.Join(seen, ","), "trial") {
		t.Errorf("a change is a trial first: %v", seen)
	}
	if _, err := os.Stat(backupPath(plan.StateDir, plan.Run.ID)); err != nil {
		t.Errorf("the copy is kept for Put back: %v", err)
	}
}

func TestFirewallChangePutBackWithNoHeartbeat(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}}
	w, _ := waiter(false, false)
	run := runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}}), w)
	if run.State != tunnel.FirewallRunRolledBack || be.restored != 1 || !strings.Contains(run.Message, "no heartbeat") {
		t.Errorf("run: %+v restored %d", run, be.restored)
	}
}

func TestTurningOnWaitsForKeep(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW}}
	w, _ := waiter(true, false)
	run := runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}, TurnOn: true, Keep: true}), w)
	if run.State != tunnel.FirewallRunRolledBack || be.restored != 1 || !strings.Contains(run.Message, "Keep") {
		t.Errorf("nobody kept it: %+v", run)
	}
	be = &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW}}
	w, _ = waiter(true, true)
	run = runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}, TurnOn: true, Keep: true}), w)
	if run.State != tunnel.FirewallRunApplied {
		t.Errorf("kept: %+v", run)
	}
}

func TestFirewallChangedSinceIsRefused(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}}
	w, _ := waiter(true, false)
	run := runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}, ExpectHash: "not-the-hash"}), w)
	if run.State != tunnel.FirewallRunFailed || be.applied != 0 || be.restored != 0 || run.After == nil {
		t.Errorf("refused before anything: %+v applied %d", run, be.applied)
	}
}

func TestFirewallChangeFailingPartWayIsPutBack(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}, failApply: errors.New("ufw: boom")}
	w, _ := waiter(true, false)
	run := runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}}), w)
	if run.State != tunnel.FirewallRunRolledBack || be.restored != 1 || !strings.Contains(run.Message, "boom") {
		t.Errorf("run: %+v", run)
	}
	be = &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}, snapErr: errors.New("disk full")}
	run = runFirewall(context.Background(), be, fwTestPlan(t, tunnel.FirewallChangeSpec{Lines: []tunnel.FirewallLine{sshLine}}), w)
	if run.State != tunnel.FirewallRunFailed || be.applied != 0 {
		t.Errorf("no copy, no change: %+v", run)
	}
}

func TestPutBackRestoresAnEarlierCopy(t *testing.T) {
	be := &fakeFW{fw: tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true}}
	w, _ := waiter(true, false)
	plan := fwTestPlan(t, tunnel.FirewallChangeSpec{PutBack: "0aaaaaaa-0000"})
	if err := saveFirewallBackup(plan.StateDir, "0aaaaaaa-0000", fwSnapshot{Kind: tunnel.FirewallUFW}); err != nil {
		t.Fatal(err)
	}
	run := runFirewall(context.Background(), be, plan, w)
	if run.State != tunnel.FirewallRunApplied || be.restored != 1 || be.applied != 0 {
		t.Errorf("put back: %+v restored %d", run, be.restored)
	}
	plan.Spec.PutBack = "0bbbbbbb-0000"
	plan.Run.ID = "0ccccccc-0000"
	if run := runFirewall(context.Background(), be, plan, w); run.State != tunnel.FirewallRunFailed {
		t.Errorf("a copy that is not there: %+v", run)
	}
}

// A ufw server: the commands the helper runs to replace one Argus line,
// take over a rule made by hand, and leave the rest alone.
func TestUFWApplyRunsTheRightCommands(t *testing.T) {
	numbered := `Status: active

     To                         Action      From
     --                         ------      ----
[ 1] 22/tcp                     ALLOW IN    Anywhere
[ 2] 8080/tcp                   ALLOW IN    10.0.0.5                   # argus fw-old
[ 3] 6379/tcp                   ALLOW IN    10.0.0.9                   # argus 6379
`
	var ran []string
	deleted := map[string]bool{}
	run := func(_ context.Context, name string, args ...string) (string, error) {
		c := name + " " + strings.Join(args, " ")
		ran = append(ran, c)
		if c == "ufw status numbered" {
			out := numbered
			if deleted["2"] {
				out = strings.Replace(out, "[ 2] 8080/tcp                   ALLOW IN    10.0.0.5                   # argus fw-old\n", "", 1)
			}
			if deleted["1"] {
				out = strings.Replace(out, "[ 1] 22/tcp                     ALLOW IN    Anywhere\n", "", 1)
			}
			return out, nil
		}
		if strings.HasPrefix(c, "ufw --force delete ") {
			deleted[strings.TrimPrefix(c, "ufw --force delete ")] = true
		}
		return "", nil
	}
	f := &linuxFW{run: run, has: func(string) bool { return true }, stateDir: t.TempDir()}
	cur := tunnel.HostFirewall{Kind: tunnel.FirewallUFW, Active: true, Installed: []string{"ufw"}, Rules: []tunnel.FirewallRule{
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "22", Raw: "22/tcp ALLOW IN Anywhere"},
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "8080", Sources: []string{"10.0.0.5"}, Name: "argus fw-old"},
		{Direction: "in", Action: "allow", Protocol: "tcp", Ports: "6379", Sources: []string{"10.0.0.9"}, Name: "argus 6379"},
	}}
	var rec []tunnel.FirewallRan
	err := f.apply(context.Background(), cur, tunnel.FirewallChangeSpec{ID: "x", Lines: []tunnel.FirewallLine{sshLine},
		TakeOver: []string{"22/tcp ALLOW IN Anywhere"}}, nil, &rec)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ran, "\n")
	for _, want := range []string{
		"ufw --force delete 2",
		"ufw --force delete 1",
		"ufw allow in proto tcp from 10.8.0.0/24 to any port 22 comment argus fw-ssh",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if deleted["3"] {
		t.Error("a database's rule is not Argus's line to remove")
	}
}

func TestFirewalldDirectRules(t *testing.T) {
	r, drop, ok := firewalldDirectRule("ipv4 filter OUTPUT 1 -p tcp -d 203.0.113.9 --dport 443 -m comment --comment 'argus fw-web' -j ACCEPT")
	if !ok || drop || r.Direction != "out" || r.Ports != "443" || r.Sources[0] != "203.0.113.9" || tunnel.ArgusLineKey(r.Name) != "web" {
		t.Errorf("direct rule: %+v", r)
	}
	if _, drop, ok := firewalldDirectRule("ipv4 filter OUTPUT 9 -m comment --comment 'argus fw-outdrop' -j DROP"); !drop || ok {
		t.Error("Argus's drop says everything else going out is blocked")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, firewallStateName)
	_ = os.WriteFile(filepath.Join(dir, firewallLines), mustJSON(map[string]fwdLine{"ssh": {Line: sshLine,
		Cmds: tunnel.FirewalldAdd("public", sshLine)}}), 0o600)
	rr := argusRichRules(state)
	if rr[`rule family="ipv4" source address="10.8.0.0/24" port port="22" protocol="tcp" accept`] != "ssh" {
		t.Errorf("Argus's rich rules are known by their text: %v", rr)
	}
}
