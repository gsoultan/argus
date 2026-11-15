package agent

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"time"
)

/* ── How a run ended, in parts a person can act on ───────────────────────── */

// Whether a run changed the server, as the agent saw it. Unknown is said
// when it could not look, never as a guess either way.
const (
	ChangedYes     = "yes"
	ChangedNo      = "no"
	ChangedUnknown = "unknown"
)

// RunOutcome is what a person needs from a run that ended badly, kept apart
// so the console can show each on its own: whether anything on the server
// changed, why it stopped, and what to do now, in order. Message stays the
// one-line summary for the audit log and alerts.
type RunOutcome struct {
	Changed string `json:"changed"`
	// Why is the tool's own reason, as it said it.
	Why string `json:"why,omitempty"`
	// Next are steps in order; commands are in `backticks`.
	Next []string `json:"next,omitempty"`
}

// RunStep is when a run reached a step, so the console can say how long
// each took.
type RunStep struct {
	Step string    `json:"step"`
	At   time.Time `json:"at"`
}

// maxRunSteps bounds Steps: a run has a handful.
const maxRunSteps = 32

// outcomeError is an error that knows how the run should be reported.
type outcomeError struct {
	msg     string
	outcome RunOutcome
}

func (e *outcomeError) Error() string { return e.msg }

// failWith ends the run failed with its outcome.
func (a *PackageApplier) failWith(msg string, o RunOutcome) PackageRunStatus {
	return a.finish(RunFailed, msg, func(s *PackageRunStatus) { s.Outcome = &o })
}

// failErr ends the run failed, with err's outcome when it carries one and
// otherwise with orElse's (nil: none).
func (a *PackageApplier) failErr(err error, orElse *RunOutcome) PackageRunStatus {
	var oe *outcomeError
	if errors.As(err, &oe) {
		return a.failWith(oe.msg, oe.outcome)
	}
	if orElse != nil {
		return a.failWith(err.Error(), *orElse)
	}
	return a.fail(err.Error())
}

// againSteps is what to do after a run that changed nothing and stopped
// for a reason to fix on the server.
var againSteps = []string{
	"Fix what it says stopped it, on the server.",
	"Press Check for updates, so Argus sees the change.",
	"Ask for the upgrade again.",
}

// dpkgLogFile is where dpkg writes each package it unpacks, configures or
// removes. Replaced in tests.
var dpkgLogFile = "/var/log/dpkg.log"

// dpkgActions are dpkg's log lines that mean a package changed.
var dpkgActions = map[string]bool{"install": true, "upgrade": true, "remove": true, "purge": true, "configure": true}

// dpkgChangedSince reads dpkg's log for a package changed at or after
// since: ChangedYes, ChangedNo, or ChangedUnknown when the log cannot be
// read. dpkg writes its log in local time.
//
//	2026-11-15 07:50:04 upgrade libc6:amd64 2.35-0ubuntu3.8 2.39-0ubuntu8
func dpkgChangedSince(since time.Time) string {
	f, err := os.Open(dpkgLogFile)
	if err != nil {
		return ChangedUnknown
	}
	defer f.Close()
	from := since.Truncate(time.Second)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 21 {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
		if err != nil || at.Before(from) {
			continue
		}
		if fs := strings.Fields(line[20:]); len(fs) > 0 && dpkgActions[fs[0]] {
			return ChangedYes
		}
	}
	if sc.Err() != nil {
		return ChangedUnknown
	}
	return ChangedNo
}
