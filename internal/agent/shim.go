package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// ShimConfig controls how a session is wrapped.
type ShimConfig struct {
	// SocketPath is the collector to stream to.
	SocketPath string
	// GatewayKeys is the path to the gateway's public keys, in authorized_keys
	// format. A session that authenticated with one of these came through
	// Argus and is already recorded. This is the authoritative signal.
	GatewayKeys string
	// GatewayAddrs is a weak fallback used only when sshd does not expose auth
	// info. See OriginDetector.Detect for why address matching alone is never
	// treated as proof.
	GatewayAddrs []string
	// SkipBrokered stops the agent recording sessions it believes came through
	// the gateway, on the grounds the gateway already recorded them.
	//
	// Off by default, and that default is deliberate. "Brokered" is inferred
	// from the key that authenticated, which really means "whoever holds the
	// gateway's key" — and that is exactly the credential an attacker steals in
	// order to bypass the gateway. Skipping on that signal would let a stolen
	// key silence the agent, turning the strongest control into an off switch.
	//
	// Recording both ends costs storage and leaves the control plane to
	// reconcile two artefacts per brokered session. That is a far better
	// trade than a recording gap an attacker can trigger on demand.
	SkipBrokered bool
	// FailOpen decides what happens when the collector is unreachable.
	//
	// False (the default) refuses the session: if it cannot be recorded, it
	// does not happen. That is what "all privileged sessions are recorded"
	// requires, and it is the correct posture for a PAM product.
	//
	// True lets the session proceed unrecorded. Only sensible during rollout,
	// when locking every admin out of the fleet because a daemon crashed is a
	// worse outcome than a gap in the audit trail.
	FailOpen bool
	// Shell overrides the login shell; empty means look it up.
	Shell string
}

// RunShim wraps the user's session, streaming it to the collector.
//
// sshd invokes this via ForceCommand, so it runs as the connecting user with
// their environment. It allocates a PTY, execs their real shell (or the command
// they asked for), and tees both directions. It never touches the recording
// file — only the socket.
//
// Returns the exit code the shim itself should exit with.
func RunShim(cfg ShimConfig) int {
	origCmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	clientAddr, _, _ := strings.Cut(os.Getenv("SSH_CONNECTION"), " ")

	// Identify the gateway by the key it authenticated with, not by where the
	// packets appear to come from. Behind NAT or a port forward every client
	// shares one source address, so an address rule would let anyone on that
	// path opt out of being recorded.
	fingerprints, err := LoadGatewayFingerprints(cfg.GatewayKeys)
	if err != nil {
		fingerprints = map[string]bool{}
	}
	origin, originReason := OriginDetector{
		GatewayKeyFingerprints: fingerprints,
		GatewayAddrs:           cfg.GatewayAddrs,
	}.Detect()

	// Only skip when the operator has explicitly accepted the trade — see
	// SkipBrokered. The safe default records everything and lets the control
	// plane deduplicate, because the alternative is a gap an attacker can
	// create simply by stealing the gateway's key.
	if origin == Brokered && cfg.SkipBrokered {
		return execSession(cfg, origCmd, nil)
	}

	conn, err := net.DialTimeout("unix", cfg.SocketPath, 5*time.Second)
	if err != nil {
		if !cfg.FailOpen {
			fmt.Fprintln(os.Stderr,
				"argus: session recording is unavailable, refusing connection.")
			fmt.Fprintln(os.Stderr,
				"argus: contact your administrator — the host agent is not running.")
			return 1
		}
		fmt.Fprintln(os.Stderr,
			"argus: WARNING — recording unavailable, this session is NOT being recorded.")
		return execSession(cfg, origCmd, nil)
	}
	defer conn.Close()

	hostname, _ := os.Hostname()
	cols, rows := terminalSize()

	start := SessionStart{
		Type:         "start",
		Principal:    currentUsername(),
		ClientAddr:   clientAddr,
		Origin:       origin,
		Command:      origCmd,
		TTY:          term.IsTerminal(),
		Cols:         cols,
		Rows:         rows,
		Hostname:     hostname,
		StartedAt:    time.Now().UTC(),
		PID:          os.Getpid(),
		OriginReason: originReason,
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(start); err != nil {
		if !cfg.FailOpen {
			fmt.Fprintln(os.Stderr, "argus: cannot start recording, refusing connection.")
			return 1
		}
		return execSession(cfg, origCmd, nil)
	}

	stream := newStream(conn, enc)
	code := execSession(cfg, origCmd, stream)

	stream.close(code)
	// Wait briefly for the daemon's acknowledgement so the chain head reaches
	// the log before the process exits. Not fatal if it does not arrive.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack Ack
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&ack); err == nil && ack.ChainHead != "" {
		fmt.Fprintf(os.Stderr, "\r\nargus: session %s recorded (chain %s…)\r\n",
			ack.SessionID, ack.ChainHead[:12])
	}
	return code
}

