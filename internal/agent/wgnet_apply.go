package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

/*
The helper's half of WireGuard networks (wgnet.go): `argus-agent
wgnet-apply --plan <file>`, as root.

Every step goes through wgnetOps so the order of things, and what is put
back on a rollback, are tested without a server.
*/

// wgnetOps is what the helper does to the server.
type wgnetOps interface {
	// haveTools says whether wg-quick is installed; install installs it.
	haveTools() bool
	install(ctx context.Context) error
	// existing are the interfaces Argus made, by the files under
	// /etc/wireguard.
	existing() ([]string, error)
	readConf(iface string) (string, bool, error)
	writeConf(iface, text string) error
	removeConf(iface string) error
	up(ctx context.Context, iface string) error
	down(ctx context.Context, iface string) error
	// openPort and closePort change ufw or firewalld; open reports whether
	// it opened anything, so a rollback closes only that.
	openPort(ctx context.Context, port int) (bool, error)
	closePort(ctx context.Context, port int) error
	readKey(stateDir, network string) (string, error)
}

// wgnetChange is what the helper did to one interface, to undo it.
type wgnetChange struct {
	iface    string
	prev     string
	hadPrev  bool
	wasUp    bool
	opened   int
	removing bool
}

// RunWGNetApply is the helper's entry point. It returns the exit code.
func RunWGNetApply(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wgnet-apply: cannot read the plan:", err)
		return 2
	}
	var plan WGNetPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		fmt.Fprintln(os.Stderr, "wgnet-apply: cannot read the plan:", err)
		return 2
	}
	ops, err := hostWGNetOps()
	if err != nil {
		writeWGNetRun(plan, WGNetFailed, err.Error())
		return 1
	}
	wait := func(ctx context.Context) bool { return waitWGNetConfirm(ctx, plan) }
	state, msg := applyWGNet(context.Background(), ops, plan, wait)
	writeWGNetRun(plan, state, msg)
	_ = os.Remove(planPath)
	if state != WGNetApplied {
		return 1
	}
	return 0
}

func writeWGNetRun(plan WGNetPlan, state, msg string) {
	r := plan.Run
	r.State, r.Message, r.At = state, msg, time.Now().UTC()
	if state != WGNetApplying && state != WGNetTrial {
		r.Unit = ""
	}
	_ = writeFileAtomic(filepath.Join(plan.StateDir, wgnetRunFile), mustJSON(r), 0o600)
}

// waitWGNetConfirm waits for the daemon to say a heartbeat reached the
// control plane after the change.
func waitWGNetConfirm(ctx context.Context, plan WGNetPlan) bool {
	trial := time.Duration(plan.TrialSeconds) * time.Second
	if trial <= 0 {
		trial = wgnetTrial
	}
	deadline := time.Now().Add(trial)
	path := filepath.Join(plan.StateDir, wgnetDir, "confirm-"+plan.Run.ID)
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

// applyWGNet makes the server's Argus interfaces what the plan says, waits
// for the change to be confirmed, and undoes it if it is not.
func applyWGNet(ctx context.Context, ops wgnetOps, plan WGNetPlan, confirmed func(context.Context) bool) (string, string) {
	var avoid []netip.Addr
	for _, a := range plan.Avoid {
		if ip, err := netip.ParseAddr(a); err == nil {
			avoid = append(avoid, ip)
		}
	}
	for _, s := range plan.Networks {
		if err := validWGNetSpec(s, avoid); err != nil {
			return WGNetFailed, "refused: " + err.Error()
		}
	}
	if len(plan.Networks) > 0 && !ops.haveTools() {
		writeWGNetRun(plan, WGNetApplying, "installing wireguard-tools")
		if err := ops.install(ctx); err != nil {
			return WGNetFailed, "cannot install wireguard-tools: " + err.Error()
		}
	}
	have, err := ops.existing()
	if err != nil {
		return WGNetFailed, "cannot read /etc/wireguard: " + err.Error()
	}
	want := map[string]WGNetSpec{}
	for _, s := range plan.Networks {
		want[s.Iface] = s
	}

	var done []wgnetChange
	undo := func(why string) (string, string) {
		for i := len(done) - 1; i >= 0; i-- {
			c := done[i]
			_ = ops.down(ctx, c.iface)
			if c.hadPrev {
				_ = ops.writeConf(c.iface, c.prev)
				if c.wasUp {
					_ = ops.up(ctx, c.iface)
				}
			} else {
				_ = ops.removeConf(c.iface)
			}
			if c.opened > 0 {
				_ = ops.closePort(ctx, c.opened)
			}
		}
		return WGNetRolledBack, why
	}

	// Interfaces no longer wanted come down first.
	for _, iface := range have {
		if _, ok := want[iface]; ok || !reWGIface.MatchString(iface) {
			continue
		}
		prev, _, err := ops.readConf(iface)
		if err != nil {
			return undo("cannot read " + iface + ": " + err.Error())
		}
		c := wgnetChange{iface: iface, prev: prev, hadPrev: true, wasUp: true, removing: true}
		_ = ops.down(ctx, iface)
		if err := ops.removeConf(iface); err != nil {
			done = append(done, c)
			return undo("cannot remove " + iface + ": " + err.Error())
		}
		done = append(done, c)
	}

	for _, s := range plan.Networks {
		key, err := ops.readKey(plan.StateDir, s.ID)
		if err != nil {
			return undo("cannot read this server's key for " + s.Name + ": " + err.Error())
		}
		text := wgConfig(s, key)
		prev, had, err := ops.readConf(s.Iface)
		if err != nil {
			return undo("cannot read " + s.Iface + ": " + err.Error())
		}
		if had && prev == text {
			continue
		}
		c := wgnetChange{iface: s.Iface, prev: prev, hadPrev: had, wasUp: had}
		if had {
			_ = ops.down(ctx, s.Iface)
		}
		if err := ops.writeConf(s.Iface, text); err != nil {
			done = append(done, c)
			return undo("cannot write " + s.Iface + ": " + err.Error())
		}
		done = append(done, c)
		if err := ops.up(ctx, s.Iface); err != nil {
			return undo("cannot bring " + s.Iface + " up: " + err.Error())
		}
		if s.OpenPort {
			opened, err := ops.openPort(ctx, s.ListenPort)
			if err != nil {
				return undo("cannot open UDP port " + strconv.Itoa(s.ListenPort) + ": " + err.Error())
			}
			if opened {
				done[len(done)-1].opened = s.ListenPort
			}
		}
	}
	if len(done) == 0 {
		return WGNetApplied, "nothing to change"
	}

	writeWGNetRun(plan, WGNetTrial, "applied; waiting to hear the server still reaches Argus")
	if !confirmed(ctx) {
		return undo("the server did not reach Argus within 2 minutes of the change, so it was undone")
	}
	return WGNetApplied, fmt.Sprintf("%d interface(s) changed", len(done))
}

var errWGNetNotHere = errors.New("WireGuard networks from the console are for Linux servers")
