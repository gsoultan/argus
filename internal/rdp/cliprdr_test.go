package rdp

import (
	"encoding/binary"
	"testing"
)

func clipType(m []byte) uint16 { return binary.LittleEndian.Uint16(m[0:2]) }

// The server says it is ready: this side answers with its capabilities and
// what it holds, which before anything is set is nothing.
func TestClipboardStartsWithCapsAndAnEmptyList(t *testing.T) {
	var c clipboard
	ev, err := c.feed(clipMessage(cbMonitorReady, 0, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.replies) != 2 || clipType(ev.replies[0]) != cbClipCaps || clipType(ev.replies[1]) != cbFormatList {
		t.Fatalf("replies = %x", ev.replies)
	}
	if len(ev.replies[1]) != 8 {
		t.Errorf("an empty clipboard offered %d bytes of formats", len(ev.replies[1])-8)
	}
}

// Copy on the server: acknowledge, ask for the text, and hand it over once
// it arrives.
func TestClipboardFetchesWhatTheServerCopied(t *testing.T) {
	var c clipboard
	list := make([]byte, 72)
	binary.LittleEndian.PutUint32(list[0:4], 1) // CF_TEXT
	binary.LittleEndian.PutUint32(list[36:40], cfUnicodeText)
	ev, err := c.feed(clipMessage(cbFormatList, 0, list))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.replies) != 2 || clipType(ev.replies[0]) != cbFormatListResponse || clipType(ev.replies[1]) != cbFormatDataRequest ||
		binary.LittleEndian.Uint32(ev.replies[1][8:12]) != cfUnicodeText {
		t.Fatalf("replies = %x", ev.replies)
	}
	ev, err = c.feed(clipMessage(cbFormatDataResponse, cbResponseOK, encodeClipText("héllo 😀")))
	if err != nil || !ev.gotIt || ev.text != "héllo 😀" {
		t.Fatalf("text = %q gotIt=%v err=%v", ev.text, ev.gotIt, err)
	}
}

// A copy with no text in it (a picture, files) is acknowledged and not asked for.
func TestClipboardIgnoresAnythingButText(t *testing.T) {
	var c clipboard
	list := make([]byte, 36)
	binary.LittleEndian.PutUint32(list[0:4], 8) // CF_DIB
	ev, _ := c.feed(clipMessage(cbFormatList, 0, list))
	if len(ev.replies) != 1 || clipType(ev.replies[0]) != cbFormatListResponse {
		t.Fatalf("replies = %x", ev.replies)
	}
}

// Copy in the browser: offered once the channel is up, and handed over when
// the server pastes.
func TestClipboardOffersTextAndGivesItOnPaste(t *testing.T) {
	var c clipboard
	if m, err := c.set("early"); err != nil || m != nil {
		t.Fatalf("set before ready sent %x, %v", m, err)
	}
	ev, _ := c.feed(clipMessage(cbMonitorReady, 0, nil))
	if len(ev.replies[1]) != 8+36 {
		t.Errorf("text set before the channel was up was not offered when it came up")
	}
	m, err := c.set("pasted")
	if err != nil || clipType(m) != cbFormatList {
		t.Fatalf("set = %x, %v", m, err)
	}
	req := binary.LittleEndian.AppendUint32(nil, cfUnicodeText)
	ev, _ = c.feed(clipMessage(cbFormatDataRequest, 0, req))
	r := ev.replies[0]
	if clipType(r) != cbFormatDataResponse || binary.LittleEndian.Uint16(r[2:4]) != cbResponseOK ||
		decodeClipText(r[8:]) != "pasted" {
		t.Fatalf("response = %x", r)
	}
	// Asked for a format it does not hold, it says so rather than hanging the paste.
	ev, _ = c.feed(clipMessage(cbFormatDataRequest, 0, binary.LittleEndian.AppendUint32(nil, 8)))
	if binary.LittleEndian.Uint16(ev.replies[0][2:4]) != cbResponseFail {
		t.Errorf("a request for a picture was not refused")
	}
}

func TestClipboardRefusesTooMuchAndMalformed(t *testing.T) {
	var c clipboard
	if _, err := c.set(string(make([]byte, ClipboardMax+1))); err == nil {
		t.Error("a clipboard over the limit was accepted")
	}
	if _, err := c.feed([]byte{1, 0, 0}); err == nil {
		t.Error("a truncated header was accepted")
	}
	m := clipMessage(cbFormatList, 0, make([]byte, 36))
	if _, err := c.feed(m[:20]); err == nil {
		t.Error("a body shorter than it claims was accepted")
	}
}

// The clipboard channel is requested after drdynvc, and only when asked for.
func TestNetworkDataRequestsTheClipboardOnlyWhenAsked(t *testing.T) {
	count := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b[4:8]) }
	if n := count(clientNetworkData(true, false)); n != 1 {
		t.Errorf("graphics only asked for %d channels", n)
	}
	b := clientNetworkData(true, true)
	if count(b) != 2 || string(b[20:27]) != cliprdrName {
		t.Errorf("network data = %x, want drdynvc then cliprdr", b)
	}
}
