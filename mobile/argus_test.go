package mobile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent"
)

/*
The agent the app embeds, with a stand-in for the app: nothing opens
without the holder's yes, the screen the app pushes is what a session
reads, and the banner's Stop ends what is open.
*/

// fakeApp is the Kotlin side: it answers consent, counts sessions and
// keeps what was played.
type fakeApp struct {
	mu      sync.Mutex
	accept  bool
	asked   []string
	open    []int
	played  []agent.AndroidInput
	started int
}

func (f *fakeApp) Consent(person, principal string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, person+" as "+principal)
	return f.accept
}

func (f *fakeApp) SessionStarted(_, _ string, open int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	f.open = append(f.open, open)
}

func (f *fakeApp) SessionEnded(open int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = append(f.open, open)
}

func (f *fakeApp) Play(inputJSON string) error {
	var in agent.AndroidInput
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, in)
	return nil
}

// pushing feeds frames while a session opens, as the capture would once
// SessionStarted is called.
func pushing(t *testing.T, a *Agent, w, h int) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		stride := w*4 + 16
		buf := make([]byte, stride*h)
		for i := 0; ; i++ {
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					p := buf[y*stride+x*4:]
					p[0], p[1], p[2], p[3] = 0x10, 0x20, byte(0x30+i), 0xFF // R G B A
				}
			}
			a.PushFrame(buf, w, h, stride)
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return func() { close(done) }
}

func TestUnattendedOpensWithNobodyToAccept(t *testing.T) {
	// A company device: no one holds it, so consent is never asked and the
	// session opens anyway. The fake would decline (accept is false), which
	// proves the agent did not call it.
	app := &fakeApp{accept: false}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel Tablet", "m2")
	a.SetUnattended(true)
	if !a.Unattended() {
		t.Fatal("SetUnattended did not take")
	}
	stop := pushing(t, a, 8, 4)
	defer stop()
	dev, err := a.openDevice(agent.AndroidOpen{Person: "dana@example.com", Principal: agent.DeviceControlPrincipal})
	if err != nil {
		t.Fatalf("unattended session did not open: %v", err)
	}
	defer dev.Close()
	app.mu.Lock()
	asked := len(app.asked)
	app.mu.Unlock()
	if asked != 0 {
		t.Fatalf("consent was asked in unattended mode: %v", app.asked)
	}
	if a.Sessions() != 1 {
		t.Fatalf("session did not count: %d", a.Sessions())
	}
	// Back to attended: consent is asked again, and a decline closes it.
	a.SetUnattended(false)
	if _, err := a.openDevice(agent.AndroidOpen{Principal: agent.DeviceControlPrincipal}); err == nil {
		t.Fatal("attended opened without consent after leaving unattended")
	}
}

func TestNothingOpensWithoutTheHoldersYes(t *testing.T) {
	app := &fakeApp{}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel 8", "m1")
	_, err := a.openDevice(agent.AndroidOpen{Person: "dana@example.com", Principal: agent.DeviceControlPrincipal})
	if err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("declined consent opened the device: %v", err)
	}
	if app.started != 0 || a.Sessions() != 0 {
		t.Fatalf("a declined session started: started=%d open=%d", app.started, a.Sessions())
	}
	if len(app.asked) != 1 || app.asked[0] != "dana@example.com as control" {
		t.Fatalf("the holder was asked %q", app.asked)
	}
}

