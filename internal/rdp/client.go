package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Client drives the RDP connection sequence and then the session.
//
// The sequence is strictly ordered and every step depends on the last, so this
// reads as a straight line rather than a state machine. That is deliberate: the
// interesting failures are all "the server stopped answering at step N", and a
// straight line makes N obvious in a stack trace.
type Client struct {
	conn   net.Conn
	log    *slog.Logger
	userID uint16
	// channel is the global channel carrying graphics and input.
	channel uint16
	shareID uint32
	rea     *Reassembler

	width, height, depth int

	// tap receives every PDU read from the target.
	//
	// Recording happens here rather than on the decoded rectangles so both
	// paths — a native client proxied through Argus, and a browser driven by
	// Argus as the client — produce the same artefact. A recording that
	// depended on which door the user came in by would be two formats wearing
	// one extension.
	tap func([]byte)

	// readTimeout bounds a single read from the target.
	//
	// Without one, a server that stops sending pins a goroutine and a socket
	// for as long as the process runs — and a silent server is exactly what a
	// hung session looks like, so it is also the case an operator most needs
	// reported rather than absorbed.
	readTimeout time.Duration

	// wmu orders writes: input comes from one goroutine, and the replies the
	// graphics pipeline needs from the one reading.
	wmu sync.Mutex

	// The graphics pipeline, when it was asked for.
	graphics   func(GraphicsPDU)
	dynvc      uint16 // MCS channel of drdynvc
	gfx        graphicsChannel
	gfxDecoded uint32
}

// ClientConfig configures a session.
type ClientConfig struct {
	Width  int
	Height int
	// Depth is 16 or 24. Higher is not offered; see gcc.go.
	Depth int
	// Logon carries the credentials for the TLS-only path, where CredSSP has
	// not already authenticated. Empty when CredSSP succeeded.
	Logon LogonInfo
	// SelectedProtocol is echoed to the server so it can confirm the two sides
	// agree about what was negotiated.
	SelectedProtocol uint32
	// ReadTimeout bounds a single read once the session is live. Zero means
	// two minutes, which is longer than any legitimate gap between updates on
	// an idle desktop and short enough to notice a wedged server.
	ReadTimeout time.Duration
	Log         *slog.Logger
	// Graphics, when set, asks for the graphics pipeline and receives its
	// commands as they arrive. Windows then stops sending bitmap updates, so
	// NextBitmaps returns none and everything drawn comes through here.
	Graphics func(GraphicsPDU)
}

// Connect runs the sequence on an established, authenticated connection.
//
// conn must already carry a completed TLS handshake and, where the target
// offered it, a completed CredSSP exchange. This function assumes the transport
// is trustworthy because everything that establishes that has already run.
func Connect(conn net.Conn, cfg ClientConfig) (*Client, error) {
	if cfg.Width == 0 {
		cfg.Width = 1024
	}
	if cfg.Height == 0 {
		cfg.Height = 768
	}
	if cfg.Depth == 0 {
		cfg.Depth = 16
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 2 * time.Minute
	}

	c := &Client{
		conn: conn, log: cfg.Log, rea: NewReassembler(),
		width: cfg.Width, height: cfg.Height, depth: cfg.Depth,
		readTimeout: cfg.ReadTimeout,
		graphics:    cfg.Graphics,
	}
	if c.graphics != nil {
		c.depth = 32
		c.gfx.zgfx = NewDecompressor()
	}

	// A server that stops answering mid-sequence would otherwise hold the
	// goroutine and the socket indefinitely.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := c.mcsConnect(cfg); err != nil {
		return nil, err
	}
	if err := c.mcsAttach(); err != nil {
		return nil, err
	}
	if err := c.sendClientInfo(cfg.Logon); err != nil {
		return nil, err
	}
	if err := c.licensing(); err != nil {
		return nil, err
	}
	if err := c.capabilities(); err != nil {
		return nil, err
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}

	// The sequence is done. Reads from here are bounded per call rather than
	// by one overall deadline, since a session has no total duration.
	_ = conn.SetDeadline(time.Time{})
	c.log.Info("rdp session established",
		"width", c.width, "height", c.height, "depth", c.depth)
	return c, nil
}

