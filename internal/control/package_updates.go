package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Operating system package updates.

The agent looks for updates on its server (apt, dnf, yum or zypper) every six
hours while the policy for that server is not "off", and reports what it
found on a heartbeat: what can be upgraded, which of it fixes a security
issue, whether the server wants a reboot. It is kept on agents.packages
(migration 050).

A run is an install: of everything waiting, of the security fixes, or of
packages named by hand. It is a row in package_runs and rides every
heartbeat reply to its server until the agent says how it ended. The agent
does the work in a unit of its own outside its sandbox
(internal/agent/packages.go), so a run survives the agent restarting and
the connection to the control plane going down; it reports the outcome
whenever it next gets through.

Runs start three ways:

  - By hand, on one server or several. Several are done one at a time, in
    the order chosen; a server that fails or is interrupted stops the rest.
  - On their own, in a maintenance window, for servers whose rule installs
    security updates or everything. One per window per server: a run that
    fails is not tried again until the next window, and a server that was
    off for its window waits for the next one.
  - Never by a server or a request from the agent: the agent only reports.

A rule can say runs asked for by hand on its servers need a second admin.
The rule itself is the approval for the runs it starts on its own: an admin
chose it, and the change is audited.

Nothing new is trusted. The control plane could already open a root shell on
every enrolled server; an install of the distribution's own packages, from
the repositories the server already uses, gives it no reach it did not have.
*/

// PackageUpdate is one package that can be upgraded, as the agent sends it.
type PackageUpdate struct {
	Name      string `json:"name"`
	Installed string `json:"installed,omitempty"`
	Available string `json:"available"`
	Security  bool   `json:"security,omitempty"`
	Origin    string `json:"origin,omitempty"`
	New       bool   `json:"new,omitempty"`
	Reboot    bool   `json:"reboot,omitempty"`
}

// PackageReport is what the agent found waiting.
type PackageReport struct {
	CheckedAt        time.Time       `json:"checked_at"`
	Manager          string          `json:"manager,omitempty"`
	Supported        bool            `json:"supported"`
	Reason           string          `json:"reason,omitempty"`
	Error            string          `json:"error,omitempty"`
	Updates          []PackageUpdate `json:"updates"`
	SecurityPatches  int             `json:"security_patches,omitempty"`
	RebootRequired   bool            `json:"reboot_required,omitempty"`
	UTCOffsetMinutes int             `json:"utc_offset_minutes"`
	// Release is the server's operating system release and the next one it
	// can be upgraded to; absent from an older agent.
	Release *ReleaseInfo `json:"release,omitempty"`
	// Postgres is what the agent found about PostgreSQL's major versions,
	// and AptSources what it can put right in apt's sources; absent from an
	// older agent and from servers without them.
	Postgres   *PostgresUpkeep `json:"postgres,omitempty"`
	AptSources *AptSourcesFix  `json:"apt_sources,omitempty"`
}

// PostgresUpkeep mirrors the agent's (internal/agent/packages_postgres.go).
type PostgresUpkeep struct {
	Clusters    []PgCluster    `json:"clusters"`
	NewerClient *PgNewerClient `json:"newer_client,omitempty"`
	Upgrade     *PgUpgrade     `json:"upgrade,omitempty"`
	OldCluster  *PgOldCluster  `json:"old_cluster,omitempty"`
}

// PgCluster is one PostgreSQL cluster on the server.
type PgCluster struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Online  bool   `json:"online"`
	DataDir string `json:"data_dir,omitempty"`
}

// PgNewerClient is a client newer than every cluster, which is harmless.
type PgNewerClient struct {
	Client     string `json:"client"`
	Server     string `json:"server"`
	ServerPsql string `json:"server_psql,omitempty"`
}

// PgUpgrade is the cluster a postgres_upgrade run can move.
type PgUpgrade struct {
	From      string   `json:"from"`
	Cluster   string   `json:"cluster"`
	Port      int      `json:"port"`
	To        string   `json:"to"`
	Packages  []string `json:"packages"`
	DropEmpty bool     `json:"drop_empty,omitempty"`
	SizeBytes int64    `json:"size_bytes"`
	FreeBytes int64    `json:"free_bytes"`
	Method    string   `json:"method"`
	Blockers  []string `json:"blockers,omitempty"`
}

func (u *PgUpgrade) expect() string { return u.From + "/" + u.Cluster + ":" + u.To }

// PgOldCluster is the stopped cluster an upgrade left.
type PgOldCluster struct {
	Version  string   `json:"version"`
	Cluster  string   `json:"cluster"`
	Port     int      `json:"port"`
	Current  string   `json:"current"`
	Packages []string `json:"packages,omitempty"`
	Backups  []string `json:"backups,omitempty"`
}

func (o *PgOldCluster) expect() string { return o.Version + "/" + o.Cluster }

// AptSourcesFix is what an apt_sources_fix run can put right.
type AptSourcesFix struct {
	Leftovers   []AptLeftover `json:"leftovers,omitempty"`
	PGDG        *PGDGSource   `json:"pgdg,omitempty"`
	Fingerprint string        `json:"fingerprint"`
}

// AptLeftover is a file apt ignores in sources.list.d.
type AptLeftover struct {
	Name    string `json:"name"`
	Noticed bool   `json:"noticed,omitempty"`
	Movable bool   `json:"movable"`
	Why     string `json:"why,omitempty"`
}

// PGDGSource is apt.postgresql.org's entry when it is off or on another
// release.
type PGDGSource struct {
	File     string   `json:"file"`
	Disabled bool     `json:"disabled,omitempty"`
	Suites   []string `json:"suites"`
	Want     string   `json:"want"`
}

