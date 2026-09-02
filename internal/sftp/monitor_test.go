package sftp

import (
	"encoding/binary"
	"strings"
	"testing"
)

/* ── Packet construction ─────────────────────────────────────────────────── */

func pkt(typ byte, body []byte) []byte {
	out := make([]byte, 4+1+len(body))
	binary.BigEndian.PutUint32(out, uint32(1+len(body)))
	out[4] = typ
	copy(out[5:], body)
	return out
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func str(s string) []byte { return append(u32(uint32(len(s))), []byte(s)...) }

func collect(t *testing.T) (*Monitor, *[]Event) {
	t.Helper()
	var events []Event
	m := New(func(e Event) { events = append(events, e) })
	return m, &events
}

/* ── The behaviours that matter ──────────────────────────────────────────── */

// The point of decoding both directions: reads and writes name only a handle,
// so the path has to come from the OPEN and its response.
func TestDownloadIsAttributedToItsPath(t *testing.T) {
	m, events := collect(t)

	// OPEN /var/lib/payments/dump.sql
	m.ClientToServer(pkt(fxpOpen, append(u32(1), append(str("/var/lib/payments/dump.sql"), u32(1)...)...)))
	// HANDLE
	m.ServerToClient(pkt(fxpHandle, append(u32(1), str("h1")...)))

	// Two reads of 1000 and 500 bytes.
	m.ClientToServer(pkt(fxpRead, append(u32(2), append(str("h1"), append(u64(0), u32(1000)...)...)...)))
	m.ServerToClient(pkt(fxpData, append(u32(2), str(strings.Repeat("x", 1000))...)))
	m.ClientToServer(pkt(fxpRead, append(u32(3), append(str("h1"), append(u64(1000), u32(500)...)...)...)))
	m.ServerToClient(pkt(fxpData, append(u32(3), str(strings.Repeat("x", 500))...)))

	m.ClientToServer(pkt(fxpClose, append(u32(4), str("h1")...)))

	if len(*events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(*events), *events)
	}
	e := (*events)[0]
	if e.Op != OpDownload {
		t.Errorf("op = %s, want download", e.Op)
	}
	if e.Path != "/var/lib/payments/dump.sql" {
		t.Errorf("path = %q", e.Path)
	}
	if e.Bytes != 1500 {
		t.Errorf("bytes = %d, want 1500", e.Bytes)
	}
}

func TestUploadIsCounted(t *testing.T) {
	m, events := collect(t)

	m.ClientToServer(pkt(fxpOpen, append(u32(1), append(str("/tmp/payload.sh"), u32(2)...)...)))
	m.ServerToClient(pkt(fxpHandle, append(u32(1), str("h1")...)))

	// WRITE: handle, offset, data
	body := append(u32(2), append(str("h1"), append(u64(0), str(strings.Repeat("y", 4096))...)...)...)
	m.ClientToServer(pkt(fxpWrite, body))
	m.ClientToServer(pkt(fxpClose, append(u32(3), str("h1")...)))

	if len(*events) != 1 {
		t.Fatalf("got %d events: %+v", len(*events), *events)
	}
	e := (*events)[0]
	if e.Op != OpUpload || e.Path != "/tmp/payload.sh" || e.Bytes != 4096 {
		t.Errorf("event = %+v", e)
	}
}

// One entry per 32 KB chunk would make the audit log unreadable. A transfer is
// one event.
func TestALargeTransferIsOneEvent(t *testing.T) {
	m, events := collect(t)

	m.ClientToServer(pkt(fxpOpen, append(u32(1), append(str("/big.iso"), u32(1)...)...)))
	m.ServerToClient(pkt(fxpHandle, append(u32(1), str("h")...)))

	const chunks = 500
	for i := 0; i < chunks; i++ {
		id := uint32(100 + i)
		m.ClientToServer(pkt(fxpRead, append(u32(id), append(str("h"), append(u64(uint64(i)*1024), u32(1024)...)...)...)))
		m.ServerToClient(pkt(fxpData, append(u32(id), str(strings.Repeat("z", 1024))...)))
	}
	m.ClientToServer(pkt(fxpClose, append(u32(9999), str("h")...)))

	if len(*events) != 1 {
		t.Fatalf("%d chunks produced %d events, want 1", chunks, len(*events))
	}
	if got := (*events)[0].Bytes; got != chunks*1024 {
		t.Errorf("bytes = %d, want %d", got, chunks*1024)
	}
}

