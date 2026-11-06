// Package mobile is the Argus agent as the Argus app for Android embeds it.
//
// gomobile binds this package into an Android library (argus.aar); the app
// is a thin Kotlin shell around it. What the app does that this package
// cannot -- capture the screen, touch it, ask whoever holds the device --
// it does through the Device interface; everything else is the agent as
// every host runs it: enrolment, the tunnel the control plane opens
// sessions on, the recording sealed before a frame is shown, the reports.
//
// So the control plane does not know a phone from a server. A device
// session is the android service of internal/agent/android.go, on the
// same grant check, the same recording and the same live view.
//
// Bound for gomobile, so the exported surface keeps to what it binds: no
// slices but []byte, one result and an error, interfaces of such methods.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/agent/reporter"
	"github.com/gsoultan/argus/internal/tunnel"
)

// Version is the app's version, set when the library is built.
var Version = "dev"

// Device is what the app provides. The screen comes in through
// Agent.PushFrame; this is the rest.
//
// Every method is called from a goroutine of the agent's, never from the
// app's main thread, so one may block: Consent does, until whoever holds
// the device has answered or the app's own time limit has passed.
type Device interface {
	// Consent asks whoever holds the device whether person may open a
	// session as principal ("view" or "control"), and reports the answer.
	// Attended mode: nothing opens without a yes.
	Consent(person, principal string) bool
	// SessionStarted says a session is open, and how many are now: the app
	// starts capturing on the first and shows who is on the device.
	SessionStarted(person, principal string, open int)
	// SessionEnded says one ended, and how many remain: zero stops the
	// capture and clears the banner.
	SessionEnded(open int)
	// Play plays one input, as agent.AndroidInput JSON. "view" sessions
	// never reach it: the agent refuses their input before this.
	Play(inputJSON string) error
}

// Logger receives the agent's log lines, for the app's log.
type Logger interface {
	Log(level, msg string)
}

// Status is the enrolment as the app shows it.
type Status struct {
	Enrolled   bool
	ControlURL string
	Hostname   string
	EnrolledAt string
}

