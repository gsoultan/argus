package agent

import (
	"bytes"
	"testing"
)

// The backlog keeps the newest bytes, and the count of everything sent, so a
// resume can carry on from an exact byte or say it cannot.
func TestABacklogKeepsTheNewestOutputAndCountsAll(t *testing.T) {
	ts := &tunnelShell{}
	ts.keep(bytes.Repeat([]byte("a"), backlogSize-10))
	ts.keep([]byte("0123456789ABCDEFGHIJ"))
	if ts.sent != int64(backlogSize+10) {
		t.Fatalf("sent %d", ts.sent)
	}
	if len(ts.backlog) != backlogSize || !bytes.HasSuffix(ts.backlog, []byte("0123456789ABCDEFGHIJ")) {
		t.Fatalf("backlog of %d bytes ending %q", len(ts.backlog), ts.backlog[len(ts.backlog)-20:])
	}
	big := bytes.Repeat([]byte("z"), backlogSize+5)
	ts.keep(big)
	if len(ts.backlog) != backlogSize || ts.backlog[0] != 'z' {
		t.Fatalf("a write larger than the backlog kept %d bytes", len(ts.backlog))
	}
}
