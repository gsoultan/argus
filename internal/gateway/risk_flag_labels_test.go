package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

/*
Every risk flag the gateway emits has words in the console.

Five did not. recording-incomplete, terminated, no-credential-injection,
legacy-credssp-binding and no-network-level-auth all reached the session table
as raw kebab-case text with an empty tooltip, because RISK_LABEL is typed
Record<RiskFlag, string> and none of them were in the RiskFlag union -- so
TypeScript was satisfied and the flag was invisible in the only place anyone
reads it. recording-incomplete is the one the tamper work exists to surface.

TypeScript keeps the label map complete on its own. What it cannot see is this
side adding a flag, which is the drift that actually happened.
*/

var (
	goFlagRe = regexp.MustCompile(`flags = append\(flags, "([a-z0-9-]+)"\)`)
	tsFlagRe = regexp.MustCompile(`\|\s*'([a-z0-9-]+)'`)
)

func TestEveryRiskFlagHasAConsoleLabel(t *testing.T) {
	root := filepath.Join("..", "..")

	emitted := map[string]string{} // flag -> file that emits it
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range goFlagRe.FindAllSubmatch(src, -1) {
				emitted[string(m[1])] = path
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(emitted) == 0 {
		t.Fatal("found no risk flags in the Go sources; the pattern this test " +
			"matches on has changed and it is no longer guarding anything")
	}

	domain := filepath.Join(root, "web", "src", "types", "domain.ts")
	src, err := os.ReadFile(domain)
	if err != nil {
		t.Fatalf("cannot read %s: %v\nif the console moved, point this test at "+
			"its new home rather than deleting it -- five flags shipped unlabelled "+
			"the last time nothing was watching", domain, err)
	}
	_, after, ok := strings.Cut(string(src), "export type RiskFlag =")
	if !ok {
		t.Fatal("no RiskFlag union in domain.ts")
	}
	union, _, _ := strings.Cut(after, "\n\n")

	known := map[string]bool{}
	for _, m := range tsFlagRe.FindAllStringSubmatch(union, -1) {
		known[m[1]] = true
	}

	var missing []string
	for flag, file := range emitted {
		if !known[flag] {
			missing = append(missing, flag+" (emitted by "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the gateway emits %d risk flag(s) the console has no words for:\n  %s\n"+
			"add them to RiskFlag in web/src/types/domain.ts and to RISK_LABEL in "+
			"web/src/components/primitives.tsx, or they render as raw text with an "+
			"empty tooltip", len(missing), strings.Join(missing, "\n  "))
	}
}
