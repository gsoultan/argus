package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/vault"
)

/*
Approvals for reading secrets.

A rule can say a person's read needs an approval (needsApproval). The person
asks with a reason; whoever decides access requests (approver, admin, owner)
approves or denies it, never the person who asked, unless nobody else could
(self_approval.go). An approved request lets that person read that one secret
through the console until it expires. argus-control then tells argus-secret
the read was approved; argus-secret still checks the rule and logs the read.

	GET  /api/v1/secrets/requests             my requests, and those waiting for me to decide
	POST /api/v1/secrets/requests             ask {name, reason, minutes}
	POST /api/v1/secrets/requests/{id}/decide {approve, note, confirmSelf}
	POST /api/v1/secrets/requests/{id}/cancel withdraw my own, while pending
*/

const (
	defaultSecretRequestMinutes = 30
	maxSecretRequestMinutes     = 480
)

// SecretReadRequest is one ask to read a secret.
type SecretReadRequest struct {
	ID           string     `json:"id"`
	Requester    string     `json:"requester"`
	Name         string     `json:"name"`
	Reason       string     `json:"reason"`
	Minutes      int        `json:"minutes"`
	State        string     `json:"state"`
	DecidedBy    string     `json:"decidedBy,omitempty"`
	DecidedAt    *time.Time `json:"decidedAt,omitempty"`
	DecisionNote string     `json:"decisionNote,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	// ApproveOwn: the reader asked for it and may approve it themselves.
	ApproveOwn bool `json:"approveOwn,omitempty"`
}

// ErrNoSecretRequest is a request that is not there, or not the reader's.
var ErrNoSecretRequest = errors.New("no such request")

const secretRequestColumns = `id::text, requester_email, secret_name, reason, minutes, state, decided_by, decided_at,
	decision_note, expires_at, created_at`

func scanSecretRequest(row pgx.Row) (SecretReadRequest, error) {
	var q SecretReadRequest
	err := row.Scan(&q.ID, &q.Requester, &q.Name, &q.Reason, &q.Minutes, &q.State, &q.DecidedBy, &q.DecidedAt,
		&q.DecisionNote, &q.ExpiresAt, &q.CreatedAt)
	return q, err
}

func collectSecretRequests(rows pgx.Rows, err error) ([]SecretReadRequest, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretReadRequest{}
	for rows.Next() {
		q, err := scanSecretRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SecretRequests are email's own requests of the last 30 days, and, for a
// decider, everyone's pending ones.
func (s *Store) SecretRequests(ctx context.Context, email string, decider bool) ([]SecretReadRequest, error) {
	return collectSecretRequests(s.db(ctx).Query(ctx, `SELECT `+secretRequestColumns+` FROM secret_read_requests
		WHERE (lower(requester_email) = lower($1) AND created_at > now() - interval '30 days')
		   OR ($2 AND state = 'pending')
		ORDER BY created_at DESC LIMIT 500`, email, decider))
}

// CreateSecretRequest asks. One pending request per person and secret.
func (s *Store) CreateSecretRequest(ctx context.Context, email, name, reason string, minutes int) (SecretReadRequest, error) {
	var out SecretReadRequest
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('argus_secret_request:' || lower($1) || ':' || $2))`, email, name); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM secret_read_requests
			WHERE lower(requester_email) = lower($1) AND secret_name = $2 AND state = 'pending'`, email, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrInvalidSecrets{"you already asked to read " + name + "; wait for the answer, or cancel it"}
		}
		var err error
		out, err = scanSecretRequest(tx.QueryRow(ctx, `INSERT INTO secret_read_requests (requester_email, secret_name, reason, minutes)
			VALUES (lower($1), $2, $3, $4) RETURNING `+secretRequestColumns, email, name, reason, minutes))
		return err
	})
	return out, err
}

// SecretRequest is one request.
func (s *Store) SecretRequest(ctx context.Context, id string) (SecretReadRequest, error) {
	if !validUUID(id) {
		return SecretReadRequest{}, ErrNoSecretRequest
	}
	q, err := scanSecretRequest(s.db(ctx).QueryRow(ctx, `SELECT `+secretRequestColumns+` FROM secret_read_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return q, ErrNoSecretRequest
	}
	return q, err
}

// DecideSecretRequest settles a pending request: approved, denied or
// cancelled. own says the decider may decide their own request.
func (s *Store) DecideSecretRequest(ctx context.Context, id, state, by, note string, own bool) (SecretReadRequest, error) {
	q, err := scanSecretRequest(s.db(ctx).QueryRow(ctx, `UPDATE secret_read_requests SET state = $2, decided_by = lower($3),
			decided_at = now(), decision_note = $4,
			expires_at = CASE WHEN $2 = 'approved' THEN now() + make_interval(mins => minutes) END
		WHERE id = $1 AND state = 'pending' AND ($5 OR $2 = 'cancelled' OR lower(requester_email) <> lower($3))
		RETURNING `+secretRequestColumns, id, state, by, note, own))
	if errors.Is(err, pgx.ErrNoRows) {
		return q, ErrNoSecretRequest
	}
	return q, err
}

