package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/credssp"
	"github.com/gsoultan/argus/internal/rdp"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Remote Desktop through the tunnel.

	browser ──wss──▶ argus-control ──"desktop" stream──▶ agent ──▶ 127.0.0.1:3389

The agent is the RDP client, to the host's own Remote Desktop listener. It
records the protocol stream as the listener sends it -- the format the
console's replay already reads -- before any of it is shown, and passes each
screen update on undecoded for the browser to draw. Nothing about RDP crosses
the network: the connection to the listener never leaves the host, and the
browser and the control plane see only display frames.

The account's password is the person's own, typed in the console and used
for this one logon (network level authentication needs one). The agent never
writes it down. A session opened this way leaves a standing password on the
host, which the session's risk flags say.
*/

// DesktopService is the name the control plane asks for.
const DesktopService = "desktop"

// DefaultDesktopAddress is the host's own Remote Desktop listener.
const DefaultDesktopAddress = "127.0.0.1:3389"

// DesktopConfig is what the desktop service needs.
type DesktopConfig struct {
	Collector *Collector
	Verifier  *tunnel.GrantVerifier
	Hostname  string
	Log       *slog.Logger
	// Address is the Remote Desktop listener. Empty is DefaultDesktopAddress.
	Address string
	// Connect opens the desktop. Nil dials Address; tests replace it.
	Connect func(cfg DesktopConfig, principal string, open tunnel.DesktopOpen,
		log *slog.Logger) (DesktopClient, DesktopInfo, error)
}

// DesktopClient is an open desktop. *rdp.Client is one.
type DesktopClient interface {
	NextBitmaps() ([]rdp.Bitmap, error)
	Send(rdp.InputEvent) error
	Refresh() error
	Size() (width, height int)
	SetTap(func([]byte))
	Close() error
}

// DesktopInfo is how the connection was made, for the session's flags and
// the recording's header.
type DesktopInfo struct {
	Protocol uint32
	// LegacyBinding is a listener that used the pre-version-5 CredSSP
	// binding, which is not bound to a nonce.
	LegacyBinding bool
}

// desktopBatch bounds one output frame of display frames. Under
// tunnel.MaxFrame, and small enough that a full-screen repaint arrives as
// several frames rather than one that stalls everything behind it.
const desktopBatch = 192 << 10

// Desktop returns the desktop service.
func Desktop(cfg DesktopConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Connect == nil {
		cfg.Connect = connectDesktop
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a desktop", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(DesktopService) {
			log.Warn("tunnel: refused a desktop on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a desktop"}.Error())
			return
		}
		// The same limit as a terminal on Windows: nothing tests a domain
		// logon yet, so none is half-opened.
		if strings.ContainsAny(g.Principal, `\@/`) {
			call.Refuse("only local accounts can be opened on this host for now, not " + g.Principal)
			return
		}
		var open tunnel.DesktopOpen
		if call.Header.Desktop != nil {
			open = *call.Header.Desktop
		}
		dlog := log.With("principal", g.Principal, "for", g.Email)
		client, info, err := cfg.Connect(cfg, g.Principal, open, dlog)
		// Gone from memory as soon as the logon is done with it.
		open.Password = ""
		if call.Header.Desktop != nil {
			call.Header.Desktop.Password = ""
		}
		if err != nil {
			dlog.Warn("tunnel: could not open the desktop", "error", err)
			call.Refuse(err.Error())
			return
		}

		width, height := client.Size()
		var d *DesktopRecording
		hangup := func() { _ = client.Close() }
		d, err = cfg.Collector.StartDesktopRecording(SessionStart{
			Type:       "start",
			Principal:  g.Principal,
			ClientAddr: g.ClientAddr,
			Origin:     Brokered,
			OriginReason: "opened through the Argus tunnel on a session grant signed by " +
				"the control plane",
			Hostname:  cfg.Hostname,
			StartedAt: time.Now().UTC(),
			Actor:     g.Email,
		}, rdp.Header{
			Width: width, Height: height,
			Title:    g.Principal + "@" + cfg.Hostname,
			Protocol: rdp.ProtocolName(info.Protocol),
		}, desktopFlags(info), hangup)
		if err != nil {
			// Nothing was shown: the first screen update is read after this.
			_ = client.Close()
			call.Refuse(fmt.Sprintf("the session could not be recorded, so it was not opened: %v", err))
			return
		}
		// Recorded as it is read, before it is decoded or sent anywhere.
		client.SetTap(func(pdu []byte) {
			if rerr := d.Screen(pdu); rerr != nil {
				dlog.Error("tunnel: recording a desktop failed; ending the session",
					"session", d.ID(), "error", rerr)
				_ = client.Close()
			}
		})
		dlog.Info("tunnel: desktop opened", "session", d.ID(), "client", g.ClientAddr,
			"protocol", rdp.ProtocolName(info.Protocol), "size", [2]int{width, height})
		if err := call.Accept(d.ID()); err != nil {
			_ = client.Close()
		}
		runDesktop(call.Stream, client, d, dlog)
	}
}

