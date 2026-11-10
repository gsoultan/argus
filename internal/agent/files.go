//go:build !windows

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
File transfer through the tunnel: the console's scp.

	browser ──wss──▶ argus-control ──"files" stream──▶ agent ──▶ file-op, as the account

Opened on a grant like a terminal, but one for "files", so neither can be
spent on the other. Each operation runs `argus-agent file-op` as the account
-- in a transient unit on a systemd host, because the agent's own sandbox has
a read-only /home -- so the account's permissions decide what it may read and
write, as they would for scp. The agent counts and hashes what passes, and the
control plane writes each transfer into the audit chain.
*/

// Files is the files service.
func Files(cfg FilesConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused file transfer", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(FilesService) {
			log.Warn("tunnel: refused file transfer on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for file transfer"}.Error())
			return
		}
		acct, err := lookupAccount(g.Principal)
		if err != nil {
			call.Refuse(err.Error())
			return
		}
		if acct.uid != os.Geteuid() && os.Geteuid() != 0 {
			call.Refuse("the agent is not running as root, so it cannot transfer files as " + acct.name)
			return
		}
		if call.Accept("") != nil {
			_ = call.Stream.Close()
			return
		}
		log.Info("tunnel: file transfer opened", "principal", acct.name, "for", g.Email,
			"client", g.ClientAddr)
		f := &fileSession{who: acct.name, home: acct.home, actor: g.Email, log: log}
		f.run = func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
			return runAs(ctx, helperRun{acct: acct, helper: cfg.Helper, spawn: cfg.Spawn,
				env: cfg.HelperEnv, args: append([]string{"file-op"}, args...), stdin: stdin,
				stdout: stdout, actor: g.Email, home: true})
		}
		f.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

// helperRun is one run of this executable as another account.
type helperRun struct {
	acct   loginAccount
	helper string
	spawn  SpawnMode
	env    []string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	actor  string
	// home runs it from the account's home directory.
	home bool
	// stderr, when set, sees the helper's stderr as it is written, for a
	// helper that reports progress there; the last line still names the
	// error when it fails.
	stderr io.Writer
}

// runAs starts this executable (or h.helper) as h.acct and waits for it: in a
// transient unit on a systemd host, because the agent's own sandbox would be
// inherited otherwise, and as a direct child with the account's credentials
// elsewhere.
func runAs(ctx context.Context, h helperRun) error {
	helper := h.helper
	if helper == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		helper = exe
	}
	mode := h.spawn
	if mode == SpawnAuto {
		mode = SpawnDirect
		if os.Geteuid() == 0 && systemdRunning() {
			mode = SpawnSystemd
		}
	}
	var cmd *exec.Cmd
	if mode == SpawnSystemd {
		// --pipe hands the unit our stdin and stdout; the unit runs outside
		// the agent's sandbox, as the account.
		sargs := []string{"--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--uid=" + h.acct.name,
			"--setenv=ARGUS_SESSION_FOR=" + h.actor}
		if h.home {
			sargs = append(sargs, "-p", "WorkingDirectory=~")
		}
		for _, e := range h.env {
			sargs = append(sargs, "--setenv="+e)
		}
		cmd = exec.CommandContext(ctx, "systemd-run", append(append(sargs, helper), h.args...)...)
	} else {
		cmd = exec.CommandContext(ctx, helper, h.args...)
		cmd.Dir = "/"
		if h.home {
			cmd.Dir = h.acct.home
		}
		cmd.Env = append([]string{
			"HOME=" + h.acct.home, "USER=" + h.acct.name, "LOGNAME=" + h.acct.name,
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"ARGUS_SESSION_FOR=" + h.actor,
		}, h.env...)
		if h.acct.uid != os.Geteuid() {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
				Uid: uint32(h.acct.uid), Gid: uint32(h.acct.gid), Groups: h.acct.groups}}
		}
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = h.stdin, h.stdout, &stderr
	if h.stderr != nil {
		cmd.Stderr = io.MultiWriter(h.stderr, &stderr)
	}
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(lastLine(msg))
		}
		return fmt.Errorf("the helper failed: %w", err)
	}
	return nil
}
