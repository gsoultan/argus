package rdp

import (
	"encoding/binary"
	"image"
)

/*
Moves of the screen's own pixels, as RDP draws them: the ScrBlt primary
drawing order (MS-RDPEGDI 2.2.2.2.1.1.2.7) in a Fast-Path orders update.

Screen sharing finds where part of the screen scrolled and sends the browser
a move of what it already shows, plus the strip that scrolled into view,
instead of every tile that changed. The recording keeps it as the order a
Remote Desktop server would send for the same scroll, so the replay, the
live view and the recording's chain read it as they read everything else.

Each order is written whole: the standard form with the type stated, every
field present, no bounds and no deltas, so it means the same read on its own
as after any other. That is also the only form read back. Remote Desktop
servers send no orders to Argus at all, as it advertises none (orderCaps), so
anything else in an orders update is left alone, as before.
*/

const (
	orderStandard   = 0x01
	orderTypeChange = 0x08
	orderScrBlt     = 0x02
	// scrBltFields is every one of ScrBlt's seven fields present.
	scrBltFields = 0x7F
	// ropSrcCopy is the raster operation that copies the source as it is.
	ropSrcCopy = 0xCC
	// scrBltSize is one order in that form: control flags, type, field
	// flags, four coordinates, the operation and the source's two.
	scrBltSize = 3 + 8 + 1 + 4
)

// FastPathCopies packs moves (Bitmaps with Copy set) into Fast-Path orders
// updates, as ScrBlt orders.
func FastPathCopies(copies []Bitmap) [][]byte {
	const most = (maxFastPath - 3 - 3 - 2) / scrBltSize
	var pdus [][]byte
	for len(copies) > 0 {
		n := min(len(copies), most)
		data := make([]byte, 2, 2+n*scrBltSize)
		binary.LittleEndian.PutUint16(data, uint16(n))
		for _, c := range copies[:n] {
			var o [scrBltSize]byte
			o[0], o[1], o[2] = orderStandard|orderTypeChange, orderScrBlt, scrBltFields
			binary.LittleEndian.PutUint16(o[3:5], uint16(int16(c.X)))
			binary.LittleEndian.PutUint16(o[5:7], uint16(int16(c.Y)))
			binary.LittleEndian.PutUint16(o[7:9], uint16(int16(c.Width)))
			binary.LittleEndian.PutUint16(o[9:11], uint16(int16(c.Height)))
			o[11] = ropSrcCopy
			binary.LittleEndian.PutUint16(o[12:14], uint16(int16(c.SrcX)))
			binary.LittleEndian.PutUint16(o[14:16], uint16(int16(c.SrcY)))
			data = append(data, o[:]...)
		}
		total := 3 + 3 + len(data)
		pdu := make([]byte, 0, total)
		pdu = append(pdu, 0x00, byte(total>>8)|0x80, byte(total))
		pdu = append(pdu, fastPathUpdateOrders|fragSingle<<4, 0, 0)
		binary.LittleEndian.PutUint16(pdu[4:6], uint16(len(data)))
		pdus = append(pdus, append(pdu, data...))
		copies = copies[n:]
	}
	return pdus
}

// parseScrBlts reads an orders update written by FastPathCopies. It stops
// at the first order in any other form, keeping the moves before it.
func parseScrBlts(data []byte) []Bitmap {
	if len(data) < 2 {
		return nil
	}
	n := int(binary.LittleEndian.Uint16(data))
	data = data[2:]
	var out []Bitmap
	for range n {
		if len(data) < scrBltSize || data[0] != orderStandard|orderTypeChange ||
			data[1] != orderScrBlt || data[2] != scrBltFields || data[11] != ropSrcCopy {
			break
		}
		i16 := func(at int) int { return int(int16(binary.LittleEndian.Uint16(data[at : at+2]))) }
		b := Bitmap{X: i16(3), Y: i16(5), Width: i16(7), Height: i16(9), SrcX: i16(12), SrcY: i16(14), Copy: true}
		data = data[scrBltSize:]
		if !CopyFits(b, MaxDimension, MaxDimension) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// CopyFits says a move lies inside a screen of width by height, source and
// destination both, and is up or down only, which is all a browser is sent.
func CopyFits(b Bitmap, width, height int) bool {
	screen := image.Rect(0, 0, width, height)
	dst := image.Rect(b.X, b.Y, b.X+b.Width, b.Y+b.Height)
	src := dst.Add(image.Pt(b.SrcX-b.X, b.SrcY-b.Y))
	return b.Width > 0 && b.Height > 0 && b.SrcX == b.X && b.SrcY != b.Y &&
		dst.In(screen) && src.In(screen) && b.SrcY-b.Y >= -0x8000 && b.SrcY-b.Y < 0x8000
}
