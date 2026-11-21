package agent

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The agent reads its addresses through netlink on Linux. A unit that
// restricts address families without AF_NETLINK makes every read fail, and
// the console then shows no address for the server at all.
func TestAgentUnitsAllowNetlink(t *testing.T) {
	line := regexp.MustCompile(`(?m)^RestrictAddressFamilies=(.*)$`)
	for _, unit := range []string{
		"../../packaging/systemd/argus-agent.service",
		"../../packaging/systemd/argus-control-host.service",
	} {
		b, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		m := line.FindSubmatch(b)
		if m == nil {
			continue // no restriction at all
		}
		if !strings.Contains(" "+string(m[1])+" ", " AF_NETLINK ") {
			t.Errorf("%s: RestrictAddressFamilies=%s leaves out AF_NETLINK, so the agent cannot read its addresses", unit, m[1])
		}
	}
}
