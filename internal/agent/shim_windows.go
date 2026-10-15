package agent

import (
	"fmt"
	"os"
)

// RunShim refuses on Windows: the sshd hook that records direct logins is
// not ported yet, and a session that cannot be recorded does not happen.
// Sessions opened through the console are recorded by the agent itself.
func RunShim(ShimConfig) int {
	fmt.Fprintln(os.Stderr, "argus: direct SSH logins cannot be recorded on Windows yet; "+
		"open this session from the Argus console")
	return 1
}
