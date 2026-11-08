package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Who is asked before a session opens, per type of asset, with an asset's own
setting where it differs. Settings › Asset types shows and edits the rules;
an asset's edit form holds its own.

	never  opens without asking or telling anyone
	ask    asks whoever is using the machine; no answer is a no
	tell   opens at once and shows them a notice naming who connected

Desktops and shared screens follow Desktop; terminals follow Terminal, which
can never be "ask": a terminal is a login of its own and takes nothing from
the person at the screen. A device is not here: the Argus app on it asks, as
its control mode says.

	GET /api/v1/settings/asset-types   admins, owners, auditors
	PUT /api/v1/settings/asset-types   admins and owners, audited
*/

// Ask answers.
const (
	ConnectNever = "never"
	ConnectAsk   = "ask"
	ConnectTell  = "tell"
)

// ConnectRule is one type's rule.
type ConnectRule struct {
	// Desktop is never, ask or tell, for Remote Desktop and screen sharing.
	Desktop string `json:"desktop"`
	// Terminal is never or tell.
	Terminal string `json:"terminal"`
	// Seconds is how long a question waits before it is a no.
	Seconds int `json:"seconds"`
}

// ConnectRules is every type's rule.
type ConnectRules struct {
	Server      ConnectRule `json:"server"`
	Workstation ConnectRule `json:"workstation"`
}

// DefaultConnectRules: nobody sits at a server; someone uses a workstation.
func DefaultConnectRules() ConnectRules {
	return ConnectRules{
		Server:      ConnectRule{Desktop: ConnectNever, Terminal: ConnectNever, Seconds: workstationAskSeconds},
		Workstation: ConnectRule{Desktop: ConnectAsk, Terminal: ConnectTell, Seconds: workstationAskSeconds},
	}
}

func (r ConnectRule) validate(kind string) error {
	switch r.Desktop {
	case ConnectNever, ConnectAsk, ConnectTell:
	default:
		return fmt.Errorf("%s: a desktop is never asked, asked or told", kind)
	}
	switch r.Terminal {
	case ConnectNever, ConnectTell:
	default:
		return fmt.Errorf("%s: a terminal is never told or told; it is never asked", kind)
	}
	if r.Seconds < 10 || r.Seconds > tunnel.MaxAskSeconds {
		return fmt.Errorf("%s: a question waits between 10 and %d seconds", kind, tunnel.MaxAskSeconds)
	}
	return nil
}

// Validate refuses rules the console never sends.
func (r ConnectRules) Validate() error {
	if err := r.Server.validate("server"); err != nil {
		return err
	}
	return r.Workstation.validate("workstation")
}

func (r ConnectRules) forKind(kind string) (ConnectRule, bool) {
	switch kind {
	case "server", "":
		return r.Server, true
	case "workstation":
		return r.Workstation, true
	}
	return ConnectRule{}, false
}

// ConnectRules reads the rules, with the default for any type not stored.
func (s *Store) ConnectRules(ctx context.Context) (ConnectRules, error) {
	out := DefaultConnectRules()
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT connect_rules FROM gateway_policy WHERE id = TRUE`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var stored map[string]ConnectRule
	if err := json.Unmarshal(raw, &stored); err != nil {
		return out, err
	}
	// A stored rule replaces the default only when it is whole; a broken one
	// leaves the default, so a bad row never opens a workstation unasked.
	if r, ok := stored["server"]; ok && r.validate("server") == nil {
		out.Server = r
	}
	if r, ok := stored["workstation"]; ok && r.validate("workstation") == nil {
		out.Workstation = r
	}
	return out, nil
}

// SetConnectRules stores the rules.
func (s *Store) SetConnectRules(ctx context.Context, r ConnectRules, actor string) error {
	b, err := json.Marshal(map[string]ConnectRule{"server": r.Server, "workstation": r.Workstation})
	if err != nil {
		return err
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE gateway_policy SET connect_rules = $1, updated_at = now(), updated_by = $2
		WHERE id = TRUE`, b, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = s.db(ctx).Exec(ctx, `INSERT INTO gateway_policy (id, connect_rules, updated_by) VALUES (TRUE, $1, $2)
			ON CONFLICT (id) DO UPDATE SET connect_rules = EXCLUDED.connect_rules`, b, actor)
	}
	return err
}