// desktopFlags are what the console highlights about how a desktop opened.
func desktopFlags(info DesktopInfo) []string {
	// The password the person typed is one they hold for this host: a
	// standing credential, which is what injection exists to remove.
	flags := []string{"no-credential-injection"}
	if info.LegacyBinding {
		flags = append(flags, "legacy-credssp-binding")
	}
	if info.Protocol == rdp.ProtocolSSL {
		// The person met the Windows sign-in screen, unauthenticated until
		// they typed something into it.
		flags = append(flags, "no-network-level-auth")
	}
	return flags
}

// connectDesktop dials the host's Remote Desktop listener as the account.
//
// No certificate pin: the connection goes to the loopback address and never
// leaves the host, and only something able to take over port 3389 -- which
// is to say an administrator, who could read the recording anyway -- could
// answer in the listener's place. Windows renews the listener's self-signed
// certificate on its own, so a pin would refuse every session after that
// for no gain.
func connectDesktop(cfg DesktopConfig, principal string, open tunnel.DesktopOpen,
	log *slog.Logger) (DesktopClient, DesktopInfo, error) {

	addr := cfg.Address
	if addr == "" {
		addr = DefaultDesktopAddress
	}
	protocol := rdp.ProtocolHybrid
	var auth *credssp.Authenticator
	var logon rdp.LogonInfo
	if open.Password != "" {
		auth = &credssp.Authenticator{Credentials: credssp.Credentials{
			User: principal, Password: open.Password, Workstation: "ARGUS",
		}}
		logon = rdp.LogonInfo{User: principal, Password: open.Password}
	} else {
		protocol = rdp.ProtocolSSL
		logon = rdp.LogonInfo{User: principal}
	}
	conn, result, err := rdp.DialTargetWithAuth(addr, "", protocol, nil, 15*time.Second, auth)
	if err != nil && auth != nil && !isAuthFailure(err) {
		// A listener that will not do CredSSP still takes TLS, with the
		// password in the client info packet.
		log.Warn("tunnel: CredSSP to the local desktop failed, retrying over TLS", "error", err)
		protocol = rdp.ProtocolSSL
		conn, result, err = rdp.DialTargetWithAuth(addr, "", protocol, nil, 15*time.Second, nil)
	}
	if err != nil {
		if strings.Contains(err.Error(), "network level authentication") && open.Password == "" {
			return nil, DesktopInfo{}, errors.New("Remote Desktop on this host requires network " +
				"level authentication: enter the account's password to sign in")
		}
		if strings.Contains(err.Error(), "dial ") {
			return nil, DesktopInfo{}, fmt.Errorf("Remote Desktop is not reachable on this host "+
				"(%s): check it is turned on", addr)
		}
		return nil, DesktopInfo{}, err
	}
	info := DesktopInfo{Protocol: protocol}
	if result != nil {
		info.LegacyBinding = result.LegacyBinding
	}
	width, height := desktopSize(open)
	client, err := rdp.Connect(conn, rdp.ClientConfig{
		Width: width, Height: height, Depth: 16,
		SelectedProtocol: protocol,
		Logon:            logon,
		Log:              log,
	})
	if err != nil {
		_ = conn.Close()
		return nil, DesktopInfo{}, err
	}
	return client, info, nil
}

