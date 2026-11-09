package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Asking, or telling, the person at a workstation before a session opens on it.
The control plane puts a tunnel.GrantAsk on a workstation's grants and none
on a server's; see tunnel/ask.go for the rule. The question is the agent's
own, shown in the signed-in person's session (WTSSendMessage on Windows), so
it names who is connecting rather than leaving Windows to ask about "another
user". Nobody signed in, or nobody using it, means nobody to ask.
*/

// Asker reaches the person at the machine. Nil in a config is this
// platform's.
type Asker interface {
	// InUse is the sessions someone is using now, the one on the machine's
	// own screen first.
	InUse(ctx context.Context) ([]tunnel.Logon, error)
	// Ask puts a yes-or-no question in a session and waits for it. answered
	// is false when the time ran out.
	Ask(ctx context.Context, l tunnel.Logon, title, text string, seconds int) (allowed, answered bool, err error)
	// Tell shows a notice in a session without waiting.
	Tell(ctx context.Context, l tunnel.Logon, title, text string) error
}

const askTitle = "Argus"

// askAtMachine applies g.Ask before a session opens. what says what the
// session does, as "open a Remote Desktop session as ops". A nil error
// means go ahead.
func askAtMachine(ctx context.Context, a Asker, g tunnel.Grant, what string, log *slog.Logger) error {
	if g.Ask == nil {
		return nil
	}
	q := *g.Ask
	if err := q.Validate(); err != nil {
		return err
	}
	if a == nil {
		a = platformAsker()
	}
	inUse, err := a.InUse(ctx)
	if err != nil {
		// Who is there cannot be read: asking nobody would open on a machine
		// someone may be using, so it does not open.
		return errors.New("could not tell who is using this machine, so nobody could be asked: " + err.Error())
	}
	if len(inUse) == 0 {
		log.Info("tunnel: nobody is using the machine; opening without asking", "for", g.Email)
		return nil
	}
	if q.Mode == tunnel.TellOnly {
		text := fmt.Sprintf("%s has connected to this computer through Argus to %s. The session is recorded.", q.Who, what)
		for _, l := range inUse {
			if err := a.Tell(ctx, l, askTitle, text); err != nil {
				log.Warn("tunnel: could not show the notice", "account", l.Account, "error", err)
			}
		}
		return nil
	}
	l := inUse[0]
	text := fmt.Sprintf("%s wants to %s through Argus. The session is recorded.\n\n"+
		"Allow it? If you do not answer in %d seconds, the answer is No.", q.Who, what, q.Seconds)
	allowed, answered, err := a.Ask(ctx, l, askTitle, text, q.Seconds)
	switch {
	case err != nil:
		return errors.New(tunnel.AskRefusedPrefix + "the question could not be shown to " + l.Account + ": " + err.Error())
	case !answered:
		log.Info("tunnel: nobody answered at the machine", "account", l.Account, "for", g.Email)
		return fmt.Errorf("%s%s did not answer in %d seconds", tunnel.AskRefusedPrefix, shortAccount(l.Account), q.Seconds)
	case !allowed:
		log.Info("tunnel: declined at the machine", "account", l.Account, "for", g.Email)
		return fmt.Errorf("%s%s said no", tunnel.AskRefusedPrefix, shortAccount(l.Account))
	}
	log.Info("tunnel: allowed at the machine", "account", l.Account, "for", g.Email)
	return nil
}

// consoleLogons is the logins on the machine's own screen, one per account,
// in the order the host listed them: whom a Unix asker asks or tells.
func consoleLogons(all []tunnel.Logon) []tunnel.Logon {
	var out []tunnel.Logon
	seen := map[string]bool{}
	for _, l := range all {
		if l.Console && !seen[l.Account] {
			seen[l.Account] = true
			out = append(out, l)
		}
	}
	return out
}

func shortAccount(a string) string {
	if i := strings.LastIndexByte(a, '\\'); i >= 0 {
		return a[i+1:]
	}
	return a
}

// askAtMachineFor is askAtMachine bounded by the question's own wait, so a
// box nobody closes cannot hold the stream open.
func askAtMachineFor(ctx context.Context, a Asker, g tunnel.Grant, what string, log *slog.Logger) error {
	if g.Ask == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(g.Ask.Seconds+15)*time.Second)
	defer cancel()
	return askAtMachine(cctx, a, g, what, log)
}
