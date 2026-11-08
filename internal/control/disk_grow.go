package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Growing disks on servers, from the console.

The agent reports each filesystem's stack and whether it can grow
(internal/agent/disks.go); this side keeps the report, plans a grow from it,
holds it for approval where a rule says so, sends it on a heartbeat reply,
and keeps how it went: each command, its output, and the partition table
before. A grow is planned only from the agent's own report: the console names
a filesystem by its key, never a device, and the agent checks the server still
matches before it writes.

These types mirror the agent's JSON; the agent links none of this package.
*/

// Disk answers, as the agent reports them.
const (
	DiskGrow    = "grow"
	DiskFull    = "full"
	DiskBlocked = "blocked"
)

// DiskLayer is one layer of a filesystem's stack.
type DiskLayer struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Size   int64  `json:"size"`
	Used   int64  `json:"used,omitempty"`
	Room   int64  `json:"room,omitempty"`
	Offset int64  `json:"offset,omitempty"`
}

// GrowPath is what growing a filesystem touches, as the agent named it.
type GrowPath struct {
	Disk            string `json:"disk,omitempty"`
	DiskSize        int64  `json:"disk_size,omitempty"`
	DiskSerial      string `json:"disk_serial,omitempty"`
	Part            string `json:"part,omitempty"`
	PartNum         int    `json:"part_num,omitempty"`
	PartStart       int64  `json:"part_start,omitempty"`
	PartSize        int64  `json:"part_size,omitempty"`
	PartRoom        int64  `json:"part_room,omitempty"`
	Table           string `json:"table,omitempty"`
	Crypt           string `json:"crypt,omitempty"`
	CryptRoom       int64  `json:"crypt_room,omitempty"`
	PV              string `json:"pv,omitempty"`
	PVRoom          int64  `json:"pv_room,omitempty"`
	VG              string `json:"vg,omitempty"`
	VGUUID          string `json:"vg_uuid,omitempty"`
	VGFree          int64  `json:"vg_free,omitempty"`
	LV              string `json:"lv,omitempty"`
	LVUUID          string `json:"lv_uuid,omitempty"`
	LVSize          int64  `json:"lv_size,omitempty"`
	FSDevice        string `json:"fs_device,omitempty"`
	FSType          string `json:"fs_type"`
	FSSize          int64  `json:"fs_size"`
	FSRoom          int64  `json:"fs_room,omitempty"`
	Mount           string `json:"mount"`
	DiskNumber      int    `json:"disk_number,omitempty"`
	PartitionNumber int    `json:"partition_number,omitempty"`
	SizeMax         int64  `json:"size_max,omitempty"`
}

// DiskFS is one filesystem, with its stack and its answer.
type DiskFS struct {
	Key         string      `json:"key"`
	Mount       string      `json:"mount"`
	FS          string      `json:"fs"`
	Total       uint64      `json:"total"`
	Used        uint64      `json:"used"`
	Avail       uint64      `json:"avail"`
	Layers      []DiskLayer `json:"layers"`
	Answer      string      `json:"answer"`
	Why         string      `json:"why,omitempty"`
	Room        int64       `json:"room,omitempty"`
	LVM         bool        `json:"lvm,omitempty"`
	Rescan      bool        `json:"rescan,omitempty"`
	Unallocated int64       `json:"unallocated,omitempty"`
	Path        *GrowPath   `json:"path,omitempty"`
}

// percent is how full f is, counted like df.
func (f DiskFS) percent() int {
	usable := f.Used + f.Avail
	if usable == 0 {
		return 0
	}
	return int((f.Used*100 + usable - 1) / usable)
}

