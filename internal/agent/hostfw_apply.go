package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Changing the server's firewall: the helper's half (tunnel/hostfirewall_change.go).

`argus-agent firewall-apply --plan <file>` runs as root in a transient
systemd unit of its own (on Windows, in the agent's process). It:

 1. reads the firewall and refuses when it changed since the console
    planned the change (the plan's ExpectHash);
 2. saves a copy of everything it may touch, kept as backup-<id>.json, the
    last twenty of them, so "Put back" can restore any of them later;
 3. makes the firewall hold exactly the plan's lines, plus what must always
    stay open (the control plane, name lookups, operating system updates)
    when everything else going out is blocked;
 4. reads it back and checks every line is there;
 5. waits for the daemon to say a heartbeat reached the control plane, two
    minutes at most, and for a person's Keep, ten minutes at most, when the
    plan asks for one. If either does not come, it puts the saved copy back
    by itself: a change that cut this server off from Argus undoes itself.

What a firewall needs done is behind fwBackend, so the order of things and
what is put back are tested without a server.
*/

// fwBackend is one firewall, as the helper changes it.
type fwBackend interface {
	read(ctx context.Context) tunnel.HostFirewall
	snapshot(ctx context.Context) (fwSnapshot, error)
	restore(ctx context.Context, s fwSnapshot) error
	// apply makes the firewall hold lines (spec.Lines, spec.Protect and
	// always), recording each command in ran.
	apply(ctx context.Context, cur tunnel.HostFirewall, spec tunnel.FirewallChangeSpec, always []tunnel.FirewallLine, ran *[]tunnel.FirewallRan) error
}

// fwSnapshot is everything a change may touch, as it was before.
type fwSnapshot struct {
	Kind   string `json:"kind"`
	Active bool   `json:"active"`
	// Files are the firewall's own saved settings, by path; nil is a file
	// that was not there.
	Files map[string]*string `json:"files,omitempty"`
	// Nft is Argus's own nftables table ("" when there was none).
	Nft string `json:"nft,omitempty"`
	// Export is Windows Firewall's export file.
	Export string    `json:"export,omitempty"`
	At     time.Time `json:"at"`
}

// FirewallPlan is what the daemon hands the helper.
type FirewallPlan struct {
	Run      tunnel.FirewallRun        `json:"run"`
	Spec     tunnel.FirewallChangeSpec `json:"spec"`
	Always   []tunnel.FirewallLine     `json:"always,omitempty"`
	StateDir string                    `json:"state_dir"`
}

const (
	firewallDir      = "firewall"
	firewallRunFile  = "firewall-run.json"
	firewallLines    = "firewall-lines.json"
	firewallBackups  = 20
	firewallApplyCap = 3 * time.Minute
)

// RunFirewallApply is the helper's entry point. It returns the exit code.
func RunFirewallApply(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "firewall-apply: cannot read the plan:", err)
		return 2
	}
	var plan FirewallPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		fmt.Fprintln(os.Stderr, "firewall-apply: cannot read the plan:", err)
		return 2
	}
	be := hostFirewallBackend(plan.StateDir)
	run := runFirewall(context.Background(), be, plan, fileWaiter(plan))
	_ = os.Remove(planPath)
	if run.State != tunnel.FirewallRunApplied {
		return 1
	}
	return 0
}

// fwWaiter is how the helper hears that a heartbeat got through, and that
// a person pressed Keep.
type fwWaiter struct {
	confirmed func(ctx context.Context, d time.Duration) bool
	kept      func(ctx context.Context, d time.Duration) bool
	write     func(tunnel.FirewallRun)
}

