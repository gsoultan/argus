package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/gsoultan/argus/internal/tunnel"
)

// The Windows half of winsoft.go: reading the Uninstall keys, and starting
// the helper as a process of its own.

func init() { hostWinInventory = readUninstallKeys }

const uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`

// readUninstallKeys reads both registry views of HKLM's Uninstall key:
// 64-bit programs, and 32-bit ones under WOW6432Node.
func readUninstallKeys() ([]WinEntry, error) {
	var out []WinEntry
	var firstErr error
	for _, v := range []struct {
		name   string
		access uint32
	}{{"64", registry.WOW64_64KEY}, {"32", registry.WOW64_32KEY}} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|v.access)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		for _, n := range names {
			sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE|v.access)
			if err != nil {
				continue
			}
			str := func(name string) string { s, _, _ := sk.GetStringValue(name); return s }
			num := func(name string) uint64 { u, _, _ := sk.GetIntegerValue(name); return u }
			out = append(out, WinEntry{
				Key: n, View: v.name,
				DisplayName: str("DisplayName"), DisplayVersion: str("DisplayVersion"), Publisher: str("Publisher"),
				InstallDate: str("InstallDate"), EstimatedSizeKB: int64(num("EstimatedSize")),
				UninstallString: str("UninstallString"), QuietUninstallString: str("QuietUninstallString"),
				WindowsInstaller: num("WindowsInstaller") == 1, SystemComponent: num("SystemComponent") == 1,
				ParentKeyName: str("ParentKeyName"), ReleaseType: str("ReleaseType"),
			})
			sk.Close()
		}
		k.Close()
	}
	if out == nil && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func hostSoftwareAsk(string) func(context.Context, tunnel.GrantSoftware) tunnel.SoftwareResult {
	return WinSoftwareAsker{Inventory: readUninstallKeys}.Ask
}

/* ── The helper process ──────────────────────────────────────────────────── */

const notLinux = "operating system updates from the console are for Linux servers; Windows Update stays in charge of Windows"

type windowsPackages struct {
	executable string
	stateDir   string
}

func hostPackagePlatform(executable, stateDir string) packagePlatform {
	return &windowsPackages{executable: executable, stateDir: stateDir}
}

func (p *windowsPackages) supported() string { return notLinux }

// installs: what a Windows server can do, when the agent runs elevated
// (as the service does, as LocalSystem).
func (p *windowsPackages) installs() []string {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return nil
	}
	return []string{PackagesRemove, PackagesScript, PackagesFileInstall}
}

func (p *windowsPackages) check(context.Context) (PackageReport, error) {
	return PackageReport{Reason: notLinux}, nil
}

// launch starts `packages-apply` detached from the service, from a copy of
// the binary so an agent update can replace the real one meanwhile, and
// notes its process id for active.
func (p *windowsPackages) launch(_ context.Context, planPath, unit string) error {
	dir := filepath.Dir(planPath)
	helper := filepath.Join(dir, "argus-agent-packages.exe")
	if err := copyFile(p.executable, helper, 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir, unit+".log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	start := func(flags uint32) (int, error) {
		cmd := exec.Command(helper, "packages-apply", "--plan", planPath)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		pid := cmd.Process.Pid
		return pid, cmd.Process.Release()
	}
	base := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	pid, err := start(base | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		if pid, err = start(base); err != nil {
			return fmt.Errorf("could not start the helper: %v", err)
		}
	}
	return os.WriteFile(filepath.Join(dir, unit+".pid"), []byte(strconv.Itoa(pid)), 0o600)
}

// active is whether the helper's process is still there.
func (p *windowsPackages) active(unit string) bool {
	b, err := os.ReadFile(filepath.Join(p.stateDir, packagesDir, unit+".pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

func (p *windowsPackages) repair(context.Context) error { return nil }

func (p *windowsPackages) bootID() string { return "" }
