package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

/*
Growing disks, from the console.

The agent says, for every filesystem on its server, the whole stack under it
(disk, partition, encryption, LVM, filesystem) and whether it can grow: is
there room somewhere below it that space can reach it from, and is anything
in the way. When the console asks, it grows the filesystem into that room
online: no unmount, no restart. Two halves, like log cleanup (logclean.go):

  - The daemon decides. It scans every six hours while the console asks it
    to, and now when asked; when the console asks for a grow it writes a
    plan and starts the helper. It reports both on the heartbeat.

  - The helper does the work: `argus-agent disks-scan` and
    `argus-agent disks-grow`, as root, in a transient systemd unit of their
    own (disks_linux.go). The scan reads partition tables and superblocks,
    which the agent's sandbox cannot; the grow writes a partition table,
    which it must not. On Windows the agent is SYSTEM, and the work runs in
    the agent itself.

What keeps it safe:

  - Growing only ever adds space. Nothing is shrunk, moved, unmounted or
    removed, and every step leaves a state the next run can carry on from:
    a partition bigger than its LVM volume, or a volume group with free
    space, is normal.
  - The console never names a device. A grow is planned from the scan's own
    report, and the helper scans again just before it writes: the layers
    must still be the ones the plan names, with the same sizes. One that
    moved stops the run before anything is written.
  - The partition table before the run is kept with the run, so the layout
    before any change is on record.

The scan reads sizes and layout: /sys, partition tables, filesystem
superblocks, the LVM report. Never a file's contents.
*/

// Disk answers: what a filesystem can do.
const (
	// DiskGrow is a filesystem with room below it it can grow into now.
	DiskGrow = "grow"
	// DiskFull is a filesystem that already uses all of its disk.
	DiskFull = "full"
	// DiskBlocked is one that cannot grow here, and Why says why.
	DiskBlocked = "blocked"
)

// diskMinRoom is the least room worth a grow: less is alignment slack.
const diskMinRoom = 64 << 20

// DiskLayer is one layer of a filesystem's stack.
type DiskLayer struct {
	// Kind is disk, partition, crypt, pv, vg, lv or fs.
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Size   int64  `json:"size"`
	// Used is what a filesystem holds, or what of a volume group its
	// volumes take.
	Used int64 `json:"used,omitempty"`
	// Room is free space this layer can grow into, or for a disk what of
	// it no partition holds.
	Room int64 `json:"room,omitempty"`
	// Offset is where a partition starts on its disk, so the console can
	// draw it to scale.
	Offset int64 `json:"offset,omitempty"`
}

// GrowPath is what growing a filesystem touches, as the plan names it
// back and the helper checks it again. Sizes are in bytes.
type GrowPath struct {
	Disk       string `json:"disk,omitempty"`
	DiskSize   int64  `json:"disk_size,omitempty"`
	DiskSerial string `json:"disk_serial,omitempty"`
	// Part is the partition's device name, PartNum its number on the disk.
	Part      string `json:"part,omitempty"`
	PartNum   int    `json:"part_num,omitempty"`
	PartStart int64  `json:"part_start,omitempty"`
	PartSize  int64  `json:"part_size,omitempty"`
	PartRoom  int64  `json:"part_room,omitempty"`
	Table     string `json:"table,omitempty"` // gpt or dos
	// Crypt is the dm-crypt mapping's name.
	Crypt     string `json:"crypt,omitempty"`
	CryptRoom int64  `json:"crypt_room,omitempty"`
	// PV is the LVM physical volume's device, as LVM names it.
	PV     string `json:"pv,omitempty"`
	PVRoom int64  `json:"pv_room,omitempty"`
	VG     string `json:"vg,omitempty"`
	VGUUID string `json:"vg_uuid,omitempty"`
	VGFree int64  `json:"vg_free,omitempty"`
	// LV is the logical volume's path, /dev/<vg>/<lv>.
	LV     string `json:"lv,omitempty"`
	LVUUID string `json:"lv_uuid,omitempty"`
	LVSize int64  `json:"lv_size,omitempty"`
	// FSDevice is the device the filesystem is on.
	FSDevice string `json:"fs_device,omitempty"`
	FSType   string `json:"fs_type"`
	FSSize   int64  `json:"fs_size"`
	FSRoom   int64  `json:"fs_room,omitempty"`
	Mount    string `json:"mount"`
	// Windows: the disk and partition numbers, and how far Windows says
	// the partition can grow.
	DiskNumber      int   `json:"disk_number,omitempty"`
	PartitionNumber int   `json:"partition_number,omitempty"`
	SizeMax         int64 `json:"size_max,omitempty"`
}

