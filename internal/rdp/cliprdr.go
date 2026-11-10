package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

// The clipboard channel (MS-RDPECLIP), text only.
//
// Copy on the server and the text reaches the browser; copy in the browser
// and it is on the server's clipboard to paste. Files and pictures are not
// offered: files cross through the Files drawer, where each one is recorded
// with its name and hash, and a picture is not worth a second way out.
//
// Short format names only: this client does not set CB_USE_LONG_FORMAT_NAMES,
// so both sides use the 32-byte names of the original protocol, which keeps
// the format list a fixed-size array.

const (
	// cliprdrName is the static channel the clipboard rides on.
	cliprdrName = "cliprdr"
	// cliprdrOptions: initialised, encrypted, compressed if the server
	// likes, and shown to the server's protocol, as mstsc requests it.
	cliprdrOptions uint32 = 0x80000000 | 0x40000000 | 0x00800000 | 0x00200000

	cbMonitorReady       = 0x0001
	cbFormatList         = 0x0002
	cbFormatListResponse = 0x0003
	cbFormatDataRequest  = 0x0004
	cbFormatDataResponse = 0x0005
	cbClipCaps           = 0x0007

	cbResponseOK   = 0x0001
	cbResponseFail = 0x0002

	cbCapsTypeGeneral = 0x0001
	cbCapsVersion2    = 0x00000002

	// cfUnicodeText is CF_UNICODETEXT: UTF-16LE, NUL terminated.
	cfUnicodeText = 13

	// ClipboardMax bounds one clipboard's text either way, in UTF-8 bytes.
	// A clipboard holding more is not something to paste through a screen,
	// and this keeps one crossing inside a tunnel frame (tunnel.MaxFrame).
	ClipboardMax = 128 << 10
)

// ErrClipboard is a malformed clipboard message.
var ErrClipboard = errors.New("rdp: clipboard")

// clipboard follows the clipboard channel for one session.
type clipboard struct {
	in staticChannel
	// local is the text this side last offered, for the server to ask for.
	local string
	// ready is set once the server said the channel is up; text set before
	// it is offered then.
	ready bool
}

// clipEvent is what one message from the server asks of this side.
type clipEvent struct {
	// replies go back to the server, in order.
	replies [][]byte
	// text is the server's clipboard, when it has just arrived.
	text  string
	gotIt bool
}

func clipMessage(msgType, flags uint16, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint16(b[0:2], msgType)
	binary.LittleEndian.PutUint16(b[2:4], flags)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(body)))
	return append(b, body...)
}

// clipCaps says this side speaks version 2 with short format names.
func clipCaps() []byte {
	b := make([]byte, 0, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // cCapabilitiesSets
	b = binary.LittleEndian.AppendUint16(b, 0) // pad
	b = binary.LittleEndian.AppendUint16(b, cbCapsTypeGeneral)
	b = binary.LittleEndian.AppendUint16(b, 12) // lengthCapability
	b = binary.LittleEndian.AppendUint32(b, cbCapsVersion2)
	b = binary.LittleEndian.AppendUint32(b, 0) // generalFlags: short names
	return clipMessage(cbClipCaps, 0, b)
}

// formatList offers text, or nothing when this side has none.
func formatList(hasText bool) []byte {
	if !hasText {
		return clipMessage(cbFormatList, 0, nil)
	}
	b := make([]byte, 36) // formatId, then a 32-byte name left empty
	binary.LittleEndian.PutUint32(b[0:4], cfUnicodeText)
	return clipMessage(cbFormatList, 0, b)
}

func encodeClipText(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2*len(u)+2)
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return append(b, 0, 0)
}

func decodeClipText(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// set makes s this side's clipboard and, once the channel is up, tells the
// server there is text to paste. It returns what to send.
func (c *clipboard) set(s string) ([]byte, error) {
	if len(s) > ClipboardMax {
		return nil, fmt.Errorf("%w: %d bytes is more than a clipboard carries here", ErrClipboard, len(s))
	}
	c.local = s
	if !c.ready {
		return nil, nil
	}
	return formatList(true), nil
}

// feed takes one whole clipboard message from the server.
func (c *clipboard) feed(msg []byte) (clipEvent, error) {
	var ev clipEvent
	if len(msg) < 8 {
		return ev, fmt.Errorf("%w: header is truncated", ErrClipboard)
	}
	msgType := binary.LittleEndian.Uint16(msg[0:2])
	flags := binary.LittleEndian.Uint16(msg[2:4])
	n := int(binary.LittleEndian.Uint32(msg[4:8]))
	if n > len(msg)-8 {
		return ev, fmt.Errorf("%w: body of %d bytes is cut short", ErrClipboard, n)
	}
	body := msg[8 : 8+n]

	switch msgType {
	case cbMonitorReady:
		// The server is listening: say what this side speaks and what it
		// holds, as a client that has just started.
		c.ready = true
		ev.replies = append(ev.replies, clipCaps(), formatList(c.local != ""))

	case cbFormatList:
		// Something was copied on the server. Acknowledge, and ask for it
		// as text if it can be had as text.
		ev.replies = append(ev.replies, clipMessage(cbFormatListResponse, cbResponseOK, nil))
		for o := 0; o+36 <= len(body); o += 36 {
			if binary.LittleEndian.Uint32(body[o:o+4]) == cfUnicodeText {
				req := binary.LittleEndian.AppendUint32(nil, cfUnicodeText)
				ev.replies = append(ev.replies, clipMessage(cbFormatDataRequest, 0, req))
				break
			}
		}

	case cbFormatDataRequest:
		// Paste on the server: hand over this side's text, or say there is
		// none in the form asked for.
		if len(body) >= 4 && binary.LittleEndian.Uint32(body[0:4]) == cfUnicodeText && c.local != "" {
			ev.replies = append(ev.replies, clipMessage(cbFormatDataResponse, cbResponseOK, encodeClipText(c.local)))
		} else {
			ev.replies = append(ev.replies, clipMessage(cbFormatDataResponse, cbResponseFail, nil))
		}

	case cbFormatDataResponse:
		if flags&cbResponseOK != 0 {
			if len(body) > 4*ClipboardMax+2 {
				return ev, nil // too large to pass on; the server keeps it
			}
			if t := decodeClipText(body); len(t) <= ClipboardMax {
				ev.text, ev.gotIt = t, true
			}
		}
	}
	// Capabilities, format list responses and the rest need nothing back.
	return ev, nil
}
