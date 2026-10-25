package agent

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// DesktopRecording is the recording of a Remote Desktop session the agent
// opened through the tunnel: the RDP protocol stream as the host's own
// listener sent it, under the same hash chain as a terminal recording, in
// the format the console's replay already reads.
type DesktopRecording struct {
	c     *Collector
	id    string
	start SessionStart
	rec   *rdp.Recorder
	file  *os.File
	path  string
	// flags are what the service knows about how the session was opened.
	flags []string

	mu                        sync.Mutex
	broken                    bool
	terminatedBy, termination string
}

// StartDesktopRecording opens a recording for a desktop session. hangup
// ends the session; a shutdown calls it when the drain window runs out.
func (c *Collector) StartDesktopRecording(m SessionStart, h rdp.Header, flags []string,
	hangup func()) (*DesktopRecording, error) {

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, ErrShuttingDown
	}
	c.wg.Add(1)
	c.mu.Unlock()

	if err := ProtectStateDir(c.dir); err != nil {
		c.wg.Done()
		return nil, err
	}
	id := newID()
	path := filepath.Join(c.dir, id+rdp.Extension)
	// Readable by the agent alone, as a terminal recording is.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		c.wg.Done()
		return nil, err
	}
	if h.Env == nil {
		h.Env = map[string]string{}
	}
	h.Env["ARGUS_SESSION"] = id
	h.Env["ARGUS_USER"] = m.Actor
	h.Env["ARGUS_ORIGIN"] = "browser"
	if h.Timestamp == 0 {
		h.Timestamp = m.StartedAt.Unix()
	}
	rec, err := rdp.NewRecorder(f, h)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		c.wg.Done()
		return nil, err
	}
	d := &DesktopRecording{c: c, id: id, start: m, rec: rec, file: f, path: path, flags: flags}
	c.mu.Lock()
	if c.hangups == nil {
		c.hangups = map[string]func(){}
	}
	c.hangups[id] = hangup
	c.trackTunnel(id, d)
	c.mu.Unlock()
	if c.OnActive != nil {
		c.OnActive(d.activeRecord())
	}
	return d, nil
}

// ID is the recording's session id.
func (d *DesktopRecording) ID() string { return d.id }

// Screen records one PDU the desktop sent. An error ends the session: what
// is shown is in the chain, or it is not shown.
func (d *DesktopRecording) Screen(pdu []byte) error {
	if err := d.rec.Write(rdp.ServerOutput, pdu); err != nil {
		d.mu.Lock()
		d.broken = true
		d.mu.Unlock()
		return err
	}
	return nil
}

// Terminate records that by ended the session from the console, for reason.
func (d *DesktopRecording) Terminate(by, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.terminatedBy == "" {
		d.terminatedBy, d.termination = by, reason
	}
}

func (d *DesktopRecording) record(state string) SessionRecord {
	_, bytes, _ := d.rec.Stats()
	d.mu.Lock()
	defer d.mu.Unlock()
	flags := append([]string(nil), d.flags...)
	if d.broken {
		// A replay that cuts off part-way is not a session that ended there.
		flags = append(flags, "recording-incomplete")
	}
	return SessionRecord{
		ID:                d.id,
		Principal:         d.start.Principal,
		ClientAddr:        d.start.ClientAddr,
		Origin:            d.start.Origin,
		Hostname:          d.start.Hostname,
		StartedAt:         d.start.StartedAt,
		Bytes:             bytes,
		Path:              d.path,
		Fidelity:          "rdp",
		Actor:             d.start.Actor,
		Protocol:          "rdp",
		State:             state,
		TerminatedBy:      d.terminatedBy,
		TerminationReason: d.termination,
		Flags:             flags,
	}
}

func (d *DesktopRecording) activeRecord() SessionRecord { return d.record("active") }

// Seal closes the recording, reports it, and returns its chain head. Call
// once, after the desktop connection has closed.
func (d *DesktopRecording) Seal() string {
	c := d.c
	c.mu.Lock()
	delete(c.hangups, d.id)
	delete(c.tunnels, d.id)
	c.mu.Unlock()
	defer c.wg.Done()

	head, err := d.rec.Close()
	if err != nil {
		c.log.Error("sealing a desktop recording failed", "session", d.id, "error", err)
	}
	_ = d.file.Close()

	d.mu.Lock()
	state := "closed"
	if d.terminatedBy != "" {
		state = "terminated"
	}
	d.mu.Unlock()
	rec := d.record(state)
	rec.EndedAt = time.Now().UTC()
	rec.ChainHead = head
	c.log.Info("recording sealed", "session", d.id, "protocol", "rdp",
		"principal", rec.Principal, "chain_head", head, "bytes", rec.Bytes)
	if c.OnSession != nil {
		c.OnSession(rec)
	}
	return head
}