// Enroll links this device to the control plane, as `argus-agent enroll`
// does for a host: url, token and pin are what the console's code carries
// (pin "off" or "" trusts the address as the system does). hostname is the
// name the console gave the device. An existing enrolment is replaced.
func Enroll(controlURL, token, pin, hostname, stateDir string) error {
	if pin == "off" {
		pin = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := agent.Enroll(ctx, agent.EnrollOptions{
		ControlURL: controlURL, Token: token, Pin: pin,
		Hostname: hostname, StateDir: stateDir, Force: true,
	})
	return err
}

// EnrolCode is what the console's QR code carries.
type EnrolCode struct {
	URL   string
	Token string
	Pin   string
	Name  string
}

// ParseEnrolCode reads the console's code: JSON with argus "enrol", url,
// token, pin ("off" or a sha256 pin) and name.
func ParseEnrolCode(text string) (*EnrolCode, error) {
	var in struct {
		Argus string `json:"argus"`
		URL   string `json:"url"`
		Token string `json:"token"`
		Pin   string `json:"pin"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &in); err != nil || in.Argus != "enrol" {
		return nil, errors.New("this is not an Argus enrolment code")
	}
	if in.URL == "" || in.Token == "" {
		return nil, errors.New("the code has no address or no token")
	}
	return &EnrolCode{URL: in.URL, Token: in.Token, Pin: in.Pin, Name: in.Name}, nil
}

// Enrolled reads the enrolment in stateDir.
func Enrolled(stateDir string) (*Status, error) {
	enr, err := agent.LoadEnrollment(stateDir)
	if err != nil {
		return nil, err
	}
	if enr == nil {
		return &Status{}, nil
	}
	return &Status{Enrolled: true, ControlURL: enr.ControlURL, Hostname: enr.Hostname,
		EnrolledAt: enr.EnrolledAt.Format(time.RFC3339)}, nil
}

// Forget removes the enrolment, so the app can be linked to another Argus.
// The credential stays valid until revoked in the console.
func Forget(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, agent.EnrollmentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Agent is the running agent: one per app process.
type Agent struct {
	stateDir string
	device   Device
	log      *slog.Logger

	// Facts the app knows about the device, reported with the hostname.
	osName    string
	machineID string
	model     string

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
	// The last frame the app pushed, as BGRX, and a channel closed when the
	// next arrives so a session waiting for a change wakes.
	frame       []byte
	width       int
	height      int
	stride      int
	seq         uint64
	nextFrame   chan struct{}
	open        int
	connected   atomic.Bool
	sessions    atomic.Int64
	endRequests atomic.Int64
}

// NewAgent makes the agent for stateDir, with the device the app provides.
// osName is what the console shows as the host's OS ("Android 14");
// machineID tells the control plane this device from another with the same
// name.
func NewAgent(stateDir string, device Device, log Logger, osName, model, machineID string) *Agent {
	var h slog.Handler
	if log != nil {
		h = loggerHandler{l: log}
	} else {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	return &Agent{
		stateDir: stateDir, device: device, log: slog.New(h),
		osName: osName, model: model, machineID: machineID,
		nextFrame: make(chan struct{}),
	}
}

// Start reports to the control plane and holds the tunnel until Stop. It
// returns at once, with an error only when the device is not enrolled.
func (a *Agent) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil
	}
	enr, err := agent.LoadEnrollment(a.stateDir)
	if err != nil {
		return err
	}
	if enr == nil {
		return errors.New("not enrolled: scan the console's code first")
	}
	clientTLS, err := enr.TLS()
	if err != nil {
		return fmt.Errorf("enrollment TLS configuration failed: %w", err)
	}
	for _, d := range []string{a.stateDir, a.recordingsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel, a.running = cancel, true
	log := a.log
	hostname := enr.Hostname

	rep := reporter.NewWithTLS(enr.ControlURL, enr.Credential,
		filepath.Join(a.stateDir, "report-spool.jsonl"), clientTLS, log)
	rep.Drain(ctx)
	rep.StartDrainLoop(ctx, 30*time.Second)

	col := agent.NewCollector(a.recordingsDir(), log)
	uploads := agent.NewUploads(a.stateDir, rep, log)
	go uploads.Run(ctx, time.Minute)
	col.OnSession = func(rec agent.SessionRecord) {
		rep.Session(ctx, agent.SessionReport(rec))
		uploads.Queue(rec)
	}
	col.OnActive = func(rec agent.SessionRecord) { rep.Session(ctx, agent.SessionReport(rec)) }

	services := map[string]agent.TunnelService{}
	services[agent.AndroidService] = agent.Android(agent.AndroidConfig{
		DesktopConfig: agent.DesktopConfig{
			Collector: col,
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			Hostname:  hostname,
			Log:       log,
		},
		Device: a.openDevice,
	})
	go agent.RunTunnel(ctx, agent.TunnelConfig{
		Dial: tunnel.DialOptions{
			ControlURL: enr.ControlURL,
			Credential: enr.Credential,
			Hostname:   hostname,
			Version:    Version,
			TLS:        clientTLS,
		},
		Services:    services,
		Log:         log,
		GrantPin:    agent.NewGrantPin(a.stateDir),
		OnConnected: func() { a.connected.Store(true) },
	})

	heartbeat := func() {
		rep.Heartbeat(ctx, map[string]any{
			"hostname":        hostname,
			"version":         Version,
			"active_sessions": col.ActiveCount(),
		})
	}
	facts := func() {
		rep.Facts(ctx, map[string]any{
			"hostname":   hostname,
			"machine_id": a.machineID,
			"os":         a.osName,
			"system":     map[string]any{"model": a.model, "virtual": false},
		})
	}
	go func() {
		facts()
		heartbeat()
		beat := time.NewTicker(30 * time.Second)
		slow := time.NewTicker(time.Hour)
		defer beat.Stop()
		defer slow.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-beat.C:
				heartbeat()
			case <-slow.C:
				facts()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = col.Close()
	}()
	log.Info("argus app starting", "version", Version, "control", enr.ControlURL, "hostname", hostname)
	return nil
}

// Stop ends every session and drops the tunnel.
func (a *Agent) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel, a.running = nil, false
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.connected.Store(false)
}

// Running reports whether Start has been called and Stop has not.
func (a *Agent) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// Connected reports whether the tunnel to the control plane is up.
func (a *Agent) Connected() bool { return a.connected.Load() }

// Sessions is how many sessions are open on the device now.
func (a *Agent) Sessions() int { return int(a.sessions.Load()) }

// EndSessions ends every open session: the Stop button on the banner.
func (a *Agent) EndSessions() {
	a.endRequests.Add(1)
	a.mu.Lock()
	close(a.nextFrame)
	a.nextFrame = make(chan struct{})
	a.mu.Unlock()
}

// PushFrame gives the agent the screen as the app captured it: top-down
// RGBA rows, stride bytes each. Called for every frame the capture
// produces; one is kept, and sessions take it as they are ready.
func (a *Agent) PushFrame(rgba []byte, width, height, stride int) {
	if width <= 0 || height <= 0 || stride < width*4 || len(rgba) < stride*(height-1)+width*4 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := width * 4
	if cap(a.frame) < out*height {
		a.frame = make([]byte, out*height)
	}
	a.frame = a.frame[:out*height]
	for y := 0; y < height; y++ {
		src := rgba[y*stride : y*stride+out]
		dst := a.frame[y*out : (y+1)*out]
		for x := 0; x < out; x += 4 {
			dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 0xFF
		}
	}
	a.width, a.height, a.stride = width, height, out
	a.seq++
	close(a.nextFrame)
	a.nextFrame = make(chan struct{})
}

func (a *Agent) recordingsDir() string { return filepath.Join(a.stateDir, "recordings") }

// openDevice is AndroidConfig.Device: one session's view of the device,
// once whoever holds it has said yes and the first frame is in.
func (a *Agent) openDevice(open agent.AndroidOpen) (agent.AndroidDevice, error) {
	if a.device == nil {
		return nil, errors.New("the app has no device to share")
	}
	if !a.device.Consent(open.Person, open.Principal) {
		return nil, errors.New("whoever holds the device did not accept the session")
	}
	a.mu.Lock()
	a.open++
	n := a.open
	a.mu.Unlock()
	a.sessions.Store(int64(n))
	a.device.SessionStarted(open.Person, open.Principal, n)

	d := &session{a: a, endsAt: a.endRequests.Load()}
	// The capture starts on SessionStarted; the first frame tells the
	// session its size.
	if _, _, _, err := d.Frame(10 * time.Second); err != nil {
		d.Close()
		return nil, err
	}
	if w, h := d.Size(); w == 0 || h == 0 {
		d.Close()
		return nil, errors.New("the device did not share its screen")
	}
	return d, nil
}

// session is one session's device: it reads frames as they are pushed and
// plays input through the app.
type session struct {
	a      *Agent
	seen   uint64
	buf    []byte
	closed atomic.Bool
	// endsAt is the number of EndSessions calls when this one opened; one
	// more ends it.
	endsAt int64
}

func (s *session) Size() (int, int) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	return s.a.width, s.a.height
}

func (s *session) Frame(wait time.Duration) ([]byte, int, bool, error) {
	if s.closed.Load() {
		return nil, 0, false, errors.New("closed")
	}
	if s.a.endRequests.Load() != s.endsAt {
		return nil, 0, false, errors.New("ended from the device")
	}
	s.a.mu.Lock()
	seq, next := s.a.seq, s.a.nextFrame
	s.a.mu.Unlock()
	if seq == s.seen {
		select {
		case <-next:
		case <-time.After(wait):
			return nil, 0, false, nil
		}
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.seq == s.seen {
		return nil, 0, false, nil
	}
	s.seen = s.a.seq
	s.buf = append(s.buf[:0], s.a.frame...)
	return s.buf, s.a.stride, true, nil
}

func (s *session) Play(in agent.AndroidInput) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return s.a.device.Play(string(b))
}

func (s *session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.a.mu.Lock()
	if s.a.open > 0 {
		s.a.open--
	}
	n := s.a.open
	s.a.mu.Unlock()
	s.a.sessions.Store(int64(n))
	s.a.device.SessionEnded(n)
	return nil
}

// loggerHandler sends slog records to the app's Logger.
type loggerHandler struct {
	l     Logger
	attrs []slog.Attr
}

func (h loggerHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h loggerHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	for _, at := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", at.Key, at.Value)
	}
	r.Attrs(func(at slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", at.Key, at.Value)
		return true
	})
	h.l.Log(strings.ToLower(r.Level.String()), b.String())
	return nil
}

func (h loggerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return loggerHandler{l: h.l, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h loggerHandler) WithGroup(string) slog.Handler { return h }