func TestTheScreenTheAppPushesIsWhatASessionReads(t *testing.T) {
	app := &fakeApp{accept: true}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel 8", "m1")
	stop := pushing(t, a, 8, 4)
	defer stop()

	dev, err := a.openDevice(agent.AndroidOpen{Person: "dana@example.com", Principal: agent.DeviceControlPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dev.Size(); w != 8 || h != 4 {
		t.Fatalf("size %dx%d, want 8x4", w, h)
	}
	fb, stride, changed, err := dev.Frame(time.Second)
	if err != nil || !changed {
		t.Fatalf("frame: changed=%v err=%v", changed, err)
	}
	if stride != 32 || len(fb) != 32*4 {
		t.Fatalf("stride %d len %d; want packed 32-byte rows", stride, len(fb))
	}
	// RGBA in, BGRX out: the blue the app drew is the first byte.
	if fb[0] < 0x30 || fb[1] != 0x20 || fb[2] != 0x10 {
		t.Fatalf("pixel %x, want BGRX of (10,20,3x)", fb[:4])
	}
	if app.started != 1 || a.Sessions() != 1 {
		t.Fatalf("started=%d open=%d", app.started, a.Sessions())
	}

	if err := dev.Play(agent.AndroidInput{Type: "device", Name: "home"}); err != nil {
		t.Fatal(err)
	}
	if len(app.played) != 1 || app.played[0].Name != "home" {
		t.Fatalf("played %+v", app.played)
	}

	_ = dev.Close()
	_ = dev.Close()
	if a.Sessions() != 0 || app.open[len(app.open)-1] != 0 {
		t.Fatalf("after close: open=%d app saw %v", a.Sessions(), app.open)
	}
	if _, _, _, err := dev.Frame(time.Millisecond); err == nil {
		t.Fatal("a closed session still reads frames")
	}
}

func TestASessionWaitsForAChangeAndNotForEver(t *testing.T) {
	app := &fakeApp{accept: true}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel 8", "m1")
	a.PushFrame(make([]byte, 4*4*4), 4, 4, 16)
	dev, err := a.openDevice(agent.AndroidOpen{Principal: agent.DeviceViewPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	// The opening read took the frame; nothing new within the wait.
	started := time.Now()
	if _, _, changed, err := dev.Frame(30 * time.Millisecond); err != nil || changed {
		t.Fatalf("changed=%v err=%v with nothing pushed", changed, err)
	}
	if time.Since(started) < 25*time.Millisecond {
		t.Fatal("did not wait for a change")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		a.PushFrame(make([]byte, 4*4*4), 4, 4, 16)
	}()
	if _, _, changed, err := dev.Frame(time.Second); err != nil || !changed {
		t.Fatalf("changed=%v err=%v after a push", changed, err)
	}
}

func TestANoScreenSessionDoesNotOpen(t *testing.T) {
	app := &fakeApp{accept: true}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel 8", "m1")
	// Nothing pushed: the wait is bounded, and the holder sees it end.
	s := &session{a: a}
	if _, _, changed, err := s.Frame(10 * time.Millisecond); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if w, h := s.Size(); w != 0 || h != 0 {
		t.Fatalf("size %dx%d with no frame", w, h)
	}
}

func TestStopOnTheBannerEndsWhatIsOpen(t *testing.T) {
	app := &fakeApp{accept: true}
	a := NewAgent(t.TempDir(), app, nil, "Android 14", "Pixel 8", "m1")
	stop := pushing(t, a, 4, 4)
	defer stop()
	dev, err := a.openDevice(agent.AndroidOpen{Principal: agent.DeviceControlPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	a.EndSessions()
	if _, _, _, err := dev.Frame(time.Second); err == nil {
		t.Fatal("the session read on after Stop")
	}
	// A session opened after Stop is a new one.
	dev2, err := a.openDevice(agent.AndroidOpen{Principal: agent.DeviceControlPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	defer dev2.Close()
	if _, _, _, err := dev2.Frame(time.Second); err != nil {
		t.Fatalf("a later session was ended by an earlier Stop: %v", err)
	}
}

func TestAFrameThatDoesNotFitIsDropped(t *testing.T) {
	a := NewAgent(t.TempDir(), &fakeApp{}, nil, "", "", "")
	a.PushFrame(make([]byte, 10), 4, 4, 16)
	a.PushFrame(make([]byte, 64), 4, 4, 8)
	if a.seq != 0 {
		t.Fatal("a short or misdeclared frame was kept")
	}
}

func TestTheCodeTheConsoleShows(t *testing.T) {
	c, err := ParseEnrolCode(`{"argus":"enrol","url":"https://argus.example","token":"tok-1","pin":"off","name":"rina-pixel-8"}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "https://argus.example" || c.Token != "tok-1" || c.Pin != "off" || c.Name != "rina-pixel-8" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"", "hello", `{"argus":"other","url":"x","token":"y"}`, `{"argus":"enrol","url":"x"}`} {
		if _, err := ParseEnrolCode(bad); err == nil {
			t.Fatalf("%q was taken for a code", bad)
		}
	}
}

func TestEnrolmentIsReadAndForgotten(t *testing.T) {
	dir := t.TempDir()
	st, err := Enrolled(dir)
	if err != nil || st.Enrolled {
		t.Fatalf("fresh dir: %+v %v", st, err)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.EnrollmentFile),
		[]byte(`{"controlUrl":"https://argus.example","hostname":"rina-pixel-8","credential":"argus-agent-x","enrolledAt":"2026-01-02T03:04:05Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = Enrolled(dir)
	if err != nil || !st.Enrolled || st.Hostname != "rina-pixel-8" || st.ControlURL != "https://argus.example" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := Forget(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ = Enrolled(dir); st.Enrolled {
		t.Fatal("still enrolled after Forget")
	}
	if err := Forget(dir); err != nil {
		t.Fatalf("forgetting twice: %v", err)
	}
	if err := a0().Start(); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("Start without an enrolment: %v", err)
	}
}

func a0() *Agent {
	return NewAgent(os.TempDir()+"/argus-mobile-none-"+time.Now().Format("150405.000"), &fakeApp{}, nil, "", "", "")
}

func TestEnrollRefusesAPlainAddress(t *testing.T) {
	err := Enroll("http://argus.example", "tok", "off", "rina", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("got %v", err)
	}
}
