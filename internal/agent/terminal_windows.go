package agent

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A session on Windows.

The shell runs on a pseudoconsole: Windows' terminal, whose screen the agent
reads and whose keyboard it writes, as it does a pty master on Unix. It runs
as the account, with a token from an S4U logon (see logon_windows.go), with
the account's profile loaded so its registry and home exist.

It is created suspended -- the process exists, but has run nothing -- and
resumed only once the recording is open. Every process the session starts is
in a job object, so the whole session, not only the shell, can be ended.
*/

/* ── The pseudoconsole ───────────────────────────────────────────────────── */

type conPTY struct {
	hpc     windows.Handle
	in, out *os.File // our ends: what the shell reads, what it writes

	closeOnce sync.Once
}

func newConPTY(cols, rows int) (*conPTY, *os.File, *os.File, error) {
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, nil, nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, nil, nil, err
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(coord(cols, rows), inR, outW, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, nil, nil, fmt.Errorf("cannot create a console: %w", err)
	}
	c := &conPTY{hpc: hpc,
		in:  os.NewFile(uintptr(inW), "conpty-in"),
		out: os.NewFile(uintptr(outR), "conpty-out")}
	// The console's ends, which the caller closes once the shell has
	// started: the console holds its own copies.
	return c, os.NewFile(uintptr(inR), "console-in"), os.NewFile(uintptr(outW), "console-out"), nil
}

func coord(cols, rows int) windows.Coord {
	return windows.Coord{X: int16(min(max(cols, 1), 32767)), Y: int16(min(max(rows, 1), 32767))}
}

func (c *conPTY) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(c.hpc, coord(cols, rows))
}

// Close closes the console. The shell and everything attached to it are
// told the console is gone; what they still write drains into our end of
// the output pipe, which reads EOF once the console has exited.
func (c *conPTY) Close() error {
	c.closeOnce.Do(func() {
		_ = c.in.Close()
		// Older Windows block in ClosePseudoConsole until the output is
		// drained, which the session's reader does, so do not wait here.
		go func() {
			windows.ClosePseudoConsole(c.hpc)
			_ = c.out.Close()
		}()
	})
	return nil
}

/* ── The shell ───────────────────────────────────────────────────────────── */

type windowsProc struct {
	proc, thread, job windows.Handle
	pid               int
	token             windows.Token
	profile           windows.Handle

	waitOnce sync.Once
	code     int
	done     chan struct{}
}

func openShell(cfg TerminalConfig, g tunnel.Grant) (terminalIO, shellProc, string, error) {
	token, err := s4uToken(g.Principal)
	if err != nil {
		return nil, nil, "", err
	}
	user, _, _ := splitAccount(g.Principal)
	profile, err := loadProfile(token, user)
	if err != nil {
		token.Close()
		return nil, nil, "", fmt.Errorf("cannot load the profile of %s: %w", user, err)
	}
	pty, consoleIn, consoleOut, err := newConPTY(startCols, startRows)
	if err != nil {
		unloadProfile(token, profile)
		token.Close()
		return nil, nil, "", err
	}
	proc, err := spawnConsoleShell(pty, token, profile, g.Email)
	_ = consoleIn.Close()
	_ = consoleOut.Close()
	if err != nil {
		_ = pty.Close()
		unloadProfile(token, profile)
		token.Close()
		return nil, nil, "", fmt.Errorf("cannot start a shell for %s: %w", user, err)
	}
	return pty, proc, user, nil
}

