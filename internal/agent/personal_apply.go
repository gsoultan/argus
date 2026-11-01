package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
)

/*
Personal accounts are made outside the agent's sandbox.

The agent's unit is ProtectSystem=strict with ProtectHome=read-only, three
capabilities and NoNewPrivileges, so from inside it useradd cannot write
/etc/passwd, the sudoers drop-in cannot be written or removed, and no home or
authorized_keys can be made. Every pass failed on a packaged install. A shell
escapes the same sandbox through systemd-run (see agent-tunnel), and so does
this: on a systemd host the daemon runs `argus-agent personal-apply` as a
transient unit, hands it the list and what it last did on stdin, and reads
the states back on stdout. The pass is the same code either way.
*/

// personalRequest is what the daemon hands the helper.
type personalRequest struct {
	Want    []PersonalAccount `json:"want"`
	Passwd  string            `json:"passwd,omitempty"`
	Sudoers string            `json:"sudoers,omitempty"`
	State   map[string]string `json:"state,omitempty"`
}

// personalReply is what the helper hands back.
type personalReply struct {
	States []PersonalAccountState `json:"states"`
	State  map[string]string      `json:"state,omitempty"`
}

func (p *Provisioner) reconcileOutside(ctx context.Context, want []PersonalAccount) []PersonalAccountState {
	failAll := func(detail string) []PersonalAccountState {
		out := make([]PersonalAccountState, 0, len(want))
		for _, a := range want {
			out = append(out, PersonalAccountState{Name: a.Name, Owner: a.Owner, State: "failed", Detail: detail})
		}
		return out
	}
	req, err := json.Marshal(personalRequest{Want: want, Passwd: p.Passwd, Sudoers: p.Sudoers, State: p.state})
	if err != nil {
		return failAll("the agent could not write the account list: " + err.Error())
	}
	body, logs, err := p.Outside(ctx, req)
	// The helper logs to stderr; that is the record of what it did.
	sc := bufio.NewScanner(bytes.NewReader(logs))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			p.Log.Info("personal accounts helper", "said", line)
		}
	}
	if err != nil {
		p.Log.Error("personal accounts: the account helper failed", "error", err)
		return failAll("the agent's account helper failed: " + err.Error())
	}
	var reply personalReply
	if err := json.Unmarshal(body, &reply); err != nil {
		p.Log.Error("personal accounts: the account helper's reply is unreadable", "error", err)
		return failAll("the agent's account helper gave an unreadable reply")
	}
	p.state = reply.State
	return reply.States
}

// RunPersonalApply is `argus-agent personal-apply`: one pass, request on
// stdin, reply on stdout, log on stderr. It is run by the daemon, as root,
// outside the daemon's sandbox.
func RunPersonalApply(stdin io.Reader, stdout, stderr io.Writer) int {
	var req personalRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&req); err != nil {
		fmt.Fprintln(stderr, "personal-apply: cannot read the request:", err)
		return 2
	}
	p := &Provisioner{
		Passwd:  req.Passwd,
		Sudoers: req.Sudoers,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		Log:   slog.New(slog.NewTextHandler(stderr, nil)),
		state: req.State,
	}
	states := p.reconcile(context.Background(), req.Want)
	if err := json.NewEncoder(stdout).Encode(personalReply{States: states, State: p.state}); err != nil {
		fmt.Fprintln(stderr, "personal-apply: cannot write the reply:", err)
		return 1
	}
	return 0
}
