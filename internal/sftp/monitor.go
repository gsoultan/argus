// Package sftp decodes the SFTP subsystem to produce per-file audit events.
//
// Teeing an SFTP session as raw bytes yields unreadable binary. Decoding it
// yields "kevin.tan downloaded /var/lib/payments/dump.sql, 4.2 GB", which is
// the question an auditor actually asks and the one a PAM tool exists to
// answer.
//
// The decoder is strictly an observer. It is handed a copy of each direction
// and never modifies, delays or blocks the stream — a bug here must degrade the
// audit trail, never the session. If it loses framing it says so and stops
// guessing rather than emitting fabricated events.
//
// Protocol: draft-ietf-secsh-filexfer. Each packet is
//
//	uint32 length | byte type | uint32 request-id | payload
//
// Paths arrive in requests and handles in responses, so both directions have to
// be read to know which file a read or write refers to.
package sftp

import (
	"encoding/binary"
	"fmt"
	"path"
	"sync"
	"time"
)

// Packet types. Only those that reveal what happened to a file are named; the
// rest are counted and skipped.
const (
	fxpInit    = 1
	fxpOpen    = 3
	fxpClose   = 4
	fxpRead    = 5
	fxpWrite   = 6
	fxpLstat   = 7
	fxpSetstat = 9
	fxpOpendir = 11
	fxpRemove  = 13
	fxpMkdir   = 14
	fxpRmdir   = 15
	fxpStat    = 17
	fxpRename  = 18
	fxpSymlink = 20

	fxpStatus = 101
	fxpHandle = 102
	fxpData   = 103
)

// Op is what happened to a path.
type Op string

const (
	OpUpload   Op = "upload"
	OpDownload Op = "download"
	OpDelete   Op = "delete"
	OpRename   Op = "rename"
	OpMkdir    Op = "mkdir"
	OpRmdir    Op = "rmdir"
	OpChmod    Op = "chmod"
	OpList     Op = "list"
	OpSymlink  Op = "symlink"
)

// Event is one auditable file operation.
type Event struct {
	At   time.Time `json:"at"`
	Op   Op        `json:"op"`
	Path string    `json:"path"`
	// NewPath is set for renames.
	NewPath string `json:"newPath,omitempty"`
	// Bytes transferred, for uploads and downloads. Accumulated across every
	// read or write against the handle, so it is the size actually moved
	// rather than the size of the file.
	Bytes int64 `json:"bytes,omitempty"`
	// Failed marks an operation the server refused. A denied attempt to read
	// /etc/shadow is at least as interesting as a successful one.
	Failed bool `json:"failed,omitempty"`
}

// Limits keep decoder state bounded. A client controls how many handles it
// opens and how many requests it has outstanding, so neither may grow freely.
const (
	maxOpenHandles     = 4096
	maxPendingRequests = 4096
	// A single SFTP packet above this is not something a real client sends;
	// treating it as a length error is safer than allocating for it.
	maxPacketSize = 1 << 22 // 4 MiB
)

// Monitor decodes one SFTP session.
//
// Safe for concurrent use: the two directions are fed from separate goroutines.
type Monitor struct {
	mu sync.Mutex

	clientBuf []byte
	serverBuf []byte

	// pending maps a request id to what the request was about, so the response
	// can be attributed when it arrives.
	pending map[uint32]pendingOp
	// handles maps an open handle to its path and running byte count.
	handles map[string]*openFile

	emit func(Event)

	// desynced is set when framing is lost. Everything after that would be
	// guesswork, so decoding stops rather than inventing events.
	desynced bool
	// Counters for the session summary.
	packets  int
	unknowns int
}

type pendingOp struct {
	op      Op
	path    string
	newPath string
	handle  string
	// length requested, for reads: SSH_FXP_DATA does not repeat the handle.
	readLen uint32
}

type openFile struct {
	path    string
	read    int64
	written int64
	opened  time.Time
}

// New builds a monitor. emit is called for each completed operation and must
// not block: it runs on the session's data path.
func New(emit func(Event)) *Monitor {
	return &Monitor{
		pending: make(map[uint32]pendingOp),
		handles: make(map[string]*openFile),
		emit:    emit,
	}
}