// ReleaseInfo is a server's release and the one it can move to, as the
// agent works it out (internal/agent/packages_release.go).
type ReleaseInfo struct {
	Distro   string   `json:"distro"`
	Current  string   `json:"current"`
	Version  string   `json:"version"`
	Next     string   `json:"next,omitempty"`
	NextName string   `json:"next_name,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Blockers []string `json:"blockers,omitempty"`
	// Stranded is what is installed that no apt source offers, which the
	// upgrade cannot carry over; absent from an older agent.
	Stranded *StrandedPackages `json:"stranded,omitempty"`
	// PrepareRabbitMQ is RabbitMQ standing in the way in a way a
	// prepare_rabbitmq run can fix; absent from an older agent.
	PrepareRabbitMQ bool `json:"prepare_rabbitmq,omitempty"`
}

// nextLabel is the release r can move to, as people name it; "" for none.
func (r *ReleaseInfo) nextLabel() string {
	if r == nil || r.Next == "" {
		return ""
	}
	if r.NextName != "" {
		return r.NextName
	}
	return strings.TrimSpace(r.Distro + " " + r.Next)
}

// StrandedPackages is what the agent found installed that no apt source
// offers (internal/agent/packages_stranded.go): the ones it can show
// nothing uses, and why it keeps the rest.
type StrandedPackages struct {
	Count     int            `json:"count"`
	Unused    []string       `json:"unused,omitempty"`
	Kept      []StrandedKept `json:"kept,omitempty"`
	Protected int            `json:"protected,omitempty"`
}

// StrandedKept is a stranded package the agent leaves, and why.
type StrandedKept struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// maxPackageUpdates bounds a stored report, whatever an agent sends.
const maxPackageUpdates = 3000

func (r *PackageReport) bounded() {
	if len(r.Updates) > maxPackageUpdates {
		r.Updates = r.Updates[:maxPackageUpdates]
	}
	clip := func(s *string, n int) {
		if len(*s) > n {
			*s = (*s)[:n]
		}
	}
	for i := range r.Updates {
		u := &r.Updates[i]
		clip(&u.Name, 200)
		clip(&u.Installed, 120)
		clip(&u.Available, 120)
		clip(&u.Origin, 200)
	}
	clip(&r.Error, 1000)
	clip(&r.Reason, 500)
	clip(&r.Manager, 20)
	if rel := r.Release; rel != nil {
		clip(&rel.Distro, 40)
		clip(&rel.Current, 120)
		clip(&rel.Version, 40)
		clip(&rel.Next, 40)
		clip(&rel.NextName, 120)
		clip(&rel.Tool, 40)
		clip(&rel.Reason, 500)
		if len(rel.Blockers) > 20 {
			rel.Blockers = rel.Blockers[:20]
		}
		for i := range rel.Blockers {
			clip(&rel.Blockers[i], 500)
		}
		if st := rel.Stranded; st != nil {
			if len(st.Unused) > 1000 {
				st.Unused = st.Unused[:1000]
			}
			for i := range st.Unused {
				clip(&st.Unused[i], 200)
			}
			if len(st.Kept) > 200 {
				st.Kept = st.Kept[:200]
			}
			for i := range st.Kept {
				clip(&st.Kept[i].Name, 200)
				clip(&st.Kept[i].Why, 300)
			}
		}
	}
	r.boundPostgres(clip)
	if r.UTCOffsetMinutes < -14*60 || r.UTCOffsetMinutes > 14*60 {
		r.UTCOffsetMinutes = 0
	}
}

// boundPostgres bounds the PostgreSQL and apt sources parts of a report.
func (r *PackageReport) boundPostgres(clip func(*string, int)) {
	list := func(l []string, n, each int) []string {
		if len(l) > n {
			l = l[:n]
		}
		for i := range l {
			clip(&l[i], each)
		}
		return l
	}
	if pg := r.Postgres; pg != nil {
		if len(pg.Clusters) > 50 {
			pg.Clusters = pg.Clusters[:50]
		}
		for i := range pg.Clusters {
			clip(&pg.Clusters[i].Version, 10)
			clip(&pg.Clusters[i].Name, 64)
			clip(&pg.Clusters[i].DataDir, 300)
		}
		if c := pg.NewerClient; c != nil {
			clip(&c.Client, 10)
			clip(&c.Server, 10)
			clip(&c.ServerPsql, 200)
		}
		if u := pg.Upgrade; u != nil {
			clip(&u.From, 10)
			clip(&u.Cluster, 64)
			clip(&u.To, 10)
			clip(&u.Method, 20)
			u.Packages = list(u.Packages, 100, 200)
			u.Blockers = list(u.Blockers, 20, 500)
		}
		if o := pg.OldCluster; o != nil {
			clip(&o.Version, 10)
			clip(&o.Cluster, 64)
			clip(&o.Current, 10)
			o.Packages = list(o.Packages, 100, 200)
			o.Backups = list(o.Backups, 20, 300)
		}
	}
	if a := r.AptSources; a != nil {
		if len(a.Leftovers) > 100 {
			a.Leftovers = a.Leftovers[:100]
		}
		for i := range a.Leftovers {
			clip(&a.Leftovers[i].Name, 200)
			clip(&a.Leftovers[i].Why, 300)
		}
		if p := a.PGDG; p != nil {
			clip(&p.File, 300)
			clip(&p.Want, 100)
			p.Suites = list(p.Suites, 10, 100)
		}
		clip(&a.Fingerprint, 64)
	}
}

// securityCount is how many updates fix a security issue.
func (r PackageReport) securityCount() int {
	n := 0
	for _, u := range r.Updates {
		if u.Security {
			n++
		}
	}
	if n == 0 {
		n = r.SecurityPatches
	}
	return n
}

// PackageRunStatus is what the agent says about a run.
type PackageRunStatus struct {
	ID             string               `json:"id"`
	State          string               `json:"state"`
	Step           string               `json:"step,omitempty"`
	Message        string               `json:"message,omitempty"`
	Planned        int                  `json:"planned,omitempty"`
	Upgraded       []PackageChange      `json:"upgraded,omitempty"`
	Restarted      []string             `json:"restarted,omitempty"`
	Started        []string             `json:"started,omitempty"`
	Script         *tunnel.ScriptReport `json:"script,omitempty"`
	RebootRequired bool                 `json:"reboot_required,omitempty"`
	Rebooted       bool                 `json:"rebooted,omitempty"`
	Log            string               `json:"log,omitempty"`
	Outcome        *RunOutcome          `json:"outcome,omitempty"`
	Steps          []RunStep            `json:"steps,omitempty"`
	StartedAt      *time.Time           `json:"started_at,omitempty"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
}

// RunOutcome is how a run that ended badly is to be read, as the agent
// says it: whether the server changed (yes, no, unknown), why it stopped,
// and the steps to take next. Commands in the steps are in backticks.
type RunOutcome struct {
	Changed string   `json:"changed"`
	Why     string   `json:"why,omitempty"`
	Next    []string `json:"next,omitempty"`
}

// RunStep is when a run reached a step.
type RunStep struct {
	Step string    `json:"step"`
	At   time.Time `json:"at"`
}

// runDetail is package_runs.detail.
type runDetail struct {
	Outcome *RunOutcome `json:"outcome,omitempty"`
	Steps   []RunStep   `json:"steps,omitempty"`
}

// boundedDetail is st's outcome and steps as stored, cut to sizes a
// heartbeat cannot grow, or nil when the agent sent neither.
func boundedDetail(st PackageRunStatus) []byte {
	if st.Outcome == nil && len(st.Steps) == 0 {
		return nil
	}
	d := runDetail{Steps: st.Steps}
	if len(d.Steps) > 32 {
		d.Steps = d.Steps[:32]
	}
	for i := range d.Steps {
		d.Steps[i].Step = clip(d.Steps[i].Step, 40)
	}
	if st.Outcome != nil {
		o := *st.Outcome
		switch o.Changed {
		case "yes", "no", "unknown":
		default:
			o.Changed = "unknown"
		}
		o.Why = clip(o.Why, 1000)
		if len(o.Next) > 10 {
			o.Next = o.Next[:10]
		}
		next := make([]string, len(o.Next))
		for i, n := range o.Next {
			next[i] = clip(n, 600)
		}
		o.Next = next
		d.Outcome = &o
	}
	b, _ := json.Marshal(d)
	return b
}

// PackageChange is one package a run upgraded.
type PackageChange struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// PackageBeat is the packages part of a heartbeat. Absent from an agent
// older than package updates.
type PackageBeat struct {
	Supported bool              `json:"supported"`
	Reason    string            `json:"reason,omitempty"`
	Report    *PackageReport    `json:"report,omitempty"`
	CheckID   string            `json:"check_id,omitempty"`
	Checking  bool              `json:"checking,omitempty"`
	Run       *PackageRunStatus `json:"run,omitempty"`
	// Installs are the software run modes a server that cannot be updated
	// from the console still does: a Windows server's remove, script and
	// file_install.
	Installs []string `json:"installs,omitempty"`
	// Reboot is an agent that does a reboot-only run.
	Reboot bool `json:"reboot,omitempty"`
	// ReleaseUpgrade is an agent that upgrades its server's operating
	// system to the next release.
	ReleaseUpgrade bool `json:"release_upgrade,omitempty"`
}

