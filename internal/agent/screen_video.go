package agent

import (
	"encoding/binary"
	"errors"
	"image"
	"runtime"
	"sync"
	"time"
)

/*
Fast, then sharp: motion as H.264, exact pixels once it stops.

A frame where a lot of the screen changed (a scroll, a window dragged, a
video) is encoded whole as H.264 and only its changed parts are shown: a
few kilobytes where exact tiles would be hundreds. A frame where little
changed (typing, a menu) goes as exact tiles, as before. A tile last shown
as video and left alone for screenSettle is sent again as exact pixels, so
text is sharp a moment after it stops moving, live and in the recording.

The H.264 travels as the graphics pipeline's commands (rdp.BuildAVC420),
which the console already decodes in hardware for Remote Desktop, and is
recorded as them.
*/

// screenVideo encodes whole screens as H.264 for motion.
type screenVideo interface {
	// Encode compresses the screen to Annex B. key asks for a key frame;
	// whether one came out is read from the stream, not trusted.
	Encode(s screenShot, key bool) ([]byte, error)
	Close()
}

const (
	// screenVideoTiles changed tiles in one frame make it motion.
	screenVideoTiles = 12
	// screenVideoLinger keeps smaller changes on video for a moment after
	// motion, so the tail of a drag does not flip between the two.
	screenVideoLinger = 150 * time.Millisecond
	// screenSettle is how long a tile shown as video stays unchanged before
	// it is sent again exactly. A quarter of a second left text soft for
	// long enough to notice after every scroll; an eighth is past the
	// pause between a scroll's steps and still reads as instant.
	screenSettle = 120 * time.Millisecond
	// screenRefineMax bounds how many tiles one frame re-sends exactly. A
	// full HD screen is 510 tiles, so it sharpens in one frame rather than
	// four, where each quarter came in with the next credit.
	screenRefineMax = 512
	// screenMaxRegions is how many rectangles a picture names before one
	// around them all is cheaper.
	screenMaxRegions = 128
)

// The helper's video messages.
const (
	// scrVideo is one H.264 picture: flags (1 = key frame), a byte unused,
	// width, height and the number of regions as uint16 LE, the regions as
	// left, top, right, bottom uint16 LE each, then the stream.
	scrVideo byte = 5
	// scrCodec says what motion is sent as: "h264", or "exact: " and why
	// not.
	scrCodec byte = 6
)

// encodeVideoMsg lays out a scrVideo message.
func encodeVideoMsg(key bool, width, height int, regions []image.Rectangle, h264 []byte) []byte {
	p := make([]byte, 0, 8+len(regions)*8+len(h264))
	var flags byte
	if key {
		flags = 1
	}
	p = append(p, flags, 0)
	p = binary.LittleEndian.AppendUint16(p, uint16(width))
	p = binary.LittleEndian.AppendUint16(p, uint16(height))
	p = binary.LittleEndian.AppendUint16(p, uint16(len(regions)))
	for _, r := range regions {
		p = binary.LittleEndian.AppendUint16(p, uint16(r.Min.X))
		p = binary.LittleEndian.AppendUint16(p, uint16(r.Min.Y))
		p = binary.LittleEndian.AppendUint16(p, uint16(r.Max.X))
		p = binary.LittleEndian.AppendUint16(p, uint16(r.Max.Y))
	}
	return append(p, h264...)
}

// videoMsg is a scrVideo message read back.
type videoMsg struct {
	key           bool
	width, height int
	regions       []image.Rectangle
	h264          []byte
}

func parseVideoMsg(p []byte) (videoMsg, error) {
	if len(p) < 8 {
		return videoMsg{}, errors.New("the screen helper sent a truncated picture")
	}
	m := videoMsg{key: p[0]&1 != 0,
		width:  int(binary.LittleEndian.Uint16(p[2:4])),
		height: int(binary.LittleEndian.Uint16(p[4:6]))}
	n := int(binary.LittleEndian.Uint16(p[6:8]))
	if len(p) < 8+n*8 || m.width == 0 || m.height == 0 {
		return videoMsg{}, errors.New("the screen helper sent a truncated picture")
	}
	bounds := image.Rect(0, 0, m.width, m.height)
	for i := range n {
		o := 8 + i*8
		r := image.Rect(int(binary.LittleEndian.Uint16(p[o:])), int(binary.LittleEndian.Uint16(p[o+2:])),
			int(binary.LittleEndian.Uint16(p[o+4:])), int(binary.LittleEndian.Uint16(p[o+6:])))
		if !r.In(bounds) || r.Empty() {
			return videoMsg{}, errors.New("the screen helper sent a picture region off the screen")
		}
		m.regions = append(m.regions, r)
	}
	m.h264 = p[8+n*8:]
	return m, nil
}

// h264HasIDR reports whether an Annex B stream holds an IDR slice: a key
// frame, which a decoder can start on.
func h264HasIDR(b []byte) bool { return h264HasNAL(b, 5) }

