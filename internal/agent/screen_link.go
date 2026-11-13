package agent

import (
	"encoding/binary"
	"time"
)

/*
Fitting screen sharing to the link.

The browser acknowledges each frame once it is drawn, and how long that took
says what the link can carry. While acknowledgements come back about as fast
as the quickest ever did, the link has room, and motion may cost more, up to
what the encoder would spend on its own. When they start taking longer,
frames are queueing behind each other: what got through in the last second
is what the link carries, and motion is held under it.

The helper is told the figure (scrRate) and does two things with it. The
H.264 encoder's bitrate follows it, so a picture is the size the link can
take in a frame's time rather than the size a LAN could. And the exact
pixels sent after motion stops are spread over frames no larger than the
link carries in a moment, so a keystroke is never waiting behind a screenful
of sharpening.
*/

// scrRate is agent to helper: what the link carries, in bits a second as
// uint32 LE. Zero is no limit known.
const scrRate byte = 13

const (
	// linkRateMin is the least motion is held to: below it H.264 at screen
	// size is not worth looking at, and the frame rate gives instead.
	linkRateMin = 300_000
	// linkSlack is how much longer than the best an acknowledgement may
	// take, beyond twice it, before frames are taken to be queueing.
	linkSlack = 150 * time.Millisecond
	// linkProbeGap and linkProbe are how the rate climbs while the link has
	// room: by an eighth, four times a second at most.
	linkProbeGap = 250 * time.Millisecond
	// linkCutGap is the least time between two cuts: one queue is one cut.
	linkCutGap = 500 * time.Millisecond
	// linkSpan is how far back what got through is measured over.
	linkSpan = time.Second
	// linkShare is how much of what got through motion is held to, leaving
	// the rest to drain what queued.
	linkShareNum, linkShareDen = 4, 5
	// linkTell is the change worth telling the helper about, as a divisor:
	// an eighth.
	linkTell = 8

	// linkRefineFor is how much of the link's time one frame of sharpening
	// may take.
	linkRefineFor = 120 * time.Millisecond
	// linkRefineMin is the fewest tiles a frame sharpens, however slow the
	// link: a typed line's worth.
	linkRefineMin = 8

	// screenWindowMax is the most frames in flight, on a link whose round
	// trip is long; screenWindowPer is the round trip that earns each one
	// past the first.
	screenWindowMax = 6
	screenWindowPer = 50 * time.Millisecond
)

// linkPoint is how much had been sent when a mark was written.
type linkPoint struct {
	id    uint32
	at    time.Time
	bytes int64
}

// linkGauge reads the link from the browser's acknowledgements. It is the
// screen client's, under its lock.
type linkGauge struct {
	// max is what motion costs with no limit: the encoder's own figure.
	max  int
	rate int // bits a second; 0 until max is known
	// least is the lowest the rate has been, for the session's last word.
	least int
	told  int // what the helper was last told

	sent   int64 // bytes written toward the browser
	points [64]linkPoint
	// got is what had been delivered at each of the last acknowledgements,
	// for the rate over linkSpan.
	got    [32]linkPoint
	gotN   int
	best   time.Duration
	probed time.Time
	cut    time.Time
}

// size sets what motion costs with no limit, from the screen's size.
func (g *linkGauge) size(width, height int) {
	g.max = screenMotionBits(width, height)
	if g.rate == 0 || g.rate > g.max {
		g.rate = g.max
	}
}

// screenMotionBits is what motion on a screen of this size costs when the
// link is no object, in bits a second.
func screenMotionBits(width, height int) int {
	return min(max(width*height*screenVideoBits, 1_000_000), screenVideoMaxBits)
}

// mark notes that mark id was written now, after everything sent so far.
func (g *linkGauge) mark(id uint32, now time.Time) {
	g.points[id%uint32(len(g.points))] = linkPoint{id: id, at: now, bytes: g.sent}
}

