//go:build linux

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// askOnDesktop asks the person at the machine's screen, with zenity (GNOME
// and most desktops) or kdialog (KDE), run as them on their display. The
// question's text is an argument, and zenity is told it is not markup, so
// nothing in a name can become anything but text.
func askOnDesktop(ctx context.Context, l tunnel.Logon, title, text string, seconds int) (bool, bool, error) {
	h := platformScreenHost().(linuxScreenHost)
	c, ok := h.console()
	if !ok || c.user == "" {
		return false, false, errors.New("nobody is signed in at this machine's screen to ask")
	}
	if c.user != shortAccount(l.Account) {
		return false, false, errors.New(l.Account + " is not the person at this machine's screen")
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + homeOf(c.uid),
		"XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(c.uid),
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + strconv.Itoa(c.uid) + "/bus",
	}
	if c.display != "" {
		env = append(env, "DISPLAY="+c.display)
		if c.auth != "" {
			env = append(env, "XAUTHORITY="+c.auth)
		} else if home := homeOf(c.uid); home != "" {
			env = append(env, "XAUTHORITY="+filepath.Join(home, ".Xauthority"))
		}
	}
	if c.wayland {
		env = append(env, "WAYLAND_DISPLAY=wayland-0")
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(seconds+5)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	zenity := false
	if bin, err := exec.LookPath("zenity"); err == nil {
		zenity = true
		cmd = exec.CommandContext(cctx, bin, "--question", "--no-markup", "--title", title,
			"--text", text, "--ok-label", "Allow", "--cancel-label", "No",
			"--timeout", strconv.Itoa(seconds))
	} else if bin, err := exec.LookPath("kdialog"); err == nil {
		// kdialog reads rich text where it sees tags: there are none.
		plain := strings.NewReplacer("<", "‹", ">", "›", "&", "+").Replace(text)
		cmd = exec.CommandContext(cctx, bin, "--title", title, "--yes-label", "Allow",
			"--no-label", "No", "--yesno", plain)
	} else {
		return false, false, errors.New("neither zenity nor kdialog is installed to show the question")
	}
	cmd.Env = env
	cmd.Dir = "/"
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 && c.uid != 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(c.uid), Gid: uint32(c.gid), Groups: []uint32{}}
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	if cctx.Err() != nil {
		return false, false, nil // no answer in time
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, true, nil
	case zenity && errors.As(err, &ee) && ee.ExitCode() == 5:
		return false, false, nil // zenity's own timeout
	}
	return false, false, err
}
