//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type linuxPackages struct {
	executable string
	stateDir   string
}

func hostPackagePlatform(executable, stateDir string) packagePlatform {
	return &linuxPackages{executable: executable, stateDir: stateDir}
}

func (p *linuxPackages) supported() string {
	switch {
	case os.Geteuid() != 0:
		return "the agent is not running as root, so it cannot install packages"
	case !systemdRunning():
		return "this server does not run systemd, which updating packages from the console needs"
	case detectManager(exec.LookPath) == nil:
		return errNoManager.Error()
	}
	return ""
}

// check runs `argus-agent packages-check` outside the agent's sandbox,
// which cannot write the package lists, and reads the report it prints.
func (p *linuxPackages) check(ctx context.Context) (PackageReport, error) {
	cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
		"--service-type=exec", "--description=Argus package check", p.executable, "packages-check")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return PackageReport{}, fmt.Errorf("checking for updates failed: %s", tail([]byte(msg), 700))
	}
	var r PackageReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return PackageReport{}, fmt.Errorf("cannot read the check's report: %w", err)
	}
	return r, nil
}

// launch starts the run in a transient unit of its own, outside the
// agent's sandbox and cgroup, so the run goes on whatever the agent does.
func (p *linuxPackages) launch(ctx context.Context, planPath, unit string) error {
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit="+unit, "--service-type=exec", "--description=Argus package update",
		p.executable, "packages-apply", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *linuxPackages) active(unit string) bool {
	// is-active exits 0 for active, activating, deactivating or reloading:
	// the helper is still there.
	return exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run() == nil
}

func (p *linuxPackages) repair(ctx context.Context) error {
	m := detectManager(exec.LookPath)
	if m == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
		"--service-type=exec", "--description=Argus package repair", p.executable, "packages-repair")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, tail(out, 300))
	}
	return nil
}

func (p *linuxPackages) bootID() string { return readBootID() }

// finishRelease runs `argus-agent packages-release-finish` outside the
// agent's sandbox, after the reboot that ends a release upgrade, and reads
// what it found.
func (p *linuxPackages) finishRelease(ctx context.Context) (ReleaseFinish, error) {
	cctx, cancel := context.WithTimeout(ctx, 50*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
		"--service-type=exec", "--description=Argus release upgrade check", p.executable,
		"packages-release-finish", "--state", p.stateDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return ReleaseFinish{}, fmt.Errorf("%v: %s", err, tail(stderr.Bytes(), 300))
	}
	var res ReleaseFinish
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return ReleaseFinish{}, fmt.Errorf("cannot read what it found: %w", err)
	}
	return res, nil
}
