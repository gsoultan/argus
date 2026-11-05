package rdp

import (
	"encoding/binary"
	"image"
)

// Graphics pipeline commands made here rather than read: what screen
// sharing sends when it encodes the console's motion as H.264 itself. They
// are the commands a Windows Remote Desktop server sends, so the browser's
// renderer, the recording and the replay are the ones Remote Desktop already
// has.

// gfxPixelXRGB is PIXEL_FORMAT_XRGB_8888 (MS-RDPEGFX 2.2.1.4).
const gfxPixelXRGB = 0x20

// gfxResetSize is the whole size of a ResetGraphics command: the spec fixes
// it at 340 bytes whatever the number of monitors.
const gfxResetSize = 340

// BuildResetGraphics says the screen is width by height, one monitor.
func BuildResetGraphics(width, height int) []byte {
	b := make([]byte, 0, gfxResetSize-gfxHeaderSize)
	b = binary.LittleEndian.AppendUint32(b, uint32(width))
	b = binary.LittleEndian.AppendUint32(b, uint32(height))
	b = binary.LittleEndian.AppendUint32(b, 1) // monitorCount
	// The monitor: left, top, right, bottom (inclusive), primary.
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(width-1))
	b = binary.LittleEndian.AppendUint32(b, uint32(height-1))
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = b[:gfxResetSize-gfxHeaderSize]
	return gfxPDU(GfxResetGraphics, b)
}

// BuildCreateSurface makes a surface of width by height.
func BuildCreateSurface(surface uint16, width, height int) []byte {
	b := binary.LittleEndian.AppendUint16(nil, surface)
	b = binary.LittleEndian.AppendUint16(b, uint16(width))
	b = binary.LittleEndian.AppendUint16(b, uint16(height))
	return gfxPDU(GfxCreateSurface, append(b, gfxPixelXRGB))
}

// BuildMapSurfaceToOutput puts a surface on screen at x, y.
func BuildMapSurfaceToOutput(surface uint16, x, y int) []byte {
	b := binary.LittleEndian.AppendUint16(nil, surface)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint32(b, uint32(x))
	b = binary.LittleEndian.AppendUint32(b, uint32(y))
	return gfxPDU(GfxMapSurfaceToOutput, b)
}

// BuildStartFrame begins frame id.
func BuildStartFrame(id uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 0) // timestamp, unused here
	return gfxPDU(GfxStartFrame, binary.LittleEndian.AppendUint32(b, id))
}

// BuildEndFrame ends frame id.
func BuildEndFrame(id uint32) []byte {
	return gfxPDU(GfxEndFrame, binary.LittleEndian.AppendUint32(nil, id))
}

// BuildAVC420 draws one H.264 picture of width by height onto a surface,
// copying only regions of it to the screen: the parts that changed. No
// regions means none; a key frame's caller passes the whole screen.
func BuildAVC420(surface uint16, width, height int, regions []image.Rectangle, h264 []byte) []byte {
	// RFX_AVC420_BITMAP_STREAM (MS-RDPEGFX 2.2.4.4): the regions, a
	// quantisation and quality for each, then the stream.
	meta := make([]byte, 0, 4+len(regions)*10)
	meta = binary.LittleEndian.AppendUint32(meta, uint32(len(regions)))
	for _, r := range regions {
		meta = binary.LittleEndian.AppendUint16(meta, uint16(r.Min.X))
		meta = binary.LittleEndian.AppendUint16(meta, uint16(r.Min.Y))
		meta = binary.LittleEndian.AppendUint16(meta, uint16(r.Max.X))
		meta = binary.LittleEndian.AppendUint16(meta, uint16(r.Max.Y))
	}
	for range regions {
		meta = append(meta, 22, 100) // qp, quality: informational only
	}

	b := make([]byte, 0, 17+len(meta)+len(h264))
	b = binary.LittleEndian.AppendUint16(b, surface)
	b = binary.LittleEndian.AppendUint16(b, CodecAVC420)
	b = append(b, gfxPixelXRGB)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(width))
	b = binary.LittleEndian.AppendUint16(b, uint16(height))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(meta)+len(h264)))
	b = append(b, meta...)
	b = append(b, h264...)
	return gfxPDU(GfxWireToSurface1, b)
}

// GraphicsTap writes graphics commands into a recording as the PDUs a
// Remote Desktop server would have sent them in: on drdynvc's channel, in
// the graphics pipeline's dynamic channel, as uncompressed RDP8 bulk data.
// Replay reads them back with the same code that reads a Remote Desktop
// session's, so there is one recording format and one way to play it.
type GraphicsTap struct {
	channel uint16
	opened  bool
}

// graphicsTapID is the dynamic channel id the tap opens.
const graphicsTapID = 1

// gfxTapChunk is how much each recorded PDU carries: well under what a
// TPKT and a PER length can say, and far fewer lines than the 1600 bytes a
// live channel is held to.
const gfxTapChunk = 15000

// NewGraphicsTap records on the MCS channel named channel, which is what
// the recording's header must give as Header.Graphics.
func NewGraphicsTap(channel uint16) *GraphicsTap {
	return &GraphicsTap{channel: channel}
}

// PDUs returns what to record for commands, in order. The first call opens
// the channel first.
func (g *GraphicsTap) PDUs(commands ...[]byte) [][]byte {
	var msgs [][]byte
	if !g.opened {
		g.opened = true
		msgs = append(msgs,
			[]byte{dvcCapability << 4, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			append([]byte{dvcCreate << 4, graphicsTapID}, append([]byte(graphicsChannelName), 0)...))
	}
	n := 2
	for _, c := range commands {
		n += len(c)
	}
	seg := make([]byte, 0, n)
	seg = append(seg, zgfxSingle, zgfxTypeRDP8)
	for _, c := range commands {
		seg = append(seg, c...)
	}
	msgs = append(msgs, dvcMessagesOf(graphicsTapID, 0, seg, gfxTapChunk-6)...)

	var out [][]byte
	for _, m := range msgs {
		for _, pdu := range channelPDUsOf(m, gfxTapChunk) {
			out = append(out, sendDataIndication(g.channel, pdu))
		}
	}
	return out
}

// sendDataIndication wraps data as the server sends it on channel.
func sendDataIndication(channel uint16, data []byte) []byte {
	frame := SendDataRequest(userChannelBase+7, channel, data)
	// The same PDU but for its tag: request and indication differ only in
	// which way they go.
	frame[tpktHeader+3] = mcsSendDataIndication << 2
	return frame
}
