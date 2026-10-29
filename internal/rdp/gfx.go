package rdp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The graphics pipeline (MS-RDPEGFX).
//
// With it, Windows stops sending bitmap updates and draws onto surfaces
// instead, with a codec it picks per region: H.264 for what moves, lossless
// codecs for text, fills and copies for the cheap cases. H.264 is the reason
// to be here. Windows encodes it (on the GPU where there is one), and it can
// reach the browser untouched, where WebCodecs decodes it in hardware: a
// screen at a few megabits a second instead of tens, and no work in the
// agent.

// ErrGraphics reports a graphics pipeline PDU this client cannot read.
var ErrGraphics = errors.New("malformed graphics pipeline PDU")

// Graphics pipeline commands (MS-RDPEGFX 2.2.1.5).
const (
	GfxWireToSurface1        uint16 = 0x0001
	GfxWireToSurface2        uint16 = 0x0002
	GfxDeleteEncodingContext uint16 = 0x0003
	GfxSolidFill             uint16 = 0x0004
	GfxSurfaceToSurface      uint16 = 0x0005
	GfxSurfaceToCache        uint16 = 0x0006
	GfxCacheToSurface        uint16 = 0x0007
	GfxEvictCacheEntry       uint16 = 0x0008
	GfxCreateSurface         uint16 = 0x0009
	GfxDeleteSurface         uint16 = 0x000A
	GfxStartFrame            uint16 = 0x000B
	GfxEndFrame              uint16 = 0x000C
	GfxFrameAcknowledge      uint16 = 0x000D
	GfxResetGraphics         uint16 = 0x000E
	GfxMapSurfaceToOutput    uint16 = 0x000F
	GfxCacheImportOffer      uint16 = 0x0010
	GfxCacheImportReply      uint16 = 0x0011
	GfxCapsAdvertise         uint16 = 0x0012
	GfxCapsConfirm           uint16 = 0x0013
	GfxMapSurfaceToWindow    uint16 = 0x0015
	GfxQoeFrameAcknowledge   uint16 = 0x0016
	GfxMapSurfaceToScaled    uint16 = 0x0017
	GfxMapSurfaceToScaledWin uint16 = 0x0018
)

// Codecs in a WireToSurface command (MS-RDPEGFX 2.2.1.6).
const (
	CodecUncompressed uint16 = 0x0000
	CodecRemoteFX     uint16 = 0x0003
	CodecClearCodec   uint16 = 0x0008
	CodecProgressive  uint16 = 0x0009
	CodecPlanar       uint16 = 0x000A
	CodecAVC420       uint16 = 0x000B
	CodecAlpha        uint16 = 0x000C
	CodecAVC444       uint16 = 0x000E
	CodecAVC444v2     uint16 = 0x000F
)

// CodecName is a codec id as a person would say it.
func CodecName(id uint16) string {
	switch id {
	case CodecUncompressed:
		return "uncompressed"
	case CodecRemoteFX:
		return "remotefx"
	case CodecClearCodec:
		return "clearcodec"
	case CodecProgressive:
		return "progressive"
	case CodecPlanar:
		return "planar"
	case CodecAVC420:
		return "avc420"
	case CodecAlpha:
		return "alpha"
	case CodecAVC444:
		return "avc444"
	case CodecAVC444v2:
		return "avc444v2"
	}
	return fmt.Sprintf("codec-0x%04x", id)
}

const (
	// gfxCapVersion81 is the capability set offered. Version 8.1 with AVC420
	// is H.264 in its plain 4:2:0 form, the one a browser decodes as is.
	// Version 10 lets Windows choose AVC444, two streams a browser would have
	// to recombine; that is a later step, if colour fidelity calls for it.
	gfxCapVersion81     uint32 = 0x00080105
	gfxCapAVC420Enabled uint32 = 0x00000010

	gfxHeaderSize = 8
)

// GraphicsPDU is one graphics pipeline command, as the server sent it.
type GraphicsPDU struct {
	Cmd uint16
	// Body is the command without its eight-byte header. It aliases the
	// client's buffers and is valid only until the next read.
	Body []byte

	// Set for WireToSurface1.
	Surface uint16
	Codec   uint16
	Left    int
	Top     int
	Right   int
	Bottom  int
	// Bitmap is the codec's data within Body.
	Bitmap []byte

	// FrameID is set for StartFrame and EndFrame.
	FrameID uint32
}

