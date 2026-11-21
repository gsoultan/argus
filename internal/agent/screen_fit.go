package agent

import (
	"slices"
	"time"
)

/*
Frames no larger than the link carries in a moment.

On a slow link a frame of changed tiles can be far more than the link
carries in a frame's time: a console listing a tree changes most of the
screen every frame, and each such frame of exact tiles took a second to
cross 1 Mbit/s. Two were in flight, so what the browser showed ran two
seconds behind, and a keystroke waited behind them.

So when the agent has said what the link carries, a frame's changed tiles
are held to what crosses it in linkFrameFor. The rest are owed: the browser
keeps what it had there, the differ remembers the hash the browser has, and
the tile is looked at again every frame until it is sent or changes back.
The oldest owed go first, so none waits for ever, after a few of the newest,
so what was just typed shows in the next frame however much is owed.
*/

const (
	// linkFrameFor is how much of the link's time one frame of changed
	// tiles may take.
	linkFrameFor = 150 * time.Millisecond
	// linkFreshTiles is how many of a frame's tiles go to what changed
	// this frame, before what is owed from earlier ones.
	linkFreshTiles = 8
)

// frame is how many changed tiles one frame may send, or 0 for all of them.
func (b *linkBudget) frame() int {
	if b.rate == 0 {
		return 0
	}
	tile := b.tile
	if tile == 0 {
		tile = 2 << 10
	}
	bytes := int(float64(b.rate) / 8 * linkFrameFor.Seconds())
	return max(bytes/tile, linkFreshTiles)
}

// fit keeps at most n of jobs (all when n is 0) and owes the rest, in the
// order to send them.
func (d *screenDiffer) fit(jobs []tileJob, n int) []tileJob {
	d.frames++
	if n == 0 || len(jobs) <= n {
		for _, j := range jobs {
			d.paid(j.index)
		}
		return jobs
	}
	if d.owed == nil {
		d.owed = make([]int, d.cols*d.rows)
		d.owedAs = make([]uint64, d.cols*d.rows)
	}
	var fresh, older []tileJob
	for _, j := range jobs {
		// New is never owed, or changed again since it was held back.
		isNew := d.owed[j.index] == 0 || d.owedAs[j.index] != j.hash
		if isNew && len(fresh) < min(linkFreshTiles, n) {
			fresh = append(fresh, j)
			continue
		}
		older = append(older, j)
	}
	// Owed longest first; what has never been owed after.
	since := func(j tileJob) int {
		if s := d.owed[j.index]; s != 0 {
			return s
		}
		return d.frames
	}
	slices.SortStableFunc(older, func(a, b tileJob) int { return since(a) - since(b) })
	keep := append(fresh, older[:n-len(fresh)]...)
	for _, j := range keep {
		d.paid(j.index)
	}
	for _, j := range older[n-len(fresh):] {
		// The browser still has what it had: the tile is found again.
		d.hashes[j.index] = j.prev
		if d.owed[j.index] == 0 {
			d.owed[j.index] = d.frames
			d.owing++
		}
		d.owedAs[j.index] = j.hash
		if d.scroll != nil {
			d.scroll.looked[j.index] = false
		}
	}
	return keep
}

// paid notes tile i sent, or the same as the browser has.
func (d *screenDiffer) paid(i int) {
	if d.owed != nil && d.owed[i] != 0 {
		d.owed[i] = 0
		d.owing--
	}
}

// owes lists the tiles owed, for find to look at whatever the capturer
// says changed.
func (d *screenDiffer) owes(look []int) []int {
	if d.owing == 0 {
		return look
	}
	in := make(map[int]bool, len(look))
	for _, i := range look {
		in[i] = true
	}
	for i, s := range d.owed {
		if s != 0 && !in[i] {
			look = append(look, i)
		}
	}
	return look
}
