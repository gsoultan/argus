package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// The GCC blocks are what the server reads to decide which bitmap format to
// send. Getting a field's offset wrong produces a session that connects and
// then displays nothing, which is a slow way to find a layout bug.
func TestClientCoreDataLayout(t *testing.T) {
	req := GCCRequest(ClientInfo{
		Width: 1920, Height: 1080, Depth: 24,
		SelectedProtocol: ProtocolHybrid, Name: "argus",
	})

	body := findBlock(t, req, blockClientCore)

	if got := binary.LittleEndian.Uint32(body[0:4]); got != 0x00080004 {
		t.Errorf("version = %#x", got)
	}
	if got := binary.LittleEndian.Uint16(body[4:6]); got != 1920 {
		t.Errorf("width = %d", got)
	}
	if got := binary.LittleEndian.Uint16(body[6:8]); got != 1080 {
		t.Errorf("height = %d", got)
	}
	// The name lands in the server's session list, so it says what opened the
	// session rather than leaving an operator guessing.
	// version(4) width(2) height(2) depth(2) sas(2) layout(4) build(4) = 20
	name := body[20:52]
	if !bytes.HasPrefix(name, utf16leBytes("argus")) {
		t.Errorf("client name = %q", name)
	}
}

// 32-bit is deliberately not advertised: it needs the surface-command path
// rather than plain bitmap updates, and a server that took it up would send
// graphics this decoder does not read.
func TestSupportedDepthsExcludes32Bit(t *testing.T) {
	const rns32BPP = 0x0008
	if supportedDepths&rns32BPP != 0 {
		t.Error("32-bit colour is advertised but the bitmap decoder cannot read it")
	}
}

// Zero for both encryption methods is required over TLS or CredSSP. Claiming
// one would invite the server to use RDP's own encryption, which is the
// standard-security path Argus refuses at negotiation.
func TestClientSecurityDeclaresNoLegacyEncryption(t *testing.T) {
	b := clientSecurityData()
	if !bytes.Equal(b[4:], make([]byte, 8)) {
		t.Errorf("encryption methods = %x, want all zero", b[4:])
	}
}

// Clipboard and drive redirection ride on virtual channels, and they are the
// two features that turn a recorded session into an unrecorded file transfer.
func TestNoVirtualChannelsAreRequestedByDefault(t *testing.T) {
	b := clientNetworkData()
	if got := binary.LittleEndian.Uint32(b[4:8]); got != 0 {
		t.Errorf("channel count = %d, want 0", got)
	}
}

func TestLongClientNamesAreTruncatedNotOverrun(t *testing.T) {
	field := utf16leFixed("a-very-long-client-name-that-will-not-fit-in-the-field", 32)
	if len(field) != 32 {
		t.Fatalf("field is %d bytes, want 32", len(field))
	}
	// The last two bytes must remain the terminator, or the field runs into
	// whatever follows it.
	if field[30] != 0 || field[31] != 0 {
		t.Error("the name field was not terminated")
	}
}

// findBlock walks the GCC block list rather than scanning for a byte pair.
//
// A scan finds the first match anywhere, and 0x01 0xC0 also occurs inside the
// fixed T.124 header — which is how a passing test can be reading the wrong
// offset entirely.
func findBlock(t *testing.T, req []byte, want uint16) []byte {
	t.Helper()
	at := indexOf(req, []byte("Duca"))
	if at < 0 {
		t.Fatal("no conference name in the request")
	}
	at += 4
	// Skip the PER length determinant covering the block list.
	if at < len(req) && req[at]&0x80 != 0 {
		at += 2
	} else {
		at++
	}

	for at+4 <= len(req) {
		kind := binary.LittleEndian.Uint16(req[at : at+2])
		size := int(binary.LittleEndian.Uint16(req[at+2 : at+4]))
		if size < 4 || at+size > len(req) {
			t.Fatalf("block at %d claims %d bytes", at, size)
		}
		if kind == want {
			return req[at+4 : at+size]
		}
		at += size
	}
	t.Fatalf("no block of type %#x", want)
	return nil
}

/* ── MCS domain PDUs ─────────────────────────────────────────────────────── */

