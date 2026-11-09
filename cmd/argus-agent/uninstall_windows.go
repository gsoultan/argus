package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/gsoultan/argus/internal/agent"
)

// stopAgent stops the service and waits for it to have stopped, which is
// after it has sealed its recordings. Not installed is not an error: there is
// nothing running to stop.
func stopAgent() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Stopped && st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
	}
	// The service asks for 70 seconds to drain and seal; a little more here.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if st, err = s.Query(); err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("the service is still stopping after 90 seconds")
}

func startAgent() error { return startService() }

// removeAgentService deletes the service. The binary is the one running this,
// which Windows does not let a process delete, so that is left to the person:
// through Apps when the installer put it there, so Windows forgets it too,
// and by hand otherwise. Run by the installer's own removal (fromPackage),
// the installer takes the program away itself.
func removeAgentService(fromPackage bool) ([]string, error) {
	var left []string
	if err := uninstallService(); err != nil && err.Error() != "not installed" {
		return nil, fmt.Errorf("the service could not be removed: %w", err)
	}
	if fromPackage {
		return nil, nil
	}
	if code := installedProductCode(); code != "" {
		return append(left, "Remove the program: Settings > Apps > Installed apps > Argus agent > "+
			"Uninstall, or msiexec /x "+code+" /qn"), nil
	}
	if exe, err := os.Executable(); err == nil {
		left = append(left, fmt.Sprintf("Delete the program: Remove-Item -Recurse '%s'",
			filepath.Dir(exe)))
	}
	return left, nil
}

// msiDisplayName is the name the installer registers under (argus-agent.wxs).
const msiDisplayName = "Argus agent"

// installedProductCode is the product code of the Argus agent the Windows
// installer put on this machine, or "" when it was installed by hand.
func installedProductCode() string {
	const uninstall = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall,
		registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return ""
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "{") {
			continue
		}
		sub, err := registry.OpenKey(k, name, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		dn, _, _ := sub.GetStringValue("DisplayName")
		wi, _, _ := sub.GetIntegerValue("WindowsInstaller")
		sub.Close()
		if dn == msiDisplayName && wi == 1 {
			return name
		}
	}
	return ""
}

// undoHostChanges puts back the one machine setting the agent changes on its
// own: the SoftwareSASGeneration policy, set so the console can send
// Ctrl+Alt+Del to the sign-in screen. Only where the agent set it -- its
// marker says so -- and only while it still holds the agent's value; one an
// administrator set or changed since is theirs and stays.
func undoHostChanges() (notes, left []string) {
	marker := filepath.Join(agent.DefaultStateDir, agent.SASPolicyMarker)
	if _, err := os.Stat(marker); err != nil {
		return nil, nil
	}
	const policy = `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, policy, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, []string{"Put back SoftwareSASGeneration under HKLM\\" + policy +
			": the agent set it to 1 and it could not be opened (" + err.Error() + ")."}
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("SoftwareSASGeneration")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		notes = append(notes, "SoftwareSASGeneration is already unset")
	case err != nil:
		return nil, []string{"Check SoftwareSASGeneration under HKLM\\" + policy + ": " + err.Error() + "."}
	case v != 1:
		notes = append(notes, "SoftwareSASGeneration was changed since the agent set it; left as it is")
	default:
		if err := k.DeleteValue("SoftwareSASGeneration"); err != nil {
			return nil, []string{"Delete SoftwareSASGeneration under HKLM\\" + policy +
				" (the agent set it to 1): " + err.Error() + "."}
		}
		notes = append(notes, "unset SoftwareSASGeneration, which the agent had set so the "+
			"console could send Ctrl+Alt+Del")
	}
	_ = os.Remove(marker)
	return notes, nil
}

// removeSSHDHook has nothing to undo: the sshd hook is not ported to Windows,
// so the agent never added one.
func removeSSHDHook() (notes, left []string, err error) {
	return []string{"the agent adds nothing to sshd on Windows; nothing to change"}, nil, nil
}

// remainingSSHWaysIn has nothing to check: the agent never stood between
// Windows' OpenSSH server and its logins.
func remainingSSHWaysIn(map[string]bool) ([]string, bool) { return nil, false }

// stopSelfForGood deletes the service, which Windows does once it has
// stopped, and asks it to stop: the same request a shutdown makes.
func stopSelfForGood() error {
	err := uninstallService()
	if err != nil && err.Error() != "not installed" {
		return err
	}
	return nil
}
