package agent

import (
	"encoding/binary"
	"hash/maphash"
	"slices"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

/*
Scrolling, sent as a move.

Scrolling a page changes nearly every tile under it, though almost nothing on
the screen is new: the same pixels sit a few rows higher or lower. Sent as
changed tiles, every scrolled frame of a full-HD page was about 300 KB, a
whole screen, so a scroll ran at a frame a second on a 2 Mbit/s link; sent
as video it went soft until it was sharpened again.

So the helper looks for it. Beside each tile's hash it keeps a hash of every
row of every 64-pixel column (a band): 8 bytes a row, 260 KB for a full-HD
screen. When many tiles change, rows of the new screen are looked up among
the old screen's rows of the same band, and the shift most of them agree on
is the scroll. Every changed tile is then checked row by row against the old
screen at that shift; those that match exactly are the browser's own pixels
moved, and go as moves (rdp.EncodeCopy, recorded as RDP's ScrBlt), tile
aligned and joined into rectangles. Only what scrolled into view, and what
did not match (the pointer, a sticky header), goes as tiles.

What the browser shows is what the helper last sent, so a move reads exactly
the pixels the old rows describe. Two things keep that true: moves go before
the frame's tiles, ordered so none reads where an earlier one wrote; and none
are sent for a while after a video picture, which the browser may still be
drawing when the move arrives. A tile moved from one still waiting to be
sharpened waits too.
*/

const (
	// scrollMinTiles is the fewest changed tiles worth looking for a scroll
	// in: under it, sending them is cheap anyway.
	scrollMinTiles = 8
	// scrollMinVotes is the fewest rows that must agree on a shift.
	scrollMinVotes = 8
	// scrollAfterVideo is how long after a video picture moves wait: the
	// browser may take up to its decode wait (a second) to draw one.
	scrollAfterVideo = 1200 * time.Millisecond
	// scrollSample is every how many rows of a changed tile are looked up.
	scrollSample = 8
)

// scrollRows is the screen as the browser has it, as a hash of each row of
// each band, and the screen being looked at now.
type scrollRows struct {
	width, height, cols int
	seed                maphash.Seed
	// rows is [band*height + y]: the screen as last sent.
	rows []uint64
	// next is the same for this frame, valid in the tiles looked at.
	next   []uint64
	looked []bool
	// index is reused for looking rows up, per band.
	index map[uint64]int
}

func newScrollRows(width, height, cols, tiles int, seed maphash.Seed) *scrollRows {
	return &scrollRows{width: width, height: height, cols: cols, seed: seed,
		rows: make([]uint64, cols*height), next: make([]uint64, cols*height),
		looked: make([]bool, tiles), index: make(map[uint64]int, height)}
}

// hash hashes tile j's rows into next and returns the tile's hash, made of
// them.
func (r *scrollRows) hash(fb []byte, stride int, i int, j tileJob) uint64 {
	band := j.x / screenTile
	base := band * r.height
	for y := j.y; y < j.y+j.h; y++ {
		o := y*stride + j.x*4
		r.next[base+y] = maphash.Bytes(r.seed, fb[o:o+j.w*4])
	}
	r.looked[i] = true
	var b [8]byte
	var mh maphash.Hash
	mh.SetSeed(r.seed)
	for y := j.y; y < j.y+j.h; y++ {
		binary.LittleEndian.PutUint64(b[:], r.next[base+y])
		_, _ = mh.Write(b[:])
	}
	return mh.Sum64()
}

// cur is row y of band as it is now.
func (r *scrollRows) cur(band, y int) uint64 {
	if r.looked[(y/screenTile)*r.cols+band] {
		return r.next[band*r.height+y]
	}
	return r.rows[band*r.height+y]
}

// prev is row y of band as the browser has it.
func (r *scrollRows) prev(band, y int) uint64 { return r.rows[band*r.height+y] }

// commit makes this frame's rows the browser's.
func (r *scrollRows) commit() {
	for i, l := range r.looked {
		if !l {
			continue
		}
		band, ty := i%r.cols, i/r.cols
		from, to := band*r.height+ty*screenTile, band*r.height+min((ty+1)*screenTile, r.height)
		copy(r.rows[from:to], r.next[from:to])
		r.looked[i] = false
	}
}

// shift is the scroll most changed rows agree on: where on the old screen
// a row now at y was, as old - y. Zero is none.
func (r *scrollRows) shift(jobs []tileJob) int {
	perBand := make(map[int][]tileJob)
	for _, j := range jobs {
		perBand[j.x/screenTile] = append(perBand[j.x/screenTile], j)
	}
	votes := make(map[int]int)
	for band, js := range perBand {
		if len(js) < 2 {
			continue
		}
		clear(r.index)
		for y := range r.height {
			h := r.prev(band, y)
			if _, seen := r.index[h]; seen {
				r.index[h] = -1 // a row that repeats says nothing about where
				continue
			}
			r.index[h] = y
		}
		for _, j := range js {
			for y := j.y + 1; y < j.y+j.h; y += scrollSample {
				h := r.cur(band, y)
				if h == r.cur(band, y-1) {
					continue // flat: matches anywhere
				}
				if was, ok := r.index[h]; ok && was >= 0 && was != y {
					votes[was-y]++
				}
			}
		}
	}
	best, most := 0, 0
	for dy, n := range votes {
		if n > most || (n == most && abs(dy) < abs(best)) {
			best, most = dy, n
		}
	}
	if most < scrollMinVotes {
		return 0
	}
	return best
}

// moved says every row of tile j now is the old screen's dy rows away.
func (r *scrollRows) moved(j tileJob, dy int) bool {
	band := j.x / screenTile
	if j.y+dy < 0 || j.y+j.h+dy > r.height {
		return false
	}
	for y := j.y; y < j.y+j.h; y++ {
		if r.cur(band, y) != r.prev(band, y+dy) {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// scrolls finds the changed tiles among jobs that are the browser's own
// pixels moved up or down, and returns them as moves, in the order to draw
// them, with the tiles still to send and the shift. Nothing is found when
// scrolling is not being looked for.
func (d *screenDiffer) scrolls(jobs []tileJob) (moves []rdp.Bitmap, rest []tileJob, dy int) {
	r := d.scroll
	if r == nil || len(jobs) < scrollMinTiles {
		return nil, jobs, 0
	}
	dy = r.shift(jobs)
	if dy == 0 {
		return nil, jobs, 0
	}
	movedTile := make(map[int]bool)
	for _, j := range jobs {
		if r.moved(j, dy) {
			movedTile[j.index] = true
		} else {
			rest = append(rest, j)
		}
	}
	if len(movedTile) < scrollMinTiles/2 {
		return nil, jobs, 0
	}
	// Each band's runs of moved tiles, then runs side by side with the
	// same rows joined into one rectangle.
	type run struct{ x, y, w, h int }
	var runs []run
	open := make(map[[2]int]int) // (y, h) -> index in runs of one ending at the band before
	for band := range d.cols {
		next := make(map[[2]int]int)
		for ty := 0; ty < d.rows; {
			if !movedTile[ty*d.cols+band] {
				ty++
				continue
			}
			from := ty
			for ty < d.rows && movedTile[ty*d.cols+band] {
				ty++
			}
			x, y := band*screenTile, from*screenTile
			w, h := min(screenTile, d.width-x), min(ty*screenTile, d.height)-y
			key := [2]int{y, h}
			if k, ok := open[key]; ok && runs[k].x+runs[k].w == x {
				runs[k].w += w
				next[key] = k
				continue
			}
			runs = append(runs, run{x, y, w, h})
			next[key] = len(runs) - 1
		}
		open = next
	}
	// Moving up (dy > 0) reads below where it writes: top first, so nothing
	// is read after it was written over. Moving down, bottom first.
	slices.SortStableFunc(runs, func(a, b run) int {
		if dy > 0 {
			return a.y - b.y
		}
		return b.y - a.y
	})
	for _, m := range runs {
		moves = append(moves, rdp.Bitmap{X: m.x, Y: m.y, Width: m.w, Height: m.h,
			SrcX: m.x, SrcY: m.y + dy, Copy: true})
	}
	return moves, rest, dy
}

// moveBlur carries the sharpener's waiting tiles along with a scroll: a
// tile moved from one still soft from video is soft where it lands, and one
// moved from an exact tile is exact.
func (d *screenDiffer) moveBlur(sharp *screenSharpener, moves []rdp.Bitmap, dy int, now time.Time) {
	if sharp.blurry == nil || sharp.count == 0 {
		return
	}
	type change struct {
		i    int
		soft bool
	}
	var changes []change
	for _, m := range moves {
		for ty := m.Y / screenTile; ty*screenTile < m.Y+m.Height; ty++ {
			for tx := m.X / screenTile; tx*screenTile < m.X+m.Width; tx++ {
				top, bottom := ty*screenTile+dy, min((ty+1)*screenTile, d.height)-1+dy
				soft := false
				for sy := top / screenTile; sy <= bottom/screenTile; sy++ {
					soft = soft || !sharp.blurry[sy*d.cols+tx].IsZero()
				}
				changes = append(changes, change{ty*d.cols + tx, soft})
			}
		}
	}
	for _, c := range changes {
		if c.soft {
			sharp.mark(c.i, now)
		} else {
			sharp.clear(c.i)
		}
	}
}

// encodeMoves is moves as display frames, for the agent.
func encodeMoves(moves []rdp.Bitmap) []byte {
	out := make([]byte, 0, len(moves)*rdp.FrameHeaderSize)
	for _, m := range moves {
		out = append(out, rdp.EncodeCopy(m.X, m.Y, m.Width, m.Height, m.SrcY)...)
	}
	return out
}
