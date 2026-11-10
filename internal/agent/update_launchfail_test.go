package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// failingLaunch is a host whose update helper cannot be started.
type failingLaunch struct{ fakeUpdates }

func (f *failingLaunch) launch(context.Context, string) error {
	return errors.New("systemd-run: unit already exists")
}

// A helper that never started must fail the update and let sessions in
// again. Root cause of the bug this guards: the launch error was assigned
// to a variable scoped to the if statement, so the check after it saw nil
// and the agent sat "restarting", refusing every new session, until it was
// restarted by hand.
func TestAnUpdateWhoseHelperCannotStartFailsAndReleasesTheHold(t *testing.T) {
	srv := releases(t, map[string]string{
		"v1.1.0/argus-agent_1.1.0_linux_amd64.deb": "new package",
	}, false)
	var busy atomic.Int64
	u := testUpdater(t, &failingLaunch{}, &busy)
	var held atomic.Bool
	u.Hold = func(on bool) { held.Store(on) }

	u.Handle(context.Background(), &UpdateRequest{ID: "r1", Version: "1.1.0", URL: srv.URL})
	s := waitState(t, u, UpdateFailed)
	if !strings.Contains(s.Message, "unit already exists") {
		t.Errorf("failure message = %q, want the launch error", s.Message)
	}
	if held.Load() {
		t.Error("new sessions are still held after the update failed to start")
	}
}