// BlockDisk is one disk the scan saw.
type BlockDisk struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Bus         string `json:"bus,omitempty"`
	Model       string `json:"model,omitempty"`
	Serial      string `json:"serial,omitempty"`
	Table       string `json:"table,omitempty"`
	Unallocated int64  `json:"unallocated,omitempty"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// DiskReport is what the agent's scan found.
type DiskReport struct {
	ScannedAt   time.Time   `json:"scanned_at"`
	OS          string      `json:"os"`
	Supported   bool        `json:"supported"`
	Reason      string      `json:"reason,omitempty"`
	Error       string      `json:"error,omitempty"`
	Hypervisor  string      `json:"hypervisor,omitempty"`
	Filesystems []DiskFS    `json:"filesystems"`
	Disks       []BlockDisk `json:"disks"`
	Rescanned   bool        `json:"rescanned,omitempty"`
}

// bounded keeps a report to what the console needs, whatever an agent sent.
func (r *DiskReport) bounded() {
	if len(r.Filesystems) > 48 {
		r.Filesystems = r.Filesystems[:48]
	}
	if len(r.Disks) > 64 {
		r.Disks = r.Disks[:64]
	}
	if r.Filesystems == nil {
		r.Filesystems = []DiskFS{}
	}
	if r.Disks == nil {
		r.Disks = []BlockDisk{}
	}
	r.Error, r.Reason, r.Hypervisor = clip(r.Error, 1000), clip(r.Reason, 500), clip(r.Hypervisor, 100)
	for i := range r.Filesystems {
		f := &r.Filesystems[i]
		if len(f.Layers) > 12 {
			f.Layers = f.Layers[:12]
		}
		if f.Layers == nil {
			f.Layers = []DiskLayer{}
		}
		f.Key, f.Mount, f.Why = clip(f.Key, 400), clip(f.Mount, 400), clip(f.Why, 600)
		for j := range f.Layers {
			f.Layers[j].Name, f.Layers[j].Detail = clip(f.Layers[j].Name, 200), clip(f.Layers[j].Detail, 200)
		}
		if f.Answer != DiskGrow {
			f.Path = nil
		}
	}
}

func (r DiskReport) fs(key string) *DiskFS {
	for i := range r.Filesystems {
		if r.Filesystems[i].Key == key {
			return &r.Filesystems[i]
		}
	}
	return nil
}

// DiskStep is one step of a grow, as it ran.
type DiskStep struct {
	Title   string `json:"title"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	State   string `json:"state"`
}

// DiskRunStatus is what the agent says about a grow.
type DiskRunStatus struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Step        string     `json:"step,omitempty"`
	Message     string     `json:"message,omitempty"`
	Steps       []DiskStep `json:"steps,omitempty"`
	TableBefore string     `json:"table_before,omitempty"`
	Before      int64      `json:"before"`
	After       int64      `json:"after,omitempty"`
}

// DiskBeat is the disks part of a heartbeat. Absent from an agent older
// than disk growing.
type DiskBeat struct {
	Supported bool           `json:"supported"`
	Reason    string         `json:"reason,omitempty"`
	Report    *DiskReport    `json:"report,omitempty"`
	ScanID    string         `json:"scan_id,omitempty"`
	Scanning  bool           `json:"scanning,omitempty"`
	Run       *DiskRunStatus `json:"run,omitempty"`
}

type disksState struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Scanning  bool   `json:"scanning,omitempty"`
}

// DiskGrowRequest is a grow as the agent reads it.
type DiskGrowRequest struct {
	ID    string   `json:"id"`
	Key   string   `json:"key"`
	Path  GrowPath `json:"path"`
	Bytes int64    `json:"bytes,omitempty"`
}

// DiskOrders is what goes on a heartbeat reply.
type DiskOrders struct {
	Scan   bool             `json:"scan"`
	ScanID string           `json:"scan_id,omitempty"`
	Rescan bool             `json:"rescan,omitempty"`
	Run    *DiskGrowRequest `json:"run,omitempty"`
}

/* ── Rules ───────────────────────────────────────────────────────────────── */

// DiskGrowRule says, for servers with a tag, whether a grow waits for a
// second admin and whether one starts on its own.
type DiskGrowRule struct {
	ID            string `json:"id,omitempty"`
	Position      int    `json:"position"`
	Tag           string `json:"tag"`
	NeedsApproval bool   `json:"needsApproval"`
	// AutoPercent grows a filesystem on its own past this percent, when it
	// has room below it; 0 never.
	AutoPercent int `json:"autoPercent"`
}