func TestDeleteRenameAndDirectoryOps(t *testing.T) {
	m, events := collect(t)

	m.ClientToServer(pkt(fxpRemove, append(u32(1), str("/etc/argus/audit.log")...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(1), u32(0)...)))

	m.ClientToServer(pkt(fxpRename, append(u32(2), append(str("/a"), str("/b")...)...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(2), u32(0)...)))

	m.ClientToServer(pkt(fxpMkdir, append(u32(3), str("/tmp/staging")...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(3), u32(0)...)))

	if len(*events) != 3 {
		t.Fatalf("got %d events: %+v", len(*events), *events)
	}
	if (*events)[0].Op != OpDelete || (*events)[0].Path != "/etc/argus/audit.log" {
		t.Errorf("delete event = %+v", (*events)[0])
	}
	if (*events)[1].Op != OpRename || (*events)[1].NewPath != "/b" {
		t.Errorf("rename event = %+v", (*events)[1])
	}
	if (*events)[2].Op != OpMkdir {
		t.Errorf("mkdir event = %+v", (*events)[2])
	}
}

// A refused read of a sensitive path is at least as interesting as a
// successful one.
func TestRefusedOperationsAreRecorded(t *testing.T) {
	m, events := collect(t)

	// Permission denied on open.
	m.ClientToServer(pkt(fxpOpen, append(u32(1), append(str("/etc/shadow"), u32(1)...)...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(1), u32(3)...))) // SSH_FX_PERMISSION_DENIED

	m.ClientToServer(pkt(fxpRemove, append(u32(2), str("/etc/passwd")...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(2), u32(3)...)))

	if len(*events) != 2 {
		t.Fatalf("got %d events: %+v", len(*events), *events)
	}
	for _, e := range *events {
		if !e.Failed {
			t.Errorf("event not marked failed: %+v", e)
		}
	}
	if (*events)[0].Path != "/etc/shadow" {
		t.Errorf("path = %q", (*events)[0].Path)
	}
}

// The stream arrives in arbitrary chunks; packets split across reads must
// still decode.
func TestPacketsSplitAcrossReads(t *testing.T) {
	m, events := collect(t)

	open := pkt(fxpOpen, append(u32(1), append(str("/split.txt"), u32(1)...)...))
	handle := pkt(fxpHandle, append(u32(1), str("h")...))
	write := pkt(fxpWrite, append(u32(2), append(str("h"), append(u64(0), str("hello")...)...)...))
	closep := pkt(fxpClose, append(u32(3), str("h")...))

	// Feed one byte at a time — the worst case a real stream can produce.
	for _, b := range open {
		m.ClientToServer([]byte{b})
	}
	for _, b := range handle {
		m.ServerToClient([]byte{b})
	}
	for _, b := range append(write, closep...) {
		m.ClientToServer([]byte{b})
	}

	if len(*events) != 1 {
		t.Fatalf("got %d events: %+v", len(*events), *events)
	}
	if (*events)[0].Path != "/split.txt" || (*events)[0].Bytes != 5 {
		t.Errorf("event = %+v", (*events)[0])
	}
}

// Several packets arriving in one read must all be decoded.
func TestCoalescedPackets(t *testing.T) {
	m, events := collect(t)

	var batch []byte
	batch = append(batch, pkt(fxpRemove, append(u32(1), str("/a")...))...)
	batch = append(batch, pkt(fxpRemove, append(u32(2), str("/b")...))...)
	m.ClientToServer(batch)

	var responses []byte
	responses = append(responses, pkt(fxpStatus, append(u32(1), u32(0)...))...)
	responses = append(responses, pkt(fxpStatus, append(u32(2), u32(0)...))...)
	m.ServerToClient(responses)

	if len(*events) != 2 {
		t.Fatalf("got %d events, want 2", len(*events))
	}
}

// A transfer interrupted by a dropped connection is exactly the one worth
// recording, so it must not be lost for want of a CLOSE.
func TestInterruptedTransferIsFlushedOnClose(t *testing.T) {
	m, events := collect(t)

	m.ClientToServer(pkt(fxpOpen, append(u32(1), append(str("/interrupted.tar"), u32(1)...)...)))
	m.ServerToClient(pkt(fxpHandle, append(u32(1), str("h")...)))
	m.ClientToServer(pkt(fxpRead, append(u32(2), append(str("h"), append(u64(0), u32(999)...)...)...)))
	m.ServerToClient(pkt(fxpData, append(u32(2), str(strings.Repeat("q", 999))...)))
	// Connection drops: no CLOSE arrives.

	if len(*events) != 0 {
		t.Fatal("an event was emitted before close")
	}
	m.Close()

	if len(*events) != 1 {
		t.Fatalf("got %d events after Close, want 1", len(*events))
	}
	if (*events)[0].Bytes != 999 {
		t.Errorf("bytes = %d, want 999", (*events)[0].Bytes)
	}
}