func (c *Client) mcsConnect(cfg ClientConfig) error {
	gcc := GCCRequest(ClientInfo{
		Width: cfg.Width, Height: cfg.Height, Depth: c.depth,
		SelectedProtocol: cfg.SelectedProtocol, Graphics: c.graphics != nil,
	})
	if _, err := c.conn.Write(x224Data(ConnectInitial(gcc))); err != nil {
		return fmt.Errorf("send mcs connect initial: %w", err)
	}

	frame, err := ReadPDU(c.conn)
	if err != nil {
		return fmt.Errorf("read mcs connect response: %w", err)
	}
	userData, err := ParseConnectResponse(frame)
	if err != nil {
		return err
	}
	channels, err := ParseServerNetworkData(userData)
	if err != nil {
		return err
	}
	c.channel = channels.GlobalChannel
	if c.graphics != nil {
		if len(channels.Extra) == 0 || channels.Extra[0] == 0 {
			return errors.New("the server did not open drdynvc, so the graphics pipeline is unavailable")
		}
		c.dynvc = channels.Extra[0]
		c.log.Info("rdp graphics: drdynvc opened", "channel", c.dynvc)
	}
	return nil
}

func (c *Client) mcsAttach() error {
	if _, err := c.conn.Write(ErectDomainRequest()); err != nil {
		return fmt.Errorf("send erect domain: %w", err)
	}
	if _, err := c.conn.Write(AttachUserRequest()); err != nil {
		return fmt.Errorf("send attach user: %w", err)
	}

	frame, err := ReadPDU(c.conn)
	if err != nil {
		return fmt.Errorf("read attach user confirm: %w", err)
	}
	c.userID, err = ParseAttachUserConfirm(frame)
	if err != nil {
		return err
	}

	// The user channel is joined before the global one; a server that receives
	// them the other way round refuses the second.
	join := []uint16{c.userID, c.channel}
	if c.dynvc != 0 {
		join = append(join, c.dynvc)
	}
	for _, ch := range join {
		if _, err := c.conn.Write(ChannelJoinRequest(c.userID, ch)); err != nil {
			return fmt.Errorf("send channel join %d: %w", ch, err)
		}
		frame, err := ReadPDU(c.conn)
		if err != nil {
			return fmt.Errorf("read channel join confirm %d: %w", ch, err)
		}
		joined, err := ParseChannelJoinConfirm(frame)
		if err != nil {
			return err
		}
		if joined != ch {
			return fmt.Errorf("%w: asked to join %d, server confirmed %d", ErrMCS, ch, joined)
		}
	}
	return nil
}

func (c *Client) sendClientInfo(logon LogonInfo) error {
	pdu := SendDataRequest(c.userID, c.channel, ClientInfoPDU(logon))
	if _, err := c.conn.Write(pdu); err != nil {
		return fmt.Errorf("send client info: %w", err)
	}
	return nil
}

// licensing consumes the licensing exchange.
//
// Against a host that is not a licensing server this is one message saying no
// license is required — an "error alert" whose meaning is success.
func (c *Client) licensing() error {
	for range 4 {
		frame, err := ReadPDU(c.conn)
		if err != nil {
			return fmt.Errorf("read licensing: %w", err)
		}
		_, payload, err := ParseSendDataIndication(frame)
		if err != nil {
			return err
		}

		// Some servers skip licensing entirely and send the demand active
		// straight away. Recording the share id and returning lets the next
		// step notice it has already arrived, rather than waiting for a PDU
		// that has been and gone.
		if sc, serr := ParseShareControl(payload); serr == nil && sc.Type == pduTypeDemandActive {
			demand, derr := ParseDemandActive(sc.Body)
			if derr != nil {
				return derr
			}
			c.shareID = demand.ShareID
			return nil
		}

		result, err := ParseLicensing(payload)
		if err != nil {
			return err
		}
		if result == LicenseComplete {
			return nil
		}
	}
	return errors.New("licensing did not conclude after four messages")
}