// DiskGrowPolicy is every rule.
type DiskGrowPolicy struct {
	Rules []DiskGrowRule `json:"rules"`
}

// Validate checks a policy before it is saved.
func (p *DiskGrowPolicy) Validate() error {
	if len(p.Rules) > 100 {
		return fmt.Errorf("at most 100 rules")
	}
	seen := map[string]bool{}
	def := 0
	for i := range p.Rules {
		r := &p.Rules[i]
		r.Tag = strings.TrimSpace(r.Tag)
		r.Position = i
		if r.Tag == "" {
			def++
		} else if len(r.Tag) > 100 || !validTag(r.Tag) {
			return fmt.Errorf("%q is not a server tag", r.Tag)
		}
		if seen[strings.ToLower(r.Tag)] {
			return fmt.Errorf("two rules for tag %q", r.Tag)
		}
		seen[strings.ToLower(r.Tag)] = true
		if r.AutoPercent != 0 && (r.AutoPercent < 50 || r.AutoPercent > 99) {
			return fmt.Errorf("growing on its own starts between 50%% and 99%% full")
		}
	}
	if def != 1 {
		return fmt.Errorf("there must be exactly one rule for every other server")
	}
	return nil
}

// validTag allows what server tags allow: letters, digits and -_.:/
func validTag(t string) bool {
	for _, c := range t {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("-_.:/ ", c):
		default:
			return false
		}
	}
	return true
}

// ruleFor is the rule for a server with tags: the first naming one of them,
// else the one for every other server.
func (p DiskGrowPolicy) ruleFor(tags []string) DiskGrowRule {
	has := map[string]bool{}
	for _, t := range tags {
		has[strings.ToLower(t)] = true
	}
	var def *DiskGrowRule
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Tag == "" {
			def = r
			continue
		}
		if has[strings.ToLower(r.Tag)] {
			return *r
		}
	}
	if def != nil {
		return *def
	}
	// No rule could be read: ask for approval rather than assume none.
	return DiskGrowRule{NeedsApproval: true}
}

