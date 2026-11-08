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
	"time"
)

/* ── The helper: growing ─────────────────────────────────────────────────── */

// diskCmd is one command a grow runs.
type diskCmd struct {
	Title string
	Argv  []string
	Stdin string
	// Show is how the command reads in the console, when Argv is a script.
	Show string
}

func (c diskCmd) String() string {
	if c.Show != "" {
		return c.Show
	}
	s := strings.Join(c.Argv, " ")
	if c.Stdin != "" {
		s = "echo " + strings.TrimSpace(c.Stdin) + " | " + s
	}
	return s
}

// diskOps is what growing needs from the host.
type diskOps interface {
	// scan reads the disks again, asking rescanDisk for its size first
	// when it is not "".
	scan(ctx context.Context, rescanDisk string) (DiskReport, error)
	run(ctx context.Context, c diskCmd) (string, error)
	// steps are the commands that grow the filesystem p names.
	steps(req DiskGrowRequest, p GrowPath) ([]diskCmd, error)
	// table is the command that prints the partition table, if there is one.
	table(ctx context.Context, p GrowPath) (diskCmd, bool)
	// rescanDisk is the disk to ask for its size before checking, or "".
	rescanDisk(p GrowPath) string
}

// DiskGrower carries out a grow. It is what `argus-agent disks-grow` runs,
// as root, in a unit of its own (on Windows, in the agent).
type DiskGrower struct {
	Plan DiskPlan
	Log  *slog.Logger
	Now  func() time.Time
	ops  diskOps

	st DiskRunStatus
}

// RunDisksGrow is `argus-agent disks-grow --plan <path>`.
func RunDisksGrow(planPath string) int {
	b, err := os.ReadFile(planPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "disks-grow:", err)
		return 2
	}
	var plan DiskPlan
	if err := json.Unmarshal(b, &plan); err != nil || plan.StateDir == "" || plan.ID == "" {
		fmt.Fprintln(os.Stderr, "disks-grow: the plan cannot be read")
		return 2
	}
	g := &DiskGrower{Plan: plan, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), ops: hostDiskOps()}
	ctx, cancel := context.WithTimeout(context.Background(), diskGrowTimeout)
	defer cancel()
	st := g.Grow(ctx)
	_ = os.Remove(planPath)
	if st.State == RunFailed {
		return 1
	}
	return 0
}

// RunDisksScan is `argus-agent disks-scan --req <path>`: the report on
// stdout.
func RunDisksScan(reqPath string, stdout io.Writer) int {
	var req DiskScanRequest
	if b, err := os.ReadFile(reqPath); err != nil || json.Unmarshal(b, &req) != nil {
		fmt.Fprintln(os.Stderr, "disks-scan: the request cannot be read")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), diskScanTimeout)
	defer cancel()
	r, err := scanHostDisks(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(r); err != nil {
		return 1
	}
	return 0
}

// Grow runs the plan and returns how it ended, which it has also written to
// the state file the daemon follows.
func (g *DiskGrower) Grow(ctx context.Context) DiskRunStatus {
	if g.Now == nil {
		g.Now = time.Now
	}
	if g.Log == nil {
		g.Log = slog.Default()
	}
	now := g.Now().UTC()
	g.st = DiskRunStatus{ID: g.Plan.ID, State: RunRunning, Step: "checking", Unit: g.Plan.Unit,
		BootID: readBootID(), StartedAt: now, At: now, Before: g.Plan.Path.FSSize, Steps: []DiskStep{}}
	if b, err := os.ReadFile(filepath.Join(g.Plan.StateDir, disksRunFile)); err == nil {
		var prev DiskRunStatus
		if json.Unmarshal(b, &prev) == nil && prev.ID == g.Plan.ID && !prev.StartedAt.IsZero() {
			g.st.StartedAt, g.st.BootID = prev.StartedAt, prev.BootID
		}
	}
	g.save()
	if err := validDiskRequest(g.Plan.DiskGrowRequest); err != nil {
		return g.finish(RunFailed, err.Error())
	}

	// 1. The layout must still be the one the console showed.
	i := g.begin("Check the layout", "")
	r, err := g.ops.scan(ctx, g.ops.rescanDisk(g.Plan.Path))
	if err != nil {
		g.end(i, "failed", err.Error())
		return g.finish(RunFailed, "the disks could not be read: "+err.Error())
	}
	fs := r.fs(g.Plan.Key)
	if why := diskSameLayout(g.Plan.DiskGrowRequest, fs); why != "" {
		g.end(i, "failed", why)
		return g.finish(RunFailed, "nothing was changed: "+why)
	}
	cur := *fs.Path
	g.end(i, "done", describePath(cur))

	steps, err := g.ops.steps(g.Plan.DiskGrowRequest, cur)
	if err != nil {
		return g.finish(RunFailed, "nothing was changed: "+err.Error())
	}

	// 2. The partition table before, for the record.
	if c, ok := g.ops.table(ctx, cur); ok {
		i := g.begin(c.Title, c.String())
		out, err := g.ops.run(ctx, c)
		if err != nil {
			g.end(i, "failed", out)
			return g.finish(RunFailed, "nothing was changed: the partition table could not be saved first")
		}
		g.st.TableBefore = clipText(out, 16000)
		g.end(i, "done", "kept with this run")
	}

	// 3. Each layer, bottom first.
	for _, c := range steps {
		if ctx.Err() != nil {
			return g.finish(RunFailed, "the grow ran out of time; every step it took is safe to leave")
		}
		i := g.begin(c.Title, c.String())
		out, err := g.ops.run(ctx, c)
		if err != nil {
			g.end(i, "failed", out+"\n"+err.Error())
			return g.finish(RunFailed, c.Title+" failed; the steps before it are done and safe to leave, "+
				"and growing again carries on from there")
		}
		g.end(i, "done", out)
	}

	// 4. Read back.
	i = g.begin("Read back", "")
	after, err := g.ops.scan(ctx, "")
	if err == nil {
		after.bounded()
		if err := writeFileAtomic(filepath.Join(g.Plan.StateDir, disksReportFile), mustJSON(after), 0o600); err != nil {
			g.Log.Error("disks: cannot keep the report", "error", err)
		}
		if f := after.fs(g.Plan.Key); f != nil {
			for _, l := range f.Layers {
				if l.Kind == "fs" {
					g.st.After = l.Size
				}
			}
			g.end(i, "done", fmt.Sprintf("%s is %s, %d%% used", f.Mount, humanBytes(g.st.After), diskPercentOf(*f)))
		} else {
			g.end(i, "done", "")
		}
	} else {
		g.end(i, "failed", err.Error())
	}
	msg := "grew " + g.Plan.Path.Mount
	if g.st.After > 0 {
		msg = fmt.Sprintf("grew %s from %s to %s", g.Plan.Path.Mount, humanBytes(g.st.Before), humanBytes(g.st.After))
	}
	return g.finish(RunDone, msg)
}

