package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/caduceus"
)

/*
When Argus itself stops.

Argus cannot tell anyone it has stopped, because it has stopped. Two things
here make an outage visible anyway:

  - The outside check. Every minute, while this process is up and its
    database answers, argus-control calls an address the operator gave it:
    a check-in service (Healthchecks.io, Uptime Kuma push, Better Stack
    heartbeat, Cronitor). When the calls stop, that service raises the
    alarm, from somewhere this server's failure cannot reach. Each replica
    calls it, so one replica stopping does not set it off while another is
    serving. Nothing about the fleet is sent: a plain GET, no body.

  - The outage record. Every control server writes its own row in
    argus_platform.control_instances each minute. A tick that finds the
    newest row older than outageAfter knows nothing was serving for that
    long -- no process running, or none able to reach the database -- and
    records the window once (the window's start is the key, so two replicas
    coming back together record it once), in the audit chain, in every
    organization's bell, and on the alert channels.

The same rows say how many control servers are running, which the "If Argus
goes down" page reads (control_readiness.go).

Upgrade and rollback: the tables are platform-wide and created IF NOT EXISTS
with the organization registry (platformDDL); an older version ignores them,
and an older control.yaml has no outside_check, which leaves it off.
*/

const (
	// livenessEvery is how often a control server says it is alive.
	livenessEvery = time.Minute
	// outageAfter is how long nothing may say so before it was an outage.
	// Three ticks: an update or a restart takes well under that.
	outageAfter = 3 * time.Minute
	// instanceAliveFor is how recent a row must be to count as running.
	instanceAliveFor = 3 * time.Minute
	// outsideCheckTimeout bounds one call to the check-in service.
	outsideCheckTimeout = 10 * time.Second
	// instanceForgetAfter drops a control server that has not been heard
	// from in this long, so a replaced server does not linger.
	instanceForgetAfter = 30 * 24 * time.Hour
)

// OutsideCheck is where argus-control checks in while it is up. An empty
// URL turns it off.
type OutsideCheck struct {
	URL   string
	Every time.Duration
}

// ValidOutsideCheckURL is an http or https address with a host and no
// credentials in it.
func ValidOutsideCheckURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not an address: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("use an https:// address")
	}
	if u.Host == "" {
		return errors.New("the address has no host")
	}
	if u.User != nil {
		return errors.New("put no user name or password in the address")
	}
	return nil
}

// ValidOutsideCheckEvery is one minute or five; empty is one minute.
func ValidOutsideCheckEvery(s string) bool {
	return s == "" || s == "1m" || s == "5m"
}

// outsideCheckHost is the host part of the address, for showing which
// service is called without showing the secret path.
func outsideCheckHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// outsideState is this process's last call to the check-in service.
type outsideState struct {
	mu      sync.Mutex
	lastRun time.Time
}

// ControlInstance is one control server, as its own row says.
type ControlInstance struct {
	Host      string    `json:"host"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	SeenAt    time.Time `json:"seenAt"`
	// Running is whether it has said so within instanceAliveFor.
	Running bool `json:"running"`
	// The last outside check-in this server made, if it makes them.
	CheckAt     *time.Time `json:"checkAt,omitempty"`
	CheckOK     *bool      `json:"checkOk,omitempty"`
	CheckDetail string     `json:"checkDetail,omitempty"`
}

// ControlOutage is a window in which no control server was working.
type ControlOutage struct {
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt"`
	FoundBy   string    `json:"foundBy"`
}