// ParseGraphics splits a decompressed graphics message into its commands.
func ParseGraphics(msg []byte) ([]GraphicsPDU, error) {
	var out []GraphicsPDU
	for len(msg) > 0 {
		if len(msg) < gfxHeaderSize {
			return nil, fmt.Errorf("%w: %d bytes left, less than a header", ErrGraphics, len(msg))
		}
		cmd := binary.LittleEndian.Uint16(msg[0:2])
		n := int(binary.LittleEndian.Uint32(msg[4:8]))
		if n < gfxHeaderSize || n > len(msg) {
			return nil, fmt.Errorf("%w: command 0x%04x claims %d bytes, %d remain", ErrGraphics, cmd, n, len(msg))
		}
		p := GraphicsPDU{Cmd: cmd, Body: msg[gfxHeaderSize:n]}
		switch cmd {
		case GfxWireToSurface1:
			b := p.Body
			if len(b) < 17 {
				return nil, fmt.Errorf("%w: WireToSurface1 is truncated", ErrGraphics)
			}
			p.Surface = binary.LittleEndian.Uint16(b[0:2])
			p.Codec = binary.LittleEndian.Uint16(b[2:4])
			p.Left = int(binary.LittleEndian.Uint16(b[5:7]))
			p.Top = int(binary.LittleEndian.Uint16(b[7:9]))
			p.Right = int(binary.LittleEndian.Uint16(b[9:11]))
			p.Bottom = int(binary.LittleEndian.Uint16(b[11:13]))
			size := int(binary.LittleEndian.Uint32(b[13:17]))
			if size > len(b)-17 {
				return nil, fmt.Errorf("%w: WireToSurface1 claims %d bytes of %d", ErrGraphics, size, len(b)-17)
			}
			p.Bitmap = b[17 : 17+size]
		case GfxWireToSurface2:
			if len(p.Body) >= 4 {
				p.Surface = binary.LittleEndian.Uint16(p.Body[0:2])
				p.Codec = binary.LittleEndian.Uint16(p.Body[2:4])
			}
		case GfxStartFrame:
			if len(p.Body) < 8 {
				return nil, fmt.Errorf("%w: StartFrame is truncated", ErrGraphics)
			}
			p.FrameID = binary.LittleEndian.Uint32(p.Body[4:8])
		case GfxEndFrame:
			if len(p.Body) < 4 {
				return nil, fmt.Errorf("%w: EndFrame is truncated", ErrGraphics)
			}
			p.FrameID = binary.LittleEndian.Uint32(p.Body[0:4])
		}
		out = append(out, p)
		msg = msg[n:]
	}
	return out, nil
}

// gfxPDU wraps a client-to-server command. These are not compressed.
func gfxPDU(cmd uint16, body []byte) []byte {
	out := make([]byte, gfxHeaderSize, gfxHeaderSize+len(body))
	binary.LittleEndian.PutUint16(out[0:2], cmd)
	binary.LittleEndian.PutUint32(out[4:8], uint32(gfxHeaderSize+len(body)))
	return append(out, body...)
}

// gfxCapsAdvertise offers version 8.1 with AVC420.
func gfxCapsAdvertise() []byte {
	b := binary.LittleEndian.AppendUint16(nil, 1)
	b = binary.LittleEndian.AppendUint32(b, gfxCapVersion81)
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = binary.LittleEndian.AppendUint32(b, gfxCapAVC420Enabled)
	return gfxPDU(GfxCapsAdvertise, b)
}

// gfxFrameAcknowledge tells the server a frame is done with, which is what
// lets it send the next: a client that never acknowledges gets a few frames
// and then a frozen screen.
func gfxFrameAcknowledge(frameID, decoded uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 0) // queueDepth: not reported
	b = binary.LittleEndian.AppendUint32(b, frameID)
	b = binary.LittleEndian.AppendUint32(b, decoded)
	return gfxPDU(GfxFrameAcknowledge, b)
}
