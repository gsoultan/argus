//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Windows: the agent is SYSTEM with no sandbox, so the scan and the grow
// run in it, the grow in a goroutine like a log cleanup.

type windowsDisks struct{ stateDir string }

func hostDiskPlatform(_ string, stateDir string) diskPlatform {
	return &windowsDisks{stateDir: stateDir}
}

func (p *windowsDisks) supported() string { return "" }

func (p *windowsDisks) scan(ctx context.Context, req DiskScanRequest) (DiskReport, error) {
	return scanHostDisks(ctx, req)
}

var windowsGrows sync.Map // unit -> struct{}

func (p *windowsDisks) launch(_ context.Context, planPath, unit string) error {
	b, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var plan DiskPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		return err
	}
	windowsGrows.Store(unit, struct{}{})
	go func() {
		defer windowsGrows.Delete(unit)
		defer os.Remove(planPath)
		ctx, cancel := context.WithTimeout(context.Background(), diskGrowTimeout)
		defer cancel()
		g := &DiskGrower{Plan: plan, Log: slog.Default(), ops: hostDiskOps()}
		g.Grow(ctx)
	}()
	return nil
}

func (p *windowsDisks) active(unit string) bool {
	_, ok := windowsGrows.Load(unit)
	return ok
}

func (p *windowsDisks) bootID() string { return "" }

func runPowerShell(ctx context.Context, script string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	argv := powershellArgv(script)
	return exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
}

func scanHostDisks(ctx context.Context, req DiskScanRequest) (DiskReport, error) {
	if req.Rescan {
		_, _ = runPowerShell(ctx, "Update-HostStorageCache")
	}
	out, err := runPowerShell(ctx, winScanScript)
	if err != nil {
		return DiskReport{}, &psError{out: out, err: err}
	}
	r, err := parseWinDisks(out)
	if err != nil {
		return r, err
	}
	r.ScannedAt, r.Rescanned = time.Now().UTC(), req.Rescan
	return r, nil
}

type psError struct {
	out []byte
	err error
}

func (e *psError) Error() string { return "reading the disks failed: " + tail(e.out, 400) }

func hostDiskOps() diskOps { return windowsDiskOps{} }

type windowsDiskOps struct{}

func (windowsDiskOps) scan(ctx context.Context, rescanDisk string) (DiskReport, error) {
	return scanHostDisks(ctx, DiskScanRequest{Rescan: rescanDisk != ""})
}

func (windowsDiskOps) run(ctx context.Context, c diskCmd) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, c.Argv[0], c.Argv[1:]...).CombinedOutput()
	return string(out), err
}

func (windowsDiskOps) steps(_ DiskGrowRequest, p GrowPath) ([]diskCmd, error) {
	return winGrowSteps(p), nil
}

func (windowsDiskOps) table(_ context.Context, p GrowPath) (diskCmd, bool) {
	return winTableCmd(p), true
}

func (windowsDiskOps) rescanDisk(p GrowPath) string { return p.Disk }
