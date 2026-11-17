package control

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"
)

/*
Settings › Control plane › If Argus goes down.

What still works on the servers when this control plane stops, and what
should be ready beforehand. Some of it Argus can see for itself: whether an
outside check-in is set and answering, how many control servers are running,
when the last backup ran, how many servers refuse plain SSH (and so can only
be reached through their provider's console while Argus is down). The rest
it cannot see -- whether backups are copied off this server, where the
signing key is kept, whether anyone knows each provider console -- so an
owner confirms those, the confirmation is audited, and it lapses after
readinessConfirmFor so the page asks again.

Platform owners only, like Status and Backups: it describes the deployment,
not one organization.
*/

// readinessConfirmFor is how long a confirmation holds.
const readinessConfirmFor = 90 * 24 * time.Hour

// Things an owner confirms, because Argus cannot see them.
const (
	ReadyBackupsOffHost   = "backups_off_host"
	ReadyKeyOffHost       = "key_off_host"
	ReadyProviderConsoles = "provider_consoles"
)

// ReadinessItems is every item that is confirmed by hand, in page order.
var ReadinessItems = []string{ReadyBackupsOffHost, ReadyKeyOffHost, ReadyProviderConsoles}

// readinessWhat is what each confirmation says, for the audit entry.
var readinessWhat = map[string]string{
	ReadyBackupsOffHost:   "backups of the control plane are copied off its server",
	ReadyKeyOffHost:       "the signing key is kept somewhere the control plane's server is not",
	ReadyProviderConsoles: "someone knows where each server's provider console is, with a local password for it",
}

// ReadinessConfirmation is who confirmed an item and until when it holds.
type ReadinessConfirmation struct {
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
	// Lapsed is a confirmation older than readinessConfirmFor.
	Lapsed bool `json:"lapsed"`
}

// ReadinessView is the whole page.
type ReadinessView struct {
	OutsideCheck ReadinessOutside   `json:"outsideCheck"`
	Servers      []ControlInstance  `json:"servers"`
	Running      int                `json:"running"`
	Backups      ReadinessBackups   `json:"backups"`
	DirectSSH    ReadinessDirectSSH `json:"directSsh"`
	// Confirmations has a key for every item in ReadinessItems; nil is
	// never confirmed.
	Confirmations map[string]*ReadinessConfirmation `json:"confirmations"`
	LastOutage    *ControlOutage                    `json:"lastOutage,omitempty"`
	// Recovery is the fleet recovery key, in short (recovery_key.go).
	Recovery    *ReadinessRecovery `json:"recovery,omitempty"`
	ConfirmDays int                `json:"confirmDays"`
}

// ReadinessOutside is the outside check-in as the running servers report it.
type ReadinessOutside struct {
	Configured bool       `json:"configured"`
	Host       string     `json:"host,omitempty"`
	Every      string     `json:"every,omitempty"`
	LastAt     *time.Time `json:"lastAt,omitempty"`
	OK         *bool      `json:"ok,omitempty"`
	Detail     string     `json:"detail,omitempty"`
}

// ReadinessBackups is the control plane's own backups, in short.
type ReadinessBackups struct {
	Installed bool       `json:"installed"`
	TimerOn   bool       `json:"timerOn"`
	LastAt    *time.Time `json:"lastAt,omitempty"`
	LastOK    bool       `json:"lastOk"`
	LastError string     `json:"lastError,omitempty"`
}

// ReadinessRecovery is whether a fleet recovery key is held, and by how many
// servers.
type ReadinessRecovery struct {
	Set     bool `json:"set"`
	Holding int  `json:"holding"`
	Total   int  `json:"total"`
}