// packagesState is agents.packages_state: what the agent last said about
// being able to update its server.
type packagesState struct {
	Supported bool     `json:"supported"`
	Reason    string   `json:"reason,omitempty"`
	Checking  bool     `json:"checking,omitempty"`
	Installs  []string `json:"installs,omitempty"`
	Reboot    bool     `json:"reboot,omitempty"`
	Release   bool     `json:"release,omitempty"`
}

// Run states.
const (
	RunAwaitingApproval = "awaiting_approval"
	RunWaiting          = "waiting"
	RunQueued           = "queued"
	RunRunning          = "running"
	RunRebooting        = "rebooting"
	RunDone             = "done"
	RunFailed           = "failed"
	RunInterrupted      = "interrupted"
	RunCancelled        = "cancelled"
	RunDenied           = "denied"
)

func runFinished(state string) bool {
	switch state {
	case RunDone, RunFailed, RunInterrupted, RunCancelled, RunDenied:
		return true
	}
	return false
}

// PackageRun is one run, as the console shows it.
type PackageRun struct {
	ID             string          `json:"id"`
	Hostname       string          `json:"hostname"`
	AssetHostname  string          `json:"assetHostname"`
	AssetID        string          `json:"assetId,omitempty"`
	Mode           string          `json:"mode"`
	Packages       []string        `json:"packages"`
	Holds          []string        `json:"holds"`
	Reboot         string          `json:"reboot"`
	Trigger        string          `json:"trigger"`
	State          string          `json:"state"`
	RequestedBy    string          `json:"requestedBy"`
	RequestedAt    time.Time       `json:"requestedAt"`
	DecidedBy      *string         `json:"decidedBy"`
	DecidedAt      *time.Time      `json:"decidedAt"`
	SentAt         *time.Time      `json:"sentAt"`
	StartedAt      *time.Time      `json:"startedAt"`
	FinishedAt     *time.Time      `json:"finishedAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Step           string          `json:"step"`
	Message        string          `json:"message"`
	Planned        int             `json:"planned"`
	Upgraded       []PackageChange `json:"upgraded"`
	Restarted      []string        `json:"restarted"`
	RebootRequired bool            `json:"rebootRequired"`
	Rebooted       bool            `json:"rebooted"`
	Log            string          `json:"log,omitempty"`
	// Outcome and Steps are the agent's, absent for runs before them.
	Outcome       *RunOutcome `json:"outcome,omitempty"`
	Steps         []RunStep   `json:"steps,omitempty"`
	BatchID       *string     `json:"batchId"`
	BatchPosition *int        `json:"batchPosition"`
	// Items, Purge and StartServices are an install or a removal's; Started
	// the services an install started; PreviewID what a person said yes to,
	// UndoOf the run it undoes.
	Items         []tunnel.SoftwareItem `json:"items"`
	Purge         bool                  `json:"purge"`
	StartServices bool                  `json:"startServices"`
	Started       []string              `json:"started"`
	PreviewID     *string               `json:"previewId"`
	UndoOf        *string               `json:"undoOf"`
	expect        string
	// A script run: the version, install or uninstall, the parameter
	// values that are not secret, and what changed on the server.
	ScriptVersionID *string              `json:"scriptVersionId,omitempty"`
	ScriptName      string               `json:"scriptName,omitempty"`
	ScriptAction    string               `json:"scriptAction,omitempty"`
	ScriptParams    map[string]string    `json:"scriptParams,omitempty"`
	ScriptReport    *tunnel.ScriptReport `json:"scriptReport,omitempty"`
	// An app run on a device: the library file an install hands it, by id
	// and by the name the library shows.
	SoftwareFileID   *string `json:"softwareFileId,omitempty"`
	SoftwareFileName string  `json:"softwareFileName,omitempty"`
	// Release is the version a release upgrade goes to, as it was approved.
	Release string `json:"release,omitempty"`
	// Target is what a PostgreSQL run was approved for: "17/main:19" for an
	// upgrade, "17/main" for dropping the old cluster.
	Target string `json:"target,omitempty"`
}

const packageRunCols = `id::text, hostname, asset_hostname, mode, packages, holds, reboot, trigger, state,
	requested_by, requested_at, decided_by, decided_at, sent_at, started_at, finished_at, updated_at,
	step, message, planned, upgraded, restarted, reboot_required, rebooted, log, batch_id::text, batch_position,
	items, purge, start_services, started, preview_id::text, undo_of::text, expect,
	script_version_id::text, script_action, script_params, script_report,
	(SELECT s.name || ' v' || v.version FROM software_script_versions v JOIN software_scripts s ON s.id = v.script_id
	  WHERE v.id = package_runs.script_version_id),
	software_file_id::text, (SELECT f.name FROM software_files f WHERE f.id = package_runs.software_file_id), detail`

func scanPackageRun(row pgx.Row, withLog bool) (PackageRun, error) {
	var (
		r        PackageRun
		upgraded []byte
		items    []byte
		params   []byte
		report   []byte
		label    *string
		fileName *string
		pos      *int32
		detail   []byte
	)
	err := row.Scan(&r.ID, &r.Hostname, &r.AssetHostname, &r.Mode, &r.Packages, &r.Holds, &r.Reboot,
		&r.Trigger, &r.State, &r.RequestedBy, &r.RequestedAt, &r.DecidedBy, &r.DecidedAt, &r.SentAt,
		&r.StartedAt, &r.FinishedAt, &r.UpdatedAt, &r.Step, &r.Message, &r.Planned, &upgraded,
		&r.Restarted, &r.RebootRequired, &r.Rebooted, &r.Log, &r.BatchID, &pos,
		&items, &r.Purge, &r.StartServices, &r.Started, &r.PreviewID, &r.UndoOf, &r.expect,
		&r.ScriptVersionID, &r.ScriptAction, &params, &report, &label, &r.SoftwareFileID, &fileName, &detail)
	if err != nil {
		return r, err
	}
	if pos != nil {
		p := int(*pos)
		r.BatchPosition = &p
	}
	_ = json.Unmarshal(upgraded, &r.Upgraded)
	if len(detail) > 0 {
		var d runDetail
		if json.Unmarshal(detail, &d) == nil {
			r.Outcome, r.Steps = d.Outcome, d.Steps
		}
	}
	_ = json.Unmarshal(items, &r.Items)
	_ = json.Unmarshal(params, &r.ScriptParams)
	if len(report) > 0 {
		r.ScriptReport = &tunnel.ScriptReport{}
		_ = json.Unmarshal(report, r.ScriptReport)
	}
	if label != nil {
		r.ScriptName = *label
	}
	if fileName != nil {
		r.SoftwareFileName = *fileName
	}
	if r.Mode == PackagesRelease {
		r.Release = r.expect
	}
	if r.Mode == PackagesPostgresUpgrade || r.Mode == PackagesPostgresDropOld {
		r.Target = r.expect
	}
	if r.Items == nil {
		r.Items = []tunnel.SoftwareItem{}
	}
	if r.Started == nil {
		r.Started = []string{}
	}
	if r.Upgraded == nil {
		r.Upgraded = []PackageChange{}
	}
	if r.Packages == nil {
		r.Packages = []string{}
	}
	if r.Holds == nil {
		r.Holds = []string{}
	}
	if r.Restarted == nil {
		r.Restarted = []string{}
	}
	if !withLog {
		r.Log = ""
	}
	return r, nil
}

/* ── Rules ───────────────────────────────────────────────────────────────── */

// Rule modes.
const (
	PackagesOff      = "off"
	PackagesNotify   = "notify"
	PackagesSecurity = "security"
	PackagesAll      = "all"
)

// PackagesReboot is a run that installs nothing and reboots the server,
// for one waiting for a reboot after its updates.
const PackagesReboot = "reboot"

// PackagesRelease upgrades the server's operating system to its next
// release. The run's expect is the version a person said yes to; the agent
// stops if the server offers another by the time it starts.
const PackagesRelease = "release_upgrade"

// PackagesPrepareRabbitMQ readies RabbitMQ for the next Ubuntu release, as
// Ubuntu's upgrader requires: 3.13 from Canonical's PPA and its feature
// flags on. Offered only when the agent says it can (PrepareRabbitMQ).
const PackagesPrepareRabbitMQ = "prepare_rabbitmq"

// PackagesPostgresUpgrade moves a PostgreSQL cluster to a newer major
// version; expect is "17/main:19". PackagesPostgresDropOld drops the stopped
// cluster an upgrade left; expect is "17/main". PackagesAptSourcesFix moves
// aside files apt ignores and points apt.postgresql.org at the server's
// release; expect is the fingerprint the console showed. Each is offered
// only when the agent's report says it can be done.
const (
	PackagesPostgresUpgrade = "postgres_upgrade"
	PackagesPostgresDropOld = "postgres_drop_old"
	PackagesAptSourcesFix   = "apt_sources_fix"
)

// isPostgresMode is a run of the PostgreSQL card on the Updates tab.
func isPostgresMode(m string) bool {
	return m == PackagesPostgresUpgrade || m == PackagesPostgresDropOld || m == PackagesAptSourcesFix
}

// PackageRule says what happens on servers with a tag.
type PackageRule struct {
	ID       string `json:"id,omitempty"`
	Position int    `json:"position"`
	// Tag is the server tag the rule is for; "" is every other server.
	Tag  string `json:"tag"`
	Mode string `json:"mode"`
	// WindowDays are the days the window opens, 0 being Sunday, by the
	// server's own clock.
	WindowDays        []int  `json:"windowDays"`
	WindowStartMinute int    `json:"windowStartMinute"`
	WindowMinutes     int    `json:"windowMinutes"`
	Reboot            string `json:"reboot"`
	NeedsApproval     bool   `json:"needsApproval"`
}

// PackagePolicy is every rule and the holds.
type PackagePolicy struct {
	Rules []PackageRule `json:"rules"`
	Holds []string      `json:"holds"`
}

var (
	reHold    = regexp.MustCompile(`^[A-Za-z0-9*?][A-Za-z0-9+._:~*?-]{0,199}$`)
	rePkgName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,199}$`)
)