func fileWaiter(plan FirewallPlan) fwWaiter {
	dir := filepath.Join(plan.StateDir, firewallDir)
	wait := func(name string) func(context.Context, time.Duration) bool {
		return func(ctx context.Context, d time.Duration) bool {
			path := filepath.Join(dir, name+"-"+plan.Run.ID)
			deadline := time.Now().Add(d)
			for time.Now().Before(deadline) {
				if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == plan.Run.ID {
					_ = os.Remove(path)
					return true
				}
				select {
				case <-ctx.Done():
					return false
				case <-time.After(time.Second):
				}
			}
			return false
		}
	}
	return fwWaiter{
		confirmed: wait("confirm"),
		kept:      wait("keep"),
		write: func(r tunnel.FirewallRun) {
			_ = writeFileAtomic(filepath.Join(plan.StateDir, firewallRunFile), mustJSON(r), 0o600)
		},
	}
}

// runFirewall makes one change and sees it through its trial.
func runFirewall(ctx context.Context, be fwBackend, plan FirewallPlan, w fwWaiter) tunnel.FirewallRun {
	run := plan.Run
	set := func(state, msg string) {
		run.State, run.Message, run.At = state, msg, time.Now().UTC()
		if run.Finished() {
			run.Unit = ""
		}
		w.write(run)
	}
	spec := plan.Spec
	if err := spec.Validate(); err != nil {
		set(tunnel.FirewallRunFailed, "refused: "+err.Error())
		return run
	}
	actx, cancel := context.WithTimeout(ctx, firewallApplyCap)
	defer cancel()
	set(tunnel.FirewallRunApplying, "saving the firewall as it is")
	cur := be.read(actx)
	if cur.Error != "" {
		set(tunnel.FirewallRunFailed, "the firewall could not be read: "+cur.Error)
		return run
	}
	if spec.ExpectHash != "" && spec.PutBack == "" && cur.Hash() != spec.ExpectHash {
		after := cur
		run.After = &after
		set(tunnel.FirewallRunFailed, "the firewall changed on the server since this change was planned; nothing was changed. Look at it again and plan the change anew")
		return run
	}
	snap, err := be.snapshot(actx)
	if err != nil {
		set(tunnel.FirewallRunFailed, "could not save the firewall first, so nothing was changed: "+err.Error())
		return run
	}
	if err := saveFirewallBackup(plan.StateDir, run.ID, snap); err != nil {
		set(tunnel.FirewallRunFailed, "could not keep a copy of the firewall, so nothing was changed: "+err.Error())
		return run
	}
	putBack := func(why string) tunnel.FirewallRun {
		rctx, rcancel := context.WithTimeout(context.Background(), firewallApplyCap)
		defer rcancel()
		if err := be.restore(rctx, snap); err != nil {
			why += "; putting it back failed too: " + err.Error() + ". Restore it with `argus-agent firewall-putback " + run.ID + "` on the server"
		}
		after := be.read(rctx)
		run.After = &after
		set(tunnel.FirewallRunRolledBack, why)
		return run
	}

	if spec.PutBack != "" {
		set(tunnel.FirewallRunApplying, "putting back the firewall saved before change "+spec.PutBack)
		old, err := loadFirewallBackup(plan.StateDir, spec.PutBack)
		if err != nil {
			set(tunnel.FirewallRunFailed, "the copy saved before change "+spec.PutBack+" is not on this server: "+err.Error())
			return run
		}
		run.Ran = append(run.Ran, tunnel.FirewallRan{Command: "put back the firewall saved at " + old.At.Format(time.RFC3339), OK: true})
		if err := be.restore(actx, old); err != nil {
			run.Ran[len(run.Ran)-1].OK = false
			run.Ran[len(run.Ran)-1].Output = err.Error()
			return putBack("putting the copy back failed, so the firewall was put back as it was before: " + err.Error())
		}
	} else {
		set(tunnel.FirewallRunApplying, "changing the firewall")
		if err := be.apply(actx, cur, spec, plan.Always, &run.Ran); err != nil {
			return putBack("the change failed part way, so the firewall was put back as it was: " + err.Error())
		}
	}
	after := be.read(actx)
	run.After = &after
	if spec.PutBack == "" {
		if missing := missingLines(after, append(append(slices.Clone(spec.Lines), spec.Protect...), plan.Always...)); len(missing) > 0 {
			return putBack("read back, the firewall did not hold " + strings.Join(missing, ", ") + ", so it was put back as it was")
		}
		if spec.TurnOn && !after.Active {
			return putBack("the firewall did not switch on, so it was put back as it was")
		}
	}

	trial := time.Duration(spec.TrialSeconds) * time.Second
	if trial <= 0 {
		trial = tunnel.FirewallTrial
	}
	set(tunnel.FirewallRunTrial, "changed; waiting for this server to reach Argus")
	if !w.confirmed(ctx, trial) {
		return putBack(fmt.Sprintf("no heartbeat reached Argus within %s of the change, so the firewall was put back as it was", trial))
	}
	if spec.Keep {
		keep := time.Duration(spec.KeepSeconds) * time.Second
		if keep <= 0 {
			keep = tunnel.FirewallKeepWindow
		}
		set(tunnel.FirewallRunWaitKeep, "on; waiting for someone to press Keep")
		if !w.kept(ctx, keep) {
			return putBack(fmt.Sprintf("nobody pressed Keep within %s, so the firewall was put back as it was", keep))
		}
	}
	set(tunnel.FirewallRunApplied, "done")
	return run
}