func spawnConsoleShell(pty *conPTY, token windows.Token, profile windows.Handle,
	actor string) (*windowsProc, error) {
	env, err := token.Environ(false)
	if err != nil {
		return nil, err
	}
	env = append(env, "ARGUS_SESSION_FOR="+actor)
	home, err := token.GetUserProfileDirectory()
	if err != nil {
		return nil, err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself, not a pointer
	// to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&pty.hpc)), unsafe.Sizeof(pty.hpc)); err != nil {
		return nil, err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// An empty desktop, not none: the account gets a window station of its
	// own instead of the service's, which it may not open.
	si.Desktop = windows.StringToUTF16Ptr("")

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	shell := loginShell()
	cmdline := windows.ComposeCommandLine([]string{shell})
	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, nil, windows.StringToUTF16Ptr(cmdline),
		nil, nil, false,
		windows.CREATE_SUSPENDED|windows.EXTENDED_STARTUPINFO_PRESENT|
			windows.CREATE_UNICODE_ENVIRONMENT,
		envBlock(env), windows.StringToUTF16Ptr(home), &si.StartupInfo, &pi); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	p := &windowsProc{proc: pi.Process, thread: pi.Thread, job: job,
		pid: int(pi.ProcessId), token: token, profile: profile, done: make(chan struct{})}
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		// Not contained, so not started: ending it later could miss what
		// it started.
		p.Abort()
		p.Wait()
		return nil, err
	}
	return p, nil
}

// envBlock is an environment as CreateProcess takes it.
func envBlock(env []string) *uint16 {
	var b []uint16
	for _, kv := range env {
		b = append(b, windows.StringToUTF16(kv)...)
	}
	b = append(b, 0)
	return &b[0]
}

// loginShell is the shell OpenSSH would start, so a session through Argus
// looks like one through ssh: its DefaultShell setting, else cmd.exe.
func loginShell() string {
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\OpenSSH`,
		registry.QUERY_VALUE); err == nil {
		defer k.Close()
		if s, _, err := k.GetStringValue("DefaultShell"); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		return root + `\System32\cmd.exe`
	}
	return `C:\Windows\System32\cmd.exe`
}

func (p *windowsProc) Pid() int        { return p.pid }
func (p *windowsProc) Summary() string { return "" }

func (p *windowsProc) Release() error {
	if _, err := windows.ResumeThread(p.thread); err != nil {
		return err
	}
	return nil
}

// Abort ends a shell that never ran.
func (p *windowsProc) Abort() { _ = windows.TerminateProcess(p.proc, 255) }

func (p *windowsProc) Wait() int {
	p.waitOnce.Do(func() {
		_, _ = windows.WaitForSingleObject(p.proc, windows.INFINITE)
		var code uint32
		if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
			code = 255
		}
		p.code = int(code)
		// Closing the job ends whatever the shell left running: nobody is
		// watching it any more.
		windows.CloseHandle(p.job)
		windows.CloseHandle(p.thread)
		windows.CloseHandle(p.proc)
		unloadProfile(p.token, p.profile)
		p.token.Close()
		close(p.done)
	})
	return p.code
}

// Hangup follows the console's closing: the session's processes were told
// it went away, and whatever is still running after hangupWait is ended.
func (p *windowsProc) Hangup() {
	go func() {
		select {
		case <-p.done:
		case <-time.After(hangupWait):
			_ = windows.TerminateJobObject(p.job, 1)
		}
	}()
}

/* ── The profile ─────────────────────────────────────────────────────────── */

var (
	modUserenv            = windows.NewLazySystemDLL("userenv.dll")
	procLoadUserProfileW  = modUserenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile = modUserenv.NewProc("UnloadUserProfile")
)

// profileInfo is PROFILEINFOW.
type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

const piNoUI = 1

// loadProfile loads the account's profile, creating it on its first
// session, as an interactive logon would.
func loadProfile(token windows.Token, user string) (windows.Handle, error) {
	pi := profileInfo{Flags: piNoUI, UserName: windows.StringToUTF16Ptr(user)}
	pi.Size = uint32(unsafe.Sizeof(pi))
	if r, _, err := procLoadUserProfileW.Call(uintptr(token), uintptr(unsafe.Pointer(&pi))); r == 0 {
		return 0, err
	}
	return pi.Profile, nil
}

func unloadProfile(token windows.Token, profile windows.Handle) {
	if profile != 0 {
		procUnloadUserProfile.Call(uintptr(token), uintptr(profile))
	}
}
