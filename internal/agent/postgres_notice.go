package agent

import (
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/gsoultan/caduceus/wire"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
pgNotices keeps what Postgres said beside a statement that did not fail.

Postgres reports some outcomes only as a NoticeResponse. The one that matters
most: a GRANT on objects the session's role neither owns nor holds a grant
option on ends with the command tag "GRANT" and a WARNING (01007) that "no
privileges were granted". the driver drops notices unless OnNotice is set, so the
console used to show that GRANT as a success while nothing changed.

The callback runs on the goroutine reading the connection, which is the
statement's own, but the mutex costs nothing and keeps it right if that ever
changes. What one statement keeps is bounded: a DO block can RAISE NOTICE in a
loop.
*/
type pgNotices struct {
	mu   sync.Mutex
	list []tunnel.PGNotice
	// dropped counts notices past the bound, so the last kept one can say so.
	dropped int
}

// add is wire.Config.OnNotice.
func (n *pgNotices) add(pn *wire.Error) {
	if pn == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.list) >= tunnel.MaxPGNotices {
		n.dropped++
		return
	}
	n.list = append(n.list, tunnel.PGNotice{
		Severity: pn.Severity,
		Code:     pn.Code,
		Message:  clipNotice(pn.Message),
		Detail:   clipNotice(pn.Detail),
		Hint:     clipNotice(pn.Hint),
	})
}

// reset forgets what an earlier statement (or the session's own prelude)
// was told.
func (n *pgNotices) reset() {
	n.mu.Lock()
	n.list, n.dropped = nil, 0
	n.mu.Unlock()
}

// take returns this statement's notices and forgets them.
func (n *pgNotices) take() []tunnel.PGNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.list
	if n.dropped > 0 && len(out) > 0 {
		last := &out[len(out)-1]
		more := fmt.Sprintf("%d more notices were not kept.", n.dropped)
		if n.dropped == 1 {
			more = "1 more notice was not kept."
		}
		if last.Detail != "" {
			more = last.Detail + " " + more
		}
		last.Detail = clipNotice(more)
	}
	n.list, n.dropped = nil, 0
	return out
}

// clipNotice keeps a notice's text within MaxPGNoticeText bytes, on a rune
// boundary.
func clipNotice(s string) string {
	if len(s) <= tunnel.MaxPGNoticeText {
		return s
	}
	cut := tunnel.MaxPGNoticeText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
