package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/storage"
)

/*
The Status page: how argus-control itself is doing.

Everything on it is platform-wide -- the host, the database every organization
shares, every organization's agents and sessions -- so only a platform owner
is shown it, and anyone else is answered 404 like the other platform routes.

Cheap numbers (process, machine, the database's answer time, the error log)
are read on every call; the console asks every 15 seconds. The ones that scan
(database size by table, recordings per organization, agents per
organization) are cached for statusSlowEvery and carry the time they were
measured, so the page can say how old they are.
*/

const (
	statusSlowEvery = time.Minute
	// statusDBTimeout bounds every query the page makes: a database too slow
	// to answer is itself the finding, not a reason for the page to hang.
	statusDBTimeout = 3 * time.Second

	dbSlowAfter         = 250 * time.Millisecond
	poolBusyShare       = 0.9
	diskWarnShare       = 0.80
	diskBadShare        = 0.90
	memWarnShare        = 0.90
	certWarnDays        = 14
	certManagedWarnDays = 7
	errorsWarnHour      = 10
	statusTopTables     = 4
)

// ControlStatus is the whole page.
type ControlStatus struct {
	Version       string    `json:"version"`
	Hostname      string    `json:"hostname"`
	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	CheckedAt     time.Time `json:"checkedAt"`

	Machine     MachineStatus      `json:"machine"`
	Process     ProcessStatus      `json:"process"`
	Database    DatabaseStatus     `json:"database"`
	Fleet       *FleetStatus       `json:"fleet,omitempty"`
	Recordings  RecordingsStatus   `json:"recordings"`
	Certificate *CertificateStatus `json:"certificate,omitempty"`
	Errors      ErrorCounts        `json:"errors"`
	// Problems is what needs a person, worst first. Empty means healthy.
	Problems []StatusProblem `json:"problems"`
}

// ProcessStatus is argus-control's own footprint.
type ProcessStatus struct {
	MemoryBytes uint64 `json:"memoryBytes"`
	Goroutines  int    `json:"goroutines"`
	// Tunnels is the agents connected to this replica.
	Tunnels   int    `json:"tunnels"`
	GoVersion string `json:"goVersion"`
}

// DatabaseStatus is the database every organization's tables live in.
type DatabaseStatus struct {
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
	LatencyMs float64 `json:"latencyMs"`
	Version   string  `json:"version,omitempty"`
	Name      string  `json:"name,omitempty"`
	InUse     int32   `json:"inUse"`
	Open      int32   `json:"open"`
	Max       int32   `json:"max"`

	SizeBytes  *int64      `json:"sizeBytes,omitempty"`
	Tables     []TableSize `json:"tables"`
	MeasuredAt *time.Time  `json:"measuredAt,omitempty"`
}

// TableSize is one table's size summed over every organization's schema.
type TableSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// FleetStatus is every enabled organization's agents and sessions.
type FleetStatus struct {
	Organizations   int       `json:"organizations"`
	AgentsEnrolled  int       `json:"agentsEnrolled"`
	AgentsConnected int       `json:"agentsConnected"`
	SessionsActive  int       `json:"sessionsActive"`
	SessionsSilent  int       `json:"sessionsSilent"`
	SessionsToday   int       `json:"sessionsToday"`
	MeasuredAt      time.Time `json:"measuredAt"`
}

// RecordingsStatus is where recordings go and how much is there.
type RecordingsStatus struct {
	// Where is "bucket", "directory" or "" when nothing is configured.
	Where string `json:"where"`
	// Location is the bucket's name or the directory's path.
	Location string `json:"location,omitempty"`
	// Count and Bytes are the stored recordings, Bytes as the agents
	// reported each one's size.
	Count      int64      `json:"count"`
	Bytes      int64      `json:"bytes"`
	WeekBytes  int64      `json:"weekBytes"`
	Oldest     *time.Time `json:"oldest,omitempty"`
	MeasuredAt *time.Time `json:"measuredAt,omitempty"`

	LastUpload      *time.Time `json:"lastUpload,omitempty"`
	LastUploadError string     `json:"lastUploadError,omitempty"`
	LastFailure     *time.Time `json:"lastFailure,omitempty"`
}

// CertificateStatus is the certificate browsers are served.
type CertificateStatus struct {
	Subject  string    `json:"subject"`
	NotAfter time.Time `json:"notAfter"`
	DaysLeft int       `json:"daysLeft"`
	Managed  bool      `json:"managed"`
}

