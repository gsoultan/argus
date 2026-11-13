//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

/*
Screen sharing on Linux: the X11 display on the machine's own screen.

The console is the graphical session logind says is active on seat0: the
person's desktop, or the display manager's sign-in screen when nobody is
signed in. Without logind (or a session that does not name its display) it
is the X server running on the machine, found among the processes.

The helper is this binary run as the session's own user, with DISPLAY and
the X server's cookie in its environment and nothing else: it can read the
screen and type into it, as anything that user runs can, and no more. The
cookie is read here, as root, from the file the X server was started with
(its -auth), which is how a sign-in screen's display, whose cookie belongs
to the display manager, is reached as well as a person's.

A Wayland session is refused, saying so: its compositor shows the screen
only to what the person approves in a dialog, so there is no unattended way
in. Signing in with "Xorg" on the sign-in screen's menu, or turning Wayland
off in the display manager, makes it shareable.
*/

type linuxScreenHost struct {
	// runDir is where logind keeps its sessions; proc is /proc. Fields so
	// tests can point them at a fixture.
	runDir, proc string
}

func platformScreenHost() screenHost {
	return linuxScreenHost{runDir: "/run/systemd/sessions", proc: "/proc"}
}

// x11Console is the graphical session on the machine's screen.
type x11Console struct {
	id      string // logind's session id, or "x" plus the display
	user    string // "" for a sign-in screen
	uid     int
	gid     int
	display string
	auth    string // the X server's -auth file, if it has one
	wayland bool
}

func (h linuxScreenHost) ConsoleSession() (uint32, bool) {
	c, ok := h.console()
	if !ok {
		return 0, false
	}
	// The interface counts sessions as Windows does; logind names them.
	// Only whether it changed matters, and a hash of the name says that.
	return crc32.ChecksumIEEE([]byte(c.id + "\x00" + c.display)), true
}

func (h linuxScreenHost) ConsoleAccount(uint32) string {
	c, ok := h.console()
	if !ok {
		return ""
	}
	return c.user
}

// SecureAttention: Linux has no key the system keeps for itself, so
// Ctrl+Alt+Del goes to the desktop as keys, as it does from a keyboard.
func (linuxScreenHost) SecureAttention() error { return errNoSecureAttention }

// console finds the graphical session on seat0.
func (h linuxScreenHost) console() (x11Console, bool) {
	servers := listXServers(h.proc)
	if c, ok := h.logindConsole(servers); ok {
		return c, true
	}
	// No logind: the X server on a local display, the lowest first.
	for _, s := range servers {
		c := x11Console{id: "x" + s.display, display: s.display, auth: s.auth, uid: s.uid, gid: s.gid}
		if s.uid != 0 {
			if u, err := user.LookupId(strconv.Itoa(s.uid)); err == nil {
				c.user = u.Username
			}
		} else {
			// A root X server is a display manager's: run as nobody in
			// particular, and say it is the sign-in screen.
			c.uid, c.gid = 0, 0
		}
		return c, true
	}
	return x11Console{}, false
}