/* ── Robustness ──────────────────────────────────────────────────────────── */

// This reads bytes from the network. A panic would take down the session it is
// only supposed to be observing.
func TestMalformedInputNeverPanics(t *testing.T) {
	inputs := [][]byte{
		{},
		{0x00},
		{0xFF, 0xFF, 0xFF, 0xFF},            // absurd length
		append(u32(10), []byte("short")...), // truncated
		pkt(fxpOpen, u32(1)),                // open with no path
		pkt(fxpRead, append(u32(1), str("h")...)),                      // read with no offset
		pkt(fxpWrite, append(u32(1), str("h")...)),                     // write with no data
		pkt(fxpHandle, []byte{}),                                       // handle with no id
		pkt(200, []byte("unknown type")),                               // unknown packet
		append(u32(8), append([]byte{fxpOpen}, u32(0xFFFFFFFF)...)...), // lying string length
	}
	for i, in := range inputs {
		t.Run("", func(t *testing.T) {
			m, _ := collect(t)
			m.ClientToServer(in)
			m.ServerToClient(in)
			m.Close()
			_ = i
		})
	}
}

// Losing framing means everything after is guesswork. Saying so beats emitting
// fabricated events.
func TestDesyncStopsDecodingRatherThanGuessing(t *testing.T) {
	m, events := collect(t)

	m.ClientToServer([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x01})
	// Well-formed traffic after the break must not be trusted.
	m.ClientToServer(pkt(fxpRemove, append(u32(1), str("/should-not-appear")...)))
	m.ServerToClient(pkt(fxpStatus, append(u32(1), u32(0)...)))

	if len(*events) != 0 {
		t.Errorf("emitted events after losing framing: %+v", *events)
	}
	if _, _, desynced := m.Stats(); !desynced {
		t.Error("desync was not reported")
	}
}

// A client controls how many handles it opens, so the map must not grow freely.
func TestHandleTableIsBounded(t *testing.T) {
	m, _ := collect(t)

	for i := 0; i < maxOpenHandles*2; i++ {
		id := uint32(i)
		h := string(rune('a'+i%26)) + string(rune(i))
		m.ClientToServer(pkt(fxpOpen, append(u32(id), append(str("/f"), u32(1)...)...)))
		m.ServerToClient(pkt(fxpHandle, append(u32(id), str(h)...)))
	}
	m.mu.Lock()
	n := len(m.handles)
	m.mu.Unlock()
	if n > maxOpenHandles {
		t.Errorf("tracking %d handles with a cap of %d", n, maxOpenHandles)
	}
}

func TestPendingTableIsBounded(t *testing.T) {
	m, _ := collect(t)
	// Requests with no response ever.
	for i := 0; i < maxPendingRequests*2; i++ {
		m.ClientToServer(pkt(fxpRemove, append(u32(uint32(i)), str("/f")...)))
	}
	m.mu.Lock()
	n := len(m.pending)
	m.mu.Unlock()
	if n > maxPendingRequests {
		t.Errorf("tracking %d pending requests with a cap of %d", n, maxPendingRequests)
	}
}

func FuzzMonitor(f *testing.F) {
	f.Add(pkt(fxpOpen, append(u32(1), str("/x")...)))
	f.Add(pkt(fxpRead, append(u32(1), str("h")...)))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		m := New(func(Event) {})
		m.ClientToServer(data)
		m.ServerToClient(data)
		m.Close()
	})
}

func TestDescribe(t *testing.T) {
	cases := map[string]Event{
		"download /d.sql (1.5 KB)":        {Op: OpDownload, Path: "/d.sql", Bytes: 1536},
		"renamed /a to /b":                {Op: OpRename, Path: "/a", NewPath: "/b"},
		"download of /etc/shadow refused": {Op: OpDownload, Path: "/etc/shadow", Failed: true},
		"delete /tmp/x":                   {Op: OpDelete, Path: "/tmp/x"},
	}
	for want, e := range cases {
		if got := e.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}