// StatusProblem is one thing on the page that needs a person.
type StatusProblem struct {
	// Severity is "critical" or "warning".
	Severity string `json:"severity"`
	// Area is which part of the page it belongs to: database, disk,
	// memory, certificate, recordings, errors.
	Area   string `json:"area"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// statusCache holds the numbers that take a scan to read.
type statusCache struct {
	mu   sync.Mutex
	at   time.Time
	slow slowStatus
}

type slowStatus struct {
	fleet      *FleetStatus
	sizeBytes  *int64
	tables     []TableSize
	dbName     string
	dbVersion  string
	recordings struct {
		count, bytes, week int64
		oldest             *time.Time
	}
}

func (a *API) getControlStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.controlStatus(r.Context(), time.Now()))
}

func (a *API) controlStatus(ctx context.Context, now time.Time) ControlStatus {
	st := ControlStatus{
		Version:       a.Version,
		StartedAt:     a.startedAt.UTC(),
		UptimeSeconds: int64(now.Sub(a.startedAt).Seconds()),
		CheckedAt:     now.UTC(),
		Errors:        a.errs.counts(now),
	}
	st.Hostname, _ = os.Hostname()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st.Process = ProcessStatus{
		MemoryBytes: m.Sys - m.HeapReleased,
		Goroutines:  runtime.NumGoroutine(),
		GoVersion:   runtime.Version(),
	}
	if a.Tunnels != nil {
		st.Process.Tunnels = a.Tunnels.count()
	}

	var cfg *Settings
	if a.Settings != nil {
		if s, err := a.Settings.Settings(); err == nil {
			cfg = &s
		}
	}
	st.Recordings = a.recordingsWhere(cfg)
	st.Machine = a.machineStatus(now, a.statusDisks())
	st.Certificate = certificateStatus(cfg, now)

	if a.store != nil {
		st.Database = a.databaseStatus(ctx)
		if st.Database.OK {
			slow, at := a.slowStatus(ctx, now)
			st.Fleet = slow.fleet
			st.Database.SizeBytes = slow.sizeBytes
			st.Database.Tables = slow.tables
			st.Database.Name, st.Database.Version = slow.dbName, slow.dbVersion
			if !at.IsZero() {
				t := at.UTC()
				st.Database.MeasuredAt = &t
				st.Recordings.MeasuredAt = &t
			}
			st.Recordings.Count = slow.recordings.count
			st.Recordings.Bytes = slow.recordings.bytes
			st.Recordings.WeekBytes = slow.recordings.week
			st.Recordings.Oldest = slow.recordings.oldest
		}
	}
	if st.Database.Tables == nil {
		st.Database.Tables = []TableSize{}
	}

	okAt, failAt, failMsg := a.uploads.snapshot()
	if !okAt.IsZero() {
		t := okAt.UTC()
		st.Recordings.LastUpload = &t
	}
	if !failAt.IsZero() {
		t := failAt.UTC()
		st.Recordings.LastFailure = &t
		st.Recordings.LastUploadError = failMsg
	}

	st.Problems = statusProblems(st)
	return st
}

// recordingsWhere says where shipped recordings are kept.
func (a *API) recordingsWhere(cfg *Settings) RecordingsStatus {
	switch rec := a.recordings.(type) {
	case nil:
		return RecordingsStatus{}
	case *storage.Dir:
		return RecordingsStatus{Where: "directory", Location: rec.Root()}
	default:
		rs := RecordingsStatus{Where: "bucket"}
		if cfg != nil {
			rs.Location = cfg.Storage.Bucket
		}
		return rs
	}
}

// statusDisks is every filesystem argus-control writes to: the spool every
// shipped recording passes through, and the recordings directory when they
// are kept on this host.
func (a *API) statusDisks() []DiskStatus {
	out := []DiskStatus{{Path: os.TempDir(), Use: "Recordings arriving from agents"}}
	if d, ok := a.recordings.(*storage.Dir); ok {
		root := filepath.Clean(d.Root())
		if root != filepath.Clean(os.TempDir()) {
			out = append([]DiskStatus{{Path: root, Use: "Recordings kept on this server"}}, out...)
		} else {
			out[0].Use = "Recordings, and recordings arriving from agents"
		}
	}
	return out
}

func certificateStatus(s *Settings, now time.Time) *CertificateStatus {
	if s == nil || !s.TLS.Enabled || s.TLS.Certificate == nil {
		return nil
	}
	c := s.TLS.Certificate
	days := int(c.NotAfter.Sub(now).Hours() / 24)
	if c.NotAfter.Before(now) {
		days = -1
	}
	return &CertificateStatus{Subject: c.Subject, NotAfter: c.NotAfter.UTC(), DaysLeft: days, Managed: s.TLS.Managed}
}

func (a *API) databaseStatus(ctx context.Context) DatabaseStatus {
	var d DatabaseStatus
	ctx, cancel := context.WithTimeout(PlatformContext(ctx), statusDBTimeout)
	defer cancel()
	start := time.Now()
	var one int
	err := a.store.db(ctx).QueryRow(ctx, `SELECT 1`).Scan(&one)
	d.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		d.Error = clipMessage(err.Error())
		if errors.Is(err, context.DeadlineExceeded) {
			d.Error = fmt.Sprintf("did not answer within %s", statusDBTimeout)
		}
	} else {
		d.OK = true
	}
	d.InUse, d.Open, d.Max = a.store.poolUse()
	return d
}

// poolUse sums every organization's pool: they share one server's
// connection limit, so it is their total that runs out.
func (s *Store) poolUse() (inUse, open, max int32) {
	add := func(st interface {
		AcquiredConns() int32
		TotalConns() int32
		MaxConns() int32
	}) {
		inUse += st.AcquiredConns()
		open += st.TotalConns()
		max += st.MaxConns()
	}
	if s.pool != nil {
		add(s.pool.Stat())
	}
	s.tmu.Lock()
	defer s.tmu.Unlock()
	for _, d := range s.tenants {
		if d != nil && d.pool != nil && d.pool != s.pool {
			add(d.pool.Stat())
		}
	}
	return inUse, open, max
}

// slowStatus returns the cached scans, refreshing them when they are older
// than statusSlowEvery. A refresh that fails keeps the previous numbers and
// their time, so the page shows how stale they are rather than blanks.
func (a *API) slowStatus(ctx context.Context, now time.Time) (slowStatus, time.Time) {
	c := &a.status
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) < statusSlowEvery {
		return c.slow, c.at
	}
	ctx, cancel := context.WithTimeout(PlatformContext(ctx), statusDBTimeout)
	defer cancel()
	s, err := a.store.readSlowStatus(ctx, now)
	if err != nil {
		a.log.Warn("status: measuring the database failed", "error", err)
		return c.slow, c.at
	}
	c.slow, c.at = s, now
	return s, now
}

func (s *Store) readSlowStatus(ctx context.Context, now time.Time) (slowStatus, error) {
	var out slowStatus
	db := s.db(ctx)

	var size int64
	if err := db.QueryRow(ctx, `SELECT current_database(), pg_database_size(current_database()),
	                                   current_setting('server_version')`).
		Scan(&out.dbName, &size, &out.dbVersion); err != nil {
		return out, fmt.Errorf("database size: %w", err)
	}
	out.sizeBytes = &size

	// Every organization's tables, summed by name: "the audit log" is one
	// thing to the person reading the page however many schemas hold it.
	rows, err := db.Query(ctx, `
		SELECT c.relname, sum(pg_total_relation_size(c.oid))::bigint
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind IN ('r', 'p')
		   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		   AND n.nspname NOT LIKE 'pg\_%'
		 GROUP BY c.relname`)
	if err != nil {
		return out, fmt.Errorf("table sizes: %w", err)
	}
	var tables []TableSize
	for rows.Next() {
		var t TableSize
		if err := rows.Scan(&t.Name, &t.Bytes); err != nil {
			rows.Close()
			return out, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.tables = topTables(tables, size, statusTopTables)

	fleet := &FleetStatus{MeasuredAt: now.UTC()}
	silence := fmt.Sprintf("%d seconds", int(SessionSilenceThreshold.Seconds()))
	fresh := fmt.Sprintf("%d seconds", int(tunnelFreshFor.Seconds()))
	var firstErr error
	err = s.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
		if firstErr != nil {
			return
		}
		var (
			enrolled, connected, active, silent, today int
			count, bytes, week                         int64
			oldest                                     *time.Time
		)
		err := s.db(tctx).QueryRow(tctx, `
			SELECT
			  (SELECT count(*) FROM agent_credentials WHERE revoked_at IS NULL),
			  (SELECT count(*) FROM agent_credentials
			     WHERE revoked_at IS NULL AND tunnel_seen_at > now() - $2::interval),
			  (SELECT count(*) FROM sessions
			     WHERE state = 'active' AND last_reported_at >  now() - $1::interval),
			  (SELECT count(*) FROM sessions
			     WHERE state = 'active' AND last_reported_at <= now() - $1::interval),
			  (SELECT count(*) FROM sessions WHERE started_at > now() - interval '24 hours'),
			  count(*), coalesce(sum(recording_bytes), 0)::bigint,
			  coalesce(sum(recording_bytes) FILTER (WHERE started_at > now() - interval '7 days'), 0)::bigint,
			  min(started_at)
			FROM sessions WHERE recording_key IS NOT NULL AND recording_key <> ''`,
			silence, fresh).
			Scan(&enrolled, &connected, &active, &silent, &today, &count, &bytes, &week, &oldest)
		if err != nil {
			firstErr = err
			return
		}
		fleet.Organizations++
		fleet.AgentsEnrolled += enrolled
		fleet.AgentsConnected += connected
		fleet.SessionsActive += active
		fleet.SessionsSilent += silent
		fleet.SessionsToday += today
		out.recordings.count += count
		out.recordings.bytes += bytes
		out.recordings.week += week
		if oldest != nil && (out.recordings.oldest == nil || oldest.Before(*out.recordings.oldest)) {
			t := oldest.UTC()
			out.recordings.oldest = &t
		}
	})
	if err == nil {
		err = firstErr
	}
	if err != nil {
		return out, fmt.Errorf("fleet: %w", err)
	}
	out.fleet = fleet
	return out, nil
}

// topTables keeps the n largest and folds the rest into one row, so the rows
// add up to the database's size as the page states it.
func topTables(all []TableSize, total int64, n int) []TableSize {
	sort.Slice(all, func(i, j int) bool {
		if all[i].Bytes != all[j].Bytes {
			return all[i].Bytes > all[j].Bytes
		}
		return all[i].Name < all[j].Name
	})
	if len(all) > n {
		all = all[:n]
	}
	out := append([]TableSize{}, all...)
	var sum int64
	for _, t := range out {
		sum += t.Bytes
	}
	if rest := total - sum; rest > 0 {
		out = append(out, TableSize{Name: "", Bytes: rest})
	}
	return out
}

// statusProblems is what on the page needs a person, worst first.
func statusProblems(st ControlStatus) []StatusProblem {
	var ps []StatusProblem
	add := func(sev, area, title, detail string) {
		ps = append(ps, StatusProblem{Severity: sev, Area: area, Title: title, Detail: detail})
	}

	db := st.Database
	switch {
	case !db.OK && db.Error != "":
		add("critical", "database", "The database is not answering",
			"Sign-ins, sessions and the audit log all need it. "+db.Error)
	case db.LatencyMs >= float64(dbSlowAfter.Milliseconds()):
		add("warning", "database", fmt.Sprintf("The database took %.0f ms to answer", db.LatencyMs),
			"Every page and every session start waits on it.")
	}
	if db.Max > 0 && float64(db.InUse) >= poolBusyShare*float64(db.Max) {
		add("warning", "database", fmt.Sprintf("%d of %d database connections are in use", db.InUse, db.Max),
			"When all are taken, requests wait for one and then fail.")
	}

	for _, d := range st.Machine.Disks {
		if d.Total == 0 {
			continue
		}
		used := 1 - float64(d.Avail)/float64(d.Total)
		pct := int(used*100 + 0.5)
		switch {
		case used >= diskBadShare:
			add("critical", "disk", fmt.Sprintf("The disk at %s is %d%% full", d.Path, pct),
				"Recordings from agents are written here first; when it fills, they cannot be stored.")
		case used >= diskWarnShare:
			add("warning", "disk", fmt.Sprintf("The disk at %s is %d%% full", d.Path, pct), "")
		}
	}

	if m := st.Machine; m.MemTotal > 0 {
		used := 1 - float64(m.MemAvailable)/float64(m.MemTotal)
		if used >= memWarnShare {
			add("warning", "memory", fmt.Sprintf("Memory is %d%% used", int(used*100+0.5)),
				"The system may start stopping processes to free some.")
		}
	}

	if c := st.Certificate; c != nil {
		switch {
		case c.DaysLeft < 0:
			add("critical", "certificate", "The certificate has expired",
				"Browsers and agents refuse the connection until it is replaced.")
		case c.Managed && c.DaysLeft < certManagedWarnDays:
			// Renewed by the control plane itself well before this; still
			// this close means the renewal is failing.
			add("warning", "certificate", fmt.Sprintf("The certificate expires in %d days and has not renewed", c.DaysLeft),
				"argus-control issues and renews this one itself; its log says why it could not.")
		case !c.Managed && c.DaysLeft < certWarnDays:
			add("warning", "certificate", fmt.Sprintf("The certificate expires in %d days", c.DaysLeft),
				"Replace it before then in Settings › Certificate.")
		}
	}

	r := st.Recordings
	if r.LastFailure != nil && (r.LastUpload == nil || r.LastFailure.After(*r.LastUpload)) {
		add("warning", "recordings", "The last recording could not be stored",
			"Agents keep it and try again. "+r.LastUploadError)
	}

	if st.Errors.LastHour >= errorsWarnHour {
		add("warning", "errors", fmt.Sprintf("%d requests failed in the last hour", st.Errors.LastHour), "")
	}

	sort.SliceStable(ps, func(i, j int) bool {
		return ps[i].Severity == "critical" && ps[j].Severity != "critical"
	})
	if ps == nil {
		ps = []StatusProblem{}
	}
	return ps
}