// stream serialises frames onto the collector connection.
//
// A session writes stdout and stdin from separate goroutines, so encoding must
// be serialised or the JSON lines interleave and corrupt the recording.
type stream struct {
	mu      sync.Mutex
	enc     *json.Encoder
	conn    net.Conn
	started time.Time
	dead    bool
}

func newStream(conn net.Conn, enc *json.Encoder) *stream {
	return &stream{enc: enc, conn: conn, started: time.Now()}
}

func (s *stream) frame(kind string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	if err := s.enc.Encode(Frame{
		Type:   kind,
		Offset: time.Since(s.started).Seconds(),
		Data:   string(data),
	}); err != nil {
		// The collector went away mid-session. Stop trying rather than
		// blocking the user's terminal on a dead socket; the daemon will seal
		// what it has.
		s.dead = true
	}
}

func (s *stream) resize(cols, rows int) {
	s.frame("r", []byte(fmt.Sprintf("%dx%d", cols, rows)))
}

func (s *stream) close(exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	_ = s.enc.Encode(SessionEnd{Type: "end", ExitCode: exitCode})
}

// execSession runs the user's shell or command, teeing through st when non-nil.
func execSession(cfg ShimConfig, origCmd string, st *stream) int {
	shell := cfg.Shell
	if shell == "" {
		shell = loginShell()
	}

	var cmd *exec.Cmd
	switch {
	case isInternalSFTP(origCmd):
		// sshd applies ForceCommand to subsystem requests too, and for
		// `Subsystem sftp internal-sftp` it hands us SSH_ORIGINAL_COMMAND
		// "internal-sftp" -- which is compiled into sshd and is not a program.
		// Running it through the shell fails, the connection closes with no
		// message, and every SFTP and SCP transfer to an agent-managed host
		// stops working the moment the agent is installed.
		//
		// That is the traffic this product most wants to see: "who moved which
		// file" is the question an exfiltration investigation opens with.
		server, err := findSFTPServer()
		if err != nil {
			fmt.Fprintf(os.Stderr, "argus: %v\n", err)
			return 1
		}
		cmd = exec.Command(server)
	case origCmd != "":
		// Non-interactive: `ssh host cmd`, scp, sftp, rsync, Ansible. This is
		// the path automation uses, so leaving it unrecorded would make the
		// most-used route the least visible one.
		cmd = exec.Command(shell, "-c", origCmd)
	default:
		cmd = exec.Command(shell, "-l")
	}
	cmd.Env = os.Environ()

	if !term.IsTerminal() {
		return execWithoutPTY(cmd, st, origCmd)
	}
	return execWithPTY(cmd, st)
}