// ReadinessDirectSSH counts servers by what their agents apply to a login
// that did not come through the console. Refusing is refuse or approve:
// approval needs Argus to answer, so while it is down both refuse.
type ReadinessDirectSSH struct {
	Refusing int `json:"refusing"`
	Total    int `json:"total"`
	// LetIn is how many of the refusing ones let named groups in once they
	// have not heard from Argus for a while (outage_logins.go).
	LetIn int `json:"letIn"`
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// ReadinessConfirmations reads every confirmation on record.
func (s *Store) ReadinessConfirmations(ctx context.Context) (map[string]ReadinessConfirmation, error) {
	rows, err := s.pool.Query(ctx, `SELECT item, confirmed_by, confirmed_at FROM argus_platform.control_readiness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ReadinessConfirmation{}
	for rows.Next() {
		var item string
		var c ReadinessConfirmation
		if err := rows.Scan(&item, &c.By, &c.At); err != nil {
			return nil, err
		}
		out[item] = c
	}
	return out, rows.Err()
}

// ConfirmReadiness records that by confirmed item now.
func (s *Store) ConfirmReadiness(ctx context.Context, item, by string) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `INSERT INTO argus_platform.control_readiness (item, confirmed_by, confirmed_at)
		VALUES ($1, $2, now())
		ON CONFLICT (item) DO UPDATE SET confirmed_by = EXCLUDED.confirmed_by, confirmed_at = now()
		RETURNING confirmed_at`, item, by).Scan(&at)
	return at, err
}

// DirectSSHCounts counts the organization's servers on ctx whose agents
// have said which direct SSH mode they apply.
func (s *Store) DirectSSHCounts(ctx context.Context) (refusing, total int, err error) {
	err = s.db(ctx).QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE agent_direct_ssh IN ('refuse', 'approve')),
		count(*) FILTER (WHERE agent_direct_ssh IS NOT NULL)
		FROM assets`).Scan(&refusing, &total)
	return refusing, total, err
}

/* ── Building the page ───────────────────────────────────────────────────── */

func (a *API) readiness(ctx context.Context) (ReadinessView, error) {
	v := ReadinessView{ConfirmDays: int(readinessConfirmFor / (24 * time.Hour)),
		Confirmations: map[string]*ReadinessConfirmation{}}
	for _, item := range ReadinessItems {
		v.Confirmations[item] = nil
	}

	servers, err := a.store.ControlInstances(ctx)
	if err != nil {
		return v, fmt.Errorf("control servers: %w", err)
	}
	v.Servers = servers
	for _, c := range servers {
		if !c.Running {
			continue
		}
		v.Running++
		// The newest check-in any running server made.
		if c.CheckAt != nil && (v.OutsideCheck.LastAt == nil || c.CheckAt.After(*v.OutsideCheck.LastAt)) {
			v.OutsideCheck.LastAt, v.OutsideCheck.OK, v.OutsideCheck.Detail = c.CheckAt, c.CheckOK, c.CheckDetail
		}
	}
	if a.OutsideCheck.URL != "" {
		v.OutsideCheck.Configured = true
		v.OutsideCheck.Host = outsideCheckHost(a.OutsideCheck.URL)
		v.OutsideCheck.Every = "1m"
		if a.OutsideCheck.Every >= 5*time.Minute {
			v.OutsideCheck.Every = "5m"
		}
	}

	if a.Backups != nil {
		b := a.Backups.view()
		v.Backups.Installed, v.Backups.TimerOn = b.Installed, b.TimerOn
		if b.Last != nil {
			at := b.Last.StartedAt
			v.Backups.LastAt, v.Backups.LastOK, v.Backups.LastError = &at, b.Last.OK, b.Last.Error
		}
	}

	if err := a.store.ForEachTenant(ctx, func(tctx context.Context, t *Tenant) {
		r, n, err := a.store.DirectSSHCounts(tctx)
		if err != nil {
			a.log.Warn("readiness: cannot count servers' direct SSH", "organization", t.Slug, "error", err)
			return
		}
		v.DirectSSH.Refusing += r
		v.DirectSSH.Total += n
		if l, err := a.store.OutageLoginsReach(tctx); err == nil {
			v.DirectSSH.LetIn += l
		} else {
			a.log.Warn("readiness: cannot count servers' outage logins", "organization", t.Slug, "error", err)
		}
	}); err != nil {
		return v, fmt.Errorf("organizations: %w", err)
	}

	conf, err := a.store.ReadinessConfirmations(ctx)
	if err != nil {
		return v, fmt.Errorf("confirmations: %w", err)
	}
	now := time.Now()
	for item, c := range conf {
		if _, known := v.Confirmations[item]; !known {
			continue
		}
		c.Until = c.At.Add(readinessConfirmFor)
		c.Lapsed = now.After(c.Until)
		v.Confirmations[item] = &c
	}

	if rk, err := a.recoveryView(ctx); err == nil {
		v.Recovery = &ReadinessRecovery{Set: rk.Set, Holding: rk.Holding, Total: rk.Total}
	} else {
		a.log.Warn("readiness: cannot read the fleet recovery key", "error", err)
	}
	if v.LastOutage, err = a.store.LastControlOutage(ctx); err != nil {
		return v, fmt.Errorf("outages: %w", err)
	}
	return v, nil
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) getReadiness(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.platformOwner(w, r); !ok {
		return
	}
	v, err := a.readiness(r.Context())
	if err != nil {
		a.log.Error("readiness", "error", err)
		writeErr(w, http.StatusInternalServerError, "could not read what Argus knows about its own outages")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

func (a *API) postReadinessConfirm(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.platformOwner(w, r)
	if !ok {
		return
	}
	item := r.PathValue("item")
	if !slices.Contains(ReadinessItems, item) {
		writeErr(w, http.StatusNotFound, "nothing to confirm by that name")
		return
	}
	at, err := a.store.ConfirmReadiness(r.Context(), item, sess.Email)
	if err != nil {
		a.log.Error("readiness confirm", "error", err)
		writeErr(w, http.StatusInternalServerError, "could not save the confirmation")
		return
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "control.readiness_confirmed", Severity: "info", ActorEmail: sess.Email,
		Target: "argus-control", Detail: fmt.Sprintf("Confirmed that %s.", readinessWhat[item]),
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "control.readiness_confirmed")
	}
	writeJSON(w, http.StatusOK, ReadinessConfirmation{By: sess.Email, At: at,
		Until: at.Add(readinessConfirmFor)})
}