// isAuthFailure is a CredSSP refusal of the credential itself, which TLS
// with the same password will not get past either.
func isAuthFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "logon failure") || strings.Contains(s, "STATUS_LOGON_FAILURE") ||
		strings.Contains(s, "access denied") || strings.Contains(s, "password")
}

// desktopSize is what the browser asked for, within what Argus offers.
func desktopSize(open tunnel.DesktopOpen) (int, int) {
	w, h := open.Width, open.Height
	if w <= 0 || h <= 0 {
		return 1280, 800
	}
	w, h = min(max(w, 640), 2560), min(max(h, 480), 1600)
	// RDP wants an even width; many servers round it down to a multiple of
	// four anyway, and a mismatch shows as a sheared screen.
	return w &^ 3, h
}

// desktopInput is a browser input message, as the console sends it.
type desktopInput struct {
	Type   string `json:"type"`
	Code   string `json:"code"`
	Down   bool   `json:"down"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Button int    `json:"button"`
	Delta  int    `json:"delta"`
}

// runDesktop relays until the desktop or the stream ends, then seals the
// recording and says how it ended.
func runDesktop(st tunnel.Stream, client DesktopClient, d *DesktopRecording, log *slog.Logger) {
	var wmu sync.Mutex
	write := func(typ byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return tunnel.WriteFrame(st, typ, p)
	}

	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		// The browser going away ends the desktop connection; Windows keeps
		// the account's session, disconnected, as it does for mstsc.
		defer client.Close()
		fr := tunnel.NewFrameReader(st)
		for {
			typ, p, err := fr.Next()
			if err != nil {
				return
			}
			switch typ {
			case tunnel.FrameInput:
				var m desktopInput
				if json.Unmarshal(p, &m) != nil {
					continue
				}
				e := rdp.InputEvent{Kind: m.Type, Down: m.Down, X: m.X, Y: m.Y,
					Button: m.Button, WheelDelta: m.Delta}
				if m.Type == "key" {
					code, extended, ok := rdp.ScancodeFor(m.Code)
					if !ok {
						// Scancode zero is a real key; an unmapped one is dropped.
						continue
					}
					e.Scancode, e.Extended = code, extended
				}
				if client.Send(e) != nil {
					return
				}
			case tunnel.FrameRefresh:
				if err := client.Refresh(); err != nil {
					log.Warn("tunnel: could not ask the desktop to repaint", "session", d.ID(), "error", err)
				}
			case tunnel.FrameTerminate:
				if k, ok := tunnel.ParseTerminate(p); ok {
					log.Warn("tunnel: desktop ended from the console", "session", d.ID(),
						"by", k.By, "reason", k.Reason)
					d.Terminate(k.By, k.Reason)
					return
				}
			}
		}
	}()

	width, height := client.Size()
	err := write(tunnel.FrameResize, tunnel.ResizePayload(width, height))
	for err == nil {
		var bitmaps []rdp.Bitmap
		bitmaps, err = client.NextBitmaps()
		if err != nil || len(bitmaps) == 0 {
			continue
		}
		batch := make([]byte, 0, 4096)
		for _, b := range bitmaps {
			f := rdp.EncodeBitmap(b)
			if len(batch)+len(f) > desktopBatch && len(batch) > 0 {
				if err = write(tunnel.FrameOutput, batch); err != nil {
					break
				}
				batch = make([]byte, 0, 4096)
			}
			batch = append(batch, f...)
		}
		if err == nil && len(batch) > 0 {
			err = write(tunnel.FrameOutput, batch)
		}
	}
	_ = client.Close()
	log.Info("tunnel: desktop closed", "session", d.ID(), "reason", err)
	head := d.Seal()
	_ = write(tunnel.FrameExit, tunnel.ExitPayload(0, head))
	// Closed before waiting: the input loop may be blocked reading a browser
	// that is still there.
	_ = st.Close()
	<-inputDone
}
