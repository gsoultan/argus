// Package wskeep keeps an idle WebSocket from being taken for a dead one.
//
// A shell nobody is typing into sends nothing for minutes at a time, and the
// proxies people put in front of Argus read that as a dead connection:
// Cloudflare's edge closes a WebSocket after about 100 seconds without traffic,
// and load balancers commonly do the same after 60. The terminal dropped while
// its operator was reading a man page.
package wskeep

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// Interval is well inside the shortest idle timeout in common use, and a ping
// is a few bytes.
const Interval = 30 * time.Second

// Run pings conn every Interval until ctx ends or a ping goes unanswered.
//
// The caller must be reading from conn, which is how the pong is received.
// Run never closes the connection: a missed pong ends the pinging, and the
// handler learns of a dead peer the way it always has, from a failed read or
// write.
func Run(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, Interval)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
