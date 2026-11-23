package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Changing the server's firewall: the daemon's half (hostfw_apply.go is the
helper's).

The console's change rides a heartbeat reply. The daemon writes it down as a
plan, with what must always stay open going out, and starts the helper; it
then follows the helper's state file and puts it on every heartbeat. A
heartbeat that reaches the control plane while the change is on trial is
the proof the helper waits for, and the daemon says so by writing
confirm-<id>; a person's Keep, when the change waits for one, arrives on a
later reply and becomes keep-<id>.
*/

type fwPlatform interface {
	// supported says why the firewall cannot be changed here, or "".
	supported() string
	launch(ctx context.Context, planPath, unit string) error
	active(unit string) bool
}

// HostFirewalls is the daemon's half.
type HostFirewalls struct {
	StateDir string
	Log      *slog.Logger
	// Changed is called when there is something new to report.
	Changed func()
	// ReadNow reads the firewall again and reports it.
	ReadNow func()
	// Always is what stays open going out when everything else is blocked;
	// an error refuses a change that would block it.
	Always func() ([]tunnel.FirewallLine, error)

	platform fwPlatform
	now      func() time.Time

	mu        sync.Mutex
	loaded    bool
	run       tunnel.FirewallRun
	confirmed string
	keptID    string
	lastRead  string
	afterSent string
}

// NewHostFirewalls builds the daemon's half for this host.
func NewHostFirewalls(stateDir, executable string, log *slog.Logger) *HostFirewalls {
	if log == nil {
		log = slog.Default()
	}
	return &HostFirewalls{StateDir: stateDir, Log: log, platform: hostFirewallPlatform(executable, stateDir), now: time.Now}
}

// Beat is what to put on the next heartbeat.
func (h *HostFirewalls) Beat() tunnel.FirewallBeat {
	h.follow()
	h.mu.Lock()
	defer h.mu.Unlock()
	b := tunnel.FirewallBeat{}
	if reason := h.platform.supported(); reason != "" {
		b.Reason = reason
	} else {
		b.Supported = true
	}
	if h.run.ID != "" {
		r := h.run
		// The firewall read back rides until a heartbeat carrying it got
		// through; after that, only how the run went.
		if h.afterSent == r.ID && r.Finished() {
			r.After = nil
		}
		b.Run = &r
	}
	return b
}

// Delivered says a heartbeat carrying b reached the control plane.
func (h *HostFirewalls) Delivered(b tunnel.FirewallBeat) {
	if b.Run == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if b.Run.Finished() && b.Run.After != nil {
		h.afterSent = b.Run.ID
	}
	if b.Run.State != tunnel.FirewallRunTrial || h.confirmed == b.Run.ID {
		return
	}
	if err := h.mark("confirm", b.Run.ID); err != nil {
		h.Log.Error("firewall: cannot confirm the change", "error", err)
		return
	}
	h.confirmed = b.Run.ID
}

func (h *HostFirewalls) mark(what, id string) error {
	dir := filepath.Join(h.StateDir, firewallDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, what+"-"+id), []byte(id), 0o600)
}

// Handle acts on the console's orders. o is nil from a control plane older
// than firewall changes, which leaves everything as it is.
func (h *HostFirewalls) Handle(ctx context.Context, o *tunnel.FirewallOrders) {
	if o == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	if o.Read != "" && o.Read != h.lastRead {
		h.lastRead = o.Read
		if h.ReadNow != nil {
			go h.ReadNow()
		}
	}
	if o.Keep != "" && o.Keep == h.run.ID && h.keptID != o.Keep {
		if err := h.mark("keep", o.Keep); err != nil {
			h.Log.Error("firewall: cannot keep the change", "error", err)
		} else {
			h.keptID = o.Keep
		}
	}
	c := o.Change
	if c == nil || c.ID == h.run.ID || h.ranBefore(c.ID) {
		return
	}
	if h.run.ID != "" && !h.run.Finished() {
		return // one at a time; the console sends it again
	}
	h.startLocked(ctx, *c)
}