// DiskGrowPolicy reads every rule.
func (s *Store) DiskGrowPolicy(ctx context.Context) (DiskGrowPolicy, error) {
	p := DiskGrowPolicy{Rules: []DiskGrowRule{}}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, position, tag, needs_approval, auto_percent
		  FROM disk_grow_rules ORDER BY (tag = '') ASC, position, tag`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var r DiskGrowRule
		if err := rows.Scan(&r.ID, &r.Position, &r.Tag, &r.NeedsApproval, &r.AutoPercent); err != nil {
			return p, err
		}
		p.Rules = append(p.Rules, r)
	}
	return p, rows.Err()
}

// SaveDiskGrowPolicy replaces every rule.
func (s *Store) SaveDiskGrowPolicy(ctx context.Context, p DiskGrowPolicy) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM disk_grow_rules`); err != nil {
		return err
	}
	for _, r := range p.Rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO disk_grow_rules (position, tag, needs_approval, auto_percent) VALUES ($1,$2,$3,$4)`,
			r.Position, r.Tag, r.NeedsApproval, r.AutoPercent); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

/* ── The agent's report ──────────────────────────────────────────────────── */

// recordDiskBeat keeps what the agent said about disks.
func (s *Store) recordDiskBeat(ctx context.Context, host string, b DiskBeat) error {
	st, err := json.Marshal(disksState{Supported: b.Supported, Reason: clip(b.Reason, 500), Scanning: b.Scanning})
	if err != nil {
		return err
	}
	if _, err := s.db(ctx).Exec(ctx, `
		UPDATE agents SET disks_state = $2::jsonb
		 WHERE lower(hostname) = lower($1) AND disks_state IS DISTINCT FROM $2::jsonb`, host, st); err != nil {
		return err
	}
	if b.ScanID != "" {
		if _, err := s.db(ctx).Exec(ctx, `
			UPDATE agents SET disks_scan_id = NULL, disks_scan_at = NULL, disks_rescan = false
			 WHERE lower(hostname) = lower($1) AND disks_scan_id = $2`, host, b.ScanID); err != nil {
			return err
		}
	}
	if b.Report == nil {
		return nil
	}
	r := *b.Report
	r.bounded()
	rb, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).Exec(ctx, `UPDATE agents SET disks = $2::jsonb WHERE lower(hostname) = lower($1)`, host, rb)
	return err
}

// diskReportFor reads what host's agent last reported.
func (s *Store) diskReportFor(ctx context.Context, host string) (*DiskReport, *disksState, bool, error) {
	var (
		rb, sb []byte
		scan   *string
	)
	err := s.db(ctx).QueryRow(ctx, `SELECT disks, disks_state, disks_scan_id FROM agents WHERE lower(hostname) = lower($1)`,
		host).Scan(&rb, &sb, &scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var (
		r  *DiskReport
		st *disksState
	)
	if len(rb) > 0 {
		r = &DiskReport{}
		if json.Unmarshal(rb, r) != nil {
			r = nil
		} else {
			r.bounded()
		}
	}
	if len(sb) > 0 {
		st = &disksState{}
		if json.Unmarshal(sb, st) != nil {
			st = nil
		}
	}
	return r, st, scan != nil, nil
}

func (s *Store) diskScanAsked(ctx context.Context, host string) (string, bool, error) {
	var (
		id     *string
		rescan bool
	)
	err := s.db(ctx).QueryRow(ctx, `SELECT disks_scan_id, disks_rescan FROM agents WHERE lower(hostname) = lower($1)`,
		host).Scan(&id, &rescan)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return *id, rescan, nil
}

// askDiskScan asks host's agent to scan on its next heartbeat; rescan asks
// the disks for their size first. A scan already asked for is kept, and a
// rescan is added to it.
func (s *Store) askDiskScan(ctx context.Context, host string, rescan bool) (string, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE agents SET disks_scan_id = COALESCE(disks_scan_id, gen_random_uuid()::text),
		       disks_scan_at = COALESCE(disks_scan_at, now()), disks_rescan = disks_rescan OR $2
		 WHERE lower(hostname) = lower($1) RETURNING disks_scan_id`, host, rescan).Scan(&id)
	return id, err
}

/* ── Runs ────────────────────────────────────────────────────────────────── */

// DiskGrowRun is one grow, as the console shows it.
type DiskGrowRun struct {
	ID            string     `json:"id"`
	Hostname      string     `json:"hostname"`
	AssetHostname string     `json:"assetHostname"`
	AssetID       string     `json:"assetId,omitempty"`
	Trigger       string     `json:"trigger"`
	State         string     `json:"state"`
	Key           string     `json:"key"`
	Mount         string     `json:"mount"`
	Path          GrowPath   `json:"path"`
	GrowBytes     int64      `json:"growBytes"`
	PlannedBytes  int64      `json:"plannedBytes"`
	Summary       string     `json:"summary"`
	Reason        string     `json:"reason"`
	RequestedBy   string     `json:"requestedBy"`
	RequestedAt   time.Time  `json:"requestedAt"`
	DecidedBy     *string    `json:"decidedBy"`
	DecidedAt     *time.Time `json:"decidedAt"`
	SentAt        *time.Time `json:"sentAt"`
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	Step          string     `json:"step"`
	Message       string     `json:"message"`
	Steps         []DiskStep `json:"steps"`
	TableBefore   string     `json:"tableBefore"`
	SizeBefore    int64      `json:"sizeBefore"`
	SizeAfter     int64      `json:"sizeAfter"`
}

const diskRunCols = `id::text, hostname, asset_hostname, trigger, state, fs_key, mount, path, grow_bytes,
	planned_bytes, summary, reason, requested_by, requested_at, decided_by, decided_at, sent_at, started_at,
	finished_at, updated_at, step, message, steps, table_before, size_before, size_after`