func (c *Client) capabilities() error {
	if c.shareID != 0 {
		// The demand arrived during licensing, which some servers do.
		return c.sendConfirmActive()
	}

	for range 8 {
		frame, err := ReadPDU(c.conn)
		if err != nil {
			return fmt.Errorf("read demand active: %w", err)
		}
		ch, payload, err := ParseSendDataIndication(frame)
		if err != nil {
			return err
		}
		if c.dynvc != 0 && ch == c.dynvc {
			if err := c.virtual(payload); err != nil {
				return err
			}
			continue
		}
		sc, err := ParseShareControl(payload)
		if err != nil {
			continue
		}
		switch sc.Type {
		case pduTypeDemandActive:
			demand, derr := ParseDemandActive(sc.Body)
			if derr != nil {
				return derr
			}
			c.shareID = demand.ShareID
			return c.sendConfirmActive()
		case pduTypeData:
			// An error info PDU here names why the server is refusing, which is
			// far more useful than the closed connection that follows it.
			if sd, serr := ParseShareData(sc.Body); serr == nil && sd.Type2 == pduType2ErrorInfo {
				return fmt.Errorf("target refused the session: %s", ErrorInfo(sd.Body))
			}
		}
	}
	return errors.New("the server never sent a demand active PDU")
}

func (c *Client) sendConfirmActive() error {
	pdu := ConfirmActivePDU(c.shareID, c.userID, c.width, c.height, c.depth)
	if _, err := c.conn.Write(SendDataRequest(c.userID, c.channel, pdu)); err != nil {
		return fmt.Errorf("send confirm active: %w", err)
	}
	return nil
}

// finalize sends the four PDUs a server waits for before it will draw anything.
func (c *Client) finalize() error {
	for _, pdu := range [][]byte{
		SynchronizePDU(c.shareID, c.userID),
		ControlPDU(c.shareID, c.userID, controlActionCooperate),
		ControlPDU(c.shareID, c.userID, controlActionRequest),
		FontListPDU(c.shareID, c.userID),
	} {
		if _, err := c.conn.Write(SendDataRequest(c.userID, c.channel, pdu)); err != nil {
			return fmt.Errorf("send finalization pdu: %w", err)
		}
	}
	return nil
}

/* ── Session ─────────────────────────────────────────────────────────────── */

// Next reads the next batch of screen rectangles.
//
// Returns no rectangles and no error for traffic that is not a bitmap update,
// so a caller loops on it without having to know what else the server sends.
func (c *Client) Next() ([]Rect, error) {
	bitmaps, err := c.NextBitmaps()
	if err != nil || len(bitmaps) == 0 {
		return nil, err
	}
	return decodeAll(bitmaps)
}

// NextBitmaps is Next without the decoding, for a caller that forwards the
// rectangles to something that decodes them itself.
func (c *Client) NextBitmaps() ([]Bitmap, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return nil, err
	}
	frame, err := ReadPDU(c.conn)
	if err != nil {
		return nil, err
	}
	if c.tap != nil {
		c.tap(frame)
	}
	if c.dynvc != 0 && frame[0]&actionMask == actionX224 {
		if ch, payload, err := ParseSendDataIndication(frame); err == nil && ch == c.dynvc {
			return nil, c.virtual(payload)
		}
	}
	return ParsePDU(c.rea, frame)
}

// virtual follows drdynvc, answering what it must and handing graphics
// commands to the caller.
func (c *Client) virtual(pdu []byte) error {
	ev, pdus, err := c.gfx.feed(pdu)
	if ev.note != "" {
		c.log.Info("rdp graphics: "+ev.note, "opened", ev.opened)
	}
	if ev.reply != nil {
		if err := c.writeVirtual(ev.reply); err != nil {
			return err
		}
	}
	if ev.opened {
		c.log.Info("rdp graphics: offering capabilities", "pdu", fmt.Sprintf("%x", gfxCapsAdvertise()))
		if err := c.writeGraphics(gfxCapsAdvertise()); err != nil {
			return err
		}
	}
	if ev.data != nil && c.gfx.messages == 1 {
		c.log.Info("rdp graphics: the first message arrived on the graphics channel", "bytes", len(ev.data))
	}
	if err != nil {
		return err
	}
	for _, p := range pdus {
		if p.Cmd == GfxCapsConfirm && len(p.Body) >= 4 {
			c.log.Info("rdp graphics: the server confirmed capabilities",
				"version", fmt.Sprintf("0x%08x", binary.LittleEndian.Uint32(p.Body[0:4])))
		}
		c.graphics(p)
		if p.Cmd == GfxEndFrame {
			c.gfxDecoded++
			if err := c.writeGraphics(gfxFrameAcknowledge(p.FrameID, c.gfxDecoded)); err != nil {
				return err
			}
		}
	}
	return nil
}