// Validate checks a policy before it is saved.
func (p *PackagePolicy) Validate() error {
	if len(p.Rules) == 0 || len(p.Rules) > 50 {
		return errors.New("give between one and fifty rules, including the one for every other server")
	}
	seen := map[string]bool{}
	defaults := 0
	for i := range p.Rules {
		r := &p.Rules[i]
		r.Tag = strings.TrimSpace(r.Tag)
		if r.Tag == "" {
			defaults++
		} else if !reTag.MatchString(r.Tag) {
			return fmt.Errorf("%q is not a server tag", r.Tag)
		}
		if seen[strings.ToLower(r.Tag)] {
			return fmt.Errorf("two rules are for the tag %q; keep one", r.Tag)
		}
		seen[strings.ToLower(r.Tag)] = true
		switch r.Mode {
		case PackagesOff, PackagesNotify, PackagesSecurity, PackagesAll:
		default:
			return fmt.Errorf("%q is not a way to update; use off, notify, security or all", r.Mode)
		}
		switch r.Reboot {
		case "", "never":
			r.Reboot = "never"
		case "if_needed":
		default:
			return fmt.Errorf("%q is not a reboot choice; use never or if_needed", r.Reboot)
		}
		if r.WindowStartMinute < 0 || r.WindowStartMinute > 1439 {
			return errors.New("a window starts between 00:00 and 23:59")
		}
		if r.WindowMinutes < 30 || r.WindowMinutes > 1440 {
			return errors.New("a window lasts between 30 minutes and 24 hours")
		}
		days := map[int]bool{}
		for _, d := range r.WindowDays {
			if d < 0 || d > 6 {
				return errors.New("window days are 0 (Sunday) to 6 (Saturday)")
			}
			days[d] = true
		}
		if (r.Mode == PackagesSecurity || r.Mode == PackagesAll) && len(days) == 0 {
			return errors.New("a rule that installs on its own needs at least one day for its window")
		}
		r.WindowDays = r.WindowDays[:0]
		for d := 0; d < 7; d++ {
			if days[d] {
				r.WindowDays = append(r.WindowDays, d)
			}
		}
		r.Position = i
	}
	if defaults != 1 {
		return errors.New("keep exactly one rule for every other server (the one with no tag)")
	}
	if len(p.Holds) > 100 {
		return errors.New("hold at most a hundred packages")
	}
	holds := []string{}
	for _, h := range p.Holds {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !reHold.MatchString(h) {
			return fmt.Errorf("%q is not a package name or pattern", h)
		}
		holds = append(holds, h)
	}
	p.Holds = dedupe(holds)
	return nil
}