func scanDiskRun(row pgx.Row) (DiskGrowRun, error) {
	var (
		r           DiskGrowRun
		path, steps []byte
	)
	err := row.Scan(&r.ID, &r.Hostname, &r.AssetHostname, &r.Trigger, &r.State, &r.Key, &r.Mount, &path,
		&r.GrowBytes, &r.PlannedBytes, &r.Summary, &r.Reason, &r.RequestedBy, &r.RequestedAt, &r.DecidedBy,
		&r.DecidedAt, &r.SentAt, &r.StartedAt, &r.FinishedAt, &r.UpdatedAt, &r.Step, &r.Message, &steps,
		&r.TableBefore, &r.SizeBefore, &r.SizeAfter)
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(path, &r.Path)
	_ = json.Unmarshal(steps, &r.Steps)
	if r.Steps == nil {
		r.Steps = []DiskStep{}
	}
	return r, nil
}

// ErrDiskRunActive is a grow asked of a server that already has one.
var ErrDiskRunActive = errors.New("this server already has a disk grow going or waiting; " +
	"wait for it to finish or take it back")

// ErrDiskRunNotFound is a grow that does not exist.
var ErrDiskRunNotFound = errors.New("no such disk grow")

// diskPlan is a grow planned from a report.
type diskPlan struct {
	FS      DiskFS
	Bytes   int64 // what the agent is told to give an LV; 0 is all
	Planned int64 // what the filesystem should gain
	Summary string
}

// planDiskGrow plans growing the filesystem key names, from the report's own
// stack. bytes asks for part of a volume group's room; 0 takes all of it.
func planDiskGrow(r DiskReport, key string, bytes int64) (diskPlan, error) {
	f := r.fs(key)
	if f == nil {
		return diskPlan{}, fmt.Errorf("this server's last scan has no such filesystem; scan it again")
	}
	if f.Answer != DiskGrow || f.Path == nil {
		why := strings.TrimSuffix(f.Why, ".")
		if why == "" {
			why = "there is no room below it"
		}
		return diskPlan{}, fmt.Errorf("%s cannot grow: %s", f.Mount, why)
	}
	p := diskPlan{FS: *f, Planned: f.Room}
	if bytes < 0 {
		return p, fmt.Errorf("the size to add cannot be negative")
	}
	if bytes > 0 {
		if !f.LVM || f.Path.LV == "" {
			return p, fmt.Errorf("%s grows into all of its room; only an LVM volume can take part of it", f.Mount)
		}
		if bytes > f.Room {
			return p, fmt.Errorf("%s has %s to grow into, less than asked", f.Mount, gbytes(f.Room))
		}
		// Whatever reaches the group from below goes to the group; the
		// volume takes what was asked, and asking for all of it is all.
		if lvRoom := f.Room - f.Path.FSRoom; bytes < lvRoom {
			p.Bytes, p.Planned = bytes, bytes+f.Path.FSRoom
		}
	}
	p.Summary = fmt.Sprintf("grow %s (%s) by %s", f.Mount, f.FS, gbytes(p.Planned))
	return p, nil
}

type newDiskRun struct {
	Hostname, AssetHostname string
	Trigger, State          string
	Plan                    diskPlan
	Reason                  string
	RequestedBy             string
	AutoDay                 *string
}

func (s *Store) hasPendingDiskRun(ctx context.Context, host string) (bool, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx, `
		SELECT count(*) FROM disk_grow_runs
		 WHERE lower(hostname) = lower($1) AND state IN ('awaiting_approval','queued','running')`, host).Scan(&n)
	return n > 0, err
}

