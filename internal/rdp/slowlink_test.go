package rdp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The connection type is said only for a slow link, and then with the flag
// that makes a server read it: without the flag the byte is ignored, which
// is how it was sent for a long time.
func TestCoreDataSaysASlowLinkAndOnlyThen(t *testing.T) {
	fields := func(info ClientInfo) (early uint16, connection byte) {
		b := clientCoreData(info)
		return binary.LittleEndian.Uint16(b[len(b)-72:]), b[len(b)-6]
	}
	if early, connection := fields(ClientInfo{}); early&0x0020 != 0 || connection != 0 {
		t.Errorf("an ordinary link sent connection type %d with flags %#x", connection, early)
	}
	early, connection := fields(ClientInfo{SlowLink: true})
	if early&0x0020 == 0 {
		t.Errorf("a slow link's connection type is not flagged as there: %#x", early)
	}
	if connection != 0x02 {
		t.Errorf("a slow link was sent as connection type %d, want low-speed broadband (2)", connection)
	}
}

// A slow link turns off what only costs bandwidth, and nothing else in the
// logon packet changes.
func TestClientInfoPerformanceFlagsForASlowLink(t *testing.T) {
	fast := ClientInfoPDU(LogonInfo{User: "ops"})
	slow := ClientInfoPDU(LogonInfo{User: "ops", SlowLink: true})
	if len(fast) != len(slow) {
		t.Fatalf("the packets differ in length: %d and %d", len(fast), len(slow))
	}
	// Performance flags, then the two-byte auto-reconnect length, end it.
	at := len(slow) - 6
	if !bytes.Equal(fast[:at], slow[:at]) || !bytes.Equal(fast[at+4:], slow[at+4:]) {
		t.Fatal("more than the performance flags changed")
	}
	if got := binary.LittleEndian.Uint32(fast[at:]); got != 0 {
		t.Errorf("an ordinary link sent performance flags %#x", got)
	}
	got := binary.LittleEndian.Uint32(slow[at:])
	for name, flag := range map[string]uint32{"wallpaper": 1, "full window drag": 2, "menu animations": 4, "cursor shadow": 0x20} {
		if got&flag == 0 {
			t.Errorf("a slow link still has %s", name)
		}
	}
	if got&0x80 != 0 || got&0x08 != 0 {
		t.Errorf("a slow link lost font smoothing or themes: %#x", got)
	}
}
