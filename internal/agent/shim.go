//go:build !windows

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

	hostname, _ := os.Hostname()
	cols, rows := terminalSize()

	start := SessionStart{
		Type:         "start",
		Principal:    currentUsername(),
		ClientAddr:   clientAddr,
		Origin:       Direct,
		Command:      origCmd,
		TTY:          term.IsTerminal(),
		Cols:         cols,
		Rows:         rows,
		Hostname:     hostname,
		StartedAt:    time.Now().UTC(),
		PID:          os.Getpid(),
		OriginReason: "a login through sshd; Argus opens its own sessions over the tunnel",
		UserAuth:     readUserAuth(),
	}

	rc, why := openRecording(cfg.SocketPath, start, restartWait, true)
	if rc == nil {
		return recorderDown(cfg, origCmd, why)
	}
	defer func() { _ = rc.conn.Close() }()
	d := rc.d
	if d.pending {
		// Held for an admin. Said on the person's terminal so the wait is not
		// mistaken for a hang, and a second answer is required: silence now
		// is a refusal, not an old daemon.
		fmt.Fprintf(os.Stderr, "argus: %s.\n", d.reason)
		if d = awaitDecision(rc.conn, rc.dec, approvalWait); d.pending || d.silent {
			fmt.Fprintln(os.Stderr, "argus: nobody approved this login in time, so it was refused.")
			return 1
		}
	}
	switch {
	case d.consoleOnly:
		// Not a recording failure, so FailOpen does not apply: the login is
		// not allowed, recorded or not.
		fmt.Fprintf(os.Stderr, "argus: %s.\n", d.reason)
		return 1
	case d.refused:
		return recorderDown(cfg, origCmd, "the host agent could not open a recording")
	case d.skip:
		// The host's recording scope, set in the console, not something the
		// shim decided. Said to the user, because a login that is reported but
		// not recorded should not look the same as one that is recorded.
		fmt.Fprintf(os.Stderr, "argus: %s.\n", d.reason)
		_ = rc.conn.Close()
		return execSession(cfg, origCmd, nil)
	}

	stream := newStream(rc, d.sessionID)
	stream.socket, stream.start = cfg.SocketPath, start
	code := execSession(cfg, origCmd, stream)

	conn, dec := stream.close(code)
	if conn == nil {
		return code
	}
	// Wait briefly for the daemon's acknowledgement so the chain head reaches
	// the log before the process exits. Not fatal if it does not arrive.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack Reply
	if err := dec.Decode(&ack); err == nil && len(ack.ChainHead) >= 12 {
		fmt.Fprintf(os.Stderr, "\r\nargus: session %s recorded (chain %s…)\r\n",
			ack.SessionID, ack.ChainHead[:12])
	}
	return code
}

// restartWait is how long a login waits for a collector that is not
// answering, when nothing says why: long enough for `systemctl restart` (a
// package upgraded by hand restarts the agent), short enough that a stopped
// agent sends an administrator to break-glass without a long pause.
const restartWait = 5 * time.Second

// updateWait is how long a login waits when the agent has said it is
// restarting for an update. The agent drains before it restarts, so this
// covers sessions it ends as well as its own start.
const updateWait = 75 * time.Second

// recording is an open connection to the collector, with its decision.
type recording struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	d    decision
}

