//go:build !windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// rabbitmqAccount is the account the packaged broker runs as, and the one
// whose home holds the Erlang cookie rabbitmqctl signs in with.
const rabbitmqAccount = "rabbitmq"

// ctlHost is what decides how rabbitmqctl is started: who the agent is,
// whether systemd can start a unit for it, and the broker's account (nil
// when this server has none).
type ctlHost struct {
	euid    int
	systemd bool
	acct    *loginAccount
}

// rabbitmqctlCmd is rabbitCtlCommand for this server.
func rabbitmqctlCmd(ctx context.Context, bin string, args []string) *exec.Cmd {
	h := ctlHost{euid: os.Geteuid(), systemd: systemdRunning()}
	if a, err := lookupAccount(rabbitmqAccount); err == nil {
		h.acct = &a
	}
	return rabbitCtlCommand(ctx, h, bin, args)
}

// rabbitCtlCommand runs rabbitmqctl as the broker's own account.
//
// The packaged rabbitmqctl is a wrapper that, started by root, runs `su
// rabbitmq`. The agent's unit keeps neither CAP_SETUID nor CAP_SETGID, so su
// fails ("su: cannot set groups: Operation not permitted"). Started as the
// rabbitmq account the wrapper runs the real CLI itself. Under systemd that
// means a transient unit, outside the agent's sandbox, the way the pg-admin
// helper runs as postgres; the password still travels on stdin (--pipe).
func rabbitCtlCommand(ctx context.Context, h ctlHost, bin string, args []string) *exec.Cmd {
	if h.acct == nil || h.acct.uid == h.euid || h.euid != 0 {
		return exec.CommandContext(ctx, bin, args...)
	}
	if h.systemd {
		sargs := []string{"--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--uid=" + h.acct.name, "--", bin}
		return exec.CommandContext(ctx, "systemd-run", append(sargs, args...)...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = "/"
	cmd.Env = []string{
		"HOME=" + h.acct.home, "USER=" + h.acct.name, "LOGNAME=" + h.acct.name,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(h.acct.uid), Gid: uint32(h.acct.gid), Groups: h.acct.groups}}
	return cmd
}
