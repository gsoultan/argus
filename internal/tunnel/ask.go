package tunnel

import (
	"errors"
	"strings"
)

/*
Asking the person at a workstation before a session opens on it.

A grant for a workstation carries an Ask. The agent looks at who is signed in
and is using the machine now: if nobody is, the session opens straight away;
if someone is, "ask" puts a question on their screen and opens only on Allow,
while "tell" opens at once and puts a notice there naming who connected. A
server's grants carry none, and open without asking anyone.
*/

// Ask modes.
const (
	AskFirst = "ask"
	TellOnly = "tell"
)

// MaxAskSeconds bounds how long a question waits for an answer.
const MaxAskSeconds = 120

// AskRefusedPrefix starts the refusal an agent gives when the person at the
// machine said no or did not answer, so the control plane can tell it from
// any other refusal.
const AskRefusedPrefix = "not allowed by the person at the machine: "

// GrantAsk is what the person at the machine is asked or told.
type GrantAsk struct {
	Mode string `json:"mode"`
	// Seconds is how long the question waits; no answer declines.
	Seconds int `json:"seconds,omitempty"`
	// Who is the person connecting, as the console names them.
	Who string `json:"who"`
}

// Validate refuses a question the console never sends.
func (a GrantAsk) Validate() error {
	switch a.Mode {
	case AskFirst:
		if a.Seconds < 10 || a.Seconds > MaxAskSeconds {
			return errors.New("a question waits between 10 and 120 seconds")
		}
	case TellOnly:
	default:
		return errors.New("unknown ask mode")
	}
	if strings.TrimSpace(a.Who) == "" || len(a.Who) > 200 {
		return errors.New("name who is connecting")
	}
	return nil
}