func TestAttachUserConfirmReadsTheUserID(t *testing.T) {
	// Tag 11 with result 0, then the user id.
	body := []byte{mcsAttachUserConfirm << 2, 0x00, 0x00, 0x07}
	frame := x224Data(body)

	got, err := ParseAttachUserConfirm(frame)
	if err != nil {
		t.Fatalf("ParseAttachUserConfirm: %v", err)
	}
	if got != 7+userChannelBase {
		t.Errorf("user id = %d, want %d", got, 7+userChannelBase)
	}
}

// Continuing past a refusal would join channels as a user that does not exist,
// and the failure would surface much later as silence.
func TestAttachUserConfirmRefusalIsAnError(t *testing.T) {
	body := []byte{mcsAttachUserConfirm<<2 | 0x01, 0x00, 0x00, 0x07}
	if _, err := ParseAttachUserConfirm(x224Data(body)); !errors.Is(err, ErrMCS) {
		t.Errorf("err = %v, want ErrMCS", err)
	}
}

func TestSendDataRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 300) // forces the two-byte PER length
	frame := SendDataRequest(1004, ChannelGlobal, payload)

	// Re-read it as though it were an indication, which shares the layout.
	inner, err := Payload(frame)
	if err != nil {
		t.Fatal(err)
	}
	inner[3] = mcsSendDataIndication << 2

	channel, got, err := ParseSendDataIndication(frame)
	if err != nil {
		t.Fatalf("ParseSendDataIndication: %v", err)
	}
	if channel != ChannelGlobal {
		t.Errorf("channel = %d", channel)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload round-tripped as %d bytes, want %d", len(got), len(payload))
	}
}

// A server that tears down the domain must be reported as such, not as a
// parse failure — the two send an operator looking in different places.
func TestDisconnectProviderIsNamed(t *testing.T) {
	frame := x224Data([]byte{mcsDisconnectProvider << 2, 0, 0, 0, 0, 0, 0})
	_, _, err := ParseSendDataIndication(frame)
	if err == nil || !errors.Is(err, ErrMCS) {
		t.Fatalf("err = %v, want ErrMCS", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("disconnected")) {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestSendDataIndicationBoundsItsPayload(t *testing.T) {
	// Claims 300 bytes of payload but carries none.
	body := []byte{mcsSendDataIndication << 2, 0, 0, 0x03, 0xEB, 0x70, 0x81, 0x2C}
	if _, _, err := ParseSendDataIndication(x224Data(body)); !errors.Is(err, ErrMCS) {
		t.Errorf("err = %v, want ErrMCS", err)
	}
}

func TestParseServerNetworkData(t *testing.T) {
	// SC_NET with a global channel and one extra.
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:2], 1004)
	binary.LittleEndian.PutUint16(body[2:4], 1)
	binary.LittleEndian.PutUint16(body[4:6], 1005)

	blk := make([]byte, 4)
	binary.LittleEndian.PutUint16(blk[0:2], 0x0C03)
	binary.LittleEndian.PutUint16(blk[2:4], uint16(4+len(body)))
	blk = append(blk, body...)

	got, err := ParseServerNetworkData(blk)
	if err != nil {
		t.Fatalf("ParseServerNetworkData: %v", err)
	}
	if got.GlobalChannel != 1004 {
		t.Errorf("global channel = %d", got.GlobalChannel)
	}
	if len(got.Extra) != 1 || got.Extra[0] != 1005 {
		t.Errorf("extra channels = %v", got.Extra)
	}
}

func TestParseServerNetworkDataRefusesBadBlocks(t *testing.T) {
	// A block claiming more than the buffer holds.
	blk := []byte{0x03, 0x0C, 0xFF, 0xFF}
	if _, err := ParseServerNetworkData(blk); !errors.Is(err, ErrMCS) {
		t.Errorf("err = %v, want ErrMCS", err)
	}
}

func FuzzParseSendDataIndication(f *testing.F) {
	f.Add(SendDataRequest(1004, ChannelGlobal, []byte{1, 2, 3}))
	f.Add([]byte{0x03, 0x00, 0x00, 0x07})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = ParseSendDataIndication(b)
		_, _ = ParseAttachUserConfirm(b)
		_, _ = ParseChannelJoinConfirm(b)
		_, _ = ParseConnectResponse(b)
	})
}
