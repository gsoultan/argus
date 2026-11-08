//go:build !windows

package agent

import (
	"context"
	"errors"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
On Linux the person at the machine is told, never asked: a terminal is a
login of its own and takes nothing from them. The notice goes to the desktop
of everyone signed in on the machine's own screen, through notify-send in
their session; a machine without one shows nothing, and the session still
opens.
*/

type unixAsker struct{ logons LogonsHost }

func platformAsker() Asker { return unixAsker{logons: platformLogons()} }

func (u unixAsker) InUse(ctx context.Context) ([]tunnel.Logon, error) {
	res, err := u.logons.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []tunnel.Logon
	seen := map[string]bool{}
	for _, l := range res.Logons {
		if l.Console && !seen[l.Account] {
			seen[l.Account] = true
			out = append(out, l)
		}
	}
	return out, nil
}

func (unixAsker) Ask(context.Context, tunnel.Logon, string, string, int) (bool, bool, error) {
	return false, false, errors.New("asking at the machine is only done on Windows")
}

func (unixAsker) Tell(ctx context.Context, l tunnel.Logon, title, text string) error {
	u, err := user.Lookup(l.Account)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return errors.New("notify-send is not installed")
	}
	runuser, err := exec.LookPath("runuser")
	if err != nil {
		return errors.New("runuser is not installed")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, runuser, "-u", u.Username, "--", bin,
		"--app-name=Argus", "--urgency=critical", "--", title, strings.TrimSpace(text))
	cmd.Env = []string{
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + u.Uid + "/bus",
		"XDG_RUNTIME_DIR=/run/user/" + u.Uid,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	return cmd.Run()
}