// CreateDiskRun creates a grow, unless the server has one pending.
func (s *Store) CreateDiskRun(ctx context.Context, n newDiskRun) (DiskGrowRun, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return DiskGrowRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('disk-grow:' || lower($1)))`, n.Hostname); err != nil {
		return DiskGrowRun{}, err
	}
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM disk_grow_runs
		 WHERE lower(hostname) = lower($1) AND state IN ('awaiting_approval','queued','running')`,
		n.Hostname).Scan(&pending); err != nil {
		return DiskGrowRun{}, err
	}
	if pending > 0 {
		return DiskGrowRun{}, ErrDiskRunActive
	}
	path, err := json.Marshal(n.Plan.FS.Path)
	if err != nil {
		return DiskGrowRun{}, err
	}
	r, err := scanDiskRun(tx.QueryRow(ctx, `
		INSERT INTO disk_grow_runs (hostname, asset_hostname, trigger, state, fs_key, mount, path, grow_bytes,
		                            planned_bytes, summary, reason, requested_by, auto_day, size_before)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13,$14)
		RETURNING `+diskRunCols, n.Hostname, n.AssetHostname, n.Trigger, n.State, n.Plan.FS.Key, n.Plan.FS.Mount, path,
		n.Plan.Bytes, n.Plan.Planned, n.Plan.Summary, n.Reason, n.RequestedBy, n.AutoDay, n.Plan.FS.Path.FSSize))
	if err != nil {
		if isUniqueViolation(err) {
			return r, ErrDiskRunActive
		}
		return r, err
	}
	return r, tx.Commit(ctx)
}

