//go:build darwin

package agent

import (
	"context"
	"errors"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
On a Mac the person at the screen is asked or told through a dialog or a
notification drawn by osascript in their own session: launchctl asuser puts
it in their login's bootstrap, and sudo -u makes it theirs rather than
root's. The text reaches the script as arguments, never as script source,
so nothing in a name or a reason can become AppleScript.
*/

type macAsker struct{ logons LogonsHost }

func platformAsker() Asker { return macAsker{logons: platformLogons()} }

func (m macAsker) InUse(ctx context.Context) ([]tunnel.Logon, error) {
	res, err := m.logons.List(ctx)
	if err != nil {
		return nil, err
	}
	return consoleLogons(res.Logons), nil
}

const macAskScript = `on run argv
	set r to display dialog (item 2 of argv) with title (item 1 of argv) buttons {"No", "Allow"} default button "No" giving up after ((item 3 of argv) as integer) with icon caution
	if gave up of r then return "timeout"
	return button returned of r
end run`

const macTellScript = `on run argv
	display notification (item 2 of argv) with title (item 1 of argv)
end run`

func (macAsker) Ask(ctx context.Context, l tunnel.Logon, title, text string, seconds int) (bool, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(seconds+10)*time.Second)
	defer cancel()
	out, err := osascriptAs(cctx, l.Account, macAskScript, title, text, strconv.Itoa(seconds))
	if err != nil {
		return false, false, err
	}
	return parseMacAnswer(out)
}

func (macAsker) Tell(ctx context.Context, l tunnel.Logon, title, text string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := osascriptAs(cctx, l.Account, macTellScript, title, strings.TrimSpace(text))
	return err
}

// osascriptAs runs script in account's GUI session with args as argv.
func osascriptAs(ctx context.Context, account, script string, args ...string) (string, error) {
	u, err := user.Lookup(account)
	if err != nil {
		return "", err
	}
	argv := append([]string{"asuser", u.Uid, "/usr/bin/sudo", "-n", "-u", u.Username,
		"/usr/bin/osascript", "-e", script, "--"}, args...)
	out, err := exec.CommandContext(ctx, "/bin/launchctl", argv...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}