// openRecording connects to the collector and sends start, waiting for an
// agent that is restarting rather than reporting it down. It returns nil and
// why when it gave up.
//
// Before this, a login in the second or two of an agent restart found no
// socket and was refused -- every agent upgrade refused whoever logged in at
// that moment, and break-glass only covered administrators.
func openRecording(socket string, start SessionStart, wait time.Duration, say bool) (*recording, string) {
	deadline := time.Now().Add(wait)
	extended, announced := false, false
	why := "the host agent is not running"
	for {
		restarting := false
		if conn, err := net.DialTimeout("unix", socket, 5*time.Second); err == nil {
			enc := json.NewEncoder(conn)
			if err := enc.Encode(start); err == nil {
				// One decoder for the life of the connection: the decision and
				// the final acknowledgement arrive on the same stream, and a
				// second buffered reader would lose whatever the first had
				// already read past.
				dec := json.NewDecoder(bufio.NewReader(conn))
				d := awaitDecision(conn, dec, decisionWait)
				if !d.restarting {
					return &recording{conn: conn, enc: enc, dec: dec, d: d}, ""
				}
				restarting = true
			} else {
				why = "the host agent closed the connection"
			}
			_ = conn.Close()
		}
		if (restarting || updateMarkerSet(socket)) && !extended {
			extended = true
			if until := time.Now().Add(updateWait); until.After(deadline) {
				deadline = until
			}
			why = "the host agent did not come back from its update"
		}
		if time.Now().After(deadline) {
			return nil, why
		}
		if say && extended && !announced {
			announced = true
			fmt.Fprintln(os.Stderr, "argus: this server's Argus agent is restarting for an update; "+
				"your login continues in a moment.")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// recorderDown is what a login gets when the recorder cannot take it: the
// agent is stopped, crashed, or mid-restart.
//
// Refusing everyone is right for an ordinary user, and wrong for the
// administrator who has to log in to start the agent again. Before break-glass
// existed, a stopped agent refused every login on the host, root included,
// and the only way back in was the hosting provider's console. So whoever is
// on the break-glass list (root and the admin groups, by default) is let in
// unrecorded, told so on their terminal, and logged to the system's auth log.
//
// This is only for a recorder that is down. A login the agent answered and
// turned away (console-only, or an approval that never came) is refused
// here exactly as before: the agent is up, so there is nothing to break out of.
func recorderDown(cfg ShimConfig, origCmd, why string) int {
	switch {
	case cfg.FailOpen:
		fmt.Fprintln(os.Stderr,
			"argus: WARNING — recording unavailable, this session is NOT being recorded.")
		return execSession(cfg, origCmd, nil)
	case breakGlassAllowed(cfg.BreakGlassGroups, currentUserInfo):
		fmt.Fprintf(os.Stderr, "argus: WARNING — %s, so this session is NOT being recorded.\n", why)
		fmt.Fprintln(os.Stderr,
			"argus: you are let in as break-glass. Start the agent again: sudo systemctl start argus-agent")
		logBreakGlass(why)
		return execSession(cfg, origCmd, nil)
	}
	fmt.Fprintf(os.Stderr, "argus: session recording is unavailable (%s), refusing connection.\n", why)
	fmt.Fprintln(os.Stderr,
		"argus: contact your administrator to start the host agent again.")
	return 1
}

// userInfo is who is logging in, as break-glass needs it.
type userInfo struct {
	uid    string
	groups []string // group names
}

// currentUserInfo looks up the connecting user. A lookup that fails gives no
// groups, so the answer falls back to refusing.
func currentUserInfo() userInfo {
	u, err := user.Current()
	if err != nil {
		return userInfo{uid: strconv.Itoa(os.Getuid())}
	}
	info := userInfo{uid: u.Uid}
	ids, _ := u.GroupIds()
	for _, id := range ids {
		if g, err := user.LookupGroupId(id); err == nil {
			info.groups = append(info.groups, g.Name)
		}
	}
	return info
}

// breakGlassAllowed reports whether the connecting user may log in while the
// recorder is down. Root always may, unless the list is "none".
func breakGlassAllowed(groups []string, who func() userInfo) bool {
	if len(groups) == 1 && groups[0] == "none" {
		return false
	}
	info := who()
	if info.uid == "0" {
		return true
	}
	for _, have := range info.groups {
		for _, want := range groups {
			if have == want {
				return true
			}
		}
	}
	return false
}

// logBreakGlass leaves a line in the auth log, so a session the recorder never
// saw still leaves a trace someone can find. Best effort: logger may be absent.
func logBreakGlass(why string) {
	clientAddr, _, _ := strings.Cut(os.Getenv("SSH_CONNECTION"), " ")
	msg := fmt.Sprintf("break-glass login by %s from %s, NOT recorded: %s",
		currentUsername(), clientAddr, why)
	_ = exec.Command("logger", "-p", "authpriv.warning", "-t", "argus-agent", msg).Run()
}

// stream serialises frames onto the collector connection.
//
// A session writes stdout and stdin from separate goroutines, so encoding must
// be serialised or the JSON lines interleave and corrupt the recording.
//
// The shim belongs to sshd, not to the agent, so an agent restart does not
// end the login -- it ends the recording. A stream whose collector went away
// keeps what the session produces and reconnects to the agent that replaces
// it, which records the rest as a new session naming this one. Before this,
// the rest of the session went unrecorded, without a word to anyone.
type stream struct {
	mu      sync.Mutex
	enc     *json.Encoder
	conn    net.Conn
	dec     *json.Decoder
	started time.Time
	dead    bool

	// For resuming: the socket, the session's start, and the recording it
	// is in now.
	socket    string
	start     SessionStart
	sessionID string
	resuming  bool
	// pending holds what the session produced while no collector was
	// listening, up to pendingMax.
	pending     []Frame
	pendingSize int
	lost        bool
	closed      bool
	exitCode    int
}

// pendingMax bounds what is kept while the agent restarts.
const pendingMax = 4 << 20

// resumeWait is how long a stream looks for the agent that replaces the one
// that went away.
const resumeWait = 2 * time.Minute

func newStream(rc *recording, sessionID string) *stream {
	return &stream{enc: rc.enc, conn: rc.conn, dec: rc.dec, started: time.Now(), sessionID: sessionID}
}

func (s *stream) frame(kind string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := Frame{Type: kind, Offset: time.Since(s.started).Seconds(), Data: string(data)}
	if !s.dead {
		if err := s.enc.Encode(f); err == nil {
			return
		}
		// The collector went away mid-session. Stop writing to it rather
		// than blocking the user's terminal on a dead socket; the daemon
		// seals what it has, and the rest is kept for the next one.
		s.dead = true
		if s.socket != "" && !s.resuming {
			s.resuming = true
			go s.resume()
		}
	}
	if !s.resuming {
		return
	}
	if s.pendingSize+len(data) > pendingMax {
		s.lost = true
		return
	}
	s.pending = append(s.pending, f)
	s.pendingSize += len(data)
}

// resume finds the collector again and continues the recording there.
func (s *stream) resume() {
	start := s.start
	s.mu.Lock()
	prev := s.sessionID
	s.mu.Unlock()
	start.StartedAt = time.Now().UTC()
	start.OriginReason = "continues session " + prev + " after the host agent restarted; " +
		"the session before that is in that recording"

	rc, why := openRecording(s.socket, start, resumeWait, false)
	if rc != nil && rc.d.pending {
		rc.d = awaitDecision(rc.conn, rc.dec, approvalWait)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rc == nil || !rc.d.record() {
		if rc != nil {
			_ = rc.conn.Close()
			why = "the agent that came back did not take it"
		}
		s.resuming, s.pending = false, nil
		fmt.Fprintf(os.Stderr, "\r\nargus: WARNING — the rest of this session is NOT being "+
			"recorded (%s).\r\n", why)
		logBreakGlass("recording of session " + prev + " stopped part way: " + why)
		return
	}
	s.conn, s.enc, s.dec, s.sessionID = rc.conn, rc.enc, rc.dec, rc.d.sessionID
	s.started = time.Now()
	for _, f := range s.pending {
		f.Offset = 0
		if err := s.enc.Encode(f); err != nil {
			break
		}
	}
	if s.lost {
		_ = s.enc.Encode(Frame{Type: "o", Data: "\r\n[argus: output past 4 MB while the agent " +
			"restarted was not kept]\r\n"})
	}
	s.pending, s.pendingSize, s.dead, s.resuming = nil, 0, false, false
	if s.closed {
		_ = s.enc.Encode(SessionEnd{Type: "end", ExitCode: s.exitCode})
	}
}

func (s *stream) resize(cols, rows int) {
	s.frame("r", []byte(fmt.Sprintf("%dx%d", cols, rows)))
}

// close ends the recording and returns the connection the acknowledgement
// arrives on, nil when there is none.
func (s *stream) close(exitCode int) (net.Conn, *json.Decoder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed, s.exitCode = true, exitCode
	if s.dead {
		return nil, nil
	}
	_ = s.enc.Encode(SessionEnd{Type: "end", ExitCode: exitCode})
	return s.conn, s.dec
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

// shellFromPasswd reads the login shell from the account database (see
// openPasswd). user.Current does not expose it, and $SHELL is not set by sshd
// for non-interactive invocations.
func shellFromPasswd(username string) string {
	f, err := openPasswd(systemPasswd)
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

// readUserAuth is the file sshd names in SSH_USER_AUTH when ExposeAuthInfo
// is on: each method the login signed in with, and the key for a key login.
// Empty when sshd did not set it.
func readUserAuth() string {
	path := os.Getenv("SSH_USER_AUTH")
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxUserAuth))
	return string(b)
}