// DiskFS is one filesystem, with its stack and its answer.
type DiskFS struct {
	// Key names it across scans: "uuid:<uuid>", or "mount:<mount>".
	Key   string `json:"key"`
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	// Total, Used and Avail are as df counts them, like Disk.
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
	// Layers are its stack, the disk first.
	Layers []DiskLayer `json:"layers"`
	Answer string      `json:"answer"`
	// Why says why it cannot grow, or what growing does.
	Why string `json:"why,omitempty"`
	// Room is what growing would add, in all.
	Room int64 `json:"room,omitempty"`
	// LVM is whether a grow can be told how much: room from a volume group
	// can be shared out, a partition's cannot.
	LVM bool `json:"lvm,omitempty"`
	// Rescan is whether the disk under it learns a new size only when
	// asked: SCSI disks a hypervisor made bigger.
	Rescan bool `json:"rescan,omitempty"`
	// Unallocated is space on the disk under it no partition holds, even
	// where this filesystem cannot reach it.
	Unallocated int64     `json:"unallocated,omitempty"`
	Path        *GrowPath `json:"path,omitempty"`
}

// BlockDisk is one disk the scan saw.
type BlockDisk struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Bus    string `json:"bus,omitempty"` // nvme, virtio, scsi, xen, ata, ...
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
	Table  string `json:"table,omitempty"`
	// Unallocated is what no partition holds.
	Unallocated int64 `json:"unallocated,omitempty"`
	ReadOnly    bool  `json:"read_only,omitempty"`
}

// DiskReport is what the scan found.
type DiskReport struct {
	ScannedAt time.Time `json:"scanned_at"`
	OS        string    `json:"os"`
	Supported bool      `json:"supported"`
	Reason    string    `json:"reason,omitempty"`
	// Error is why the last scan failed; the rest is the scan before it.
	Error string `json:"error,omitempty"`
	// Hypervisor names what the server runs on, from DMI, so the console
	// can say how to make its disk bigger.
	Hypervisor  string      `json:"hypervisor,omitempty"`
	Filesystems []DiskFS    `json:"filesystems"`
	Disks       []BlockDisk `json:"disks"`
	// Rescanned is whether the scan asked the disks for their size first.
	Rescanned bool `json:"rescanned,omitempty"`
}

// Bounds on a report, which rides a heartbeat.
const (
	maxDiskFS     = 48
	maxBlockDisks = 64
)

// bounded keeps a report within what a heartbeat carries.
func (r *DiskReport) bounded() {
	if len(r.Filesystems) > maxDiskFS {
		r.Filesystems = r.Filesystems[:maxDiskFS]
	}
	if len(r.Disks) > maxBlockDisks {
		r.Disks = r.Disks[:maxBlockDisks]
	}
	for i := range r.Filesystems {
		if len(r.Filesystems[i].Layers) > 12 {
			r.Filesystems[i].Layers = r.Filesystems[i].Layers[:12]
		}
	}
}

// fs is the filesystem with key, if the report has it.
func (r DiskReport) fs(key string) *DiskFS {
	for i := range r.Filesystems {
		if r.Filesystems[i].Key == key {
			return &r.Filesystems[i]
		}
	}
	return nil
}

// DiskOrders is what the control plane says about disks on a heartbeat
// reply. Absent from a control plane older than disk growing: the agent
// then does not scan.
type DiskOrders struct {
	Scan bool `json:"scan"`
	// ScanID asks for a scan now; Rescan asks the disks for their size
	// first. The agent reports ScanID back once done.
	ScanID string           `json:"scan_id,omitempty"`
	Rescan bool             `json:"rescan,omitempty"`
	Run    *DiskGrowRequest `json:"run,omitempty"`
}

// DiskGrowRequest is a grow the console asked for.
type DiskGrowRequest struct {
	ID string `json:"id"`
	// Key is the filesystem's key from the scan, and Path what the scan
	// said is under it: the helper refuses to write if either moved.
	Key  string   `json:"key"`
	Path GrowPath `json:"path"`
	// Bytes is how much to give a logical volume from its group's free
	// space; 0 is all of it. A partition always takes all its room.
	Bytes int64 `json:"bytes,omitempty"`
}