// PackagePolicy reads every rule and the holds.
func (s *Store) PackagePolicy(ctx context.Context) (PackagePolicy, error) {
	p := PackagePolicy{Rules: []PackageRule{}, Holds: []string{}}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, position, tag, mode, window_days, window_start_minute, window_minutes, reboot,
		       needs_approval
		  FROM package_update_rules ORDER BY (tag = '') ASC, position, tag`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r    PackageRule
			days []int32
		)
		if err := rows.Scan(&r.ID, &r.Position, &r.Tag, &r.Mode, &days, &r.WindowStartMinute,
			&r.WindowMinutes, &r.Reboot, &r.NeedsApproval); err != nil {
			return p, err
		}
		r.WindowDays = []int{}
		for _, d := range days {
			r.WindowDays = append(r.WindowDays, int(d))
		}
		p.Rules = append(p.Rules, r)
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	err = s.db(ctx).QueryRow(ctx, `SELECT package_holds FROM gateway_policy LIMIT 1`).Scan(&p.Holds)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if p.Holds == nil {
		p.Holds = []string{}
	}
	return p, err
}

// SavePackagePolicy replaces every rule and the holds.
func (s *Store) SavePackagePolicy(ctx context.Context, p PackagePolicy) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM package_update_rules`); err != nil {
		return err
	}
	for _, r := range p.Rules {
		days := make([]int32, len(r.WindowDays))
		for i, d := range r.WindowDays {
			days[i] = int32(d)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO package_update_rules (position, tag, mode, window_days, window_start_minute,
			                                  window_minutes, reboot, needs_approval)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			r.Position, r.Tag, r.Mode, days, r.WindowStartMinute, r.WindowMinutes, r.Reboot,
			r.NeedsApproval); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE gateway_policy SET package_holds = $1`, p.Holds); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ruleFor is the rule for a server with tags: the first rule naming one of
// them, else the one for every other server.
func (p PackagePolicy) ruleFor(tags []string) PackageRule {
	has := map[string]bool{}
	for _, t := range tags {
		has[strings.ToLower(t)] = true
	}
	var def *PackageRule
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
	// No rule at all (a database edited by hand): the safe answer.
	return PackageRule{Mode: PackagesNotify, Reboot: "never", WindowDays: []int{}}
}

// windowStart is the start of the window r is in at now, by a clock
// offset minutes from UTC; ok is false outside any window.
func (r PackageRule) windowStart(now time.Time, offsetMinutes int) (time.Time, bool) {
	local := now.UTC().Add(time.Duration(offsetMinutes) * time.Minute)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	// A window that opened yesterday may still be open after midnight.
	for back := 0; back <= 1; back++ {
		d := day.AddDate(0, 0, -back)
		wd := int(d.Weekday())
		for _, want := range r.WindowDays {
			if want != wd {
				continue
			}
			start := d.Add(time.Duration(r.WindowStartMinute) * time.Minute)
			end := start.Add(time.Duration(r.WindowMinutes) * time.Minute)
			if !local.Before(start) && local.Before(end) {
				return start.Add(-time.Duration(offsetMinutes) * time.Minute), true
			}
		}
	}
	return time.Time{}, false
}

func heldBy(name string, holds []string) bool {
	base, _, _ := strings.Cut(name, ".")
	for _, h := range holds {
		if ok, _ := path.Match(h, name); ok {
			return true
		}
		if ok, _ := path.Match(h, base); ok {
			return true
		}
	}
	return false
}

// wanted is how many updates a rule's mode would install from r.
func wanted(r PackageReport, mode string, holds []string) int {
	n := 0
	for _, u := range r.Updates {
		if u.New || heldBy(u.Name, holds) {
			continue
		}
		if mode == PackagesAll || u.Security {
			n++
		}
	}
	if n == 0 && mode == PackagesSecurity && r.Manager == "zypper" {
		n = r.SecurityPatches
	}
	return n
}

/* ── What the agent reported ─────────────────────────────────────────────── */

// assetForAgent is the server an agent reports for, matched as a heartbeat
// matches it.
func (s *Store) assetForAgent(ctx context.Context, agentHost string) (id, hostname string, tags []string,
	err error) {
	err = s.db(ctx).QueryRow(ctx, `
		SELECT id::text, hostname, tags FROM assets
		 WHERE archived_at IS NULL
		   AND (hostname = $1 OR agent_hostname = $1 OR split_part(hostname, '.', 1) = $1)
		 ORDER BY (hostname = $1) DESC, (agent_hostname = $1) DESC
		 LIMIT 1`, agentHost).Scan(&id, &hostname, &tags)
	return id, hostname, tags, err
}

// recordPackageBeat keeps what the agent said about packages. It returns
// the report it replaced when b carries a new one, for alerting on what is
// new.
func (s *Store) recordPackageBeat(ctx context.Context, host string, b PackageBeat) (prev *PackageReport,
	err error) {
	st, err := json.Marshal(packagesState{Supported: b.Supported, Reason: b.Reason, Checking: b.Checking,
		Installs: validInstalls(b.Installs), Reboot: b.Reboot && b.Supported,
		Release: b.ReleaseUpgrade && b.Supported})
	if err != nil {
		return nil, err
	}
	// Written only when it changed: this rides every heartbeat.
	if _, err := s.db(ctx).Exec(ctx, `
		UPDATE agents SET packages_state = $2::jsonb
		 WHERE lower(hostname) = lower($1) AND packages_state IS DISTINCT FROM $2::jsonb`, host, st); err != nil {
		return nil, err
	}
	if b.CheckID != "" {
		if _, err := s.db(ctx).Exec(ctx, `
			UPDATE agents SET packages_check_id = NULL, packages_check_at = NULL
			 WHERE lower(hostname) = lower($1) AND packages_check_id = $2`, host, b.CheckID); err != nil {
			return nil, err
		}
	}
	if b.Report == nil {
		return nil, nil
	}
	r := *b.Report
	r.bounded()
	if r.Updates == nil {
		r.Updates = []PackageUpdate{}
	}
	rb, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var old []byte
	err = s.db(ctx).QueryRow(ctx, `
		WITH old AS (SELECT hostname, packages FROM agents WHERE lower(hostname) = lower($1) FOR UPDATE)
		UPDATE agents a SET packages = $2::jsonb FROM old WHERE a.hostname = old.hostname
		RETURNING old.packages`, host, rb).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil || len(old) == 0 {
		return nil, err
	}
	var p PackageReport
	if json.Unmarshal(old, &p) != nil {
		return nil, nil
	}
	return &p, nil
}

// recordPackageRunStatus keeps what the agent said about a run it was
// sent. It returns the run when the report ended it, so the end is audited
// and announced once.
func (s *Store) recordPackageRunStatus(ctx context.Context, host string, st PackageRunStatus) (*PackageRun,
	error) {
	state := st.State
	switch state {
	case "starting", RunRunning:
		state = RunRunning
	case RunRebooting, RunDone, RunFailed, RunInterrupted:
	default:
		return nil, nil
	}
	if len(st.Log) > 48<<10 {
		st.Log = st.Log[len(st.Log)-48<<10:]
	}
	if len(st.Message) > 2000 {
		st.Message = st.Message[:2000]
	}
	if len(st.Upgraded) > maxPackageUpdates {
		st.Upgraded = st.Upgraded[:maxPackageUpdates]
	}
	if len(st.Restarted) > 200 {
		st.Restarted = st.Restarted[:200]
	}
	up, err := json.Marshal(st.Upgraded)
	if err != nil {
		return nil, err
	}
	if st.Upgraded == nil {
		up = []byte("[]")
	}
	restarted := st.Restarted
	if restarted == nil {
		restarted = []string{}
	}
	var scriptReport []byte
	if st.Script != nil {
		bounded := *st.Script
		if len(bounded.Packages) > 500 {
			bounded.Packages = bounded.Packages[:500]
		}
		scriptReport, _ = json.Marshal(bounded)
	}
	started := st.Started
	if len(started) > 200 {
		started = started[:200]
	}
	if started == nil {
		started = []string{}
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE package_runs SET
		       state = $3, step = $4, message = $5, planned = $6, upgraded = $7::jsonb,
		       restarted = $8, reboot_required = $9, rebooted = $10, log = $11, started = $12,
		       script_report = COALESCE($13::jsonb, script_report),
		       detail = COALESCE($14::jsonb, detail),
		       started_at = COALESCE(started_at, now()),
		       finished_at = CASE WHEN $3 IN ('done','failed','interrupted') THEN now() ELSE NULL END,
		       updated_at = now()
		 WHERE id::text = $2 AND lower(hostname) = lower($1)
		   AND (state IN ('queued','running','rebooting')
		        -- Ended here because the server went quiet: what it says
		        -- when it is back is the truth.
		        OR (state = 'interrupted' AND step = 'stale'))
		   AND ((state, step, message, log, rebooted) IS DISTINCT FROM ($3, $4, $5, $11, $10)
		        OR updated_at < now() - interval '5 minutes')
		RETURNING `+packageRunCols, host, st.ID, state, st.Step, st.Message, st.Planned, up, restarted,
		st.RebootRequired, st.Rebooted, st.Log, started, scriptReport, boundedDetail(st))
	r, err := scanPackageRun(row, false)
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

// PackageOrders is what goes on a heartbeat reply.
type PackageOrders struct {
	Check   bool             `json:"check"`
	CheckID string           `json:"check_id,omitempty"`
	Run     *PackageRunOrder `json:"run,omitempty"`
}

