package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"image"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Screen sharing: a Windows desktop without Remote Desktop.

	browser ──wss──▶ argus-control ──"screen" stream──▶ agent ──pipes──▶ helper
	                                                                     (console session)

For a host where Remote Desktop is off, or is Windows Home and has none, the
agent shows the console itself: whatever is on the physical screen, the
sign-in screen included, and drives it with the keyboard and mouse as a
person sitting there would.

A service runs in session 0 and cannot see the console, so the agent starts
a helper -- itself, as "screen-helper" -- in the console session, as
LocalSystem, which is what lets it follow the screen onto the sign-in and
UAC desktops. The helper captures, finds the tiles that changed and encodes
them as RDP bitmaps; it talks to nothing but the two pipes the agent gave it.
The agent records those bitmaps as Fast-Path updates, the same PDUs a Remote
Desktop server sends, before passing them on: the recording, its chain, the
replay and the live view are the ones Remote Desktop already has.

Whoever is signed in at the console is whoever the session acts as, which is
the choice this mode makes. The agent therefore records which account that is
when the session opens, and again every time it changes, in the recording's
chain, so a reconstruction knows whose desktop was driven.

If the console's session changes underneath (a sign-out ends it and Windows
starts another for the sign-in screen) the agent starts a new helper in the
new one and carries on.
*/

// ScreenService is the name the control plane asks for. A grant for it
// cannot be spent on Remote Desktop, nor the reverse.
const ScreenService = "screen"

// screenProtocol is what the recording's header calls a captured session.
const screenProtocol = "screen"

// screenTile is the side of the squares the screen is compared in.
const screenTile = 64

// screenMinGap is the least time between frames: sixty a second at most.
// There is no clock beyond that: a frame is captured when the screen changes
// and the agent has room for it.
const screenMinGap = time.Second / 60

// screenIdleMax is the longest a polling capture (GDI) waits between looks
// at a screen that is not changing. Input cuts it short.
const screenIdleMax = 250 * time.Millisecond

// screenEncoders is how many cores a full-screen change is encoded on.
const screenEncoders = 4

// screenWindow is how many frames may be on their way to the browser, not
// yet drawn, before the agent stops asking for more: enough to keep a link
// busy, few enough that what is shown is never far behind.
const screenWindow = 2

// screenAckFloor keeps the screen, and so the recording, moving at least
// once a second when a browser stops acknowledging frames.
const screenAckFloor = time.Second

// The helper's pipes carry messages: a type byte, a four-byte length and the
// payload.
const (
	// helper → agent
	scrSize  byte = 1 // width, height as uint16 LE
	scrRects byte = 2 // rdp.EncodeBitmap frames, back to back
	scrFail  byte = 3 // why the helper stopped, as text
	scrFrame byte = 4 // the frame's rectangles are all sent

	// agent → helper
	scrInput   byte = 10 // one rdp.InputEvent as JSON
	scrRefresh byte = 11 // send every tile next time
	scrNext    byte = 12 // room for one more frame
)

// maxScreenMsg bounds one message from the helper. A full 4K screen of
// incompressible tiles is about 25 MB; the helper splits what it sends well
// below that, and anything larger is not from the helper.
const maxScreenMsg = 4 << 20

