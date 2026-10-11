package agent

import "testing"

// systemd 255's systemd-run --wait exits 1 for any failing unit, so a shell's
// own exit status has to come from the summary it prints. Seen in the install
// drill: `exit 7` reported as 1.
func TestTheShellsExitStatusIsReadFromTheUnitSummary(t *testing.T) {
	for _, c := range []struct {
		summary string
		want    int
		ok      bool
	}{
		{"Running as unit: argus-session-1.service\nFinished with result: exit-code\n" +
			"Main processes terminated with: code=exited/status=7\nService runtime: 41ms\n", 7, true},
		{"Finished with result: success\nMain processes terminated with: code=exited/status=0\n", 0, true},
		{"Finished with result: signal\nMain processes terminated with: code=killed/status=HUP\n", 129, true},
		{"Failed to start transient service unit: Access denied\n", 0, false},
	} {
		got, ok := unitExitStatus(c.summary)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %d %v, want %d %v", c.summary, got, ok, c.want, c.ok)
		}
	}
}