// PackageRunOrder is a run as the agent reads it.
type PackageRunOrder struct {
	ID       string   `json:"id"`
	Mode     string   `json:"mode"`
	Packages []string `json:"packages,omitempty"`
	Holds    []string `json:"holds,omitempty"`
	Reboot   string   `json:"reboot"`
	// Items, Purge, StartServices and Expect are an install or a removal's.
	Items         []tunnel.SoftwareItem `json:"items,omitempty"`
	Purge         bool                  `json:"purge,omitempty"`
	StartServices bool                  `json:"start_services,omitempty"`
	Expect        string                `json:"expect,omitempty"`
	// Script is a reviewed script to run, for mode script.
	Script *ScriptRunOrder `json:"script,omitempty"`
	// App is what a device needs for an app_* run: the file to fetch for an
	// install, what it must say about itself, and the package.
	App *AppRunOrder `json:"app,omitempty"`
	// File is the library file a file_install run fetches and installs.
	File *FileRunOrder `json:"file,omitempty"`
}

// packageRunToSend is the run to put on host's heartbeat reply, marking it
// sent: from then on it cannot be taken back.
func (s *Store) packageRunToSend(ctx context.Context, host string) (*PackageRunOrder, error) {
	var (
		o     PackageRunOrder
		items []byte
	)
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE package_runs SET sent_at = COALESCE(sent_at, now())
		 WHERE id = (SELECT id FROM package_runs
		              WHERE lower(hostname) = lower($1) AND state IN ('queued','running','rebooting')
		              ORDER BY requested_at LIMIT 1)
		RETURNING id::text, mode, packages, holds, reboot, items, purge, start_services, expect`, host).Scan(&o.ID,
		&o.Mode, &o.Packages, &o.Holds, &o.Reboot, &items, &o.Purge, &o.StartServices, &o.Expect)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(items, &o.Items)
	return &o, nil
}

// packageCheckAsked is a check the console asked of host and the agent has
// not answered.
func (s *Store) packageCheckAsked(ctx context.Context, host string) (string, error) {
	var id *string
	err := s.db(ctx).QueryRow(ctx, `SELECT packages_check_id FROM agents WHERE lower(hostname) = lower($1)`,
		host).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return "", nil
	}
	return *id, err
}

// packageReportFor reads what host's agent last reported.
func (s *Store) packageReportFor(ctx context.Context, host string) (*PackageReport, *packagesState, error) {
	var rb, sb []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT packages, packages_state FROM agents WHERE lower(hostname) = lower($1)`,
		host).Scan(&rb, &sb)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var (
		r  *PackageReport
		st *packagesState
	)
	if len(rb) > 0 {
		r = &PackageReport{}
		if json.Unmarshal(rb, r) != nil {
			r = nil
		}
	}
	if len(sb) > 0 {
		st = &packagesState{}
		if json.Unmarshal(sb, st) != nil {
			st = nil
		}
	}
	return r, st, nil
}

/* ── Runs ────────────────────────────────────────────────────────────────── */

// ErrRunActive is a run asked of a server that already has one going.
var ErrRunActive = errors.New("this server already has an update run going or waiting; " +
	"wait for it to finish or take it back")

// newRun is a run to create.
type newRun struct {
	Hostname, AssetHostname string
	Mode                    string
	Packages, Holds         []string
	Reboot, Trigger, State  string
	RequestedBy             string
	BatchID                 *string
	BatchPosition           *int
	WindowStart             *time.Time
	// A script run.
	ScriptVersionID *string
	ScriptAction    string
	ScriptParams    map[string]string
	ScriptSecrets   []byte
	// An install or a removal.
	Items                []tunnel.SoftwareItem
	Purge, StartServices bool
	Expect               string
	PreviewID, UndoOf    *string
	// An app run on a device.
	SoftwareFileID *string
}

// hasPendingRun reports whether host has a run not yet finished.
func (s *Store) hasPendingRun(ctx context.Context, q pgxQuerier, host string) (bool, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM package_runs
		 WHERE lower(hostname) = lower($1)
		   AND state IN ('awaiting_approval','waiting','queued','running','rebooting')`, host).Scan(&n)
	return n > 0, err
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) insertRun(ctx context.Context, q pgxQuerier, n newRun) (PackageRun, error) {
	if n.Packages == nil {
		n.Packages = []string{}
	}
	if n.Holds == nil {
		n.Holds = []string{}
	}
	var pos *int32
	if n.BatchPosition != nil {
		p := int32(*n.BatchPosition)
		pos = &p
	}
	if n.Items == nil {
		n.Items = []tunnel.SoftwareItem{}
	}
	items, err := json.Marshal(n.Items)
	if err != nil {
		return PackageRun{}, err
	}
	if n.ScriptParams == nil {
		n.ScriptParams = map[string]string{}
	}
	params, err := json.Marshal(n.ScriptParams)
	if err != nil {
		return PackageRun{}, err
	}
	row := q.QueryRow(ctx, `
		INSERT INTO package_runs (hostname, asset_hostname, mode, packages, holds, reboot, trigger, state,
		                          requested_by, batch_id, batch_position, window_start,
		                          items, purge, start_services, expect, preview_id, undo_of,
		                          script_version_id, script_action, script_params, script_secrets, software_file_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::uuid,$11,$12,$13::jsonb,$14,$15,$16,$17::uuid,$18::uuid,
		        $19::uuid,$20,$21::jsonb,$22,$23::uuid)
		RETURNING `+packageRunCols,
		n.Hostname, n.AssetHostname, n.Mode, n.Packages, n.Holds, n.Reboot, n.Trigger, n.State,
		n.RequestedBy, n.BatchID, pos, n.WindowStart, items, n.Purge, n.StartServices, n.Expect,
		n.PreviewID, n.UndoOf, n.ScriptVersionID, n.ScriptAction, params, n.ScriptSecrets, n.SoftwareFileID)
	r, err := scanPackageRun(row, false)
	if err != nil && isUniqueViolation(err) {
		return r, ErrRunActive
	}
	return r, err
}

// CreatePackageRuns creates runs, one per server, in order. One server
// starts now (or waits for approval); the others wait their turn. Nothing
// is created when any server already has a run pending.
func (s *Store) CreatePackageRuns(ctx context.Context, runs []newRun) ([]PackageRun, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var batch *string
	if len(runs) > 1 {
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return nil, err
		}
		batch = &id
	}
	out := make([]PackageRun, 0, len(runs))
	for i, n := range runs {
		pending, err := s.hasPendingRun(ctx, tx, n.Hostname)
		if err != nil {
			return nil, err
		}
		if pending {
			return nil, fmt.Errorf("%s: %w", n.AssetHostname, ErrRunActive)
		}
		if batch != nil {
			pos := i
			n.BatchID, n.BatchPosition = batch, &pos
			if n.State == RunQueued && i > 0 {
				n.State = RunWaiting
			}
		}
		r, err := s.insertRun(ctx, tx, n)
		if err != nil {
			if errors.Is(err, ErrRunActive) {
				return nil, fmt.Errorf("%s: %w", n.AssetHostname, err)
			}
			return nil, err
		}
		out = append(out, r)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if batch != nil {
		if err := s.advanceBatch(ctx, *batch); err != nil {
			return out, err
		}
	}
	return out, nil
}

// advanceBatch moves a batch along: the next server's run is queued once
// every run before it has ended, and a failure or an interruption ends the
// batch, cancelling what had not started.
func (s *Store) advanceBatch(ctx context.Context, batch string) error {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT id::text, asset_hostname, state FROM package_runs
		 WHERE batch_id = $1::uuid ORDER BY batch_position`, batch)
	if err != nil {
		return err
	}
	type item struct{ id, host, state string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.host, &it.state); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	stopped, busy := "", false
	for _, it := range items {
		switch {
		case stopped != "" && (it.state == RunWaiting || it.state == RunAwaitingApproval):
			if _, err := s.db(ctx).Exec(ctx, `
				UPDATE package_runs SET state = 'cancelled', finished_at = now(), updated_at = now(),
				       message = $2
				 WHERE id = $1::uuid AND state IN ('waiting','awaiting_approval')`, it.id,
				"not started: the update of "+stopped+" did not finish, which stops the servers after it"); err != nil {
				return err
			}
		case it.state == RunFailed || it.state == RunInterrupted:
			stopped = it.host
		case it.state == RunWaiting && !busy && stopped == "":
			if _, err := s.db(ctx).Exec(ctx, `
				UPDATE package_runs SET state = 'queued', updated_at = now()
				 WHERE id = $1::uuid AND state = 'waiting'`, it.id); err != nil {
				if isUniqueViolation(err) {
					busy = true
					continue
				}
				return err
			}
			busy = true
		case !runFinished(it.state):
			busy = true
		}
	}
	return nil
}