// GraphicsChannel is the MCS channel the graphics pipeline rides on, inside
// drdynvc, or zero when it was not asked for. A recording needs it to find
// the pipeline again.
func (c *Client) GraphicsChannel() uint16 { return c.dynvc }

func (c *Client) writeGraphics(msg []byte) error {
	for _, m := range dvcMessages(c.gfx.dynamic.graphics, c.gfx.dynamic.cbID, msg) {
		if err := c.writeVirtual(m); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) writeVirtual(msg []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for _, pdu := range channelPDUs(msg) {
		if _, err := c.conn.Write(SendDataRequest(c.userID, c.dynvc, pdu)); err != nil {
			return fmt.Errorf("send on drdynvc: %w", err)
		}
	}
	return nil
}

// DecodePDU turns one PDU into screen rectangles.
//
// Shared by the live path and by replay, which is the whole reason a recording
// can be trusted to show what the operator saw. Replay once had its own version
// that handled only the fast path; the live session drew through the slow one,
// so every recording decoded to an empty screen while reporting success.
//
// Returns no rectangles and no error for traffic that is not a screen update,
// so a caller loops on it without having to know what else a server sends.
func DecodePDU(rea *Reassembler, frame []byte) ([]Rect, error) {
	bitmaps, err := ParsePDU(rea, frame)
	if err != nil || len(bitmaps) == 0 {
		return nil, err
	}
	return decodeAll(bitmaps)
}

// ParsePDU is DecodePDU without the decoding: the rectangles a PDU carries,
// with their headers checked and their pixel data as the target sent it.
func ParsePDU(rea *Reassembler, frame []byte) ([]Bitmap, error) {
	if len(frame) == 0 {
		return nil, nil
	}

	// Fast-Path carries most updates; anything TPKT-framed at this point is a
	// share-control PDU, which may be a slow-path update or an error the
	// operator needs to see.
	if frame[0]&actionMask == actionX224 {
		_, payload, perr := ParseSendDataIndication(frame)
		if perr != nil {
			return nil, nil
		}
		sc, scerr := ParseShareControl(payload)
		if scerr != nil {
			return nil, nil
		}
		if sc.Type == pduTypeDeactivateAll {
			return nil, errors.New("the server deactivated the session")
		}
		sd, sderr := ParseShareData(sc.Body)
		if sderr != nil {
			return nil, nil
		}
		switch sd.Type2 {
		case pduType2ErrorInfo:
			return nil, fmt.Errorf("session ended: %s", ErrorInfo(sd.Body))
		case pduType2Update:
			// The slow path. Servers use it when fast-path output was not
			// negotiated, and some send updates this way regardless.
			if len(sd.Body) < 2 {
				return nil, nil
			}
			if binary.LittleEndian.Uint16(sd.Body[0:2]) != updateTypeBitmap {
				return nil, nil
			}
			return ParseBitmaps(sd.Body[2:])
		}
		return nil, nil
	}

	return rea.FeedBitmaps(frame)
}

// Send delivers one input event to the target.
func (c *Client) Send(e InputEvent) error {
	pdu, err := EncodeInput(e)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(pdu)
	return err
}

// SetTap registers a function called with every PDU read from the target.
//
// Called before decoding, so the recording holds what arrived rather than what
// this build could make of it — a later version that understands more of the
// protocol can replay an older recording in more detail.
func (c *Client) SetTap(fn func([]byte)) { c.tap = fn }

// Size reports the desktop dimensions.
func (c *Client) Size() (width, height int) { return c.width, c.height }

// Close ends the session.
func (c *Client) Close() error { return c.conn.Close() }

// Refresh asks the target to resend the whole screen.
//
// Used when a viewer attaches to a session already running: updates are deltas,
// and without a redraw the newcomer sees only what changes from that moment on.
func (c *Client) Refresh() error {
	if c.shareID == 0 {
		return errors.New("session is not active")
	}
	pdu := RefreshRectPDU(c.shareID, c.userID, c.width, c.height)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(SendDataRequest(c.userID, c.channel, pdu))
	return err
}