// ack takes the browser's acknowledgement of mark id and returns how long
// it took, or false when the mark is too old to say.
func (g *linkGauge) ack(id uint32, now time.Time) (time.Duration, bool) {
	p := g.points[id%uint32(len(g.points))]
	if p.id != id || p.at.IsZero() {
		return 0, false
	}
	took := now.Sub(p.at)
	g.got[g.gotN%len(g.got)] = linkPoint{id: id, at: now, bytes: p.bytes}
	g.gotN++
	if g.best == 0 || took < g.best {
		g.best = took
	}
	if g.max == 0 {
		return took, true
	}
	if took <= 2*g.best+linkSlack {
		if g.rate < g.max && now.Sub(g.probed) >= linkProbeGap {
			g.rate = min(g.rate+g.rate/8, g.max)
			g.probed = now
		}
		return took, true
	}
	if now.Sub(g.cut) >= linkCutGap {
		through := g.through(now)
		if through == 0 {
			// Too little went to measure: a step down instead.
			through = g.rate * 7 / 8
		}
		g.rate = max(min(through*linkShareNum/linkShareDen, g.rate), linkRateMin)
		if g.least == 0 || g.rate < g.least {
			g.least = g.rate
		}
		g.cut, g.probed = now, now
	}
	return took, true
}

// through is what the browser acknowledged over the last linkSpan, in bits
// a second, or zero when that is too little to call a rate.
func (g *linkGauge) through(now time.Time) int {
	n := min(g.gotN, len(g.got))
	if n < 2 {
		return 0
	}
	last := g.got[(g.gotN-1)%len(g.got)]
	first := last
	for i := 2; i <= n; i++ {
		p := g.got[(g.gotN-i)%len(g.got)]
		if now.Sub(p.at) > linkSpan {
			break
		}
		first = p
	}
	span, bytes := last.at.Sub(first.at), last.bytes-first.bytes
	if span < 100*time.Millisecond || bytes < 16<<10 {
		return 0
	}
	return int(float64(bytes*8) / span.Seconds())
}

// tell returns what the helper should be told the link carries, when that
// has moved enough since the last telling to be worth it. Zero is no limit:
// the link takes all that motion costs.
func (g *linkGauge) tell() (uint32, bool) {
	if g.max == 0 {
		return 0, false
	}
	rate := g.rate
	if rate >= g.max {
		rate = 0
	}
	switch {
	case rate == g.told:
		return 0, false
	case rate != 0 && g.told > 0:
		if d := rate - g.told; d < g.told/linkTell && -d < g.told/linkTell {
			return 0, false
		}
	case rate == 0 && g.told < 0:
		// A helper told nothing assumes no limit already.
		g.told = 0
		return 0, false
	}
	g.told = rate
	return uint32(rate), true
}

// window is how many frames may be in flight: two, and more on a link whose
// round trip is long, where two would hold the frame rate to the round trip.
func (g *linkGauge) window() uint32 {
	return uint32(min(max(1+int(g.best/screenWindowPer), screenWindow), screenWindowMax))
}

func encodeRate(bits uint32) []byte {
	return binary.LittleEndian.AppendUint32(nil, bits)
}

// screenRater is a video encoder whose bitrate can follow the link.
type screenRater interface {
	SetBitrate(bits int)
}

// linkBudget is the helper's side: what the agent last said the link
// carries, and what follows from it.
type linkBudget struct {
	rate int // bits a second; 0 is no limit known
	// tile is the running size of an exactly encoded tile, in bytes.
	tile int
}

// refine is how many tiles one frame may sharpen: what the link carries in
// linkRefineFor, at the size tiles have been.
func (b *linkBudget) refine() int {
	if b.rate == 0 {
		return screenRefineMax
	}
	tile := b.tile
	if tile == 0 {
		tile = 2 << 10
	}
	bytes := int(float64(b.rate) / 8 * linkRefineFor.Seconds())
	return min(max(bytes/tile, linkRefineMin), screenRefineMax)
}

// sized notes what n exactly encoded tiles came to.
func (b *linkBudget) sized(n, bytes int) {
	if n == 0 {
		return
	}
	each := bytes / n
	if b.tile == 0 {
		b.tile = each
		return
	}
	b.tile += (each - b.tile) / 4
}
