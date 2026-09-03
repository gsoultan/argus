package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// tpkt wraps a payload in TPKT framing.
func tpkt(payload []byte) []byte {
	out := make([]byte, tpktHeader+len(payload))
	out[0] = tpktVersion
	binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
	copy(out[tpktHeader:], payload)
	return out
}

// connectionRequest builds a client opening PDU the way mstsc does.
func connectionRequest(cookie string, protocols uint32, withNeg bool) []byte {
	var v bytes.Buffer
	if cookie != "" {
		v.WriteString("Cookie: mstshash=" + cookie + "\r\n")
	}
	if withNeg {
		neg := make([]byte, negLength)
		neg[0] = negTypeRequest
		binary.LittleEndian.PutUint16(neg[2:4], negLength)
		binary.LittleEndian.PutUint32(neg[4:8], protocols)
		v.Write(neg)
	}

	x := make([]byte, 7+v.Len())
	x[0] = byte(len(x) - 1)
	x[1] = tpduConnectionRequest
	copy(x[7:], v.Bytes())
	return tpkt(x)
}

func TestReadPDUReturnsTheWholeFrame(t *testing.T) {
	frame := connectionRequest("ops", ProtocolHybrid, true)
	got, err := ReadPDU(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("ReadPDU: %v", err)
	}
	// Byte-identical, because a proxy forwards what it received rather than a
	// re-encoding of what it understood.
	if !bytes.Equal(got, frame) {
		t.Errorf("frame was altered:\n got %x\nwant %x", got, frame)
	}
}

func TestReadPDUReadsFramesBackToBack(t *testing.T) {
	a := connectionRequest("ops", ProtocolSSL, true)
	b := tpkt([]byte{0x02, tpduData, 0x80, 0xAA, 0xBB})
	r := bytes.NewReader(append(append([]byte{}, a...), b...))

	first, err := ReadPDU(r)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ReadPDU(r)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(first, a) || !bytes.Equal(second, b) {
		t.Error("frames did not round-trip in order")
	}
	if _, err := ReadPDU(r); !errors.Is(err, io.EOF) {
		t.Errorf("third read err = %v, want EOF", err)
	}
}

// The RDP listener is internet-facing and gets scanned continuously. Whatever
// arrives must be rejected without allocating on the peer's say-so.
func TestReadPDURejectsJunk(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"an HTTP request", []byte("GET / HTTP/1.1\r\n\r\n"), ErrNotTPKT},
		{"a TLS ClientHello", []byte{0x16, 0x03, 0x01, 0x00, 0x05}, ErrNotTPKT},
		{"zero bytes of header", []byte{0x03, 0x00, 0x00, 0x00}, ErrShortPDU},
		{"length below the header", []byte{0x03, 0x00, 0x00, 0x03}, ErrShortPDU},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadPDU(bytes.NewReader(tc.in)); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A length field larger than the bytes that follow must not hang or over-read;
// it should end as a short read.
func TestReadPDUTruncatedBody(t *testing.T) {
	frame := []byte{0x03, 0x00, 0x00, 0x20, 0x01, 0x02}
	_, err := ReadPDU(bytes.NewReader(frame))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want unexpected EOF", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want X224Kind
	}{
		{"connection request", connectionRequest("ops", ProtocolSSL, true), ConnectionRequest},
		{"connection confirm", BuildConnectionConfirm(ProtocolHybrid), ConnectionConfirm},
		{"data", tpkt([]byte{0x02, tpduData, 0x80}), Data},
		{"unknown code", tpkt([]byte{0x02, 0x10, 0x00}), Unknown},
		{"too short to classify", tpkt([]byte{0x00}), Unknown},
		{"not a frame at all", []byte{0x00}, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Errorf("Classify = %q, want %q", got, tc.want)
			}
		})
	}
}

// The length indicator is peer-supplied and used to slice. A value past the end
// of the frame must be refused rather than read.
func TestVariablePartCannotReadPastTheFrame(t *testing.T) {
	frame := connectionRequest("ops", ProtocolSSL, true)
	// Claim a much longer X.224 header than the frame contains.
	frame[tpktHeader] = 0xFF

	if _, ok := x224Variable(frame); ok {
		t.Error("an overlong length indicator was accepted")
	}
	if _, err := ParseConnectionRequest(frame); err == nil {
		t.Error("ParseConnectionRequest accepted an overlong length indicator")
	}
}

// The parsers run on bytes from an unauthenticated peer, before any policy has
// been applied. A panic here takes down the listener for everyone.
func FuzzParseConnectionRequest(f *testing.F) {
	f.Add(connectionRequest("ops:pay-01", ProtocolHybrid, true))
	f.Add(connectionRequest("", ProtocolRDP, false))
	f.Add(tpkt([]byte{0x06, tpduConnectionRequest, 0, 0, 0, 0, 0}))
	f.Add([]byte{0x03, 0x00, 0x00, 0x04})

	f.Fuzz(func(t *testing.T, frame []byte) {
		info, err := ParseConnectionRequest(frame)
		if err != nil {
			return
		}
		// A successful parse must not claim protocols from a structure it never
		// found; that would be a policy decision made on uninitialised data.
		if !info.HasNegotiation && info.RequestedProtocols != 0 {
			t.Fatalf("protocols %#x reported without a negotiation structure",
				info.RequestedProtocols)
		}
	})
}

func FuzzClassify(f *testing.F) {
	f.Add(connectionRequest("ops", ProtocolSSL, true))
	f.Add([]byte{0x03})
	f.Fuzz(func(t *testing.T, frame []byte) {
		_ = Classify(frame)
		_, _, _ = ParseConnectionConfirm(frame)
	})
}

// connectionRequestRaw builds a CR with a caller-supplied prefix line, for
// cases the cookie-only helper cannot express.
func connectionRequestRaw(prefix string, protocols uint32) []byte {
	var v bytes.Buffer
	v.WriteString(prefix)
	neg := make([]byte, negLength)
	neg[0] = negTypeRequest
	binary.LittleEndian.PutUint16(neg[2:4], negLength)
	binary.LittleEndian.PutUint32(neg[4:8], protocols)
	v.Write(neg)

	x := make([]byte, 7+v.Len())
	x[0] = byte(len(x) - 1)
	x[1] = tpduConnectionRequest
	copy(x[7:], v.Bytes())
	return tpkt(x)
}