// h264HasNAL reports whether an Annex B stream holds a NAL unit of kind
// typ (5 an IDR slice, 7 a sequence parameter set).
func h264HasNAL(b []byte, typ byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		at := -1
		if b[i+2] == 1 {
			at = i + 3
		} else if b[i+2] == 0 && b[i+3] == 1 {
			at = i + 4
		}
		if at < 0 || at >= len(b) {
			continue
		}
		if b[at]&0x1F == typ {
			return true
		}
		i = at - 1
	}
	return false
}

// tileRegions turns tile indexes, in order, into rectangles: runs along a
// row, joined with the run below when they line up. Past screenMaxRegions,
// one rectangle around them all.
func tileRegions(cols, width, height int, tiles []int) []image.Rectangle {
	var runs []image.Rectangle
	for i := 0; i < len(tiles); {
		j := i
		for j+1 < len(tiles) && tiles[j+1] == tiles[j]+1 && tiles[j+1]%cols != 0 {
			j++
		}
		a, b := tiles[i], tiles[j]
		r := image.Rect((a%cols)*screenTile, (a/cols)*screenTile,
			min((b%cols+1)*screenTile, width), min((a/cols+1)*screenTile, height))
		runs = append(runs, r)
		i = j + 1
	}
	// Join a run onto one directly above it with the same columns.
	var out []image.Rectangle
	for _, r := range runs {
		joined := false
		for k := len(out) - 1; k >= 0; k-- {
			o := out[k]
			if o.Max.Y < r.Min.Y {
				break
			}
			if o.Min.X == r.Min.X && o.Max.X == r.Max.X && o.Max.Y == r.Min.Y {
				out[k].Max.Y = r.Max.Y
				joined = true
				break
			}
		}
		if !joined {
			out = append(out, r)
		}
	}
	if len(out) > screenMaxRegions {
		u := out[0]
		for _, r := range out[1:] {
			u = u.Union(r)
		}
		return []image.Rectangle{u}
	}
	return out
}

// screenSharpener remembers which tiles were last shown as video, and when,
// so they are sent again exactly once they settle.
type screenSharpener struct {
	blurry []time.Time
	count  int
}

func (s *screenSharpener) reset(tiles int) {
	s.blurry = make([]time.Time, tiles)
	s.count = 0
}

func (s *screenSharpener) mark(i int, at time.Time) {
	if s.blurry[i].IsZero() {
		s.count++
	}
	s.blurry[i] = at
}

func (s *screenSharpener) clear(i int) {
	if !s.blurry[i].IsZero() {
		s.count--
		s.blurry[i] = time.Time{}
	}
}

// due takes the tiles that have settled by now, at most limit, skipping
// those in busy, and clears them.
func (s *screenSharpener) due(now time.Time, busy map[int]bool, limit int) []int {
	if s.count == 0 {
		return nil
	}
	var out []int
	for i, t := range s.blurry {
		if len(out) == limit {
			break
		}
		if t.IsZero() || now.Sub(t) < screenSettle || busy[i] {
			continue
		}
		out = append(out, i)
		s.clear(i)
	}
	return out
}

// next is when the next tile settles; zero when none is waiting.
func (s *screenSharpener) next() time.Time {
	if s.count == 0 {
		return time.Time{}
	}
	var first time.Time
	for _, t := range s.blurry {
		if !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first.Add(screenSettle)
}

// bgrxToNV12 converts a top-down BGRX screen to NV12 (BT.601, limited
// range), the form hardware and Microsoft's encoder take: a plane of luma,
// then one of interleaved chroma at half the size each way. width and
// height are even. Spread over a few cores: a full-HD screen is two
// million pixels.
func bgrxToNV12(dst, fb []byte, stride, width, height int) {
	ySize := width * height
	luma, chroma := dst[:ySize], dst[ySize:ySize+ySize/2]
	pairs := height / 2
	workers := min(runtime.GOMAXPROCS(0), screenEncoders, max(1, pairs/64))
	var wg sync.WaitGroup
	for w := range workers {
		from, to := pairs*w/workers, pairs*(w+1)/workers
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := from; p < to; p++ {
				nv12Rows(luma, chroma, fb, stride, width, p)
			}
		}()
	}
	wg.Wait()
}

// nv12Rows converts the two rows of pair p, and their one row of chroma.
func nv12Rows(luma, chroma, fb []byte, stride, width, p int) {
	r0, r1 := fb[2*p*stride:], fb[(2*p+1)*stride:]
	y0, y1 := luma[2*p*width:], luma[(2*p+1)*width:]
	c := chroma[p*width:]
	for x := 0; x < width; x += 2 {
		var rs, gs, bs int
		for k, row := range [2][]byte{r0, r1} {
			ys := y0
			if k == 1 {
				ys = y1
			}
			for d := range 2 {
				o := (x + d) * 4
				b, g, r := int(row[o]), int(row[o+1]), int(row[o+2])
				ys[x+d] = byte((66*r+129*g+25*b+128)>>8 + 16)
				rs, gs, bs = rs+r, gs+g, bs+b
			}
		}
		r, g, b := rs/4, gs/4, bs/4
		c[x] = byte((-38*r-74*g+112*b+128)>>8 + 128)
		c[x+1] = byte((112*r-94*g-18*b+128)>>8 + 128)
	}
}