// assetConnect reads a target's kind and its own setting.
func (s *Store) assetConnect(ctx context.Context, target string) (kind string, own *string, err error) {
	err = s.db(ctx).QueryRow(ctx, `
		SELECT a.kind, a.connect_ask FROM assets a
		WHERE a.archived_at IS NULL
		  AND (a.hostname = lower($1) OR split_part(a.hostname, '.', 1) = lower($1))
		ORDER BY (a.hostname = lower($1)) DESC
		LIMIT 1`, target).Scan(&kind, &own)
	if errors.Is(err, pgx.ErrNoRows) {
		return "server", nil, nil
	}
	return kind, own, err
}

// askFor is the Ask a grant to host carries, or nil to open without asking
// anyone. desktop is a Remote Desktop or a shared screen; otherwise a
// terminal. An error means the rule could not be read, and the session does
// not open: opening unasked on a machine someone may be using is the
// failure this exists to prevent.
func (a *API) askFor(ctx context.Context, host, email string, desktop bool) (*tunnel.GrantAsk, error) {
	kind, own, err := a.store.assetConnect(ctx, host)
	if err != nil {
		return nil, err
	}
	rules, err := a.store.ConnectRules(ctx)
	if err != nil {
		return nil, err
	}
	rule, ok := rules.forKind(kind)
	if !ok {
		return nil, nil
	}
	mode := rule.Terminal
	if desktop {
		mode = rule.Desktop
	}
	if own != nil {
		mode = *own
		if !desktop && mode == ConnectAsk {
			mode = ConnectTell
		}
	}
	switch mode {
	case ConnectAsk:
		return &tunnel.GrantAsk{Mode: tunnel.AskFirst, Seconds: rule.Seconds, Who: email}, nil
	case ConnectTell:
		return &tunnel.GrantAsk{Mode: tunnel.TellOnly, Who: email}, nil
	}
	return nil, nil
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

func (a *API) registerConnectRuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/settings/asset-types", a.getConnectRules)
	mux.HandleFunc("PUT /api/v1/settings/asset-types", a.putConnectRules)
}

func (a *API) getConnectRules(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "asset type rules are for administrators, owners and auditors")
		return
	}
	rules, err := a.store.ConnectRules(r.Context())
	if err != nil {
		a.fail(w, "asset type rules", err)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (a *API) putConnectRules(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing who is asked before connecting", "admin", "owner") {
		return
	}
	var in ConnectRules
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, err := a.store.ConnectRules(r.Context())
	if err != nil {
		a.fail(w, "asset type rules", err)
		return
	}
	if err := a.store.SetConnectRules(r.Context(), in, sess.Email); err != nil {
		a.fail(w, "asset type rules", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action:     "settings.connect_rules",
		Severity:   "notice",
		ActorEmail: sess.Email,
		Target:     "asset types",
		Detail: "Before connecting: servers were " + describeRule(before.Server) + ", now " + describeRule(in.Server) +
			"; workstations were " + describeRule(before.Workstation) + ", now " + describeRule(in.Workstation) + ".",
	})
	writeJSON(w, http.StatusOK, in)
}

func describeRule(r ConnectRule) string {
	d := map[string]string{ConnectNever: "never asked", ConnectAsk: "asked first", ConnectTell: "told"}
	s := "desktop " + d[r.Desktop] + ", terminal " + d[r.Terminal]
	if r.Desktop == ConnectAsk {
		s += fmt.Sprintf(", %d s to answer", r.Seconds)
	}
	return strings.TrimSpace(s)
}