// DiskStep is one step of a grow, as it ran.
type DiskStep struct {
	Title   string `json:"title"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	// State is running, done, failed or skipped.
	State string `json:"state"`
}

// DiskRunStatus is what the agent says about a grow, on every heartbeat
// until the console has it.
type DiskRunStatus struct {
	ID      string     `json:"id"`
	State   string     `json:"state"`
	Step    string     `json:"step,omitempty"`
	Message string     `json:"message,omitempty"`
	Steps   []DiskStep `json:"steps,omitempty"`
	// TableBefore is the partition table before anything was written.
	TableBefore string `json:"table_before,omitempty"`
	// Before and After are the filesystem's size.
	Before     int64     `json:"before"`
	After      int64     `json:"after,omitempty"`
	Unit       string    `json:"unit,omitempty"`
	BootID     string    `json:"boot_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	At         time.Time `json:"at"`
}

// Finished reports whether the grow has reached an end.
func (s DiskRunStatus) Finished() bool {
	switch s.State {
	case RunDone, RunFailed, RunInterrupted:
		return true
	}
	return false
}

// DiskScanRequest is what the scan is handed.
type DiskScanRequest struct {
	StateDir string `json:"state_dir"`
	Rescan   bool   `json:"rescan,omitempty"`
}

// DiskPlan is what the grow helper is handed.
type DiskPlan struct {
	DiskGrowRequest
	StateDir string `json:"state_dir"`
	Unit     string `json:"unit"`
}

const (
	disksDir        = "disks"
	disksReportFile = "disks.json"
	disksRunFile    = "disks-run.json"
	diskScanEvery   = 6 * time.Hour
	diskScanRetry   = 30 * time.Minute
	diskScanTimeout = 5 * time.Minute
	diskGrowTimeout = 30 * time.Minute
	// maxStepOutput bounds what a step keeps of its command's output.
	maxStepOutput = 4000
)

// diskPlatform is the part of disk growing that depends on the host.
type diskPlatform interface {
	// supported says why disks cannot be grown from the console here, or
	// "" when they can.
	supported() string
	scan(ctx context.Context, req DiskScanRequest) (DiskReport, error)
	// launch starts the helper for a grow and returns without waiting.
	launch(ctx context.Context, planPath, unit string) error
	active(unit string) bool
	bootID() string
}

// Disks is the daemon's half of disk growing.
type Disks struct {
	StateDir string
	Log      *slog.Logger
	// Changed is called when there is something new to report.
	Changed func()

	platform diskPlatform
	now      func() time.Time

	mu         sync.Mutex
	enabled    bool
	report     *DiskReport
	sent       time.Time
	scanning   bool
	lastTry    time.Time
	scanID     string
	rescan     bool
	scanDone   string
	answered   string
	run        DiskRunStatus
	loaded     bool
	lastChange time.Time
}

// NewDisks builds the daemon's half for this host. executable is the agent
// binary the helper runs from.
func NewDisks(stateDir, executable string, log *slog.Logger) *Disks {
	if log == nil {
		log = slog.Default()
	}
	return &Disks{StateDir: stateDir, Log: log, platform: hostDiskPlatform(executable, stateDir), now: time.Now}
}

// DiskBeat is what rides the heartbeat.
type DiskBeat struct {
	Supported bool           `json:"supported"`
	Reason    string         `json:"reason,omitempty"`
	Report    *DiskReport    `json:"report,omitempty"`
	ScanID    string         `json:"scan_id,omitempty"`
	Scanning  bool           `json:"scanning,omitempty"`
	Run       *DiskRunStatus `json:"run,omitempty"`
}

// Beat is what to put on the next heartbeat.
func (d *Disks) Beat() DiskBeat {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	b := DiskBeat{Scanning: d.scanning, ScanID: d.scanDone}
	if reason := d.platform.supported(); reason != "" {
		b.Reason = reason
	} else {
		b.Supported = true
	}
	if d.report != nil && d.report.ScannedAt.After(d.sent) {
		r := *d.report
		b.Report = &r
	}
	if d.run.ID != "" {
		r := d.run
		b.Run = &r
	}
	return b
}

// Delivered says a heartbeat carrying b reached the control plane.
func (d *Disks) Delivered(b DiskBeat) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b.Report != nil && b.Report.ScannedAt.After(d.sent) {
		d.sent = b.Report.ScannedAt
	}
	if b.ScanID != "" && b.ScanID == d.scanDone {
		d.scanDone = ""
	}
}

// Handle acts on what the control plane said on a heartbeat reply. o is nil
// from a control plane older than disk growing.
func (d *Disks) Handle(ctx context.Context, o *DiskOrders) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	if o == nil {
		d.enabled = false
		return
	}
	d.enabled = o.Scan
	if o.ScanID != "" && o.ScanID != d.scanID && o.ScanID != d.scanDone && o.ScanID != d.answered {
		d.scanID, d.rescan = o.ScanID, o.Rescan
	}
	if o.Run != nil {
		d.startLocked(ctx, *o.Run)
	}
}