func (h *HostFirewalls) startLocked(ctx context.Context, spec tunnel.FirewallChangeSpec) {
	run := tunnel.FirewallRun{ID: spec.ID, State: tunnel.FirewallRunApplying, Message: "starting", At: h.now().UTC()}
	fail := func(msg string) {
		run.State, run.Message, run.Unit = tunnel.FirewallRunFailed, msg, ""
		h.setRunLocked(run)
		h.changedAsync()
	}
	if err := spec.Validate(); err != nil {
		if !tunnel.ValidFirewallRunID(spec.ID) {
			h.Log.Error("firewall: refusing a change with no id", "error", err)
			return
		}
		fail("refused: " + err.Error())
		return
	}
	if reason := h.platform.supported(); reason != "" {
		fail(reason)
		return
	}
	plan := FirewallPlan{Spec: spec, StateDir: h.StateDir}
	if spec.DefaultOut == "deny" && spec.PutBack == "" {
		if h.Always == nil {
			fail("this agent cannot work out what must stay open going out, so nothing going out is blocked")
			return
		}
		always, err := h.Always()
		if err != nil {
			fail("refused, so this server keeps reaching Argus: " + err.Error())
			return
		}
		plan.Always = always
	}
	dir := filepath.Join(h.StateDir, firewallDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("cannot prepare the change: " + err.Error())
		return
	}
	run.Unit = "argus-fw-" + strings.ReplaceAll(spec.ID, "-", "")[:16]
	plan.Run = run
	planPath := filepath.Join(dir, "plan-"+spec.ID+".json")
	if err := writeFileAtomic(planPath, mustJSON(plan), 0o600); err != nil {
		fail("cannot prepare the change: " + err.Error())
		return
	}
	h.setRunLocked(run)
	h.noteRan(spec.ID)
	if err := h.platform.launch(ctx, planPath, run.Unit); err != nil {
		fail("cannot start the change: " + err.Error())
	}
}

// ranBefore says a change was started on this server already: the console
// never gets a change run twice, even after a restart.
func (h *HostFirewalls) ranBefore(id string) bool {
	b, err := os.ReadFile(filepath.Join(h.StateDir, firewallDir, "ran.json"))
	if err != nil {
		return false
	}
	var ids []string
	_ = json.Unmarshal(b, &ids)
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func (h *HostFirewalls) noteRan(id string) {
	path := filepath.Join(h.StateDir, firewallDir, "ran.json")
	var ids []string
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &ids)
	}
	ids = append(ids, id)
	if len(ids) > 100 {
		ids = ids[len(ids)-100:]
	}
	_ = writeFileAtomic(path, mustJSON(ids), 0o600)
}

func (h *HostFirewalls) changedAsync() {
	if h.Changed != nil {
		go h.Changed()
	}
}

func (h *HostFirewalls) setRunLocked(r tunnel.FirewallRun) {
	h.run = r
	if err := writeFileAtomic(filepath.Join(h.StateDir, firewallRunFile), mustJSON(r), 0o600); err != nil {
		h.Log.Error("firewall: cannot keep the change's state", "error", err)
	}
}

func (h *HostFirewalls) loadLocked() {
	if h.loaded {
		return
	}
	h.loaded = true
	if b, err := os.ReadFile(filepath.Join(h.StateDir, firewallRunFile)); err == nil {
		_ = json.Unmarshal(b, &h.run)
	}
}

// Watch follows a change the helper is making. It returns when ctx ends.
func (h *HostFirewalls) Watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if h.follow() {
			if h.Changed != nil {
				h.Changed()
			}
			if h.ReadNow != nil && h.finished() {
				h.ReadNow()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *HostFirewalls) finished() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run.Finished()
}

// follow reads the helper's state and settles a run whose helper is gone.
// It reports whether anything changed.
func (h *HostFirewalls) follow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadLocked()
	cur := h.run
	if cur.ID == "" || cur.Finished() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(h.StateDir, firewallRunFile))
	if err != nil {
		return false
	}
	var disk tunnel.FirewallRun
	if json.Unmarshal(b, &disk) != nil || disk.ID != cur.ID {
		return false
	}
	if !disk.Finished() && disk.Unit != "" && !h.platform.active(disk.Unit) && h.now().Sub(disk.At) > 30*time.Second {
		disk.State, disk.At = tunnel.FirewallRunFailed, h.now().UTC()
		disk.Message = "the change stopped before it finished; look at the firewall on the server, and put back the copy saved before it if needed"
		h.setRunLocked(disk)
		return true
	}
	if disk.State == cur.State && disk.Message == cur.Message {
		return false
	}
	h.run = disk
	return true
}

// errNoControlAddr refuses blocking everything going out when the agent
// cannot name the control plane's addresses.
var errNoControlAddr = errors.New("the control plane's address could not be found")