// diskSameLayout says why the filesystem the scan found now is not the one the
// plan names, or "" when it is.
func diskSameLayout(req DiskGrowRequest, f *DiskFS) string {
	want := req.Path
	switch {
	case f == nil:
		return "the filesystem is no longer mounted at " + want.Mount
	case f.Answer != DiskGrow || f.Path == nil:
		if f.Why != "" {
			return "it cannot grow now: " + strings.TrimSuffix(f.Why, ".")
		}
		return "there is no longer room for it to grow into"
	}
	got := *f.Path
	moved := func(what string) string { return what + " changed since the scan; scan again and look before growing" }
	switch {
	case got.Mount != want.Mount || got.FSType != want.FSType:
		return moved("the filesystem")
	case got.Disk != want.Disk || got.Part != want.Part || got.PartNum != want.PartNum || got.PartStart != want.PartStart:
		return moved("the partition")
	case got.PartSize != want.PartSize:
		return moved("the partition's size")
	case got.Crypt != want.Crypt:
		return moved("the encryption")
	case got.VGUUID != want.VGUUID || got.LVUUID != want.LVUUID || got.PV != want.PV:
		return moved("the LVM layout")
	case got.DiskNumber != want.DiskNumber || got.PartitionNumber != want.PartitionNumber:
		return moved("the partition")
	}
	// The filesystem may have been grown by hand since; within 1% is the
	// same, and more than that is someone else's change.
	if d := got.FSSize - want.FSSize; d > want.FSSize/100 || -d > want.FSSize/100 {
		return moved("the filesystem's size")
	}
	if req.Bytes > 0 && req.Bytes > got.VGFree+got.PVRoom+got.PartRoom+got.CryptRoom {
		return "the volume group no longer has the space asked for"
	}
	return ""
}

func describePath(p GrowPath) string {
	var parts []string
	if p.Disk != "" {
		parts = append(parts, fmt.Sprintf("%s %s", p.Disk, humanBytes(p.DiskSize)))
	}
	if p.Part != "" {
		parts = append(parts, fmt.Sprintf("%s %s", p.Part, humanBytes(p.PartSize)))
	}
	if p.VG != "" {
		parts = append(parts, fmt.Sprintf("%s %s free", p.VG, humanBytes(p.VGFree)))
	}
	parts = append(parts, fmt.Sprintf("%s %s %s", p.FSType, p.Mount, humanBytes(p.FSSize)))
	return strings.Join(parts, " · ") + ", as planned"
}

func (g *DiskGrower) begin(title, cmd string) int {
	g.st.Steps = append(g.st.Steps, DiskStep{Title: title, Command: cmd, State: "running"})
	g.st.Step = title
	g.save()
	return len(g.st.Steps) - 1
}

func (g *DiskGrower) end(i int, state, out string) {
	g.st.Steps[i].State, g.st.Steps[i].Output = state, clipText(strings.TrimSpace(out), maxStepOutput)
	g.save()
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (g *DiskGrower) save() {
	g.st.At = g.Now().UTC()
	if err := writeFileAtomic(filepath.Join(g.Plan.StateDir, disksRunFile), mustJSON(g.st), 0o600); err != nil {
		g.Log.Error("disks: cannot keep the grow's state", "error", err)
	}
}

func (g *DiskGrower) finish(state, msg string) DiskRunStatus {
	now := g.Now().UTC()
	g.st.State, g.st.Message, g.st.Step, g.st.FinishedAt = state, msg, "", now
	g.save()
	g.Log.Info("disks: grow ended", "id", g.Plan.ID, "state", state, "message", msg)
	return g.st
}