// startLocked starts a grow the console asked for, once.
func (d *Disks) startLocked(ctx context.Context, req DiskGrowRequest) {
	if req.ID == d.run.ID {
		return
	}
	if !reRunID.MatchString(req.ID) {
		d.Log.Error("disks: refusing a grow with a malformed id")
		return
	}
	if d.run.ID != "" && !d.run.Finished() {
		d.Log.Warn("disks: a grow was asked for while another is going; it waits", "going", d.run.ID, "asked", req.ID)
		return
	}
	now := d.now().UTC()
	st := DiskRunStatus{ID: req.ID, StartedAt: now, At: now, Before: req.Path.FSSize}
	fail := func(msg string) {
		st.State, st.Message, st.FinishedAt = RunFailed, msg, now
		d.setRunLocked(st)
	}
	if reason := d.platform.supported(); reason != "" {
		fail(reason)
		return
	}
	if err := validDiskRequest(req); err != nil {
		fail(err.Error())
		return
	}
	dir := filepath.Join(d.StateDir, disksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("cannot prepare the grow: " + err.Error())
		return
	}
	short := strings.ReplaceAll(req.ID, "-", "")
	if len(short) > 12 {
		short = short[:12]
	}
	unit := "argus-diskgrow-" + short
	planPath := filepath.Join(dir, "plan-"+req.ID+".json")
	if err := writeFileAtomic(planPath, mustJSON(DiskPlan{DiskGrowRequest: req, StateDir: d.StateDir, Unit: unit}),
		0o600); err != nil {
		fail("cannot prepare the grow: " + err.Error())
		return
	}
	st.State, st.Step, st.Message, st.Unit, st.BootID = RunStarting, "starting", "starting", unit, d.platform.bootID()
	d.setRunLocked(st)
	if err := d.platform.launch(ctx, planPath, unit); err != nil {
		st.Unit = ""
		fail("cannot start the grow: " + err.Error())
		return
	}
	d.Log.Info("disks: grow started", "id", req.ID, "mount", req.Path.Mount, "unit", unit)
}

var (
	reDiskKey  = regexp.MustCompile(`^(uuid|mount):[^\x00-\x1f]{1,300}$`)
	reDiskName = regexp.MustCompile(`^[A-Za-z0-9._:+@/-]{0,200}$`)
)

// validDiskRequest checks the shape of a request before anything starts.
// Whether it still matches the server is checked by the helper.
func validDiskRequest(req DiskGrowRequest) error {
	p := req.Path
	if !reDiskKey.MatchString(req.Key) {
		return fmt.Errorf("the filesystem to grow is not named")
	}
	if p.Mount == "" || p.FSType == "" {
		return fmt.Errorf("the filesystem to grow is not named")
	}
	for _, s := range []string{p.Disk, p.Part, p.Crypt, p.PV, p.VG, p.LV, p.FSDevice, p.FSType} {
		if !reDiskName.MatchString(s) || strings.Contains(s, "..") {
			return fmt.Errorf("%q is not a device Argus grows", s)
		}
	}
	if req.Bytes < 0 || p.PartRoom < 0 || p.PVRoom < 0 || p.VGFree < 0 || p.FSRoom < 0 || p.CryptRoom < 0 {
		return fmt.Errorf("sizes cannot be negative")
	}
	if req.Bytes > 0 && p.LV == "" {
		return fmt.Errorf("only a logical volume can be grown by part of its room")
	}
	return nil
}

func (d *Disks) setRunLocked(st DiskRunStatus) {
	d.run = st
	if err := writeFileAtomic(filepath.Join(d.StateDir, disksRunFile), mustJSON(st), 0o600); err != nil {
		d.Log.Error("disks: cannot keep the grow's state", "error", err)
	}
	d.lastChange = d.now()
}

// loadLocked reads what an earlier agent left.
func (d *Disks) loadLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	if b, err := os.ReadFile(filepath.Join(d.StateDir, disksRunFile)); err == nil {
		var st DiskRunStatus
		if json.Unmarshal(b, &st) == nil {
			d.run = st
		}
	}
	if b, err := os.ReadFile(filepath.Join(d.StateDir, disksReportFile)); err == nil {
		var r DiskReport
		if json.Unmarshal(b, &r) == nil {
			d.report = &r
		}
	}
}

