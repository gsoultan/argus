package aws

import (
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// The manage template makes both console roles with their AWS policies and
// the same trust as the others; the read template makes neither.
func TestConsoleRolesInTheTemplate(t *testing.T) {
	m := Template("arn:aws:iam::111111111111:role/argus", "ext-1", true)
	for _, want := range []string{"RoleName: ArgusConsoleRead", "arn:aws:iam::aws:policy/ReadOnlyAccess",
		"RoleName: ArgusConsoleOperate", "arn:aws:iam::aws:policy/PowerUserAccess"} {
		if !strings.Contains(m, want) {
			t.Fatalf("manage template lacks %q", want)
		}
	}
	if strings.Count(m, `sts:ExternalId: "ext-1"`) != 4 || strings.Count(m, `sts:SourceIdentity: "false"`) != 4 {
		t.Fatal("every role requires the external id and a source identity")
	}
	if strings.Contains(Template("p", "e", false), "ArgusConsole") {
		t.Fatal("the read template makes no console role")
	}
	p := &Provider{}
	if got := p.ConsoleRole("222222222222", cloud.ConsoleOperate); got != "arn:aws:iam::222222222222:role/ArgusConsoleOperate" {
		t.Fatal(got)
	}
}