// Minutes is the outage's length, rounded to the minute.
func (o ControlOutage) Minutes() int {
	return int(o.EndedAt.Sub(o.StartedAt).Round(time.Minute) / time.Minute)
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// NoteControlAlive records that host is serving, and returns the outage it
// is the first to find, if the newest row before this one is older than
// outageAfter. Times are the database's, so replicas with skewed clocks
// agree.
func (s *Store) NoteControlAlive(ctx context.Context, host, version string) (*ControlOutage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One writer at a time, so two replicas starting together do not both
	// read the old newest row after the other has written its new one.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('argus.control_instances'))`); err != nil {
		return nil, err
	}
	var last *time.Time
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT max(seen_at), now() FROM argus_platform.control_instances`).
		Scan(&last, &now); err != nil {
		return nil, err
	}
	var found *ControlOutage
	if last != nil && now.Sub(*last) > outageAfter {
		// A version older than this file writes no row, so after going back
		// to one and forward again, the gap is the time it served, not an
		// outage. It wrote to the audit log meanwhile, which nothing can do
		// while no control server is working -- except the commands on the
		// host, whose actor is <user>@host.
		var served bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events
			WHERE at > $1::timestamptz + interval '1 minute' AND at < $2::timestamptz - interval '1 minute'
			  AND actor_email NOT LIKE '%@host')`, *last, now).Scan(&served); err != nil {
			return nil, err
		}
		if served {
			last = nil
		}
	}
	if last != nil && now.Sub(*last) > outageAfter {
		o := ControlOutage{StartedAt: *last, EndedAt: now, FoundBy: host}
		tag, err := tx.Exec(ctx, `INSERT INTO argus_platform.control_outages (started_at, ended_at, found_by)
			VALUES ($1, $2, $3) ON CONFLICT (started_at) DO NOTHING`, o.StartedAt, o.EndedAt, o.FoundBy)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			found = &o
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO argus_platform.control_instances (host, version, started_at, seen_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (host) DO UPDATE SET version = EXCLUDED.version, seen_at = now(),
		  started_at = CASE WHEN argus_platform.control_instances.seen_at < now() - $3::interval
		                    THEN now() ELSE argus_platform.control_instances.started_at END`,
		host, version, fmt.Sprintf("%d seconds", int(outageAfter.Seconds()))); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM argus_platform.control_instances WHERE seen_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(instanceForgetAfter.Seconds()))); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return found, nil
}

// NoteOutsideCheck records how host's last check-in went.
func (s *Store) NoteOutsideCheck(ctx context.Context, host string, ok bool, detail string) error {
	_, err := s.pool.Exec(ctx, `UPDATE argus_platform.control_instances
		SET check_at = now(), check_ok = $2, check_detail = $3 WHERE host = $1`, host, ok, clip(detail, 300))
	return err
}

// ControlInstances lists every control server heard from in the last 30
// days, running ones first.
func (s *Store) ControlInstances(ctx context.Context) ([]ControlInstance, error) {
	rows, err := s.pool.Query(ctx, `SELECT host, version, started_at, seen_at,
		       seen_at > now() - $1::interval, check_at, check_ok, check_detail
		  FROM argus_platform.control_instances ORDER BY 5 DESC, host`,
		fmt.Sprintf("%d seconds", int(instanceAliveFor.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ControlInstance{}
	for rows.Next() {
		var c ControlInstance
		if err := rows.Scan(&c.Host, &c.Version, &c.StartedAt, &c.SeenAt, &c.Running,
			&c.CheckAt, &c.CheckOK, &c.CheckDetail); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastControlOutage is the most recent outage recorded, if any.
func (s *Store) LastControlOutage(ctx context.Context) (*ControlOutage, error) {
	var o ControlOutage
	err := s.pool.QueryRow(ctx, `SELECT started_at, ended_at, found_by FROM argus_platform.control_outages
		ORDER BY started_at DESC LIMIT 1`).Scan(&o.StartedAt, &o.EndedAt, &o.FoundBy)
	if errors.Is(err, caduceus.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

/* ── The loop ────────────────────────────────────────────────────────────── */

// controlHostname names this control server in control_instances.
func (a *API) controlHostname() string {
	if a.InstanceName != "" {
		return a.InstanceName
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "argus-control"
	}
	return h
}

// RunLiveness says this control server is alive every minute, records an
// outage the first tick after one, and checks in outside when configured.
// It ticks once at once, so an outage is reported as soon as Argus is back.
func (a *API) RunLiveness(ctx context.Context) {
	a.livenessTick(ctx, time.Now())
	t := time.NewTicker(livenessEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.livenessTick(ctx, now)
		}
	}
}

func (a *API) livenessTick(ctx context.Context, now time.Time) {
	host := a.controlHostname()
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	outage, err := a.store.NoteControlAlive(tctx, host, a.Version)
	if err != nil {
		// No check-in either: the database not answering is exactly the
		// state the outside service exists to notice.
		a.log.Warn("cannot record that this control server is alive", "error", err)
		return
	}
	if outage != nil {
		a.reportOutage(ctx, *outage)
	}
	if a.outsideDue(now) {
		ok, detail := a.checkInOutside(tctx)
		if err := a.store.NoteOutsideCheck(tctx, host, ok, detail); err != nil {
			a.log.Warn("cannot record the outside check-in", "error", err)
		}
	}
}

// outsideDue is whether a check-in is due at now.
func (a *API) outsideDue(now time.Time) bool {
	if a.OutsideCheck.URL == "" {
		return false
	}
	every := a.OutsideCheck.Every
	if every <= 0 {
		every = time.Minute
	}
	a.outside.mu.Lock()
	defer a.outside.mu.Unlock()
	// A few seconds' slack: the ticker is a minute, and a tick a moment
	// early must not skip a whole interval.
	if !a.outside.lastRun.IsZero() && now.Sub(a.outside.lastRun) < every-5*time.Second {
		return false
	}
	a.outside.lastRun = now
	return true
}

// checkInOutside calls the check-in address once.
func (a *API) checkInOutside(ctx context.Context) (bool, string) {
	raw := a.OutsideCheck.URL
	if raw == "" {
		return false, "no check-in address is set"
	}
	cctx, cancel := context.WithTimeout(ctx, outsideCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, raw, nil)
	if err != nil {
		return false, "the address is not valid"
	}
	req.Header.Set("User-Agent", "argus-control/"+a.Version)
	client := a.outsideClient
	if client == nil {
		client = &http.Client{Timeout: outsideCheckTimeout}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// The error names the address, which is the secret; keep the host.
		a.log.Warn("outside check-in failed", "host", outsideCheckHost(raw), "error", redactURL(err.Error(), raw))
		return false, "could not reach " + outsideCheckHost(raw) + ": " + redactURL(err.Error(), raw)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	took := time.Since(start).Round(time.Millisecond)
	if resp.StatusCode/100 != 2 {
		a.log.Warn("outside check-in refused", "host", outsideCheckHost(raw), "status", resp.StatusCode)
		return false, fmt.Sprintf("%s answered %s", outsideCheckHost(raw), resp.Status)
	}
	return true, fmt.Sprintf("%s from %s in %s", resp.Status, outsideCheckHost(raw), took)
}

// redactURL takes the address out of an error that quotes it.
func redactURL(msg, raw string) string {
	if raw == "" {
		return msg
	}
	return strings.ReplaceAll(msg, raw, outsideCheckHost(raw))
}

// reportOutage tells everyone an outage happened, once.
func (a *API) reportOutage(ctx context.Context, o ControlOutage) {
	mins := o.Minutes()
	title := fmt.Sprintf("Argus was down for %d minute%s", mins, plural(mins))
	detail := fmt.Sprintf("From %s to %s UTC no control server was working: none was running, or none "+
		"could reach the database. New sessions, approvals and the console were unavailable. Servers kept "+
		"recording and send what they held as they reconnect. Database backups and cloud schedules that "+
		"were due ran late, or were skipped if more than two hours late.",
		o.StartedAt.UTC().Format("2 Jan 15:04"), o.EndedAt.UTC().Format("15:04"))
	a.log.Warn("argus-control was down", "from", o.StartedAt, "to", o.EndedAt, "minutes", mins)

	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The audit chain of the default organization, where platform events go.
	if _, err := a.store.AppendAudit(WithTenant(actx, &Tenant{ID: DefaultTenantID}), AuditEvent{
		Action: "control.outage", Severity: "warn", ActorEmail: "argus-control",
		Target: "argus-control", Detail: title + ". " + detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "control.outage")
	}
	// Every organization's admins went without the console too.
	if err := a.store.ForEachTenant(actx, func(tctx context.Context, _ *Tenant) {
		a.raiseAlert(tctx, ConsoleAlert{
			Kind: AlertSecurity, Severity: "warning", Audience: AudienceAdmins,
			Actor: "argus-control", Title: title, Detail: detail, Link: "/activity",
		})
	}); err != nil {
		a.log.Error("cannot tell organizations about the outage", "error", err)
	}
	if a.Alerts != nil {
		a.Alerts.SecurityEvent("Argus: "+strings.ToLower(title[:1])+title[1:], detail)
	}
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// postOutsideCheckTest calls the check-in address now, for the Test button.
func (a *API) postOutsideCheckTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	if a.OutsideCheck.URL == "" {
		writeErr(w, http.StatusConflict, "no check-in address is saved yet: save one under Alerts first")
		return
	}
	ok, detail := a.checkInOutside(r.Context())
	if err := a.store.NoteOutsideCheck(r.Context(), a.controlHostname(), ok, detail); err != nil {
		a.log.Warn("cannot record the outside check-in", "error", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "detail": detail})
}

// decodeSmall reads a small JSON body.
func decodeSmall(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(v)
}