// Watch follows a grow and scans when it is time. It returns when ctx ends.
func (d *Disks) Watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Disks) tick(ctx context.Context) {
	if d.followRun() {
		d.changed()
	}
	d.maybeScan(ctx)
}

func (d *Disks) changed() {
	if d.Changed != nil {
		d.Changed()
	}
}

// followRun reads the grow's state from the helper's file, and settles one
// whose helper is gone. It reports whether anything changed.
func (d *Disks) followRun() bool {
	d.mu.Lock()
	d.loadLocked()
	cur := d.run
	d.mu.Unlock()
	if cur.ID == "" || cur.Finished() {
		return false
	}
	b, err := os.ReadFile(filepath.Join(d.StateDir, disksRunFile))
	if err != nil {
		return false
	}
	var disk DiskRunStatus
	if json.Unmarshal(b, &disk) != nil || disk.ID != cur.ID {
		return false
	}
	if !disk.Finished() && disk.Unit != "" && !d.platform.active(disk.Unit) &&
		d.now().Sub(disk.StartedAt) > 10*time.Second {
		now := d.now().UTC()
		disk.State, disk.Step, disk.At, disk.FinishedAt = RunInterrupted, "", now, now
		disk.Message = "the grow stopped before it finished; every step it took is safe to leave, and scanning again shows where it got to"
		if disk.BootID != "" && disk.BootID != d.platform.bootID() {
			disk.Message = "the server restarted during the grow; every step it took is safe to leave, and scanning again shows where it got to"
		}
		if err := writeFileAtomic(filepath.Join(d.StateDir, disksRunFile), mustJSON(disk), 0o600); err != nil {
			d.Log.Error("disks: cannot keep the grow's state", "error", err)
		}
		d.mu.Lock()
		if d.scanID == "" {
			d.scanID = "after-" + disk.ID
		}
		d.mu.Unlock()
		d.Log.Warn("disks: grow settled without its helper", "id", disk.ID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if disk.At.Equal(d.run.At) && disk.State == d.run.State && len(disk.Steps) == len(d.run.Steps) {
		return false
	}
	d.run = disk
	d.lastChange = d.now()
	if disk.Finished() {
		// The helper scanned again once it was done.
		if b, err := os.ReadFile(filepath.Join(d.StateDir, disksReportFile)); err == nil {
			var r DiskReport
			if json.Unmarshal(b, &r) == nil && (d.report == nil || r.ScannedAt.After(d.report.ScannedAt)) {
				d.report = &r
			}
		}
	}
	return true
}

// maybeScan scans when the console asked for it now, or when it is time.
func (d *Disks) maybeScan(ctx context.Context) {
	d.mu.Lock()
	running := d.run.ID != "" && !d.run.Finished()
	due := d.enabled && (d.report == nil || d.now().Sub(d.report.ScannedAt) >= diskScanEvery)
	if d.report != nil && d.report.Error != "" && d.enabled {
		due = d.now().Sub(d.lastTry) >= diskScanRetry
	} else if due && d.now().Sub(d.lastTry) < diskScanRetry {
		due = false
	}
	asked, rescan := d.scanID, d.rescan
	unsupported := d.platform.supported() != ""
	if d.scanning || running || (!due && asked == "") || unsupported {
		if asked != "" && unsupported {
			d.scanDone, d.scanID, d.answered = asked, "", asked
		}
		d.mu.Unlock()
		return
	}
	d.scanning, d.lastTry = true, d.now()
	d.mu.Unlock()
	d.changed()

	cctx, cancel := context.WithTimeout(ctx, diskScanTimeout)
	r, err := d.platform.scan(cctx, DiskScanRequest{StateDir: d.StateDir, Rescan: asked != "" && rescan})
	cancel()

	d.mu.Lock()
	d.scanning = false
	if err != nil {
		prev := DiskReport{Supported: true, Filesystems: []DiskFS{}, Disks: []BlockDisk{}}
		if d.report != nil {
			prev = *d.report
		}
		prev.Error, prev.ScannedAt = err.Error(), d.now().UTC()
		r = prev
		d.Log.Warn("disks: scan failed", "error", err)
	}
	r.bounded()
	d.report = &r
	if err := writeFileAtomic(filepath.Join(d.StateDir, disksReportFile), mustJSON(r), 0o600); err != nil {
		d.Log.Error("disks: cannot keep the report", "error", err)
	}
	if asked != "" && d.scanID == asked {
		d.scanDone, d.scanID, d.answered, d.rescan = asked, "", asked, false
	}
	d.mu.Unlock()
	d.changed()
}
