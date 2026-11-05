//go:build !windows

package agent

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

/*
rabbitmqctl as the broker's own account.

The packaged rabbitmqctl (/usr/sbin/rabbitmqctl, Debian's and the RPM's
rabbitmq-script-wrapper) runs `su rabbitmq` when root starts it. The agent's
unit keeps no CAP_SETGID or CAP_SETUID, so su died with "su: cannot set
groups: Operation not permitted" and no RabbitMQ session could open. Started
as the rabbitmq account, the wrapper runs the real CLI itself and never
calls su.
*/

var brokerAcct = loginAccount{name: "rabbitmq", home: "/var/lib/rabbitmq", uid: 112, gid: 120, groups: []uint32{120}}

func TestRabbitmqctlAsRootUnderSystemdRunsOutsideTheSandboxAsTheBroker(t *testing.T) {
	cmd := rabbitCtlCommand(context.Background(), ctlHost{euid: 0, systemd: true, acct: &brokerAcct},
		"/usr/sbin/rabbitmqctl", []string{"add_user", "argus-ana-read"})
	if filepath.Base(cmd.Path) != "systemd-run" {
		t.Fatalf("runs %s, want systemd-run: the agent's own sandbox cannot switch accounts", cmd.Path)
	}
	for _, want := range []string{"--pipe", "--wait", "--uid=rabbitmq"} {
		if !slices.Contains(cmd.Args, want) {
			t.Fatalf("%v lacks %s", cmd.Args, want)
		}
	}
	tail := cmd.Args[len(cmd.Args)-3:]
	if !slices.Equal(tail, []string{"/usr/sbin/rabbitmqctl", "add_user", "argus-ana-read"}) {
		t.Fatalf("command is %v", tail)
	}
}

func TestRabbitmqctlAsRootWithoutSystemdSwitchesToTheBroker(t *testing.T) {
	cmd := rabbitCtlCommand(context.Background(), ctlHost{euid: 0, acct: &brokerAcct},
		"/usr/sbin/rabbitmqctl", []string{"list_users"})
	if cmd.Path != "/usr/sbin/rabbitmqctl" {
		t.Fatalf("runs %s", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != 112 {
		t.Fatalf("not started as the rabbitmq account: %+v", cmd.SysProcAttr)
	}
	// The CLI finds the Erlang cookie in the broker's home.
	if !slices.Contains(cmd.Env, "HOME=/var/lib/rabbitmq") {
		t.Fatalf("env %v", cmd.Env)
	}
}

func TestRabbitmqctlRunsDirectlyWhenThereIsNoAccountToSwitchTo(t *testing.T) {
	for name, h := range map[string]ctlHost{
		"no rabbitmq account":     {euid: 0, systemd: true},
		"already the broker":      {euid: 112, systemd: true, acct: &brokerAcct},
		"not root, cannot switch": {euid: 1000, systemd: true, acct: &brokerAcct},
	} {
		cmd := rabbitCtlCommand(context.Background(), h, "/usr/sbin/rabbitmqctl", []string{"list_users"})
		if cmd.Path != "/usr/sbin/rabbitmqctl" || cmd.SysProcAttr != nil {
			t.Fatalf("%s: runs %s with %+v", name, cmd.Path, cmd.SysProcAttr)
		}
	}
}
