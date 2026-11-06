//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// agentServiceName is the service `argus-agent service install` registers.
const agentServiceName = "argus-agent"

// windowsHowToLook is where a person looks when an update or restart fails.
const windowsHowToLook = `Sign in to the server as an administrator and run: sc.exe query argus-agent, ` +
	`and read C:\ProgramData\Argus\agent.log and C:\ProgramData\Argus\update-helper.log`

// helperLog is where the update helper writes what it did. The helper has
// no console and runs after the agent that would log for it has stopped.
const helperLog = "update-helper.log"

type windowsUpdates struct {
	executable string

	// The service's registration changes only when someone reinstalls it,
	// so it is read once a few minutes, not on every heartbeat.
	mu      sync.Mutex
	checked time.Time
	reason  string
}

func hostUpdatePlatform(executable string) updatePlatform {
	return &windowsUpdates{executable: executable}
}

// check is what restarting needs, plus a processor there is a release for.
func (p *windowsUpdates) check() string {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "there is no agent release for this processor (" + runtime.GOARCH + ")"
	}
	return p.restartCheck()
}

// restartCheck needs the agent to be the argus-agent service, running the
// file it would replace, with the rights to stop and start it.
func (p *windowsUpdates) restartCheck() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.checked.IsZero() && time.Since(p.checked) < 5*time.Minute {
		return p.reason
	}
	p.reason, p.checked = p.serviceReason(), time.Now()
	return p.reason
}

func (p *windowsUpdates) serviceReason() string {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return "the agent is not running as an administrator or SYSTEM, so it cannot replace " +
			"itself or restart its service"
	}
	if ok, err := svc.IsWindowsService(); err != nil || !ok {
		return "the agent is not running as the argus-agent Windows service; install it with " +
			"argus-agent service install, then updates work from here"
	}
	m, err := mgr.Connect()
	if err != nil {
		return "the agent cannot reach the Windows service manager: " + err.Error()
	}
	defer m.Disconnect()
	s, err := m.OpenService(agentServiceName)
	if err != nil {
		return "there is no argus-agent service on this server; install it with " +
			"argus-agent service install, then updates work from here"
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return "the agent cannot read its service's settings: " + err.Error()
	}
	if bin := serviceBinary(cfg.BinaryPathName); !strings.EqualFold(filepath.Clean(bin), filepath.Clean(p.executable)) {
		return "the argus-agent service starts " + bin + ", not the running agent at " +
			p.executable + "; reinstall the service from the agent it should run"
	}
	return ""
}

// serviceBinary is the executable in a service's command line, which may be
// quoted and is followed by its arguments.
func serviceBinary(cmdline string) string {
	cmdline = strings.TrimSpace(cmdline)
	if strings.HasPrefix(cmdline, `"`) {
		if end := strings.Index(cmdline[1:], `"`); end >= 0 {
			return cmdline[1 : end+1]
		}
		return strings.Trim(cmdline, `"`)
	}
	if i := strings.Index(strings.ToLower(cmdline), ".exe"); i >= 0 {
		return cmdline[:i+4]
	}
	return cmdline
}

func (p *windowsUpdates) format() string { return "zip" }

// fileName is the archive goreleaser publishes for Windows; the agent is
// argus-agent.exe inside it.
func (p *windowsUpdates) fileName(version string) string {
	return fmt.Sprintf("argus_%s_windows_%s.zip", version, runtime.GOARCH)
}

// unpack takes argus-agent.exe out of the verified archive.
func (p *windowsUpdates) unpack(archive, dir string) (string, error) {
	return unpackFromZip(archive, dir, "argus-agent.exe")
}

// launch starts the helper as a process of its own, detached from the
// service, which it is about to stop: the service manager does not end a
// service's other processes when it stops, and the helper leaves any job
// the agent is in. It runs from a copy of this binary, because the binary
// is what it replaces.
func (p *windowsUpdates) launch(ctx context.Context, planPath string) error {
	dir := filepath.Dir(planPath)
	helper := filepath.Join(dir, "argus-agent-update.exe")
	if err := copyFile(p.executable, helper, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(dir), helperLog)
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	start := func(flags uint32) error {
		cmd := exec.Command(helper, "update-apply", "--plan", planPath)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	base := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if err := start(base | windows.CREATE_BREAKAWAY_FROM_JOB); err != nil {
		// A job that does not allow leaving it refuses the flag; the
		// service manager still leaves the helper running.
		if err2 := start(base); err2 != nil {
			return fmt.Errorf("could not start the update helper: %v", err2)
		}
	}
	return nil
}

// RunUpdateApply is `argus-agent update-apply --plan <path>`.
func RunUpdateApply(planPath string, stdout io.Writer) int {
	log := slog.New(slog.NewTextHandler(stdout, nil))
	b, err := os.ReadFile(planPath)
	if err != nil {
		log.Error("update: cannot read the plan", "error", err)
		return 1
	}
	var plan UpdatePlan
	if err := json.Unmarshal(b, &plan); err != nil {
		log.Error("update: cannot read the plan", "error", err)
		return 1
	}
	name := plan.Unit
	if name == "" {
		name = agentServiceName
	}
	a := &ServiceApplier{Plan: plan, Log: log, Service: scmService{name: name},
		HowToLook: windowsHowToLook}
	if a.Apply() != UpdateDone {
		return 1
	}
	return 0
}

/* ── The service manager ─────────────────────────────────────────────────── */

type scmService struct{ name string }

func (s scmService) with(f func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	svcH, err := m.OpenService(s.name)
	if err != nil {
		return err
	}
	defer svcH.Close()
	return f(svcH)
}

func (s scmService) Stop(wait time.Duration) error {
	return s.with(func(h *mgr.Service) error {
		if _, err := h.Control(svc.Stop); err != nil &&
			!errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) &&
			!errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
			return fmt.Errorf("asking it to stop: %w", err)
		}
		if waitServiceState(h, svc.Stopped, wait) {
			return nil
		}
		// Not stopped in time: end its process, as systemd would after
		// TimeoutStopSec.
		if q, err := h.Query(); err == nil && q.ProcessId != 0 {
			if ph, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, q.ProcessId); err == nil {
				_ = windows.TerminateProcess(ph, 1)
				_ = windows.CloseHandle(ph)
			}
		}
		if waitServiceState(h, svc.Stopped, 15*time.Second) {
			return nil
		}
		return fmt.Errorf("it did not stop within %s", wait)
	})
}

func (s scmService) Start() error {
	return s.with(func(h *mgr.Service) error {
		if err := h.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		return nil
	})
}

func (s scmService) Process() uint32 {
	var pid uint32
	_ = s.with(func(h *mgr.Service) error {
		q, err := h.Query()
		if err == nil && q.State != svc.Stopped && q.State != svc.StopPending {
			pid = q.ProcessId
		}
		return err
	})
	return pid
}

func waitServiceState(h *mgr.Service, want svc.State, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		q, err := h.Query()
		if err == nil && q.State == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