// execWithPTY runs the command on a pseudo-terminal, which is what makes an
// interactive shell behave normally (job control, line editing, colours).
func execWithPTY(cmd *exec.Cmd, st *stream) int {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus: cannot allocate pty: %v\r\n", err)
		return 1
	}
	defer func() { _ = ptmx.Close() }()

	// Mirror the client's window size onto the pty, then keep them in step.
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	go func() {
		for range resize {
			if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
				continue
			}
			if st != nil {
				c, r := terminalSize()
				st.resize(c, r)
			}
		}
	}()
	resize <- syscall.SIGWINCH
	defer func() { signal.Stop(resize); close(resize) }()

	// Raw mode so keystrokes reach the remote shell untouched rather than being
	// line-buffered by the local terminal.
	restore, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer restore()
	}

	var wg sync.WaitGroup
	wg.Add(1)

	// stdin: user -> pty. Recorded as "i", which is what lets the console
	// reconstruct commands exactly instead of scraping echoed output.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if st != nil {
					st.frame("i", buf[:n])
				}
				if _, werr := ptmx.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// stdout: pty -> user. This is the replay stream.
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if st != nil {
					st.frame("o", buf[:n])
				}
				if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()
	return waitCode(cmd)
}

// isInternalSFTP reports whether sshd is asking for its built-in SFTP server.
//
// Matched on the exact strings sshd uses. A prefix match would catch a user
// command that merely mentions sftp and quietly run a file server instead.
func isInternalSFTP(origCmd string) bool {
	switch strings.TrimSpace(origCmd) {
	case "internal-sftp", "sftp-server":
		return true
	}
	return false
}

// sftpServerPaths are where the OpenSSH SFTP server lives, by distribution.
var sftpServerPaths = []string{
	"/usr/lib/openssh/sftp-server",     // Debian, Ubuntu
	"/usr/libexec/openssh/sftp-server", // RHEL, Rocky, Alma, Fedora
	"/usr/libexec/sftp-server",         // Alpine, some BSD-ish layouts
	"/usr/lib/ssh/sftp-server",         // Arch
	"/usr/local/libexec/sftp-server",   // built from source
}

// findSFTPServer locates a real SFTP server to run in place of internal-sftp.
//
// A clear refusal when there is none. The host has sshd's built-in server and
// no standalone one, which is a supported sshd configuration and an impossible
// one for a ForceCommand -- and an operator whose transfers stopped needs to be
// told to install a package, not left with a closed connection.
func findSFTPServer() (string, error) {
	for _, p := range sftpServerPaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	if p, err := exec.LookPath("sftp-server"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("this host has no sftp-server binary, only sshd's " +
		"built-in one, which cannot be run from a ForceCommand; install it " +
		"(Debian/Ubuntu: openssh-sftp-server, RHEL: openssh-server) so file " +
		"transfers work and can be recorded")
}

// execWithoutPTY handles `ssh host cmd` and subsystems, which have no terminal.
func execWithoutPTY(cmd *exec.Cmd, st *stream, origCmd string) int {
	if st != nil && origCmd != "" {
		// Record the command itself; without a PTY there is no echo to capture.
		st.frame("i", []byte(origCmd+"\r\n"))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "argus: %v\n", err)
		return 1
	}

	// Only stdout and stderr gate completion.
	//
	// The stdin copier must NOT be waited on: a client that holds its stdin
	// open — which is exactly what happens when the gateway proxies an exec
	// request — would block this forever and hang the session. Feeding stdin is
	// best-effort and ends when the command does.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); tee(stdout, os.Stdout, st, "o") }()
	go func() { defer wg.Done(); tee(stderr, os.Stderr, st, "o") }()

	go func() {
		defer stdin.Close()
		tee(os.Stdin, stdin, st, "i")
	}()

	wg.Wait()
	return waitCode(cmd)
}

func tee(src io.Reader, dst io.Writer, st *stream, kind string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if st != nil {
				st.frame(kind, buf[:n])
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// waitCode returns the command's exit status, preserving it for the client so
// scripts calling through Argus see what actually happened.
func waitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 255
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return "uid:" + strconv.Itoa(os.Getuid())
}

func loginShell() string {
	if u, err := user.Current(); err == nil {
		if sh := shellFromPasswd(u.Username); sh != "" {
			return sh
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// shellFromPasswd reads the login shell from /etc/passwd. user.Current does not
// expose it, and $SHELL is not set by sshd for non-interactive invocations.
func shellFromPasswd(username string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[0] == username {
			return fields[6]
		}
	}
	return ""
}

func terminalSize() (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}
