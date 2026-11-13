//go:build darwin

package agent

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

/*
Screen sharing on macOS: the Mac's own display, as the person at it sees it.

The console is whoever owns /dev/console: the person signed in at the
screen, or root at the login window. The helper is this binary, started by
launchctl asuser in that person's login (the only place the window server
will show the screen to), which then gives up root for their user before
it looks at anything: it can see and drive their screen, as anything they
run can, and nothing of the agent's.

macOS asks the person, once, before anything records the screen or plays
input: argus-agent has to be allowed under Privacy & Security, in Screen
Recording and in Accessibility. A helper that is not says so, and the
session ends with that as its reason; an organisation's MDM can allow
Accessibility in advance with a privacy profile, and Screen Recording only
by letting the person allow it.
*/

type macScreenHost struct{}

func platformScreenHost() screenHost { return macScreenHost{} }

// consoleUID is the owner of /dev/console.
func consoleUID() (uint32, bool) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func (macScreenHost) ConsoleSession() (uint32, bool) {
	uid, ok := consoleUID()
	if !ok {
		return 0, false
	}
	// A session per person at the screen: a switch of user is a new one.
	return crc32.ChecksumIEEE([]byte("console:" + strconv.FormatUint(uint64(uid), 10))), true
}

func (macScreenHost) ConsoleAccount(uint32) string {
	uid, ok := consoleUID()
	if !ok || uid == 0 {
		return "" // the login window
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return ""
	}
	return u.Username
}

// SecureAttention: a Mac has no such sequence; the keys go as keys.
func (macScreenHost) SecureAttention() error { return errNoSecureAttention }

// macScreenUIDEnv and macScreenGIDEnv tell the helper whom to become.
const (
	macScreenUIDEnv = "ARGUS_SCREEN_UID"
	macScreenGIDEnv = "ARGUS_SCREEN_GID"
)

func (macScreenHost) StartHelper(_ uint32, mode helperMode) (io.ReadWriteCloser, error) {
	uid, ok := consoleUID()
	if !ok {
		return nil, errors.New("could not tell who is at this Mac's screen")
	}
	gid := uint32(20) // staff
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		if g, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
			gid = uint32(g)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := append([]string{"asuser", strconv.FormatUint(uint64(uid), 10), exe}, helperArgs(mode)...)
	cmd := exec.Command("/bin/launchctl", args...)
	cmd.Env = []string{
		macScreenUIDEnv + "=" + strconv.FormatUint(uint64(uid), 10),
		macScreenGIDEnv + "=" + strconv.FormatUint(uint64(gid), 10),
		"PATH=/usr/bin:/bin",
	}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startHelperCmd(cmd)
}

// growPipe: macOS pipes grow on their own, to 64 KB, and cannot be set.
func growPipe(*os.File) {}

/* ── The helper ──────────────────────────────────────────────────────────── */

// RunScreenHelper is "argus-agent screen-helper" on macOS: in the person's
// login, it becomes them and serves the screen on stdin and stdout until
// stdin closes.
func RunScreenHelper(args []string, in io.Reader, out io.Writer) int {
	fail := func(err error) int {
		_ = writeScreenMsg(out, scrFail, []byte(err.Error()))
		return 1
	}
	if err := becomeConsoleUser(); err != nil {
		return fail(err)
	}
	cg, err := loadCoreGraphics()
	if err != nil {
		return fail(err)
	}
	if !cg.screenAllowed() {
		return fail(errors.New("macOS has not allowed argus-agent to record the screen: allow it in " +
			"System Settings, Privacy & Security, Screen Recording (or Screen & System Audio Recording), " +
			"then connect again"))
	}
	if !cg.inputAllowed() {
		return fail(errors.New("macOS has not allowed argus-agent to control the computer: allow it in " +
			"System Settings, Privacy & Security, Accessibility, then connect again"))
	}
	c, err := newMacCapturer(cg)
	if err != nil {
		return fail(err)
	}
	newInput := func() screenInputer { return newMacInputer(cg) }
	var newVideo func(int, int) (screenVideo, error)
	for _, a := range args {
		if a == "--video" {
			newVideo = newMacVideo
		}
	}
	if err := serveScreen(c, newInput, in, out, defaultScreenPace, newVideo); err != nil {
		return fail(err)
	}
	return 0
}

// becomeConsoleUser gives up root for the user the agent named, if it was
// started as root to get into their login.
func becomeConsoleUser() error {
	if os.Getuid() != 0 {
		return nil
	}
	uid, err1 := strconv.Atoi(os.Getenv(macScreenUIDEnv))
	gid, err2 := strconv.Atoi(os.Getenv(macScreenGIDEnv))
	if err1 != nil || err2 != nil {
		return errors.New("the screen helper was not told whose screen it is")
	}
	if uid == 0 {
		// The login window: nobody's account to become.
		return nil
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("dropping root's groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("becoming group %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("becoming user %d: %w", uid, err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid {
		return errors.New("the screen helper is still root after giving it up")
	}
	return nil
}
