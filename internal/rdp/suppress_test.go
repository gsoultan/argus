package rdp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The share data header is 18 bytes (share control 6, share data 12); the
// PDU type sits at byte 14 and the payload follows the header.
func TestSuppressOutputPDU(t *testing.T) {
	off := SuppressOutputPDU(7, 1001, false, 1920, 1080)
	if off[14] != pduType2SuppressOut {
		t.Fatalf("pdu type %d, want %d", off[14], pduType2SuppressOut)
	}
	if got := off[18:]; !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("pause payload %x", got)
	}

	on := SuppressOutputPDU(7, 1001, true, 1920, 1080)
	p := on[18:]
	if len(p) != 12 || p[0] != 1 {
		t.Fatalf("resume payload %x", p)
	}
	right, bottom := binary.LittleEndian.Uint16(p[8:]), binary.LittleEndian.Uint16(p[10:])
	if right != 1919 || bottom != 1079 {
		t.Fatalf("rectangle ends at %d,%d, want 1919,1079", right, bottom)
	}
	if n := binary.LittleEndian.Uint16(on[0:]); int(n) != len(on) {
		t.Fatalf("total length %d, PDU is %d bytes", n, len(on))
	}
}
