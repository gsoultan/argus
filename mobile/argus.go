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

// Installer is how the app changes apps on the device, for the console's
// Apps tab. Each call blocks until Android has answered, and returns an
// error when it did not install or remove: refused, or the person holding
// the device said no, or nobody answered in time. With the attended engine
// Android asks the person; with the unattended engine the shell helper
// does it without asking. The agent has already checked an install's file
// is the one the console showed before Install is called.
type Installer interface {
	// Install installs the package file at path, which is pkg.
	Install(path, pkg string) error
	// Uninstall uninstalls pkg.
	Uninstall(pkg string) error
	// Disable disables a preinstalled pkg for the device's user, never
	// removing it from the device (unattended only).
	Disable(pkg string) error
	// Restore undoes Disable.
	Restore(pkg string) error
}

// Removed is how the app hears that this device has left Argus, from the
// console or from the app's own Remove button. By the time it is called the
// credential is revoked (or the control plane could not be told, which
// summary says) and deleted, and the agent has stopped; what is left is the
// app's own part: turn off the unattended engine and forget its pairing,
// switch off the accessibility service, stop the foreground service, and
// tell whoever holds the device.
type Removed interface {
	// Removed says the device left Argus. byConsole is true when an
	// administrator removed it in the console. summary is a sentence for
	// whoever holds the device.
	Removed(byConsole bool, summary string)
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
	frame     []byte
	width     int
	height    int
	stride    int
	seq       uint64
	nextFrame chan struct{}
	// Activity the app pushes as it happens -- the app in front, a lock,
	// secure content -- kept in a small queue each open session drains into
	// its recording. acts[0] is event number actBase.
	acts    []agent.AndroidActivity
	actBase uint64
	nextAct chan struct{}
	// appsJSON is the installed-apps list the app last pushed, reported to the
	// control plane with the device's facts for the Apps tab. Empty until the
	// app pushes one.
	appsJSON    []byte
	open        int
	connected   atomic.Bool
	sessions    atomic.Int64
	endRequests atomic.Int64
	// unattended is set while the device runs the unattended engine: a
	// company device with nobody to accept a session, opened by the helper
	// over local ADB. The agent then opens a session without asking, which
	// is what unattended means, so the "nobody accepts" is enforced here and
	// not only in the app.
	unattended atomic.Bool
	// wirelessDebugging is what the console shows as a fact: unattended needs
	// it on, and on Android 11+ it listens on every interface, so nobody
	// should discover it later.
	wirelessDebugging atomic.Bool
	// installer is how the app changes apps, nil until SetInstaller; an app
	// without one does not tell the control plane it can, so the console
	// asks for the app to be updated instead of sending it orders.
	installer Installer
	// canInstall is Android's "Install unknown apps" for this app, which the
	// person allows once; reported so the console says it is not set up.
	canInstall atomic.Int32
	apps       *agent.AppRuns

	// removed is told when the device leaves Argus; nil until SetRemoved.
	removed Removed
	// collectorClosed is closed once the running agent's recordings are
	// sealed after Stop, so leaving ships them whole.
	collectorClosed chan struct{}
	// leaving is held while the device leaves Argus, so the console's
	// request and the app's button do not run it twice at once.
	leaving   sync.Mutex
	lastLeave time.Time
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
		nextFrame: make(chan struct{}), nextAct: make(chan struct{}),
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

	var apps *agent.AppRuns
	if a.installer != nil {
		apps = &agent.AppRuns{
			Dir: agent.AppRunsDir(a.stateDir), Fetch: rep.Download, Installer: a.installer, Log: log,
			Attended: func() bool { return !a.unattended.Load() },
		}
	}
	a.apps = apps
	var facts func()
	heartbeat := func() {
		beat := map[string]any{
			"hostname":        hostname,
			"version":         Version,
			"active_sessions": col.ActiveCount(),
		}
		if apps != nil {
			// Not supported as a server's updates are: the control plane
			// sends this device app runs only, and only from its Apps tab.
			beat["packages"] = agent.PackageBeat{Reason: "an Android device changes apps from its Apps tab",
				Run: apps.Status()}
		}
		reply, ok := rep.HeartbeatWithReply(ctx, beat)
		if ok && reply.Uninstall {
			// Removed in the console: the device takes itself off Argus,
			// as a server's agent does. Not on this goroutine, which
			// leaving stops.
			go a.leaveForConsole()
			return
		}
		if !ok || apps == nil || len(reply.Packages) == 0 {
			return
		}
		var o agent.PackageOrders
		if err := json.Unmarshal(reply.Packages, &o); err != nil || o.Run == nil || !agent.IsAppMode(o.Run.Mode) {
			return
		}
		// What is installed changed: the app reads the list again and the
		// facts carry it, so the Apps tab shows it without waiting an hour.
		apps.Start(ctx, *o.Run, func() { go facts() })
	}
	facts = func() {
		engine := "attended"
		if a.unattended.Load() {
			engine = "unattended"
		}
		f := map[string]any{
			"hostname":   hostname,
			"machine_id": a.machineID,
			"os":         a.osName,
			"system": map[string]any{
				"model":              a.model,
				"virtual":            false,
				"engine":             engine,
				"wireless_debugging": a.wirelessDebugging.Load(),
			},
		}
		if c := a.canInstall.Load(); c != 0 {
			f["system"].(map[string]any)["can_install_apps"] = c == 1
		}
		a.mu.Lock()
		apps := a.appsJSON
		a.mu.Unlock()
		if len(apps) > 0 {
			// Carried as-is so the control plane decodes it into AndroidApps;
			// the app already shaped it with PushApps.
			f["android_apps"] = json.RawMessage(apps)
		}
		rep.Facts(ctx, f)
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
	closed := make(chan struct{})
	a.collectorClosed = closed
	go func() {
		<-ctx.Done()
		_ = col.Close()
		close(closed)
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

// SetUnattended tells the agent the device runs the unattended engine (true)
// or the attended one (false). Unattended opens a session without asking, so
// the app sets this only once its helper is armed, and clears it the moment
// the helper is not. The console's control mode is still what gates approval.
func (a *Agent) SetUnattended(on bool) { a.unattended.Store(on) }

// Unattended reports the engine the agent is opening sessions with.
func (a *Agent) Unattended() bool { return a.unattended.Load() }

// SetWirelessDebugging tells the agent whether wireless debugging is on, which
// the console shows as a fact on the device: unattended needs it, and it is a
// shell-level door nobody should find by surprise.
func (a *Agent) SetWirelessDebugging(on bool) { a.wirelessDebugging.Store(on) }

// SetInstaller gives the agent the app's Installer. Called before Start.
func (a *Agent) SetInstaller(i Installer) {
	a.mu.Lock()
	a.installer = i
	a.mu.Unlock()
}

// SetCanInstallApps says whether Android lets this app install apps
// ("Install unknown apps"), reported in the device's facts.
func (a *Agent) SetCanInstallApps(on bool) {
	if on {
		a.canInstall.Store(1)
	} else {
		a.canInstall.Store(2)
	}
}

// SetRemoved gives the agent the app's Removed. Called before Start.
func (a *Agent) SetRemoved(r Removed) {
	a.mu.Lock()
	a.removed = r
	a.mu.Unlock()
}

// Leave takes this device off Argus, from the app: the Remove button. As
// `argus-agent uninstall` does on a server, in the same order: the agent
// stops, so every open recording is sealed; what has not reached the control
// plane is shipped; the credential is revoked and deleted; and the app is
// told through Removed. Recordings shipped stay until the app is uninstalled.
//
// Without force it refuses while recordings or reports have not reached the
// control plane, since once the credential is gone nothing can send them, and
// the agent is started again. With force it goes on, and a control plane that
// cannot be told is left to an administrator to revoke in the console; the
// summary says which. Blocks for as long as shipping takes, up to two minutes.
func (a *Agent) Leave(force bool) (string, error) {
	return a.leave(force, false, "whoever holds the device, from the Argus app", 2*time.Minute)
}

// leaveForConsole is the console's removal, asked on a heartbeat. Evidence
// that cannot be shipped stays on the device, as on a server; a control plane
// that cannot be told is asked again on a later heartbeat, at most every ten
// minutes, since the credential is still needed to tell it.
func (a *Agent) leaveForConsole() {
	a.mu.Lock()
	recent := time.Since(a.lastLeave) < 10*time.Minute
	if !recent {
		a.lastLeave = time.Now()
	}
	a.mu.Unlock()
	if recent {
		return
	}
	a.log.Warn("this device was removed from Argus; taking it off")
	if _, err := a.leave(true, true, "the Argus console (device removed)", 10*time.Minute); err != nil {
		a.log.Error("leaving Argus: " + err.Error() + "; will try again")
	}
}

func (a *Agent) leave(force, byConsole bool, ranBy string, limit time.Duration) (string, error) {
	if !a.leaving.TryLock() {
		return "", errors.New("this device is already leaving Argus")
	}
	defer a.leaving.Unlock()

	enr, err := agent.LoadEnrollment(a.stateDir)
	if err != nil {
		return "", err
	}
	if enr == nil {
		a.Stop()
		a.tellRemoved(byConsole, "This device is not linked to Argus.")
		return "This device is not linked to Argus.", nil
	}
	wasRunning := a.Running()
	a.stopAndWait()
	restart := func() {
		if wasRunning {
			if err := a.Start(); err != nil {
				a.log.Error("the agent could not be started again", "error", err)
			}
		}
	}

	tlsCfg, err := enr.TLS()
	if err != nil {
		restart()
		return "", fmt.Errorf("the enrolment's TLS configuration failed: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	rep := reporter.NewWithTLS(enr.ControlURL, enr.Credential,
		filepath.Join(a.stateDir, "report-spool.jsonl"), tlsCfg, a.log)
	rep.Drain(ctx)
	recordings := agent.NewUploads(a.stateDir, rep, a.log).Pass(ctx)
	reports := rep.Spooled()
	var kept string
	if recordings > 0 || reports > 0 {
		if !force {
			restart()
			return "", fmt.Errorf("%d recording(s) and %d report(s) have not reached Argus at %s "+
				"yet, and once this device's link is revoked nothing can send them. Connect to a "+
				"network that reaches Argus and try again, or remove it anyway and they stay on "+
				"this device until the app is uninstalled", recordings, reports, enr.ControlURL)
		}
		kept = fmt.Sprintf(" %d recording(s) and %d report(s) never reached Argus and stay on "+
			"this device until the app is uninstalled.", recordings, reports)
		a.log.Warn("leaving Argus: some evidence never reached the control plane; it stays on the device",
			"recordings", recordings, "reports", reports)
	}

	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	err = rep.Deregister(dctx, map[string]string{"hostname": enr.Hostname, "ran_by": ranBy})
	dcancel()
	told := " Argus revoked its link."
	if err != nil {
		if byConsole {
			// The credential is how the control plane is told; keep it.
			restart()
			return "", fmt.Errorf("the control plane could not be told: %w", err)
		}
		told = " Argus could not be told (" + err.Error() + "): an administrator should remove " +
			enr.Hostname + " in the console."
	}
	for _, name := range agent.SecretStateFiles {
		p := filepath.Join(a.stateDir, name)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Error("leaving Argus: could not delete "+name, "error", err)
		}
	}
	summary := "This device has left Argus." + told + kept
	if byConsole {
		summary = "An administrator removed this device from Argus." + told + kept
	}
	a.log.Warn(summary)
	a.tellRemoved(byConsole, summary)
	return summary, nil
}

func (a *Agent) tellRemoved(byConsole bool, summary string) {
	a.mu.Lock()
	r := a.removed
	a.mu.Unlock()
	if r != nil {
		r.Removed(byConsole, summary)
	}
}

// stopAndWait is Stop, then waiting for every open recording to be sealed,
// so leaving ships it whole. Bounded: a recording that will not seal in half
// a minute is shipped as far as it got on the next try.
func (a *Agent) stopAndWait() {
	a.mu.Lock()
	closed := a.collectorClosed
	a.mu.Unlock()
	a.Stop()
	if closed == nil {
		return
	}
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
	}
}

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

// maxPushedActivity bounds the activity queue, so a flood of events from the
// app cannot grow it without limit. The oldest fall off; a session reading
// slower than that misses them, which is far better than the app running out
// of memory.
const maxPushedActivity = 1024

// PushActivity gives the agent one thing that happened on the device, as
// agent.AndroidActivity JSON: the app in front, the screen locked, secure
// content hidden, text typed. Each open session chains it into its recording.
// The app calls this from the accessibility service.
func (a *Agent) PushActivity(activityJSON string) {
	var ev agent.AndroidActivity
	if err := json.Unmarshal([]byte(activityJSON), &ev); err != nil {
		a.log.Warn("android: a bad activity event was dropped", "error", err)
		return
	}
	a.mu.Lock()
	a.acts = append(a.acts, ev)
	if len(a.acts) > maxPushedActivity {
		drop := len(a.acts) - maxPushedActivity
		a.acts = append(a.acts[:0], a.acts[drop:]...)
		a.actBase += uint64(drop)
	}
	close(a.nextAct)
	a.nextAct = make(chan struct{})
	a.mu.Unlock()
}

// PushApps gives the agent the device's installed apps, as agent.AndroidApps
// JSON. It is reported to the control plane with the device's facts and shown
// on the device page's Apps tab. The app calls this after reading the package
// manager.
func (a *Agent) PushApps(appsJSON string) {
	var apps agent.AndroidApps
	if err := json.Unmarshal([]byte(appsJSON), &apps); err != nil {
		a.log.Warn("android: a bad apps list was dropped", "error", err)
		return
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.appsJSON = b
	a.mu.Unlock()
}

func (a *Agent) recordingsDir() string { return filepath.Join(a.stateDir, "recordings") }

// openDevice is AndroidConfig.Device: one session's view of the device,
// once whoever holds it has said yes and the first frame is in.
func (a *Agent) openDevice(open agent.AndroidOpen) (agent.AndroidDevice, error) {
	if a.device == nil {
		return nil, errors.New("the app has no device to share")
	}
	// Attended asks whoever holds the device; unattended is a company device
	// with nobody there, so the session opens without a prompt. The console's
	// control mode decided which, and it still signed a grant for a named
	// person, recorded as the actor.
	if !a.unattended.Load() && !a.device.Consent(open.Person, open.Principal) {
		return nil, errors.New("whoever holds the device did not accept the session")
	}
	a.mu.Lock()
	a.open++
	n := a.open
	a.mu.Unlock()
	a.sessions.Store(int64(n))
	a.device.SessionStarted(open.Person, open.Principal, n)

	a.mu.Lock()
	actHead := a.actBase + uint64(len(a.acts))
	a.mu.Unlock()
	d := &session{a: a, endsAt: a.endRequests.Load(), actSeen: actHead}
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
	a       *Agent
	seen    uint64
	actSeen uint64
	buf     []byte
	closed  atomic.Bool
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

// Activity hands the recording the next thing the app reported, waiting up to
// wait for one. It satisfies the android client's activity source, so the app
// in front, locks and typed text are chained into this session's recording.
// Only events from after the session opened are returned.
func (s *session) Activity(wait time.Duration) (agent.AndroidActivity, bool) {
	if s.closed.Load() {
		return agent.AndroidActivity{}, false
	}
	s.a.mu.Lock()
	head := s.a.actBase + uint64(len(s.a.acts))
	if s.actSeen < s.a.actBase {
		// Events fell off the queue before this session read them; skip to
		// what is still here rather than reading past the start of the slice.
		s.actSeen = s.a.actBase
	}
	if s.actSeen >= head {
		next := s.a.nextAct
		s.a.mu.Unlock()
		select {
		case <-next:
		case <-time.After(wait):
			return agent.AndroidActivity{}, false
		}
		s.a.mu.Lock()
	}
	if s.actSeen < s.a.actBase {
		s.actSeen = s.a.actBase
	}
	if s.actSeen >= s.a.actBase+uint64(len(s.a.acts)) {
		s.a.mu.Unlock()
		return agent.AndroidActivity{}, false
	}
	ev := s.a.acts[s.actSeen-s.a.actBase]
	s.actSeen++
	s.a.mu.Unlock()
	return ev, true
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