// logindConsole is the active graphical session on seat0, from logind's
// session files.
func (h linuxScreenHost) logindConsole(servers []xServer) (x11Console, bool) {
	entries, err := os.ReadDir(h.runDir)
	if err != nil {
		return x11Console{}, false
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".") {
			continue // .ref files and the like
		}
		kv := readKeyValues(filepath.Join(h.runDir, e.Name()))
		if kv["SEAT"] != "seat0" || kv["ACTIVE"] != "1" || kv["REMOTE"] == "1" {
			continue
		}
		typ := kv["TYPE"]
		if typ != "x11" && typ != "wayland" {
			continue
		}
		uid, err := strconv.Atoi(kv["UID"])
		if err != nil {
			continue
		}
		c := x11Console{id: e.Name(), user: kv["USER"], uid: uid, gid: uid, display: kv["DISPLAY"],
			wayland: typ == "wayland"}
		if u, err := user.LookupId(kv["UID"]); err == nil {
			if g, err := strconv.Atoi(u.Gid); err == nil {
				c.gid = g
			}
		}
		if kv["CLASS"] == "greeter" {
			c.user = "" // the sign-in screen, as the Windows one is ""
		}
		// The X server: the one on the session's display, else the one on
		// its virtual terminal, else the one its user runs.
		vt := kv["VTNR"]
		for _, s := range servers {
			if (c.display != "" && s.display == c.display) ||
				(c.display == "" && vt != "" && s.vt == vt) {
				c.display, c.auth = s.display, s.auth
				break
			}
		}
		if c.display == "" {
			for _, s := range servers {
				if s.uid == uid {
					c.display, c.auth = s.display, s.auth
					break
				}
			}
		}
		return c, true
	}
	return x11Console{}, false
}

// readKeyValues reads a KEY=value file such as logind's session files.
func readKeyValues(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && !strings.HasPrefix(k, "#") {
			out[k] = v
		}
	}
	return out
}

// xServer is an X server running on this machine.
type xServer struct {
	pid      int
	uid, gid int
	display  string
	auth     string
	vt       string // "7" for vt7
}

// listXServers finds the X servers among the processes, lowest display
// first. Xwayland is left out: it shows only the X windows of a Wayland
// desktop, not the screen.
func listXServers(proc string) []xServer {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	var out []xServer
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(proc, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		s, ok := parseXServer(args)
		if !ok {
			continue
		}
		s.pid = pid
		if st, err := os.Stat(filepath.Join(proc, e.Name())); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok {
				s.uid, s.gid = int(sys.Uid), int(sys.Gid)
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := x11DisplayNumber(out[i].display)
		b, _ := x11DisplayNumber(out[j].display)
		return a < b
	})
	return out
}

// parseXServer reads an X server's command line: its display, -auth file
// and virtual terminal. ok is false for anything that is not an X server.
func parseXServer(args []string) (xServer, bool) {
	if len(args) == 0 {
		return xServer{}, false
	}
	switch filepath.Base(args[0]) {
	case "Xorg", "X", "Xorg.bin", "Xvfb", "Xvnc", "Xephyr", "Xnest":
	default:
		return xServer{}, false
	}
	var s xServer
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, ":"):
			if _, ok := x11DisplayNumber(a); ok {
				s.display = a
			}
		case a == "-auth" && i+1 < len(args):
			s.auth = args[i+1]
			i++
		case strings.HasPrefix(a, "vt") && len(a) > 2:
			if _, err := strconv.Atoi(a[2:]); err == nil {
				s.vt = a[2:]
			}
		}
	}
	if s.display == "" {
		// An X server started with no display argument takes :0.
		s.display = ":0"
	}
	return s, true
}

// x11Cookie reads the MIT-MAGIC-COOKIE-1 for display from an Xauthority
// file, as hex: the entry for that display number, else one for any.
func x11Cookie(path, display string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	n, _ := x11DisplayNumber(display)
	want := strconv.Itoa(n)
	var anyDisplay string
	r := bytes.NewReader(data)
	read := func() ([]byte, error) {
		var l uint16
		if err := binary.Read(r, binary.BigEndian, &l); err != nil {
			return nil, err
		}
		b := make([]byte, l)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	for {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			break
		}
		var fields [4][]byte // address, display number, name, data
		for i := range fields {
			if fields[i], err = read(); err != nil {
				return "", errors.New("the X server's authority file is truncated")
			}
		}
		if string(fields[2]) != "MIT-MAGIC-COOKIE-1" || len(fields[3]) != 16 {
			continue
		}
		switch string(fields[1]) {
		case want:
			return hex.EncodeToString(fields[3]), nil
		case "":
			anyDisplay = hex.EncodeToString(fields[3])
		default:
			if anyDisplay == "" {
				anyDisplay = hex.EncodeToString(fields[3])
			}
		}
	}
	if anyDisplay != "" {
		return anyDisplay, nil
	}
	return "", errors.New("the X server's authority file has no cookie for " + display)
}

