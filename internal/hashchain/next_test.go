package hashchain

import (
	"bytes"
	"testing"
)

// Next must agree with what a Chain writes, link for link: argus-secret's read
// log and the session recordings are checked by the same rule.
func TestNextMatchesChain(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf)
	head := Genesis
	for _, line := range []string{"first", `{"a":1}`, "", "third"} {
		if err := c.Emit([]byte(line)); err != nil {
			t.Fatal(err)
		}
		head = Next(head, []byte(line))
		if head != c.Head() {
			t.Fatalf("after %q: Next %s, Chain %s", line, head, c.Head())
		}
	}
}