// DiskRunByID reads a grow.
func (s *Store) DiskRunByID(ctx context.Context, id string) (DiskGrowRun, error) {
	r, err := scanDiskRun(s.db(ctx).QueryRow(ctx, `SELECT `+diskRunCols+` FROM disk_grow_runs WHERE id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrDiskRunNotFound
	}
	return r, err
}

// DiskRunsFor lists host's grows, newest first.
func (s *Store) DiskRunsFor(ctx context.Context, host string, limit int) ([]DiskGrowRun, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+diskRunCols+` FROM disk_grow_runs
		WHERE lower(hostname) = lower($1) ORDER BY requested_at DESC LIMIT $2`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DiskGrowRun{}
	for rows.Next() {
		r, err := scanDiskRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// diskRunToSend is the grow to put on host's heartbeat reply, marking it
// sent: from then on it cannot be taken back.
func (s *Store) diskRunToSend(ctx context.Context, host string) (*DiskGrowRun, error) {
	r, err := scanDiskRun(s.db(ctx).QueryRow(ctx, `
		UPDATE disk_grow_runs SET sent_at = COALESCE(sent_at, now())
		 WHERE id = (SELECT id FROM disk_grow_runs
		              WHERE lower(hostname) = lower($1) AND state IN ('queued','running')
		              ORDER BY requested_at LIMIT 1)
		RETURNING `+diskRunCols, host))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// recordDiskRunStatus keeps what the agent said about a grow it was sent.
// It returns the grow when the report ended it, so the end is audited once.
func (s *Store) recordDiskRunStatus(ctx context.Context, host string, st DiskRunStatus) (*DiskGrowRun, error) {
	state := st.State
	switch state {
	case "starting", RunRunning:
		state = RunRunning
	case RunDone, RunFailed, RunInterrupted:
	default:
		return nil, nil
	}
	if len(st.Steps) > 30 {
		st.Steps = st.Steps[:30]
	}
	for i := range st.Steps {
		s := &st.Steps[i]
		s.Title, s.Command, s.Output, s.State = clip(s.Title, 200), clip(s.Command, 600), clip(s.Output, 4000),
			clip(s.State, 20)
	}
	steps, _ := json.Marshal(st.Steps)
	if st.Steps == nil {
		steps = []byte("[]")
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE disk_grow_runs SET
		       state = $3, step = $4, message = $5, steps = $6::jsonb,
		       table_before = CASE WHEN $7 <> '' THEN $7 ELSE table_before END,
		       size_after = $8,
		       started_at = COALESCE(started_at, now()),
		       finished_at = CASE WHEN $3 IN ('done','failed','interrupted') THEN now() ELSE NULL END,
		       updated_at = now()
		 WHERE id::text = $2 AND lower(hostname) = lower($1)
		   AND (state IN ('queued','running') OR (state = 'interrupted' AND step = 'stale'))
		   AND ((state, step, message, steps) IS DISTINCT FROM ($3, $4, $5, $6::jsonb)
		        OR updated_at < now() - interval '5 minutes')
		RETURNING `+diskRunCols, host, st.ID, state, clip(st.Step, 300), clip(st.Message, 2000), steps,
		clip(st.TableBefore, 16000), st.After)
	r, err := scanDiskRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !runFinished(r.State) {
		return nil, nil
	}
	return &r, nil
}

// DecideDiskRun approves or denies a grow waiting for approval. The one who
// asked cannot decide it.
func (s *Store) DecideDiskRun(ctx context.Context, id, by string, approve bool) (DiskGrowRun, error) {
	r, err := s.DiskRunByID(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != RunAwaitingApproval {
		return r, fmt.Errorf("%w: this grow is %s, not waiting for approval", ErrRunState,
			strings.ReplaceAll(r.State, "_", " "))
	}
	if strings.EqualFold(r.RequestedBy, by) {
		return r, fmt.Errorf("%w: a grow you asked for is approved by another admin", ErrRunState)
	}
	next := RunDenied
	if approve {
		next = RunQueued
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE disk_grow_runs SET state = $2, decided_by = $3, decided_at = now(), updated_at = now(),
		       finished_at = CASE WHEN $2 = 'denied' THEN now() ELSE NULL END,
		       message = CASE WHEN $2 = 'denied' THEN 'denied by ' || $3 ELSE message END
		 WHERE id::text = $1 AND state = 'awaiting_approval'
		RETURNING `+diskRunCols, id, next, by)
	r, err = scanDiskRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: someone decided this grow already", ErrRunState)
	}
	if err != nil && isUniqueViolation(err) {
		return r, ErrDiskRunActive
	}
	return r, err
}

// CancelDiskRun takes back a grow that has not reached the server.
func (s *Store) CancelDiskRun(ctx context.Context, id, by string) (DiskGrowRun, error) {
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE disk_grow_runs SET state = 'cancelled', finished_at = now(), updated_at = now(),
		       message = 'taken back by ' || $2
		 WHERE id::text = $1
		   AND (state = 'awaiting_approval' OR (state = 'queued' AND sent_at IS NULL))
		RETURNING `+diskRunCols, id, by)
	r, err := scanDiskRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.DiskRunByID(ctx, id)
		if gerr != nil {
			return cur, gerr
		}
		return cur, fmt.Errorf("%w: the server has already started this grow; it reports how it ended", ErrRunState)
	}
	return r, err
}

// expireStaleDiskRuns ends grows the server stopped reporting on. A grow
// takes seconds; an hour of silence is a server that is down.
func (s *Store) expireStaleDiskRuns(ctx context.Context, host string) ([]DiskGrowRun, error) {
	rows, err := s.db(ctx).Query(ctx, `
		UPDATE disk_grow_runs SET state = 'interrupted', step = 'stale', finished_at = now(), updated_at = now(),
		       message = 'the server stopped reporting on this grow for an hour; scan it again to see where it got to'
		 WHERE state = 'running' AND updated_at < now() - interval '1 hour'
		   AND ($1 = '' OR lower(hostname) = lower($1))
		RETURNING `+diskRunCols, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiskGrowRun
	for rows.Next() {
		r, err := scanDiskRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ── The view ────────────────────────────────────────────────────────────── */

// DisksView is a server's Disks tab.
type DisksView struct {
	Hostname  string        `json:"hostname"`
	Enrolled  bool          `json:"enrolled"`
	Reported  bool          `json:"reported"`
	Supported bool          `json:"supported"`
	Reason    string        `json:"reason"`
	Scanning  bool          `json:"scanning"`
	ScanAsked bool          `json:"scanAsked"`
	Report    *DiskReport   `json:"report"`
	Rule      DiskGrowRule  `json:"rule"`
	Runs      []DiskGrowRun `json:"runs"`
}
