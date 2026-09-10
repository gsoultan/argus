package control

import (
	"strconv"
	"strings"
	"testing"
)

// The warning key is built from field names a reporter chose, so the map that
// holds it is a map an authenticated reporter can grow from the outside.
func TestUnknownFieldWarningsAreBounded(t *testing.T) {
	warnedMu.Lock()
	warnedFields = map[string]struct{}{}
	warnedMu.Unlock()

	log := &capturingLogger{}
	for i := 0; i < maxWarnedFieldSets*3; i++ {
		warnUnknownOnce(log, "/report/session", []string{"field" + strconv.Itoa(i)})
	}

	warnedMu.Lock()
	n := len(warnedFields)
	warnedMu.Unlock()
	if n > maxWarnedFieldSets {
		t.Fatalf("warnedFields holds %d entries after %d distinct field sets; "+
			"the cap is %d and a reporter chooses the keys",
			n, maxWarnedFieldSets*3, maxWarnedFieldSets)
	}
}

// One report cannot put thousands of names into a key or a log line.
func TestOneReportCannotBuildAnUnboundedKey(t *testing.T) {
	warnedMu.Lock()
	warnedFields = map[string]struct{}{}
	warnedMu.Unlock()

	many := make([]string, 5000)
	for i := range many {
		many[i] = "field" + strconv.Itoa(i)
	}
	log := &capturingLogger{}
	warnUnknownOnce(log, "/report/session", many)

	warnedMu.Lock()
	var key string
	for k := range warnedFields {
		key = k
	}
	warnedMu.Unlock()

	if strings.Count(key, ",") >= len(many)/2 {
		t.Fatalf("the key carries %d names; one 1 MiB body should not decide "+
			"how much memory a map entry costs", strings.Count(key, ",")+1)
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "more") {
		t.Errorf("the log line did not say it was truncated: %v", log.lines)
	}
}