func writeScreenMsg(w io.Writer, typ byte, p []byte) error {
	var h [5]byte
	h[0] = typ
	binary.LittleEndian.PutUint32(h[1:], uint32(len(p)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func readScreenMsg(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(h[1:])
	if n > maxScreenMsg {
		return 0, nil, fmt.Errorf("screen helper sent a %d-byte message", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

/* ── The helper ──────────────────────────────────────────────────────────── */

// screenShot is the screen as one capture saw it.
type screenShot struct {
	// fb is the whole screen, top-down 32-bit BGRX, stride bytes a row. It
	// may be reused by the next capture.
	fb            []byte
	stride        int
	width, height int
	// dirty is where the screen may have changed since the last shot, when
	// the capturer knows (Desktop Duplication does). Nil means anywhere.
	dirty []image.Rectangle
}

// screenCapturer is the console's screen, as the helper sees it. Every call
// is made from the one OS thread the helper locks for capture, which is what
// Windows needs to keep that thread on the input desktop.
type screenCapturer interface {
	// Capture waits up to wait for the screen to change and returns it;
	// changed is false when nothing did. A capturer that cannot wait (GDI)
	// returns at once, changed, and the comparison finds what moved.
	Capture(wait time.Duration) (shot screenShot, changed bool, err error)
	Close()
}

// screenInputer plays input to the console. It is made and used on an OS
// thread of its own, so a keystroke never waits behind a capture.
type screenInputer interface {
	Input(rdp.InputEvent) error
	Close()
}

// screenPace is how fast the helper captures.
type screenPace struct {
	// minGap is the least time between two frames: 60 a second at most.
	minGap time.Duration
	// idleMax is the longest a polling capturer waits between looks at a
	// screen that is not changing. Input resets it.
	idleMax time.Duration
}

var defaultScreenPace = screenPace{minGap: screenMinGap, idleMax: screenIdleMax}

// screenDiffer finds the tiles of a frame that differ from the last one it
// sent. It keeps a 64-bit hash of each tile rather than a copy of the
// screen: 16 KB for a 4K display instead of 33 MB.
type screenDiffer struct {
	width, height int
	cols, rows    int
	hashes        []uint64
	seed          maphash.Seed
	// scratch is one encoding buffer per worker.
	scratch [][]byte
}

// tileJob is one tile to encode, and what it became.
type tileJob struct {
	index      int
	x, y, w, h int
	hash       uint64
	frame      []byte
}

// changed returns the changed tiles as display frames (rdp.EncodeBitmap),
// grouped into batches under limit bytes. force sends every tile.
func (d *screenDiffer) changed(s screenShot, force bool, limit int) [][]byte {
	return d.batches(s, d.find(s, force), limit)
}

// find returns the tiles that changed since they were last found, and
// remembers them as they are now. force returns every tile, as does a
// screen of a new size.
func (d *screenDiffer) find(s screenShot, force bool) []tileJob {
	if s.width != d.width || s.height != d.height || d.hashes == nil {
		d.width, d.height = s.width, s.height
		d.cols = (s.width + screenTile - 1) / screenTile
		d.rows = (s.height + screenTile - 1) / screenTile
		d.hashes = make([]uint64, d.cols*d.rows)
		d.seed = maphash.MakeSeed()
		force = true
	}
	look := d.tilesToLook(s.dirty, force)
	var jobs []tileJob
	for _, i := range look {
		j := d.job(s, i)
		h := tileHash(d.seed, s.fb, s.stride, j.x, j.y, j.w, j.h)
		if !force && h == d.hashes[i] {
			continue
		}
		d.hashes[i] = h
		j.hash = h
		jobs = append(jobs, j)
	}
	return jobs
}

// job is tile i of s, to encode.
func (d *screenDiffer) job(s screenShot, i int) tileJob {
	tx, ty := (i%d.cols)*screenTile, (i/d.cols)*screenTile
	return tileJob{index: i, x: tx, y: ty, w: min(screenTile, s.width-tx), h: min(screenTile, s.height-ty)}
}

// batches encodes jobs as display frames, grouped under limit bytes.
func (d *screenDiffer) batches(s screenShot, jobs []tileJob, limit int) [][]byte {
	d.encode(s, jobs)
	var batches [][]byte
	batch := []byte(nil)
	for _, j := range jobs {
		if len(batch)+len(j.frame) > limit && len(batch) > 0 {
			batches = append(batches, batch)
			batch = nil
		}
		batch = append(batch, j.frame...)
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

// tilesToLook lists the tiles a shot's dirty rectangles touch, in order;
// all of them when it has none or force is set.
func (d *screenDiffer) tilesToLook(dirty []image.Rectangle, force bool) []int {
	n := d.cols * d.rows
	if force || dirty == nil {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all
	}
	mark := make([]bool, n)
	bounds := image.Rect(0, 0, d.width, d.height)
	for _, r := range dirty {
		r = r.Intersect(bounds)
		if r.Empty() {
			continue
		}
		for ty := r.Min.Y / screenTile; ty <= (r.Max.Y-1)/screenTile; ty++ {
			for tx := r.Min.X / screenTile; tx <= (r.Max.X-1)/screenTile; tx++ {
				mark[ty*d.cols+tx] = true
			}
		}
	}
	var out []int
	for i, m := range mark {
		if m {
			out = append(out, i)
		}
	}
	return out
}

// encode compresses the jobs' tiles, spread over a few cores when there are
// many: a whole new screen is then encoded in a quarter of the time, and a
// keystroke's few tiles are not worth a goroutine.
func (d *screenDiffer) encode(s screenShot, jobs []tileJob) {
	workers := 1
	if len(jobs) >= 32 {
		workers = min(runtime.GOMAXPROCS(0), screenEncoders, len(jobs)/16)
	}
	for len(d.scratch) < workers {
		d.scratch = append(d.scratch, nil)
	}
	if workers <= 1 {
		for i := range jobs {
			jobs[i].frame = d.encodeOne(0, s, jobs[i])
		}
		return
	}
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(jobs); i += workers {
				jobs[i].frame = d.encodeOne(w, s, jobs[i])
			}
		}()
	}
	wg.Wait()
}

func (d *screenDiffer) encodeOne(worker int, s screenShot, j tileJob) []byte {
	b := rdp.CompressRect(d.scratch[worker], s.fb, s.stride, j.x, j.y, j.w, j.h)
	d.scratch[worker] = b.Data[:0]
	return rdp.EncodeBitmap(b)
}

// tileHash hashes one tile's pixels, row by row.
func tileHash(seed maphash.Seed, fb []byte, stride, x, y, w, h int) uint64 {
	var mh maphash.Hash
	mh.SetSeed(seed)
	for r := y; r < y+h; r++ {
		o := r*stride + x*4
		_, _ = mh.Write(fb[o : o+w*4])
	}
	return mh.Sum64()
}

// serveScreen is the helper's loop: input from in is played to the console,
// and what changes on screen is written to out, until in closes.
//
// Three threads, so nothing waits behind anything else: one reads the agent,
// one plays input (and wakes capture, so a keystroke's answer is looked for
// at once), and this one captures. Capture sends a frame only when the agent
// has given it a credit (scrNext): the agent hands them out as the browser
// takes frames, so a slow link gets fewer, newer frames instead of a queue of
// old ones, and the screen a person sees is the one that is there now.
func serveScreen(c screenCapturer, newInput func() screenInputer, in io.Reader, out io.Writer,
	pace screenPace, newVideo func(width, height int) (screenVideo, error)) error {
	runtime.LockOSThread()
	defer c.Close()

	inputs := make(chan rdp.InputEvent, 256)
	refresh := make(chan struct{}, 1)
	credit := make(chan struct{}, 64)
	poke := make(chan struct{}, 1)
	done := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)

	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	go func() {
		for {
			typ, p, err := readScreenMsg(in)
			if err != nil {
				done <- err
				return
			}
			switch typ {
			case scrInput:
				var e rdp.InputEvent
				if json.Unmarshal(p, &e) == nil {
					select {
					case inputs <- e:
					case <-stop:
						return
					}
				}
			case scrRefresh:
				signal(refresh)
			case scrNext:
				signal(credit)
			}
		}
	}()

	go func() {
		// Its own OS thread, for its own desktop: SendInput goes to the
		// desktop the calling thread is on.
		runtime.LockOSThread()
		player := newInput()
		defer player.Close()
		var held *rdp.InputEvent
		for {
			var e rdp.InputEvent
			if held != nil {
				e, held = *held, nil
			} else {
				select {
				case e = <-inputs:
				case <-stop:
					return
				}
			}
			// Mouse moves that have queued up behind each other are one
			// move, to where the pointer is now.
			for e.Kind == "mouse" {
				select {
				case next := <-inputs:
					if next.Kind == "mouse" {
						e = next
						continue
					}
					held = &next
				default:
				}
				break
			}
			_ = player.Input(e)
			signal(poke)
		}
	}()

	var diff screenDiffer
	var sharp screenSharpener
	mo := motion{newVideo: newVideo, out: out}
	defer mo.close()
	credits := 1
	force := true
	lastW, lastH := 0, 0
	lastSent := time.Time{}
	idle := pace.minGap
	// last is the whole screen as last captured. The capturer reuses its
	// buffer, which always holds the screen as it is now; a capture that
	// found nothing new returns no shot, and tiles are sharpened from this.
	var last screenShot
	for {
		// A credit, first: the agent has room for another frame.
		for credits == 0 {
			select {
			case err := <-done:
				return endOf(err)
			case <-credit:
				credits++
			case <-refresh:
				force = true
			}
		}
		// No faster than the frame rate allows.
		if wait := pace.minGap - time.Since(lastSent); wait > 0 {
			if err := sleepOr(wait, done); err != nil {
				return endOf(err)
			}
		}
		select {
		case err := <-done:
			return endOf(err)
		case <-refresh:
			force = true
		default:
		}

		// Wait no longer than the next tile takes to settle.
		wait := idle
		if at := sharp.next(); !at.IsZero() {
			wait = max(0, min(wait, time.Until(at)))
		}
		started := time.Now()
		shot, changed, err := c.Capture(wait)
		if err != nil {
			// The input desktop switching (a sign-in, a UAC prompt) fails
			// a capture; the next one is on the new desktop.
			if err := sleepOr(pace.minGap, done); err != nil {
				return endOf(err)
			}
			continue
		}
		if changed {
			last = shot
		}
		var jobs []tileJob
		repaint := false
		if (changed || force) && last.fb != nil {
			if last.width != lastW || last.height != lastH {
				var p [4]byte
				binary.LittleEndian.PutUint16(p[0:], uint16(last.width))
				binary.LittleEndian.PutUint16(p[2:], uint16(last.height))
				if err := writeScreenMsg(out, scrSize, p[:]); err != nil {
					return err
				}
				lastW, lastH, force = last.width, last.height, true
			}
			repaint = force
			jobs = diff.find(last, force)
			force = false
		}
		if repaint {
			// Everything goes exactly; and a viewer who arrived late (what
			// a refresh is for) needs a key frame before the next picture.
			sharp.reset(diff.cols * diff.rows)
			mo.needKey = true
		}

		now := time.Now()
		sent := false
		if !repaint && mo.wants(len(jobs), now) {
			ok, err := mo.send(last, &diff, &sharp, jobs, now)
			if err != nil {
				return err
			}
			if ok {
				// The changed tiles went as video; they are sharpened later.
				jobs, sent = nil, true
			}
		}
		// Tiles that settled after video go again, exactly.
		busy := make(map[int]bool, len(jobs))
		for _, j := range jobs {
			busy[j.index] = true
			if sharp.blurry != nil {
				sharp.clear(j.index)
			}
		}
		if last.fb != nil {
			for _, i := range sharp.due(now, busy, screenRefineMax) {
				jobs = append(jobs, diff.job(last, i))
			}
		}
		batches := diff.batches(last, jobs, desktopBatch)
		if len(batches) == 0 && !sent {
			// Nothing new. A capturer that waited has already spent the
			// time; one that looked and returned (GDI) waits here, a
			// little longer each time the screen stays still, and not at
			// all once input arrives.
			rest := idle - time.Since(started)
			if at := sharp.next(); !at.IsZero() {
				rest = min(rest, time.Until(at))
			}
			if rest > 0 {
				select {
				case err := <-done:
					return endOf(err)
				case <-poke:
					idle = pace.minGap
				case <-refresh:
					force = true
				case <-time.After(rest):
					idle = min(idle*2, pace.idleMax)
				}
			} else {
				idle = min(idle*2, pace.idleMax)
			}
			continue
		}
		for _, b := range batches {
			if err := writeScreenMsg(out, scrRects, b); err != nil {
				return err
			}
		}
		if err := writeScreenMsg(out, scrFrame, nil); err != nil {
			return err
		}
		// The encoder is made once the first frame is out, not when motion
		// first comes, so the agent hears at once what motion will go as,
		// and an encoder that cannot be had is found before it is needed.
		if mo.newVideo != nil && !mo.off && (mo.video == nil || mo.w != lastW || mo.h != lastH) {
			if err := mo.open(lastW, lastH); err != nil {
				return err
			}
		}
		credits--
		lastSent = time.Now()
		idle = pace.minGap
	}
}

// motion sends the frames where much of the screen changed as H.264.
type motion struct {
	newVideo func(width, height int) (screenVideo, error)
	out      io.Writer
	video    screenVideo
	w, h     int
	// off is set once video cannot be had; why was sent to the agent.
	off     bool
	needKey bool
	last    time.Time
}

// wants says whether a frame of changed tiles is motion.
func (m *motion) wants(changed int, now time.Time) bool {
	if m.newVideo == nil || m.off || changed == 0 {
		return false
	}
	return changed >= screenVideoTiles || now.Sub(m.last) < screenVideoLinger
}

// send encodes the screen and writes it as a picture showing the changed
// tiles. It reports false, having written nothing, when the frame should go
// as exact tiles instead: no encoder, or no key frame where one is needed.
func (m *motion) send(s screenShot, d *screenDiffer, sharp *screenSharpener, jobs []tileJob,
	now time.Time) (bool, error) {
	if m.video == nil || m.w != s.width || m.h != s.height {
		if err := m.open(s.width, s.height); err != nil {
			return false, err
		}
		if m.off {
			return false, nil
		}
	}
	h264, err := m.video.Encode(s, m.needKey)
	if err != nil {
		return false, m.disable("the H.264 encoder failed: " + err.Error())
	}
	key := h264HasIDR(h264)
	if len(h264) == 0 || (m.needKey && !key) {
		// Nothing a viewer can start on; these tiles go exactly, and the
		// next picture asks again.
		return false, nil
	}
	var regions []image.Rectangle
	if key {
		// A key frame starts the picture afresh, so all of it is shown,
		// and every tile is sharpened once it settles.
		regions = []image.Rectangle{image.Rect(0, 0, s.width, s.height)}
		for i := range sharp.blurry {
			sharp.mark(i, now)
		}
	} else {
		tiles := make([]int, len(jobs))
		for i, j := range jobs {
			tiles[i] = j.index
			sharp.mark(j.index, now)
		}
		regions = tileRegions(d.cols, s.width, s.height, tiles)
	}
	if err := writeScreenMsg(m.out, scrVideo, encodeVideoMsg(key, s.width, s.height, regions, h264)); err != nil {
		return false, err
	}
	m.needKey, m.last = false, now
	return true, nil
}

// open makes an encoder for a screen of width by height.
func (m *motion) open(width, height int) error {
	if m.video != nil {
		m.video.Close()
		m.video = nil
	}
	if width%2 != 0 || height%2 != 0 {
		return m.disable("the screen's size is odd, which H.264 does not encode")
	}
	v, err := m.newVideo(width, height)
	if err != nil {
		return m.disable(err.Error())
	}
	m.video, m.w, m.h, m.needKey = v, width, height, true
	return writeScreenMsg(m.out, scrCodec, []byte("h264"))
}

// disable turns video off for the helper's life and tells the agent why.
func (m *motion) disable(why string) error {
	m.close()
	m.off = true
	return writeScreenMsg(m.out, scrCodec, []byte("exact: "+why))
}

func (m *motion) close() {
	if m.video != nil {
		m.video.Close()
		m.video = nil
	}
}

func endOf(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// sleepOr sleeps for d unless the agent's pipe ends first.
func sleepOr(d time.Duration, done chan error) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-done:
		// Put back for the loop's own check.
		done <- err
		return err
	case <-t.C:
		return nil
	}
}

/* ── The agent's side ────────────────────────────────────────────────────── */

// screenHost is what the agent needs from Windows to share the console.
type screenHost interface {
	// ConsoleSession is the session attached to the physical console.
	ConsoleSession() (uint32, bool)
	// ConsoleAccount is who is signed in to that session, as DOMAIN\name;
	// "" for the sign-in screen.
	ConsoleAccount(session uint32) string
	// StartHelper runs the helper in the session, its stdin and stdout the
	// returned pipe. Closing the pipe ends the helper.
	StartHelper(session uint32, mode helperMode) (io.ReadWriteCloser, error)
	// SecureAttention is Ctrl+Alt+Del, which no keystroke can send.
	SecureAttention() error
}

// helperMode is how a helper is to capture and send the screen.
type helperMode struct {
	// gdiOnly keeps it to GDI capture and exact tiles, the paths with
	// nothing new in them, for a helper started after one failed.
	gdiOnly bool
	// video lets it send motion as H.264, for a browser that decodes it.
	video bool
}

// screenGraphicsChannel is the MCS channel a screen session's pictures are
// recorded on, as a Remote Desktop server's graphics pipeline would be.
const screenGraphicsChannel = 1004

// screenSurface is the one surface a screen session draws pictures on.
const screenSurface = 1

// screenMsg is one message from the helper, or how its pipe ended.
type screenMsg struct {
	typ byte
	p   []byte
	err error
	// gen is which helper sent it: one from a helper already replaced is
	// dropped.
	gen int
}

// screenClient is a shared console as a DesktopClient.
type screenClient struct {
	host screenHost
	log  *slog.Logger
	// note records an observation in the session's recording.
	note func(v any) error

	msgs chan screenMsg
	// done is closed with the client, so a helper's reader never blocks on
	// msgs after nobody reads it.
	done chan struct{}

	// video is set when the browser decodes H.264; gfx then takes the
	// pictures for the browser, and gtap turns them into what is recorded.
	video   bool
	gfx     *GraphicsSink
	gtap    *rdp.GraphicsTap
	frameID uint32
	codec   string
	// pictures and keys count what went as H.264, for the log at the end;
	// they and codec are held by mu.
	pictures, keys int

	mu            sync.Mutex
	helper        io.ReadWriteCloser
	gen           int
	session       uint32
	account       string
	width, height int
	tap           func([]byte)
	closed        bool
	restarts      []time.Time
	// failed counts helpers that stopped on their own; after one has, the
	// next are kept to GDI capture, the path with nothing new in it.
	failed int

	// ctrl and alt held, so Ctrl+Alt+Del can become the secure attention
	// sequence it means.
	ctrl, alt bool

	checkEvery time.Duration
	// nextCheck is when follow runs next; NextBitmaps' goroutine only.
	nextCheck time.Time

	// Frame flow. The helper sends a frame only on a credit; the agent gives
	// one when the browser has room, which it says by acknowledging the
	// marks written after each frame. A browser that never acknowledges
	// (an older console) is given a frame whenever the last one is written.
	holding  bool // the helper has a credit it has not used
	acks     bool // the browser acknowledges frames
	marked   uint32
	acked    uint32
	granted  time.Time
	mark     uint32 // a frame ended and its mark is not yet written
	markedAt [8]time.Time
	rtt      time.Duration
}

// openScreen starts the helper in the console's session and waits for it
// to say how large the screen is.
func openScreen(host screenHost, video bool, log *slog.Logger) (*screenClient, error) {
	session, ok := host.ConsoleSession()
	if !ok {
		return nil, errors.New("this host has no console session to share; " +
			"it may be starting or shutting down")
	}
	c := &screenClient{host: host, log: log, msgs: make(chan screenMsg, 8),
		done: make(chan struct{}), checkEvery: time.Second, video: video}
	if video {
		c.gfx, c.gtap = &GraphicsSink{}, rdp.NewGraphicsTap(screenGraphicsChannel)
	}
	c.account = host.ConsoleAccount(session)
	if err := c.start(session); err != nil {
		return nil, fmt.Errorf("could not start sharing the screen: %w", err)
	}
	timeout := time.After(15 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			if m.err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("the screen helper stopped before showing the screen: %v", m.err)
			}
			if m.typ == scrFail {
				_ = c.Close()
				return nil, fmt.Errorf("could not capture the screen: %s", m.p)
			}
			if m.typ == scrSize && len(m.p) == 4 {
				c.width = int(binary.LittleEndian.Uint16(m.p[0:]))
				c.height = int(binary.LittleEndian.Uint16(m.p[2:]))
				// The first tiles follow; NextBitmaps reads them.
				return c, nil
			}
		case <-timeout:
			_ = c.Close()
			return nil, errors.New("the screen helper did not show the screen within 15 seconds")
		}
	}
}

// start runs a helper in session and reads it until its pipe ends.
func (c *screenClient) start(session uint32) error {
	c.mu.Lock()
	failed := c.failed > 0
	c.mu.Unlock()
	h, err := c.host.StartHelper(session, helperMode{gdiOnly: failed, video: c.video && !failed})
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = h.Close()
		return errors.New("closed")
	}
	if c.helper != nil {
		_ = c.helper.Close()
	}
	c.gen++
	gen := c.gen
	c.helper, c.session = h, session
	// A new helper starts with one credit of its own.
	c.holding, c.granted = true, time.Now()
	c.mu.Unlock()
	go func() {
		for {
			typ, p, err := readScreenMsg(h)
			select {
			case c.msgs <- screenMsg{typ: typ, p: p, err: err, gen: gen}:
			case <-c.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

// Account is who was signed in at the console when it was last checked.
func (c *screenClient) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.account
}

func (c *screenClient) NextBitmaps() ([]rdp.Bitmap, error) {
	// Checked on a schedule of its own, not from each call: a busy screen
	// returns from here many times a second, and a timer started per call
	// would never fire.
	if c.nextCheck.IsZero() {
		c.nextCheck = time.Now().Add(c.checkEvery)
	}
	check := time.NewTimer(time.Until(c.nextCheck))
	defer check.Stop()
	for {
		select {
		case m := <-c.msgs:
			c.mu.Lock()
			current, closed := m.gen == c.gen, c.closed
			c.mu.Unlock()
			if closed {
				return nil, errors.New("closed")
			}
			if !current {
				continue
			}
			if m.err != nil {
				if err := c.restart("the screen helper stopped: " + m.err.Error()); err != nil {
					return nil, err
				}
				continue
			}
			switch m.typ {
			case scrFail:
				if err := c.restart("the screen helper failed: " + string(m.p)); err != nil {
					return nil, err
				}
			case scrSize:
				if len(m.p) == 4 {
					w := int(binary.LittleEndian.Uint16(m.p[0:]))
					h := int(binary.LittleEndian.Uint16(m.p[2:]))
					c.mu.Lock()
					c.width, c.height = w, h
					c.mu.Unlock()
					c.record(map[string]any{"type": "resize", "width": w, "height": h})
					return nil, nil
				}
			case scrRects:
				return c.rects(m.p)
			case scrVideo:
				if err := c.picture(m.p); err != nil {
					return nil, err
				}
				return nil, nil
			case scrCodec:
				c.mu.Lock()
				was := c.codec
				c.codec = string(m.p)
				c.mu.Unlock()
				if codec := string(m.p); codec != was {
					c.log.Info("tunnel: screen sharing sends motion as", "codec", codec)
					c.record(map[string]any{"type": "screen-codec", "codec": codec})
				}
			case scrFrame:
				c.mu.Lock()
				c.holding = false
				c.marked++
				c.mark = c.marked
				c.grantLocked(false)
				c.mu.Unlock()
				return nil, nil
			}
		case <-check.C:
			c.nextCheck = time.Now().Add(c.checkEvery)
			if err := c.follow(); err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.grantLocked(true)
			c.mu.Unlock()
			check.Reset(c.checkEvery)
		}
	}
}

// rects reads the helper's display frames back into bitmaps, recording them
// as the Fast-Path PDUs a Remote Desktop server would have sent.
func (c *screenClient) rects(p []byte) ([]rdp.Bitmap, error) {
	var out []rdp.Bitmap
	for len(p) >= rdp.FrameHeaderSize {
		if p[0] != rdp.FrameEncoded {
			return nil, errors.New("the screen helper sent something other than a bitmap")
		}
		n := int(binary.LittleEndian.Uint16(p[10:12]))
		size := rdp.FrameHeaderSize + (n+3)&^3
		if size > len(p) {
			return nil, errors.New("the screen helper sent a truncated bitmap")
		}
		b := rdp.Bitmap{
			X: int(binary.LittleEndian.Uint16(p[2:4])), Y: int(binary.LittleEndian.Uint16(p[4:6])),
			Width: int(binary.LittleEndian.Uint16(p[6:8])), Height: int(binary.LittleEndian.Uint16(p[8:10])),
			BPP: int(p[1] & 0x7F), Compressed: p[1]&0x80 != 0,
			Data: p[rdp.FrameHeaderSize : rdp.FrameHeaderSize+n],
		}
		if b.Width <= 0 || b.Height <= 0 || b.Width > screenTile || b.Height > screenTile {
			return nil, errors.New("the screen helper sent a bitmap of the wrong size")
		}
		out = append(out, b)
		p = p[size:]
	}
	c.mu.Lock()
	tap := c.tap
	c.mu.Unlock()
	if tap != nil {
		for _, pdu := range rdp.FastPathBitmaps(out) {
			tap(pdu)
		}
	}
	return out, nil
}

// picture passes one H.264 picture on as graphics pipeline commands, and
// records it as them. A key frame starts the surface afresh, which is also
// what lets a viewer who joined late begin to see video.
func (c *screenClient) picture(p []byte) error {
	m, err := parseVideoMsg(p)
	if err != nil {
		return err
	}
	if c.gfx == nil {
		return errors.New("the screen helper sent video nobody asked for")
	}
	c.mu.Lock()
	c.pictures++
	if m.key {
		c.keys++
	}
	c.mu.Unlock()
	var cmds [][]byte
	if m.key {
		cmds = append(cmds, rdp.BuildResetGraphics(m.width, m.height),
			rdp.BuildCreateSurface(screenSurface, m.width, m.height),
			rdp.BuildMapSurfaceToOutput(screenSurface, 0, 0))
	}
	c.frameID++
	cmds = append(cmds, rdp.BuildStartFrame(c.frameID),
		rdp.BuildAVC420(screenSurface, m.width, m.height, m.regions, m.h264),
		rdp.BuildEndFrame(c.frameID))
	for _, cmd := range cmds {
		c.gfx.addCommand(cmd)
	}
	c.mu.Lock()
	tap := c.tap
	c.mu.Unlock()
	if tap != nil {
		for _, pdu := range c.gtap.PDUs(cmds...) {
			tap(pdu)
		}
	}
	return nil
}

// follow notices the console changing hands: another session (a sign-out,
// a switch of user) gets a new helper, and another account is recorded.
func (c *screenClient) follow() error {
	session, ok := c.host.ConsoleSession()
	if !ok {
		return nil
	}
	c.mu.Lock()
	moved := session != c.session
	c.mu.Unlock()
	if moved {
		c.log.Info("tunnel: the console moved to another session; following it", "session", session)
		if err := c.start(session); err != nil {
			return fmt.Errorf("the console moved to another session and could not be followed: %w", err)
		}
	}
	account := c.host.ConsoleAccount(session)
	c.mu.Lock()
	changed := account != c.account
	c.account = account
	c.mu.Unlock()
	if changed {
		c.log.Info("tunnel: the console's account changed", "account", account)
		c.record(map[string]any{"type": "console-account", "account": account})
	}
	return nil
}

// restart replaces a helper that stopped, a few times a minute at most: one
// that keeps failing ends the session rather than spinning.
func (c *screenClient) restart(why string) error {
	now := time.Now()
	c.mu.Lock()
	recent := c.restarts[:0]
	for _, t := range c.restarts {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	c.restarts = append(recent, now)
	c.failed++
	tooMany := len(c.restarts) > 3
	c.mu.Unlock()
	if tooMany {
		return errors.New(why)
	}
	c.log.Warn("tunnel: restarting the screen helper", "reason", why)
	session, ok := c.host.ConsoleSession()
	if !ok {
		return errors.New(why)
	}
	time.Sleep(200 * time.Millisecond)
	if err := c.start(session); err != nil {
		return fmt.Errorf("%s, and could not be restarted: %w", why, err)
	}
	return nil
}

// grantLocked gives the helper a credit when it has none and the browser has
// room; floor gives one anyway when none has been given for a second, so a
// browser that stopped drawing does not stop the recording. c.mu is held.
func (c *screenClient) grantLocked(floor bool) {
	if c.holding || c.closed || c.helper == nil {
		return
	}
	room := !c.acks || c.marked-c.acked < screenWindow
	if !room && !(floor && time.Since(c.granted) >= screenAckFloor) {
		return
	}
	if writeScreenMsg(c.helper, scrNext, nil) == nil {
		c.holding, c.granted = true, time.Now()
	}
}

// TakeMark returns the mark of the frame that just ended, if one did since
// it was last asked, and the last measured round trip to the browser.
func (c *screenClient) TakeMark() (uint32, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mark == 0 {
		return 0, 0, false
	}
	id := c.mark
	c.mark = 0
	c.markedAt[id%uint32(len(c.markedAt))] = time.Now()
	return id, c.rtt, true
}

// Ack is the browser saying it has drawn every frame up to id.
func (c *screenClient) Ack(id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == 0 || id > c.marked {
		return
	}
	c.acks = true
	if c.marked-id < uint32(len(c.markedAt)) {
		if at := c.markedAt[id%uint32(len(c.markedAt))]; !at.IsZero() {
			c.rtt = time.Since(at)
		}
	}
	if id > c.acked {
		c.acked = id
	}
	c.grantLocked(false)
}

func (c *screenClient) record(v any) {
	if c.note == nil {
		return
	}
	if err := c.note(v); err != nil {
		// What the console did is in the chain, or the session ends.
		c.log.Error("tunnel: recording a screen event failed; ending the session", "error", err)
		_ = c.Close()
	}
}

func (c *screenClient) send(typ byte, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.helper == nil {
		return errors.New("closed")
	}
	return writeScreenMsg(c.helper, typ, p)
}

func (c *screenClient) Send(e rdp.InputEvent) error {
	if e.Kind == "key" {
		switch {
		case e.Scancode == 0x1D: // control
			c.ctrl = e.Down
		case e.Scancode == 0x38: // alt
			c.alt = e.Down
		case e.Scancode == 0x53 && e.Extended && e.Down && c.ctrl && c.alt:
			// Delete with Ctrl and Alt held: the secure attention
			// sequence, which Windows takes from no keyboard but its own.
			if err := c.host.SecureAttention(); err != nil {
				c.log.Warn("tunnel: could not send Ctrl+Alt+Del", "error", err)
			}
			return nil
		}
	}
	p, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := c.send(scrInput, p); err != nil {
		// A helper being replaced drops the keystroke, not the session.
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return err
		}
	}
	return nil
}

func (c *screenClient) Refresh() error {
	err := c.send(scrRefresh, nil)
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return err
	}
	return nil
}

func (c *screenClient) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

func (c *screenClient) SetTap(fn func([]byte)) {
	c.mu.Lock()
	c.tap = fn
	c.mu.Unlock()
}

func (c *screenClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if c.video {
		c.log.Info("tunnel: screen sharing ended", "codec", c.codec,
			"pictures", c.pictures, "key_frames", c.keys)
	}
	if c.helper != nil {
		return c.helper.Close()
	}
	return nil
}

// connectScreen opens the console for a session through the tunnel.
func connectScreen(cfg DesktopConfig, _ string, open tunnel.DesktopOpen,
	log *slog.Logger) (DesktopClient, DesktopInfo, error) {
	host := cfg.ScreenHost
	if host == nil {
		host = platformScreenHost()
	}
	if host == nil {
		return nil, DesktopInfo{}, errors.New("screen sharing works on Windows hosts only")
	}
	c, err := openScreen(host, open.Graphics, log)
	if err != nil {
		return nil, DesktopInfo{}, err
	}
	log.Info("tunnel: sharing the console's screen", "width", c.width, "height", c.height,
		"h264_asked", c.video)
	info := DesktopInfo{Screen: true}
	if c.video {
		info.Graphics, info.GraphicsChannel = c.gfx, screenGraphicsChannel
	}
	return c, info, nil
}
