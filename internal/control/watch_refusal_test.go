package control

import (
	"strings"
	"testing"
	"time"
)

// A refusal for a session nothing has reported for minutes says so, and
// where the login most likely went; one still being reported says only what
// the agent said.
func TestWatchRefusalExplainsASilentSession(t *testing.T) {
	why := "the session is not running on this server and this agent has no record of it ending"
	live := &Session{Principal: "ops", State: "active", LastReportedAt: time.Now()}
	if got := watchRefusal("imp-crm", why, live); got != "the agent on imp-crm refused: "+why {
		t.Errorf("reported session: %q", got)
	}
	quiet := &Session{Principal: "ops", State: "active", Silent: true,
		LastReportedAt: time.Date(2026, 11, 20, 9, 40, 0, 0, time.UTC)}
	got := watchRefusal("imp-crm", why, quiet)
	for _, want := range []string{why, "since 2026-11-20 09:40 UTC", "after the agent restarted",
		"newer live session on imp-crm as ops"} {
		if !strings.Contains(got, want) {
			t.Errorf("silent session: %q lacks %q", got, want)
		}
	}
}
