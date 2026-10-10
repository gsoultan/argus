//go:build wasip1

// Command argus-pixels-wasm is the RDP bitmap decoder, built for the browser.
//
// The gateway forwards screen updates as the target sent them; the console's
// RDP worker loads this module and expands them to RGBA. It is the same code
// the gateway uses to replay recordings (internal/rdp/pixels), so the live view
// and the replay cannot disagree about what a screen showed.
//
// Built by scripts/build-wasm.sh into web/src/workers/rdp-pixels.wasm. Without
// TinyGo it falls back to:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o web/src/workers/rdp-pixels.wasm ./cmd/argus-pixels-wasm
//
// The interface is three exports over linear memory, because anything richer
// costs a copy per rectangle:
//
//	input(n)               reserve n bytes for the next rectangle; returns where
//	decode(w, h, format)   decode the reserved bytes; returns where the RGBA is,
//	                       or 0 if the data was malformed
//	output()               the RGBA's length, w*h*4, for the last decode
//
// format is the frame header's format byte: colour depth in the low seven bits,
// 0x80 for RLE.
package main

import (
	"unsafe"

	"github.com/gsoultan/argus/internal/rdp/pixels"
)

var (
	in      []byte
	out     []byte
	decoder pixels.Decoder
)

func main() {}

//go:wasmexport input
func input(n int32) uint32 {
	if n < 0 {
		return 0
	}
	if cap(in) < int(n) {
		in = make([]byte, n)
	}
	in = in[:n]
	if n == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&in[0])))
}

//go:wasmexport decode
func decode(width, height, format int32) uint32 {
	px, err := decoder.Decode(in, int(width), int(height), int(format&0x7F), format&0x80 != 0)
	if err != nil || len(px) == 0 {
		out = nil
		return 0
	}
	out = px
	return uint32(uintptr(unsafe.Pointer(&px[0])))
}

//go:wasmexport output
func output() int32 { return int32(len(out)) }
