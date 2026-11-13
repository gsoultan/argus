package agent

import (
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// gaugeAt plays a link to the gauge: frames of size bytes every gap, each
// acknowledged took after its mark.
func gaugeAt(g *linkGauge, now *time.Time, id *uint32, frames, size int, gap, took time.Duration) {
	for range frames {
		*id++
		g.sent += int64(size)
		g.mark(*id, *now)
		g.ack(*id, now.Add(took))
		*now = now.Add(gap)
	}
}

// A link with room is told nothing: motion costs what it always did. When
// frames start to queue, motion is held under what got through; when the
// link clears, it climbs back and the limit is lifted.
func TestLinkGaugeFollowsTheLink(t *testing.T) {
	var g linkGauge
	g.size(1920, 1080)
	full := screenMotionBits(1920, 1080)
	now, id := time.Unix(1000, 0), uint32(0)

	gaugeAt(&g, &now, &id, 30, 30<<10, 33*time.Millisecond, 20*time.Millisecond)
	if g.rate != full {
		t.Fatalf("a link with room is held to %d bit/s, want all %d", g.rate, full)
	}
	if bits, ok := g.tell(); ok {
		t.Fatalf("a link with room had the helper told %d", bits)
	}

	// The link now carries 1 Mbit/s: 12.5 kB frames get through ten a
	// second, each well over a second late.
	gaugeAt(&g, &now, &id, 30, 12500, 100*time.Millisecond, 1500*time.Millisecond)
	if g.rate < 600_000 || g.rate > 1_000_000 {
		t.Fatalf("on a 1 Mbit/s link motion is held to %d bit/s, want a little under the link", g.rate)
	}
	bits, ok := g.tell()
	if !ok || int(bits) != g.rate {
		t.Fatalf("the helper was told %d (%v), want %d", bits, ok, g.rate)
	}
	if _, again := g.tell(); again {
		t.Fatal("the helper was told the same thing twice")
	}

	// The link clears: prompt acknowledgements, and the rate climbs back.
	gaugeAt(&g, &now, &id, 400, 30<<10, 100*time.Millisecond, 20*time.Millisecond)
	if g.rate != full {
		t.Fatalf("a link that cleared is still held to %d bit/s", g.rate)
	}
	if bits, ok := g.tell(); !ok || bits != 0 {
		t.Fatalf("the limit was not lifted: told %d (%v)", bits, ok)
	}
}

// However slow the link, motion is not held below the least worth sending.
func TestLinkGaugeHasAFloor(t *testing.T) {
	var g linkGauge
	g.size(1280, 720)
	now, id := time.Unix(1000, 0), uint32(0)
	gaugeAt(&g, &now, &id, 5, 4<<10, 33*time.Millisecond, 10*time.Millisecond)
	gaugeAt(&g, &now, &id, 200, 2000, time.Second, 5*time.Second)
	if g.rate != linkRateMin {
		t.Fatalf("the rate fell to %d, want the floor of %d", g.rate, linkRateMin)
	}
}

// Two frames in flight on a short link; more where the round trip is long,
// up to a bound.
func TestLinkGaugeWindowGrowsWithTheRoundTrip(t *testing.T) {
	for _, c := range []struct {
		best time.Duration
		want uint32
	}{{0, 2}, {20 * time.Millisecond, 2}, {120 * time.Millisecond, 3}, {250 * time.Millisecond, 6}, {2 * time.Second, 6}} {
		g := linkGauge{best: c.best}
		if got := g.window(); got != c.want {
			t.Errorf("with a %v round trip %d frames may be in flight, want %d", c.best, got, c.want)
		}
	}
}

// Sharpening is spread to what the link carries in a moment, and not at
// all when no limit is known.
func TestLinkBudgetSpreadsSharpening(t *testing.T) {
	var b linkBudget
	if got := b.refine(); got != screenRefineMax {
		t.Fatalf("with no limit a frame sharpens %d tiles, want %d", got, screenRefineMax)
	}
	b.rate = 2_000_000
	b.sized(10, 30<<10) // 3 kB a tile
	// 2 Mbit/s for 120 ms is 30 kB: ten tiles.
	if got := b.refine(); got != 9 && got != 10 {
		t.Fatalf("at 2 Mbit/s a frame sharpens %d tiles of 3 kB, want about ten", got)
	}
	b.rate = linkRateMin
	if got := b.refine(); got != linkRefineMin {
		t.Fatalf("on the slowest link a frame sharpens %d tiles, want %d", got, linkRefineMin)
	}
}

// ratedVideo is a fakeVideo that takes a bitrate.
type ratedVideo struct {
	fakeVideo
	rmu   sync.Mutex
	rates []int
}

func (v *ratedVideo) open(w, h int) (screenVideo, error) {
	if _, err := v.fakeVideo.open(w, h); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *ratedVideo) SetBitrate(bits int) {
	v.rmu.Lock()
	defer v.rmu.Unlock()
	if n := len(v.rates); n == 0 || v.rates[n-1] != bits {
		v.rates = append(v.rates, bits)
	}
}

// noise fills the screen with pixels nothing compresses, so an exact tile
// is as large as one gets.
func (s *fakeScreen) noise(seed int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := rand.New(rand.NewSource(seed))
	_, _ = r.Read(s.fb)
}

// Told the link is slow, the helper holds the encoder to it and sends the
// exact pixels after motion a few tiles a frame, where it sent the whole
// screen in one: a keystroke is then never behind a screenful of them.
func TestScreenOnASlowLinkSharpensAFewTilesAFrame(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	screen.noise(1)
	v := &ratedVideo{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	go func() {
		_ = serveScreen(screen, func() screenInputer { return screen }, inR, outW, testPace, v.open)
		_ = outW.Close()
		close(served)
	}()
	h := helperOut{t: t, r: outR, in: inW}
	defer func() {
		_ = inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		<-served
	}()
	readSize(t, h)
	if first := h.next(); len(first.tiles) != 32 {
		t.Fatalf("the first frame had %d tiles, want all 32", len(first.tiles))
	}

	if err := writeScreenMsg(inW, scrRate, encodeRate(linkRateMin)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	screen.noise(2)
	if f := h.next(); len(f.videos) != 1 {
		t.Fatalf("a whole screen changing went as %d pictures", len(f.videos))
	}
	exact, frames := 0, 0
	deadline := time.Now().Add(10 * time.Second)
	for exact < 32 && time.Now().Before(deadline) {
		f := h.next()
		if len(f.tiles) > linkRefineMin {
			t.Fatalf("a frame sharpened %d tiles on the slowest link, want at most %d",
				len(f.tiles), linkRefineMin)
		}
		exact += len(f.tiles)
		frames++
	}
	if exact != 32 {
		t.Fatalf("%d of 32 tiles came again exactly", exact)
	}
	if frames < 32/linkRefineMin {
		t.Fatalf("the screen sharpened in %d frames", frames)
	}
	v.rmu.Lock()
	rates := append([]int(nil), v.rates...)
	v.rmu.Unlock()
	if len(rates) == 0 || rates[len(rates)-1] != linkRateMin {
		t.Fatalf("the encoder was told %v, want %d last", rates, linkRateMin)
	}
}

// What the client sends toward the browser is what the gauge measures the
// link by; without it a slow link reads as carrying nothing.
func TestScreenClientCountsWhatItSends(t *testing.T) {
	host := &fakeScreenHost{session: 1, account: `HOST\alice`,
		screens: map[uint32]*fakeScreen{1: newFakeScreen(130, 70, 0x40)}}
	c, err := openScreen(host, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	bytes := 0
	for _, b := range nextRects(t, c) {
		bytes += len(b.Data)
	}
	c.mu.Lock()
	sent, full := c.link.sent, c.link.max
	c.mu.Unlock()
	if sent < int64(bytes) || bytes == 0 {
		t.Fatalf("the gauge counted %d bytes of the %d sent", sent, bytes)
	}
	if full != screenMotionBits(130, 70) {
		t.Fatalf("the gauge was not told the screen's size: %d", full)
	}
}

// Held to a slow link, a picture is never given more than a frame of the
// encoder's time, however long after the last it comes: the encoder spends
// by that time, and a picture ten frames late would be ten frames large.
func TestPictureClockGivesAHeldPictureOneFrame(t *testing.T) {
	now := time.Unix(1000, 0)
	var free, held pictureClock
	if free.stamp(now, false) != 0 || held.stamp(now, true) != 0 {
		t.Fatal("the first picture is not at zero")
	}
	for i := 1; i <= 5; i++ {
		now = now.Add(300 * time.Millisecond)
		if got, want := free.stamp(now, false), time.Duration(i)*300*time.Millisecond; got != want {
			t.Fatalf("with room, picture %d is at %v, want %v", i, got, want)
		}
		if got, want := held.stamp(now, true), time.Duration(i)*screenVideoFrame; got != want {
			t.Fatalf("held, picture %d is at %v, want %v", i, got, want)
		}
	}
	// Pictures closer than a frame keep their real spacing, and time never
	// stands still, which an encoder refuses.
	now = now.Add(10 * time.Millisecond)
	at := held.stamp(now, true)
	if got := at - 5*screenVideoFrame; got != 10*time.Millisecond {
		t.Fatalf("a picture 10ms on moved the clock %v", got)
	}
	if again := held.stamp(now, true); again <= at {
		t.Fatalf("two pictures at once are both at %v", at)
	}
}

// A browser that says its link is slow is not sent a whole screen of exact
// tiles to find that out: megabytes, and seconds of nothing. The first
// frame is one key picture, there at once, and the screen sharpens after.
func TestScreenOnASlowLinkOpensWithAPicture(t *testing.T) {
	screen := newFakeScreen(512, 256, 0)
	screen.noise(1)
	v := &ratedVideo{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	pace := testPace
	pace.linkRate = linkRateSlowStart
	go func() {
		_ = serveScreen(screen, func() screenInputer { return screen }, inR, outW, pace, v.open)
		_ = outW.Close()
		close(served)
	}()
	h := helperOut{t: t, r: outR, in: inW}
	defer func() {
		_ = inW.Close()
		go func() { _, _ = io.Copy(io.Discard, outR) }()
		<-served
	}()
	readSize(t, h)
	first := h.next()
	if len(first.videos) != 1 || len(first.tiles) != 0 {
		t.Fatalf("the first frame was %d pictures and %d tiles, want one picture", len(first.videos), len(first.tiles))
	}
	exact := 0
	deadline := time.Now().Add(10 * time.Second)
	for exact < 32 && time.Now().Before(deadline) {
		exact += len(h.next().tiles)
	}
	if exact != 32 {
		t.Fatalf("%d of 32 tiles came exactly after the picture", exact)
	}
}

// The gauge starts where the helper was started: under a slow link's rate,
// not at the full one waiting for a queue to say otherwise.
func TestLinkGaugeStartsSlowWhenToldSo(t *testing.T) {
	var g linkGauge
	g.size(1920, 1080)
	g.slow()
	if g.rate != linkRateSlowStart {
		t.Fatalf("a slow link starts at %d bit/s, want %d", g.rate, linkRateSlowStart)
	}
	now, id := time.Unix(1000, 0), uint32(0)
	gaugeAt(&g, &now, &id, 400, 30<<10, 100*time.Millisecond, 20*time.Millisecond)
	if g.rate != screenMotionBits(1920, 1080) {
		t.Fatalf("a link that turned out fast is still held to %d bit/s", g.rate)
	}
}