// ClientToServer feeds bytes travelling from the user to the host.
func (m *Monitor) ClientToServer(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientBuf = m.consume(append(m.clientBuf, b...), m.handleRequest)
}

// ServerToClient feeds bytes travelling from the host to the user.
func (m *Monitor) ServerToClient(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serverBuf = m.consume(append(m.serverBuf, b...), m.handleResponse)
}

// consume pulls whole packets out of buf and returns the remainder.
func (m *Monitor) consume(buf []byte, handle func(typ byte, payload []byte)) []byte {
	if m.desynced {
		// Drop rather than accumulate: holding a growing buffer for a stream
		// we can no longer parse is just a memory leak.
		return nil
	}

	for len(buf) >= 4 {
		length := binary.BigEndian.Uint32(buf[:4])
		if length == 0 || length > maxPacketSize {
			// Either framing was lost or this is not SFTP at all.
			m.desynced = true
			return nil
		}
		if uint32(len(buf)-4) < length {
			return buf // wait for the rest
		}

		packet := buf[4 : 4+length]
		buf = buf[4+length:]
		m.packets++

		if len(packet) < 1 {
			continue
		}
		handle(packet[0], packet[1:])
	}
	return buf
}

func (m *Monitor) handleRequest(typ byte, payload []byte) {
	// INIT carries a version rather than a request id.
	if typ == fxpInit {
		return
	}

	id, rest, ok := readUint32(payload)
	if !ok {
		return
	}

	switch typ {
	case fxpOpen:
		p, r, ok := readString(rest)
		if !ok {
			return
		}
		// pflags tell upload from download, but a client may open read-write
		// and do either, so the direction is decided by what actually moves.
		_ = r
		m.remember(id, pendingOp{op: OpUpload, path: p})

	case fxpOpendir:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpList, path: p})
		}

	case fxpClose:
		if h, _, ok := readString(rest); ok {
			m.closeHandle(h)
		}

	case fxpRead:
		h, r, ok := readString(rest)
		if !ok {
			return
		}
		// offset (8) then length (4)
		if len(r) < 12 {
			return
		}
		n := binary.BigEndian.Uint32(r[8:12])
		m.remember(id, pendingOp{op: OpDownload, handle: h, readLen: n})

	case fxpWrite:
		h, r, ok := readString(rest)
		if !ok || len(r) < 8 {
			return
		}
		data, _, ok := readString(r[8:])
		if !ok {
			return
		}
		// A write is counted when sent; the response only confirms it.
		if f := m.handles[h]; f != nil {
			f.written += int64(len(data))
		}

	case fxpRemove:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpDelete, path: p})
		}
	case fxpRmdir:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpRmdir, path: p})
		}
	case fxpMkdir:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpMkdir, path: p})
		}
	case fxpSetstat:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpChmod, path: p})
		}
	case fxpRename:
		old, r, ok := readString(rest)
		if !ok {
			return
		}
		if nw, _, ok := readString(r); ok {
			m.remember(id, pendingOp{op: OpRename, path: old, newPath: nw})
		}
	case fxpSymlink:
		if p, _, ok := readString(rest); ok {
			m.remember(id, pendingOp{op: OpSymlink, path: p})
		}

	case fxpStat, fxpLstat:
		// Metadata lookups are noise on their own; a transfer is always
		// preceded by several. Counted, not reported.

	default:
		m.unknowns++
	}
}