// StartHelper runs "argus-agent screen-helper" as the session's user, in
// a process group of its own, with the display and its cookie.
func (h linuxScreenHost) StartHelper(_ uint32, mode helperMode) (io.ReadWriteCloser, error) {
	c, ok := h.console()
	if !ok {
		return nil, errors.New("nothing is showing on this machine's screen: no X server is running")
	}
	if c.wayland {
		return nil, errors.New("the session on this machine's screen is Wayland, which shows the screen " +
			"only to what the person at it approves; sign in with an Xorg session, or turn Wayland " +
			"off in the display manager, to share it")
	}
	if c.display == "" {
		return nil, errors.New("could not find the X server of the session on this machine's screen")
	}
	env := []string{"DISPLAY=" + c.display, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if c.auth != "" {
		cookie, err := x11Cookie(c.auth, c.display)
		if err != nil {
			return nil, fmt.Errorf("reading the X server's cookie: %w", err)
		}
		env = append(env, x11CookieEnv+"="+cookie)
	} else if home := homeOf(c.uid); home != "" {
		// Started without -auth (startx, xinit): the user's own file.
		if cookie, err := x11Cookie(filepath.Join(home, ".Xauthority"), c.display); err == nil {
			env = append(env, x11CookieEnv+"="+cookie)
		}
	}
	return startUnixHelper(env, c.uid, c.gid, mode)
}

func homeOf(uid int) string {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// startUnixHelper starts the helper with env, as uid and gid when this
// process may change to them, in a process group of its own that dies with
// the agent.
func startUnixHelper(env []string, uid, gid int, mode helperMode) (io.ReadWriteCloser, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, helperArgs(mode)...)
	cmd.Env = env
	cmd.Dir = "/"
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 && uid != 0 {
		// No supplementary groups: root's are not the helper's to keep.
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	}
	cmd.SysProcAttr = attr
	return startHelperCmd(cmd)
}

/* ── The helper ──────────────────────────────────────────────────────────── */

// RunScreenHelper is "argus-agent screen-helper" on Linux: it serves the X
// display in DISPLAY on stdin and stdout until stdin closes.
func RunScreenHelper(args []string, in io.Reader, out io.Writer) int {
	// Run as a person, it must be only them: the agent's capabilities end
	// at the change of user, and a helper that still has any refuses to
	// look at the screen with them.
	if os.Getuid() != 0 {
		if eff := effectiveCaps(); eff != 0 {
			_ = writeScreenMsg(out, scrFail, []byte(fmt.Sprintf(
				"the screen helper still holds capabilities (%#x) after changing user", eff)))
			return 1
		}
	}
	c, err := newX11Capturer()
	if err != nil {
		_ = writeScreenMsg(out, scrFail, []byte(err.Error()))
		return 1
	}
	newInput := func() screenInputer { return newX11Inputer() }
	// No H.264 encoder here yet: motion goes as exact tiles, which the
	// helper says, so the agent and its log know.
	var newVideo func(int, int) (screenVideo, error)
	for _, a := range args {
		if a == "--video" {
			newVideo = newLinuxVideo
		}
	}
	if err := serveScreen(c, newInput, in, out, defaultScreenPace, newVideo); err != nil {
		_ = writeScreenMsg(out, scrFail, []byte(err.Error()))
		return 1
	}
	return 0
}

// effectiveCaps reads this process's effective capabilities.
func effectiveCaps() uint64 {
	kv := map[string]string{}
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				kv[k] = strings.TrimSpace(v)
			}
		}
	}
	v, _ := strconv.ParseUint(kv["CapEff"], 16, 64)
	return v
}