// secretReadApproved is whether email has an approved, unexpired request to
// read name.
func (s *Store) secretReadApproved(ctx context.Context, email, name string) (bool, error) {
	var ok bool
	err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM secret_read_requests
		WHERE lower(requester_email) = lower($1) AND secret_name = $2 AND state = 'approved' AND expires_at > now())`,
		email, name).Scan(&ok)
	return ok, err
}

func secretRequestSubject(id string) string { return "secret-request:" + id }

func isDecider(role string) bool {
	for _, r := range requestDeciders {
		if r == role {
			return true
		}
	}
	return false
}

func (a *API) getSecretRequests(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	reqs, err := a.store.SecretRequests(r.Context(), sess.Email, isDecider(sess.Role))
	if err != nil {
		a.secretsErr(w, "list secret requests", err)
		return
	}
	sole := false
	if isDecider(sess.Role) {
		if sole, err = a.store.SoleDecider(r.Context(), sess.Email, requestDeciders); err != nil {
			a.secretsErr(w, "list secret requests", err)
			return
		}
	}
	for i := range reqs {
		reqs[i].ApproveOwn = sole && reqs[i].State == "pending" && strings.EqualFold(reqs[i].Requester, sess.Email)
	}
	writeJSON(w, http.StatusOK, reqs)
}

func (a *API) postSecretRequest(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Name    string `json:"name"`
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if err := vault.ValidName(in.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		writeErr(w, http.StatusBadRequest, "say why you need to read it, in up to 500 characters")
		return
	}
	if in.Minutes == 0 {
		in.Minutes = defaultSecretRequestMinutes
	}
	if in.Minutes < 5 || in.Minutes > maxSecretRequestMinutes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("an approval lasts 5 to %d minutes", maxSecretRequestMinutes))
		return
	}
	if _, err := a.store.SecretsServerFor(r.Context(), in.Name); err != nil {
		a.secretsErr(w, "ask to read secret", err)
		return
	}
	q, err := a.store.CreateSecretRequest(r.Context(), sess.Email, in.Name, in.Reason, in.Minutes)
	if err != nil {
		a.secretsErr(w, "ask to read secret", err)
		return
	}
	a.audit(r, "secrets.request", sess.Email, "secret "+q.Name,
		fmt.Sprintf("Asked to read %s for %d minutes: %s", q.Name, q.Minutes, q.Reason))
	a.raiseAlert(r.Context(), ConsoleAlert{Kind: AlertApproval, Severity: "warning", Audience: AudienceDeciders,
		Actor: sess.Email, Title: sess.Email + " asks to read the secret " + q.Name, Detail: q.Reason,
		Link: "/secrets/requests", Subject: secretRequestSubject(q.ID)})
	writeJSON(w, http.StatusCreated, q)
}

func (a *API) postSecretRequestDecide(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !isDecider(sess.Role) {
		writeErr(w, http.StatusForbidden, "only an approver, admin or owner decides who may read a secret")
		return
	}
	var in struct {
		Approve     bool   `json:"approve"`
		Note        string `json:"note"`
		ConfirmSelf string `json:"confirmSelf"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if !in.Approve && in.Note == "" {
		writeErr(w, http.StatusBadRequest, "say why it is denied")
		return
	}
	if len([]rune(in.Note)) > 500 {
		writeErr(w, http.StatusBadRequest, "a note is at most 500 characters")
		return
	}
	q, err := a.store.SecretRequest(r.Context(), r.PathValue("id"))
	if err != nil || q.State != "pending" {
		writeErr(w, http.StatusNotFound, "no such request waiting")
		return
	}
	own := strings.EqualFold(q.Requester, sess.Email)
	if own && !a.approveOwn(w, r, sess.Email, in.Approve, requestDeciders, false, in.ConfirmSelf,
		"someone else decides your own request", "request to read the secret "+q.Name, "secret "+q.Name) {
		return
	}
	state := "denied"
	if in.Approve {
		state = "approved"
	}
	q, err = a.store.DecideSecretRequest(r.Context(), q.ID, state, sess.Email, in.Note, own)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such request waiting")
		return
	}
	detail := fmt.Sprintf("%s %s's request to read %s.", strings.ToUpper(state[:1])+state[1:], q.Requester, q.Name)
	if q.ExpiresAt != nil {
		detail += " Until " + q.ExpiresAt.UTC().Format(time.RFC3339) + "."
	}
	if in.Note != "" {
		detail += " Note: " + in.Note
	}
	a.audit(r, "secrets.request."+state, sess.Email, "secret "+q.Name, detail)
	a.closeAlerts(r.Context(), secretRequestSubject(q.ID), strings.ToUpper(state[:1])+state[1:]+" by "+sess.Email)
	title := "You may read " + q.Name
	if !in.Approve {
		title = "Your request to read " + q.Name + " was denied"
	}
	a.raiseAlert(r.Context(), ConsoleAlert{Kind: AlertApproval, Audience: AudiencePerson, Recipient: q.Requester,
		Actor: sess.Email, Title: title, Detail: detail, Link: "/secrets"})
	writeJSON(w, http.StatusOK, q)
}

func (a *API) postSecretRequestCancel(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q, err := a.store.SecretRequest(r.Context(), r.PathValue("id"))
	if err != nil || !strings.EqualFold(q.Requester, sess.Email) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	q, err = a.store.DecideSecretRequest(r.Context(), q.ID, "cancelled", sess.Email, "", true)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such request waiting")
		return
	}
	a.audit(r, "secrets.request.cancelled", sess.Email, "secret "+q.Name, "Cancelled their request to read "+q.Name+".")
	a.closeAlerts(r.Context(), secretRequestSubject(q.ID), "Cancelled by "+sess.Email)
	writeJSON(w, http.StatusOK, q)
}