// missingLines names the lines a firewall read does not hold as asked.
func missingLines(fw tunnel.HostFirewall, want []tunnel.FirewallLine) []string {
	have := fw.ArgusLines()
	var out []string
	for _, l := range want {
		h, ok := have[l.Key]
		if !ok || !tunnel.SameLine(h, l) {
			out = append(out, tunnel.FirewallLineMark(l.Key))
		}
	}
	return out
}

// RunFirewallPutBack is `argus-agent firewall-putback <id>`: the way back
// when the console cannot reach the server.
func RunFirewallPutBack(stateDir, id string) int {
	if !tunnel.ValidFirewallRunID(id) {
		fmt.Fprintln(os.Stderr, "firewall-putback: name the change whose copy to put back")
		return 2
	}
	snap, err := loadFirewallBackup(stateDir, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "firewall-putback:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallApplyCap)
	defer cancel()
	if err := hostFirewallBackend(stateDir).restore(ctx, snap); err != nil {
		fmt.Fprintln(os.Stderr, "firewall-putback:", err)
		return 1
	}
	fmt.Println("put back the firewall saved at", snap.At.Format(time.RFC3339))
	return 0
}

func backupPath(stateDir, id string) string {
	return filepath.Join(stateDir, firewallDir, "backup-"+id+".json")
}

func saveFirewallBackup(stateDir, id string, s fwSnapshot) error {
	dir := filepath.Join(stateDir, firewallDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if s.At.IsZero() {
		s.At = time.Now().UTC()
	}
	if err := writeFileAtomic(backupPath(stateDir, id), mustJSON(s), 0o600); err != nil {
		return err
	}
	// The newest twenty are kept.
	entries, _ := os.ReadDir(dir)
	type bk struct {
		name string
		at   time.Time
	}
	var all []bk
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "backup-") {
			continue
		}
		if info, err := e.Info(); err == nil {
			all = append(all, bk{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for i := firewallBackups; i < len(all); i++ {
		_ = os.Remove(filepath.Join(dir, all[i].name))
	}
	return nil
}

func loadFirewallBackup(stateDir, id string) (fwSnapshot, error) {
	var s fwSnapshot
	b, err := os.ReadFile(backupPath(stateDir, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, errors.New("no copy was kept (only the last twenty are)")
		}
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// ranCmd runs one command through run and records it.
func ranCmd(ctx context.Context, run runCmd, ran *[]tunnel.FirewallRan, c tunnel.FirewallCmd) error {
	out, err := run(ctx, c[0], c[1:]...)
	r := tunnel.FirewallRan{Command: c.String(), OK: err == nil, Output: strings.TrimSpace(tail([]byte(out), 400))}
	*ran = append(*ran, r)
	if err != nil {
		return fmt.Errorf("%s: %v", c.String(), err)
	}
	return nil
}
