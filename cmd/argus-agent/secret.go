package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/agent/secretclient"
)

/*
argus-agent secret: read from the organization's argus-secret servers.

	argus-agent secret get  REF                         print a value
	argus-agent secret run  --env VAR=REF ... -- CMD    run CMD with values in its environment
	argus-agent secret file REF --out PATH [--mode 0400] [--owner user[:group]]

REF is folder/name, optionally #field, optionally @version or @label:
prod/payments/db#password, prod/tls@previous.

As root the agent reads with this host's own credential. Anyone else reads
through the running agent's socket, if the socket lets them in (members of
the argus-secrets group); either way the read is in the server's log under
this host, and through the socket with the caller's uid and pid.
*/

func runSecret(args []string) int {
	if len(args) < 1 {
		secretUsage()
		return 2
	}
	switch args[0] {
	case "get":
		return runSecretGet(args[1:])
	case "run":
		return runSecretRun(args[1:])
	case "file":
		return runSecretFile(args[1:])
	default:
		secretUsage()
		return 2
	}
}

func secretUsage() {
	fmt.Fprint(os.Stderr, `argus-agent secret: read from argus-secret

  argus-agent secret get  REF                          print a value
  argus-agent secret run  --env VAR=REF ... -- CMD     run CMD with values in its environment
  argus-agent secret file REF --out PATH [--mode 0400] [--owner user[:group]]

REF is folder/name[#field][@version|@label], such as prod/payments/db#password.
`)
}

// secretReader is the host's credential as root, else the agent's socket.
func secretReader(stateDir string) (secretclient.Reader, error) {
	enr, err := agent.LoadEnrollment(stateDir)
	if err == nil && enr != nil {
		tlsCfg, err := enr.TLS()
		if err != nil {
			return nil, err
		}
		return &secretclient.Client{ControlURL: enr.ControlURL, ControlTLS: tlsCfg, Credential: enr.Credential, StateDir: stateDir}, nil
	}
	if err == nil && enr == nil {
		return nil, errors.New("this host is not enrolled: run argus-agent enroll first")
	}
	if !errors.Is(err, fs.ErrPermission) || agent.SecretsSocketPath == "" {
		return nil, err
	}
	return secretclient.SocketClient{Path: agent.SecretsSocketPath}, nil
}

func secretContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	ctx, cancel2 := context.WithTimeout(ctx, time.Minute)
	return ctx, func() { cancel2(); cancel() }
}

