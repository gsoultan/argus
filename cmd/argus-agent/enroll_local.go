package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/agent"
)

/*
`argus-control-host enroll-local`: the helper on argus-control's own server
enrolling itself.

The argus-control package installs this binary as argus-control-host, and
argus-control leaves a single-use token for its own hostname in
/var/lib/argus-control/host/enroll.json (internal/control/control_host.go).
This runs as root from the helper's unit before the daemon starts, and
exchanges that token through the same POST /api/v1/agents/enroll as any
agent.

The file is written by argus-control's unprivileged account, so it is read
as input from a less trusted party: not through a symlink, not when anyone
but that account or root can write it, a bounded size, and only the fields
enrolling takes. The hostname is this host's own, never the file's; a file
naming another host is refused. Nothing in it is run or written anywhere
but the enrollment, which is what `argus-agent enroll` writes from a token
pasted by hand.
*/

// controlHostRequest is the part of enroll.json the helper reads.
type controlHostRequest struct {
	ControlURL string    `json:"controlUrl"`
	Token      string    `json:"token"`
	Pin        string    `json:"pin"`
	Hostname   string    `json:"hostname"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

func runEnrollLocal(args []string) int {
	fs := flag.NewFlagSet("enroll-local", flag.ExitOnError)
	reqPath := fs.String("request", "/var/lib/argus-control/host/enroll.json",
		"the request argus-control leaves for the helper on its own server")
	stateDir := fs.String("state", agent.DefaultStateDir, "where the enrollment is kept")
	_ = fs.Parse(args)

	if !isPrivileged() {
		fmt.Fprintln(os.Stderr, "enroll-local: run as "+privilegedName+
			" -- the credential must not be readable by the users it records")
		return 1
	}
	if _, err := os.Stat(filepath.Join(*stateDir, agent.EnrollmentFile)); err == nil {
		fmt.Println("already enrolled")
		return 0
	}
	req, err := readControlHostRequest(*reqPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll-local: %v\n", err)
		return 1
	}
	host, err := os.Hostname()
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll-local: hostname: %v\n", err)
		return 1
	}
	if !strings.EqualFold(req.Hostname, host) {
		fmt.Fprintf(os.Stderr, "enroll-local: the request is for %q and this host is %q; "+
			"argus-control makes a new one within five minutes\n", req.Hostname, host)
		return 1
	}
	if !req.ExpiresAt.IsZero() && time.Now().After(req.ExpiresAt) {
		fmt.Fprintln(os.Stderr, "enroll-local: the request has expired; argus-control makes "+
			"a new one within five minutes")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	enr, err := agent.Enroll(ctx, agent.EnrollOptions{
		ControlURL: req.ControlURL, Token: req.Token, Pin: req.Pin,
		Hostname: host, StateDir: *stateDir, ControlHost: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "enroll-local: %v\n", err)
		return 1
	}
	fmt.Printf("enrolled as %s with %s, argus-control's own server\n", enr.Hostname, enr.ControlURL)
	return 0
}

// maxControlHostRequest bounds the read: the real file is a few hundred bytes.
const maxControlHostRequest = 16 << 10

// readControlHostRequest reads and checks enroll.json.
func readControlHostRequest(path string) (controlHostRequest, error) {
	var r controlHostRequest
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, fmt.Errorf("%s does not exist: argus-control has not asked this server "+
				"to enroll (it does within five minutes of starting, when it serves TLS itself)", path)
		}
		return r, err
	}
	defer f.Close()
	if err := checkRequestOwner(f); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxControlHostRequest+1))
	if err != nil {
		return r, err
	}
	if len(b) > maxControlHostRequest {
		return r, fmt.Errorf("%s is larger than any request argus-control writes", path)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	if !strings.HasPrefix(r.ControlURL, "https://") {
		return r, fmt.Errorf("%s: the control URL must be https://", path)
	}
	if r.Token == "" || r.Hostname == "" {
		return r, fmt.Errorf("%s: no token or hostname in it", path)
	}
	return r, nil
}

// standDownControlHost turns off argus-control-host once argus-agent has
// taken over its enrollment, so the two never both start at boot. Best
// effort: the units exist only where the argus-control package is installed.
func standDownControlHost() {
	if runtime.GOOS != "linux" {
		return
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return
	}
	out, err := exec.Command(systemctl, "disable", "--now",
		"argus-control-host-enroll.path", "argus-control-host.service").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not turn off argus-control-host (%v): %s\n"+
			"run: systemctl disable --now argus-control-host-enroll.path argus-control-host\n",
			err, strings.TrimSpace(string(out)))
	}
}