// PackageRunByID reads one run, with its output.
func (s *Store) PackageRunByID(ctx context.Context, id string) (PackageRun, error) {
	r, err := scanPackageRun(s.db(ctx).QueryRow(ctx, `SELECT `+packageRunCols+` FROM package_runs
		WHERE id::text = $1`, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrRunNotFound
	}
	return r, err
}

// ErrRunNotFound is a run that does not exist.
var ErrRunNotFound = errors.New("no such update run")

// PackageRunsFor lists host's runs, newest first.
func (s *Store) PackageRunsFor(ctx context.Context, host string, limit int) ([]PackageRun, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+packageRunCols+` FROM package_runs
		WHERE lower(hostname) = lower($1) ORDER BY requested_at DESC LIMIT $2`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PackageRun{}
	for rows.Next() {
		r, err := scanPackageRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecidePackageRun approves or denies a run waiting for approval. The one
// who asked cannot decide it, unless own says nobody else could
// (self_approval.go).
func (s *Store) DecidePackageRun(ctx context.Context, id, by string, approve, own bool) (PackageRun, error) {
	r, err := s.PackageRunByID(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != RunAwaitingApproval {
		return r, fmt.Errorf("%w: this run is %s, not waiting for approval", ErrRunState, strings.ReplaceAll(r.State, "_", " "))
	}
	if !own && strings.EqualFold(r.RequestedBy, by) {
		return r, errOwnRun{"a run you asked for is approved by another admin"}
	}
	next := RunDenied
	if approve {
		next = RunQueued
		if r.BatchID != nil {
			next = RunWaiting // advanceBatch queues it when its turn comes
		}
	}
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE package_runs SET state = $2, decided_by = $3, decided_at = now(), updated_at = now(),
		       finished_at = CASE WHEN $2 = 'denied' THEN now() ELSE NULL END,
		       message = CASE WHEN $2 = 'denied' THEN 'denied by ' || $3 ELSE message END
		 WHERE id::text = $1 AND state = 'awaiting_approval'
		RETURNING `+packageRunCols, id, next, by)
	r, err = scanPackageRun(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: someone decided this run already", ErrRunState)
	}
	if err != nil && isUniqueViolation(err) {
		return r, ErrRunActive
	}
	if err != nil {
		return r, err
	}
	if r.BatchID != nil {
		if err := s.advanceBatch(ctx, *r.BatchID); err != nil {
			return r, err
		}
		return s.PackageRunByID(ctx, id)
	}
	return r, nil
}

// ErrRunState is a run in the wrong state for what was asked.
var ErrRunState = errors.New("cannot")

// CancelPackageRun takes back a run that has not reached the server.
func (s *Store) CancelPackageRun(ctx context.Context, id, by string) (PackageRun, error) {
	row := s.db(ctx).QueryRow(ctx, `
		UPDATE package_runs SET state = 'cancelled', finished_at = now(), updated_at = now(),
		       message = 'taken back by ' || $2
		 WHERE id::text = $1
		   AND (state IN ('awaiting_approval','waiting') OR (state = 'queued' AND sent_at IS NULL))
		RETURNING `+packageRunCols, id, by)
	r, err := scanPackageRun(row, false)
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.PackageRunByID(ctx, id)
		if gerr != nil {
			return cur, gerr
		}
		return cur, fmt.Errorf("%w: the server has already started this run, and a package manager "+
			"must not be stopped half way; it reports how it ended", ErrRunState)
	}
	if err != nil {
		return r, err
	}
	if r.BatchID != nil {
		if err := s.advanceBatch(ctx, *r.BatchID); err != nil {
			return r, err
		}
	}
	return r, nil
}

// expireStaleRuns ends runs the server stopped reporting on, on host or,
// with "", everywhere. The agent repeats a run's status on every heartbeat
// while it runs, and the row is touched at least every five minutes, so
// three quiet hours is a server that is down or lost its agent. A late
// report still lands: see recordPackageRunStatus.
func (s *Store) expireStaleRuns(ctx context.Context, host string) ([]PackageRun, error) {
	rows, err := s.db(ctx).Query(ctx, `
		UPDATE package_runs SET state = 'interrupted', step = 'stale', finished_at = now(), updated_at = now(),
		       message = CASE WHEN mode = 'release_upgrade'
		                 THEN 'the server stopped reporting on this release upgrade for eight hours; it may be ' ||
		                      'part way between releases, so look at the server before trying again'
		                 WHEN mode = 'postgres_upgrade'
		                 THEN 'the server stopped reporting on this PostgreSQL upgrade for eight hours; look at ' ||
		                      'sudo pg_lsclusters on the server before trying again'
		                 ELSE 'the server stopped reporting on this run for three hours; look at the server ' ||
		                      'before trying again' END
		 WHERE state IN ('running','rebooting')
		   -- A release upgrade goes quiet for longer: leapp upgrades in a
		   -- boot of its own, with no agent running.
		   -- A big database's pg_upgrade and analyze take as long.
		   AND updated_at < now() - CASE WHEN mode IN ('release_upgrade', 'postgres_upgrade') THEN interval '8 hours'
		                                 ELSE interval '3 hours' END
		   AND ($1 = '' OR lower(hostname) = lower($1))
		RETURNING `+packageRunCols, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackageRun
	for rows.Next() {
		r, err := scanPackageRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ── The view ────────────────────────────────────────────────────────────── */

// PackagesView is a server's Updates tab.
type PackagesView struct {
	Hostname string `json:"hostname"`
	// Enrolled is whether the server has an agent.
	Enrolled bool `json:"enrolled"`
	// Reported is whether its agent has said anything about packages; an
	// agent older than package updates never does.
	Reported  bool   `json:"reported"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Installs are what a server that cannot be updated here still does
	// (Windows): remove, script, file_install.
	Installs []string `json:"installs,omitempty"`
	// CanReboot is an agent that reboots on its own when asked; an older
	// one does not, and is asked to update first.
	CanReboot bool `json:"canReboot"`
	// CanUpgradeRelease is an agent that upgrades the operating system to
	// its next release; an older one is asked to update first.
	CanUpgradeRelease bool `json:"canUpgradeRelease"`
	Checking          bool `json:"checking"`
	// CheckAsked is a check the console asked for, not yet answered.
	CheckAsked bool           `json:"checkAsked"`
	Report     *PackageReport `json:"report"`
	Rule       PackageRule    `json:"rule"`
	Holds      []string       `json:"holds"`
	// NextWindow is when the next automatic run may start, nil when the
	// rule starts none.
	NextWindow *time.Time `json:"nextWindow"`
	// HeardAt is when the server's agent last reported, so the console can
	// say how fresh a run's progress is.
	HeardAt *time.Time   `json:"heardAt"`
	Runs    []PackageRun `json:"runs"`
}

// nextWindow is when r's window next opens after now.
func (r PackageRule) nextWindow(now time.Time, offset int) *time.Time {
	if r.Mode != PackagesSecurity && r.Mode != PackagesAll || len(r.WindowDays) == 0 {
		return nil
	}
	if start, ok := r.windowStart(now, offset); ok {
		return &start
	}
	local := now.UTC().Add(time.Duration(offset) * time.Minute)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i <= 7; i++ {
		d := day.AddDate(0, 0, i)
		for _, want := range r.WindowDays {
			if int(d.Weekday()) != want {
				continue
			}
			start := d.Add(time.Duration(r.WindowStartMinute) * time.Minute)
			if start.After(local) {
				t := start.Add(-time.Duration(offset) * time.Minute)
				return &t
			}
		}
	}
	return nil
}

// PackageFleetRow is one server on the Servers page's updates view.
type PackageFleetRow struct {
	AssetID        string     `json:"assetId"`
	Hostname       string     `json:"hostname"`
	Tags           []string   `json:"tags"`
	OS             string     `json:"os"`
	Reported       bool       `json:"reported"`
	Supported      bool       `json:"supported"`
	Reason         string     `json:"reason,omitempty"`
	Manager        string     `json:"manager"`
	CheckedAt      *time.Time `json:"checkedAt"`
	Error          string     `json:"error,omitempty"`
	Updates        int        `json:"updates"`
	Security       int        `json:"security"`
	RebootRequired bool       `json:"rebootRequired"`
	CanReboot      bool       `json:"canReboot"`
	// ReleaseNext is the operating system release the server can be
	// upgraded to ("Ubuntu 24.04 LTS"), empty when there is none or the
	// agent is too old to say.
	ReleaseNext string `json:"releaseNext,omitempty"`
	Mode        string `json:"mode"`
	// Run is the run going or waiting on the server, if any.
	Run *PackageRun `json:"run"`
}

// packageSummary is the part of a stored PackageReport the fleet view shows,
// read in SQL so the list of updates never leaves the database.
type packageSummary struct {
	checkedAt, manager, err, securityPatches *string
	updates, security                        int
	reboot                                   bool
	// The release it can move to: release.distro, .next and .next_name.
	distro, next, nextName *string
}

// apply fills a fleet row as decoding the whole report would have. A
// checked_at that is not a time is a report that would not have decoded, and
// is shown as none.
func (p packageSummary) apply(row *PackageFleetRow) {
	var at time.Time
	if p.checkedAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *p.checkedAt)
		if err != nil {
			return
		}
		at = t
	}
	row.CheckedAt = &at
	if p.manager != nil {
		row.Manager = *p.manager
	}
	if p.err != nil {
		row.Error = *p.err
	}
	row.Updates, row.RebootRequired = p.updates, p.reboot
	if p.next != nil && *p.next != "" {
		r := ReleaseInfo{Next: *p.next}
		if p.distro != nil {
			r.Distro = *p.distro
		}
		if p.nextName != nil {
			r.NextName = *p.nextName
		}
		row.ReleaseNext = r.nextLabel()
	}
	// securityCount: the flagged updates, or the agent's own count when it
	// flags none.
	row.Security = p.security
	if row.Security == 0 && p.securityPatches != nil {
		if n, err := strconv.Atoi(*p.securityPatches); err == nil {
			row.Security = n
		}
	}
}

// PackageFleet lists every server with an agent and what is waiting on it.
func (s *Store) PackageFleet(ctx context.Context) ([]PackageFleetRow, error) {
	policy, err := s.PackagePolicy(ctx)
	if err != nil {
		return nil, err
	}
	// The report is summarised here rather than shipped whole. The list of
	// waiting updates is most of each row, and sending every server's as text
	// to be decoded in Go cost this page 310 ms at 7,000 servers where the
	// summary costs 220, and read 28 MB out of the database to do it.
	rows, err := s.db(ctx).Query(ctx, `
		SELECT a.id::text, a.hostname, a.tags, a.os, g.hostname, g.packages_state,
		       g.packages IS NOT NULL,
		       g.packages->>'checked_at', g.packages->>'manager', g.packages->>'error',
		       CASE WHEN jsonb_typeof(g.packages->'updates') = 'array'
		            THEN jsonb_array_length(g.packages->'updates') ELSE 0 END,
		       CASE WHEN jsonb_typeof(g.packages->'updates') = 'array'
		            THEN jsonb_array_length(jsonb_path_query_array(g.packages->'updates',
		                   'strict $[*] ? (@.security == true)')) ELSE 0 END,
		       g.packages->>'security_patches',
		       COALESCE(g.packages->'reboot_required' = 'true'::jsonb, false),
		       g.packages->'release'->>'distro', g.packages->'release'->>'next', g.packages->'release'->>'next_name'
		  FROM assets a
		  JOIN LATERAL (SELECT hostname, packages, packages_state FROM agents g
		                 WHERE g.hostname = COALESCE(NULLIF(a.agent_hostname, ''), a.hostname)
		                    OR g.hostname = split_part(a.hostname, '.', 1)
		                 ORDER BY (g.hostname = a.hostname) DESC LIMIT 1) g ON true
		 WHERE a.archived_at IS NULL AND a.protocol = 'ssh'
		 ORDER BY a.hostname`)
	if err != nil {
		return nil, err
	}
	type pending struct {
		row   PackageFleetRow
		agent string
	}
	var list []pending
	for rows.Next() {
		var (
			p        pending
			sb       []byte
			reported bool
			sum      packageSummary
		)
		if err := rows.Scan(&p.row.AssetID, &p.row.Hostname, &p.row.Tags, &p.row.OS, &p.agent, &sb,
			&reported, &sum.checkedAt, &sum.manager, &sum.err, &sum.updates, &sum.security,
			&sum.securityPatches, &sum.reboot, &sum.distro, &sum.next, &sum.nextName); err != nil {
			rows.Close()
			return nil, err
		}
		if p.row.Tags == nil {
			p.row.Tags = []string{}
		}
		if len(sb) > 0 {
			var st packagesState
			if json.Unmarshal(sb, &st) == nil {
				p.row.Reported, p.row.Supported, p.row.Reason = true, st.Supported, st.Reason
				p.row.CanReboot = st.Reboot
			}
		}
		if reported {
			sum.apply(&p.row)
		}
		p.row.Mode = policy.ruleFor(p.row.Tags).Mode
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	active := map[string]PackageRun{}
	arows, err := s.db(ctx).Query(ctx, `SELECT `+packageRunCols+` FROM package_runs
		WHERE state IN ('awaiting_approval','waiting','queued','running','rebooting')`)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		r, err := scanPackageRun(arows, false)
		if err != nil {
			arows.Close()
			return nil, err
		}
		active[strings.ToLower(r.Hostname)] = r
	}
	arows.Close()
	out := make([]PackageFleetRow, 0, len(list))
	for _, p := range list {
		if r, ok := active[strings.ToLower(p.agent)]; ok {
			r := r
			p.row.Run = &r
		}
		out = append(out, p.row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}
