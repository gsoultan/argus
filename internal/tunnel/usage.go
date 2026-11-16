package tunnel

import "time"

/*
The usage.live service: a server's usage figures, sampled while someone in
the console is looking at them.

	argus-control ──"usage.live" stream──▶ agent

After the header and the answer the agent sends one 'u' frame (the agent's
Usage, JSON) every UsageLiveEvery until the stream closes. Nothing else is
read from the stream; the control plane closing it is how sampling stops.

It costs nothing while nobody watches: there is no stream then. The control
plane opens at most one per server however many consoles are watching, and
nothing it carries is written to the database -- the heartbeat goes on
reporting usage about once a minute, and that is what history is built from.

No grant: the figures are the ones the heartbeat already sends, and the
stream can only be opened by the control plane the agent's tunnel is to. An
old agent refuses the service as unknown, and the console keeps showing the
heartbeat's figures.
*/

// UsageLiveService is the tunnel service name.
const UsageLiveService = "usage.live"

// FrameUsage carries one sample.
const FrameUsage byte = 'u'

// UsageLiveEvery is how often a live stream samples unless the header asks
// for another pace, and UsageLiveMin and UsageLiveMax bound what it may ask.
const (
	UsageLiveEvery = 2 * time.Second
	UsageLiveMin   = time.Second
	UsageLiveMax   = 10 * time.Second
)

// UsagePace is the pace a header asks for, within bounds; zero asks for
// UsageLiveEvery.
func UsagePace(ms int) time.Duration {
	if ms <= 0 {
		return UsageLiveEvery
	}
	d := time.Duration(ms) * time.Millisecond
	return min(max(d, UsageLiveMin), UsageLiveMax)
}