func (m *Monitor) handleResponse(typ byte, payload []byte) {
	id, rest, ok := readUint32(payload)
	if !ok {
		return
	}
	p, tracked := m.pending[id]
	if tracked {
		delete(m.pending, id)
	}

	switch typ {
	case fxpHandle:
		h, _, ok := readString(rest)
		if !ok || !tracked {
			return
		}
		// Binding the handle to the path is the whole reason both directions
		// are decoded: reads and writes name only a handle.
		if len(m.handles) < maxOpenHandles {
			m.handles[h] = &openFile{path: p.path, opened: time.Now()}
		}

	case fxpData:
		if !tracked || p.op != OpDownload {
			return
		}
		if data, _, ok := readString(rest); ok {
			if f := m.handles[p.handle]; f != nil {
				f.read += int64(len(data))
			}
		}

	case fxpStatus:
		if !tracked {
			return
		}
		code, _, ok := readUint32(rest)
		if !ok {
			return
		}
		failed := code != 0 // SSH_FX_OK

		switch p.op {
		case OpDelete, OpRmdir, OpMkdir, OpRename, OpChmod, OpSymlink:
			m.emitEvent(Event{
				At: time.Now(), Op: p.op, Path: p.path,
				NewPath: p.newPath, Failed: failed,
			})
		case OpUpload:
			// A failed open never produced a handle, so report the attempt.
			// A refused read of a sensitive path is worth as much as a
			// successful one.
			if failed {
				m.emitEvent(Event{At: time.Now(), Op: OpDownload, Path: p.path, Failed: true})
			}
		case OpDownload:
			// EOF ends a read loop and is not a failure worth reporting.
		}
	}
}

// closeHandle emits the transfer summary for a handle.
//
// Reported at close rather than per packet: a 4 GB download is thousands of
// reads, and an audit log with one entry per 32 KB chunk is one nobody reads.
func (m *Monitor) closeHandle(h string) {
	f, ok := m.handles[h]
	if !ok {
		return
	}
	delete(m.handles, h)

	switch {
	case f.written > 0:
		m.emitEvent(Event{At: time.Now(), Op: OpUpload, Path: f.path, Bytes: f.written})
	case f.read > 0:
		m.emitEvent(Event{At: time.Now(), Op: OpDownload, Path: f.path, Bytes: f.read})
	default:
		// Opened and closed with nothing moved — a stat by another name.
	}
}

func (m *Monitor) remember(id uint32, p pendingOp) {
	if len(m.pending) >= maxPendingRequests {
		// A client with thousands of outstanding requests is not doing
		// anything a real transfer does. Stop tracking rather than grow.
		return
	}
	m.pending[id] = p
}

func (m *Monitor) emitEvent(e Event) {
	if e.Path != "" {
		e.Path = path.Clean(e.Path)
	}
	if e.NewPath != "" {
		e.NewPath = path.Clean(e.NewPath)
	}
	if m.emit != nil {
		m.emit(e)
	}
}

// Close flushes handles still open when the session ended.
//
// A transfer interrupted by a dropped connection is exactly the one worth
// recording, so it must not be lost for want of a CLOSE packet.
func (m *Monitor) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h := range m.handles {
		m.closeHandle(h)
	}
}

// Stats summarises what was decoded, for diagnosing a session that produced no
// events when one was expected.
func (m *Monitor) Stats() (packets, unknown int, desynced bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.packets, m.unknowns, m.desynced
}

/* ── Wire helpers ────────────────────────────────────────────────────────── */
//
// Every read is bounds-checked. These parse bytes from the network before any
// policy has run, and a panic here would take down the session.

func readUint32(b []byte) (uint32, []byte, bool) {
	if len(b) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], true
}

func readString(b []byte) (string, []byte, bool) {
	n, rest, ok := readUint32(b)
	if !ok {
		return "", nil, false
	}
	// A length field claiming more than the buffer holds is the classic
	// out-of-bounds read.
	if uint64(n) > uint64(len(rest)) {
		return "", nil, false
	}
	return string(rest[:n]), rest[n:], true
}

// Describe renders an event for a log line or the console.
func (e Event) Describe() string {
	switch e.Op {
	case OpRename:
		return fmt.Sprintf("renamed %s to %s", e.Path, e.NewPath)
	case OpUpload, OpDownload:
		if e.Failed {
			return fmt.Sprintf("%s of %s refused", e.Op, e.Path)
		}
		return fmt.Sprintf("%s %s (%s)", e.Op, e.Path, humanBytes(e.Bytes))
	default:
		verb := string(e.Op)
		if e.Failed {
			return fmt.Sprintf("%s of %s refused", verb, e.Path)
		}
		return fmt.Sprintf("%s %s", verb, e.Path)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