func runSecretGet(args []string) int {
	fset := flag.NewFlagSet("secret get", flag.ContinueOnError)
	stateDir := fset.String("state", agent.DefaultStateDir, "the agent's state directory")
	newline := fset.Bool("n", false, "end the value with a newline")
	if err := fset.Parse(args); err != nil || fset.NArg() != 1 {
		secretUsage()
		return 2
	}
	ref, err := secretclient.ParseRef(fset.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 2
	}
	r, err := secretReader(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	ctx, cancel := secretContext()
	defer cancel()
	v, err := r.Read(ctx, ref, "argus-agent secret get"+byWhom())
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	_, _ = os.Stdout.Write(v.Bytes)
	if *newline {
		_, _ = os.Stdout.Write([]byte("\n"))
	}
	clear(v.Bytes)
	return 0
}

// envFlags collects --env VAR=REF.
type envFlags []string

func (e *envFlags) String() string     { return strings.Join(*e, ",") }
func (e *envFlags) Set(s string) error { *e = append(*e, s); return nil }

func runSecretRun(args []string) int {
	fset := flag.NewFlagSet("secret run", flag.ContinueOnError)
	stateDir := fset.String("state", agent.DefaultStateDir, "the agent's state directory")
	var envs envFlags
	fset.Var(&envs, "env", "VAR=REF: put REF's value in VAR (repeatable)")
	if err := fset.Parse(args); err != nil || fset.NArg() == 0 || len(envs) == 0 {
		secretUsage()
		return 2
	}
	type want struct {
		name string
		ref  secretclient.Ref
	}
	var wants []want
	for _, e := range envs {
		k, v, ok := strings.Cut(e, "=")
		if !ok || !validEnvName(k) {
			fmt.Fprintf(os.Stderr, "argus-agent secret: --env %q is VAR=folder/name[#field]\n", e)
			return 2
		}
		ref, err := secretclient.ParseRef(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
			return 2
		}
		wants = append(wants, want{k, ref})
	}
	r, err := secretReader(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	ctx, cancel := secretContext()
	env := os.Environ()
	for _, w := range wants {
		v, err := r.Read(ctx, w.ref, "argus-agent secret run "+fset.Arg(0)+byWhom())
		if err != nil {
			cancel()
			fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
			return 1
		}
		if strings.ContainsRune(string(v.Bytes), 0) {
			cancel()
			fmt.Fprintf(os.Stderr, "argus-agent secret: %s holds a NUL byte and cannot go in an environment variable; use secret file\n", w.ref.Name)
			return 1
		}
		env = append(env, w.name+"="+string(v.Bytes))
	}
	cancel()
	cmd := exec.Command(fset.Arg(0), fset.Args()[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	// Signals go to the child; this process only waits for it.
	signal.Ignore(os.Interrupt)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 127
	}
	return 0
}

func validEnvName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func runSecretFile(args []string) int {
	fset := flag.NewFlagSet("secret file", flag.ContinueOnError)
	stateDir := fset.String("state", agent.DefaultStateDir, "the agent's state directory")
	out := fset.String("out", "", "where to write the value")
	modeStr := fset.String("mode", "0400", "the file's mode")
	owner := fset.String("owner", "", "user[:group] to give the file to (root only)")
	// Flags may come after REF.
	var refArg string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		refArg, rest = rest[0], rest[1:]
	}
	if err := fset.Parse(rest); err != nil {
		secretUsage()
		return 2
	}
	if refArg == "" && fset.NArg() == 1 {
		refArg = fset.Arg(0)
	}
	if refArg == "" || *out == "" {
		secretUsage()
		return 2
	}
	mode, err := strconv.ParseUint(*modeStr, 8, 32)
	if err != nil || mode > 0o777 || mode&0o007 != 0 {
		fmt.Fprintln(os.Stderr, "argus-agent secret: --mode is octal, such as 0400 or 0440, and never readable by everyone")
		return 2
	}
	ref, err := secretclient.ParseRef(refArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 2
	}
	uid, gid := -1, -1
	if *owner != "" {
		if uid, gid, err = lookupOwner(*owner); err != nil {
			fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
			return 2
		}
	}
	r, err := secretReader(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	ctx, cancel := secretContext()
	defer cancel()
	v, err := r.Read(ctx, ref, "argus-agent secret file "+*out+byWhom())
	if err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	defer clear(v.Bytes)
	if err := writeSecretFile(*out, v.Bytes, os.FileMode(mode), uid, gid); err != nil {
		fmt.Fprintln(os.Stderr, "argus-agent secret:", err)
		return 1
	}
	return 0
}

// writeSecretFile writes atomically: a temporary file made with the final
// mode beside the target, then renamed over it, so a reader never sees half
// a value or a moment of wider permissions.
func writeSecretFile(path string, value []byte, mode os.FileMode, uid, gid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".argus-secret-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("give %s to its owner: %w", path, err)
		}
	}
	if _, err := tmp.Write(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func lookupOwner(s string) (int, int, error) {
	un, gn, _ := strings.Cut(s, ":")
	u, err := user.Lookup(un)
	if err != nil {
		return -1, -1, fmt.Errorf("no user %q", un)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if gn != "" {
		g, err := user.LookupGroup(gn)
		if err != nil {
			return -1, -1, fmt.Errorf("no group %q", gn)
		}
		gid, _ = strconv.Atoi(g.Gid)
	}
	return uid, gid, nil
}

// byWhom names who ran the command, for the server's read log.
func byWhom() string {
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	if s := os.Getenv("SUDO_USER"); s != "" {
		who += " via sudo by " + s
	}
	if who == "" {
		return ""
	}
	return " by " + who
}

// serveSecretsSocket runs the secrets socket for an enrolled agent until
// ctx ends. Nothing listens where the platform has no such socket.
func serveSecretsSocket(ctx context.Context, enr *agent.Enrollment, stateDir string, log *slog.Logger) {
	if agent.SecretsSocketPath == "" || enr == nil {
		return
	}
	tlsCfg, err := enr.TLS()
	if err != nil {
		log.Error("secrets socket: enrollment TLS", "error", err)
		return
	}
	gid := -1
	if g, err := user.LookupGroup(secretclient.SocketGroup); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	}
	ln, err := secretclient.Listen(agent.SecretsSocketPath, gid)
	if err != nil {
		log.Error("secrets socket: cannot listen", "path", agent.SecretsSocketPath, "error", err)
		return
	}
	s := &secretclient.Socket{
		Client: &secretclient.Client{ControlURL: enr.ControlURL, ControlTLS: tlsCfg, Credential: enr.Credential, StateDir: stateDir},
		Log:    log,
		PeerOf: func(c net.Conn) (secretclient.Peer, error) {
			p, err := agent.SocketPeer(c)
			return secretclient.Peer{UID: p.UID, PID: p.PID, Known: p.Known}, err
		},
	}
	who := "root only"
	if gid >= 0 {
		who = "root and the " + secretclient.SocketGroup + " group"
	}
	log.Info("secrets socket listening", "path", agent.SecretsSocketPath, "for", who)
	if err := s.Serve(ctx, ln); err != nil {
		log.Error("secrets socket", "error", err)
	}
}
