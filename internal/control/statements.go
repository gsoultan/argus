package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5"
)

/*
Approval for each statement on a production database. See migration 046 and
internal/tunnel/approval.go for which statements.

	POST /api/v1/databases/{id}/statements             ask: the statements, exactly as they will run
	GET  /api/v1/databases/{id}/statements/{request}   how the ask stands

The ask is an ordinary access request (principal db:statement), so it waits
in Approvals beside the others, alerts whoever may decide it, and cannot be
decided by the person who asked. Once approved, each of its statements may run
once, within the hour, from that person's session on that database: the relay
compares the statement and its values with what was approved (by digest),
marks it used and only then passes it to the agent, which records the
approval's id with the statement.
*/

const (
	// statementApprovalMin is how long an approval stays usable.
	statementApprovalMin = 60
	maxApprovalItems     = 500
	maxApprovalSQL       = 100000
	maxApprovalTotal     = 2 << 20
	// maxApprovalSummary holds the per-table counts and a preview of 20 rows
	// a statement, cut to 200 characters a value, for a few statements.
	maxApprovalSummary = 512 << 10
	statementPrincipal = "db:statement"
)

// StatementItem is one statement of an ask.
type StatementItem struct {
	SQL    string    `json:"sql"`
	Params []*string `json:"params,omitempty"`
	// Rows is how many rows the dry run counted when it was asked for, and
	// the most it may change once approved. Nil is a statement that does not
	// count rows, or one asked for without a dry run.
	Rows *int64 `json:"rows,omitempty"`
	Used bool   `json:"used,omitempty"`
}

// StatementAsk is what a db:statement request is for, for the approver's
// card and the asker's wait.
type StatementAsk struct {
	RequestID    string          `json:"requestId"`
	DatabaseID   string          `json:"databaseId"`
	DatabaseName string          `json:"databaseName"`
	Items        []StatementItem `json:"items"`
	Summary      json.RawMessage `json:"summary,omitempty"`
	State        string          `json:"state"`
	ExpiresAt    *time.Time      `json:"expiresAt,omitempty"`
	DecidedBy    *string         `json:"decidedBy,omitempty"`
	Note         *string         `json:"note,omitempty"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errNoApproval is a statement's approval not standing; its text says why.
type errNoApproval struct{ why string }

func (e errNoApproval) Error() string { return e.why }

// CreateStatementAsk files a request to run items on db.
func (s *Store) CreateStatementAsk(ctx context.Context, email string, db Database, items []StatementItem,
	summary json.RawMessage, justification string) (AccessRequest, error) {
	justification = strings.TrimSpace(justification)
	if len(justification) < 20 {
		return AccessRequest{}, ErrInvalidDatabase{"say why, in at least 20 characters; the approver reads it"}
	}
	if len(items) == 0 || len(items) > maxApprovalItems {
		return AccessRequest{}, ErrInvalidDatabase{fmt.Sprintf("ask for 1 to %d statements at a time", maxApprovalItems)}
	}
	total := 0
	for _, it := range items {
		if strings.TrimSpace(it.SQL) == "" || len(it.SQL) > maxApprovalSQL {
			return AccessRequest{}, ErrInvalidDatabase{fmt.Sprintf("each statement is 1 to %d characters", maxApprovalSQL)}
		}
		total += len(it.SQL)
		if it.Rows != nil && *it.Rows < 0 {
			return AccessRequest{}, ErrInvalidDatabase{"a statement's row count cannot be negative"}
		}
		for _, p := range it.Params {
			if p != nil {
				total += len(*p)
			}
		}
	}
	if total > maxApprovalTotal {
		return AccessRequest{}, ErrInvalidDatabase{"the statements are too long to ask for at once"}
	}
	if len(summary) > maxApprovalSummary {
		return AccessRequest{}, ErrInvalidDatabase{"the dry run's summary is too long to keep with the ask"}
	}
	if len(summary) == 0 || !json.Valid(summary) {
		summary = json.RawMessage(`{}`)
	}

	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return AccessRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r := AccessRequest{RequesterEmail: email, AssetHostnames: []string{db.Hostname}, Principal: statementPrincipal,
		Justification: justification, DurationMinutes: statementApprovalMin, State: "pending"}
	if err := tx.QueryRow(ctx, `
		INSERT INTO access_requests
			(requester_email, asset_hostnames, principal, justification,
			 duration_minutes, state, break_glass, database_id)
		VALUES ($1,$2,$3,$4,$5,'pending',false,$6::uuid)
		RETURNING id::text, created_at`,
		r.RequesterEmail, r.AssetHostnames, r.Principal, r.Justification, r.DurationMinutes, db.ID).
		Scan(&r.ID, &r.CreatedAt); err != nil {
		return r, fmt.Errorf("create request: %w", err)
	}
	var sid string
	if err := tx.QueryRow(ctx, `
		INSERT INTO database_statements (request_id, database_id, requester_email, summary)
		VALUES ($1::uuid, $2::uuid, $3, $4::jsonb) RETURNING id::text`,
		r.ID, db.ID, email, string(summary)).Scan(&sid); err != nil {
		return r, fmt.Errorf("create statement ask: %w", err)
	}
	for n, it := range items {
		params := it.Params
		if params == nil {
			params = []*string{}
		}
		p, _ := json.Marshal(params)
		if _, err := tx.Exec(ctx, `
			INSERT INTO database_statement_items (statement_id, seq, sql, params, digest, row_cap)
			VALUES ($1::uuid, $2, $3, $4::jsonb, $5, $6)`,
			sid, n, it.SQL, string(p), tunnel.StatementDigest(it.SQL, it.Params), it.Rows); err != nil {
			return r, fmt.Errorf("create statement ask: %w", err)
		}
	}
	return r, tx.Commit(ctx)
}

// StatementAsks reads what the db:statement requests ids are for.
func (s *Store) StatementAsks(ctx context.Context, ids []string) (map[string]*StatementAsk, error) {
	out := map[string]*StatementAsk{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db(ctx).Query(ctx, `
		SELECT r.id::text, s.database_id::text, coalesce(d.dbname, ''), s.summary::text, r.state,
		       r.expires_at, r.decided_by_email, r.decision_note,
		       i.sql, i.params::text, i.row_cap, i.used_at IS NOT NULL
		FROM access_requests r
		JOIN database_statements s ON s.request_id = r.id
		LEFT JOIN databases d ON d.id = s.database_id
		JOIN database_statement_items i ON i.statement_id = s.id
		WHERE r.id = ANY($1::uuid[])
		ORDER BY r.id, i.seq`, ids)
	if err != nil {
		return nil, fmt.Errorf("statement asks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a StatementAsk
		var summary, params string
		var it StatementItem
		if err := rows.Scan(&a.RequestID, &a.DatabaseID, &a.DatabaseName, &summary, &a.State,
			&a.ExpiresAt, &a.DecidedBy, &a.Note, &it.SQL, &params, &it.Rows, &it.Used); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &it.Params)
		cur := out[a.RequestID]
		if cur == nil {
			a.Summary = json.RawMessage(summary)
			a.Items = []StatementItem{}
			cur = &a
			out[a.RequestID] = cur
		}
		cur.Items = append(cur.Items, it)
	}
	return out, rows.Err()
}

// UseStatementApproval marks the approved statement used, or says why it
// cannot run. Each approved statement runs once. It returns the most rows
// the statement may change, or nil for no cap.
func (s *Store) UseStatementApproval(ctx context.Context, requestID, email, dbID string, sql string, params []*string) (*int64, error) {
	if !uuidRE.MatchString(requestID) {
		return nil, errNoApproval{"that approval does not exist"}
	}
	digest := tunnel.StatementDigest(sql, params)
	var n int
	var rowCap *int64
	err := s.db(ctx).QueryRow(ctx, `
		UPDATE database_statement_items i SET used_at = now()
		WHERE (i.statement_id, i.seq) = (
			SELECT i2.statement_id, i2.seq
			FROM database_statement_items i2
			JOIN database_statements s ON s.id = i2.statement_id
			JOIN access_requests r ON r.id = s.request_id
			WHERE r.id = $1::uuid AND r.principal = 'db:statement' AND r.state = 'approved'
			  AND r.expires_at > now() AND lower(s.requester_email) = lower($2)
			  AND s.database_id = $3::uuid AND i2.digest = $4 AND i2.used_at IS NULL
			ORDER BY i2.seq LIMIT 1
			FOR UPDATE OF i2)
		  AND i.used_at IS NULL
		RETURNING i.seq, i.row_cap`, requestID, email, dbID, digest).Scan(&n, &rowCap)
	if err == nil {
		return rowCap, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("checking the approval: %w", err)
	}
	// Why not, in words.
	var state, requester, db string
	var expires *time.Time
	var matching, unused int
	err = s.db(ctx).QueryRow(ctx, `
		SELECT r.state, s.requester_email, s.database_id::text, r.expires_at,
		       count(*) FILTER (WHERE i.digest = $2),
		       count(*) FILTER (WHERE i.digest = $2 AND i.used_at IS NULL)
		FROM access_requests r
		JOIN database_statements s ON s.request_id = r.id
		JOIN database_statement_items i ON i.statement_id = s.id
		WHERE r.id = $1::uuid
		GROUP BY r.state, s.requester_email, s.database_id, r.expires_at`, requestID, digest).
		Scan(&state, &requester, &db, &expires, &matching, &unused)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, errNoApproval{"that approval does not exist"}
	case err != nil:
		return nil, fmt.Errorf("checking the approval: %w", err)
	case !strings.EqualFold(requester, email) || db != dbID:
		return nil, errNoApproval{"that approval is for someone else's statement"}
	case state == "pending":
		return nil, errNoApproval{"the approval is still waiting for a decision"}
	case state == "denied":
		return nil, errNoApproval{"the approval was denied"}
	case state != "approved" || expires == nil || !expires.After(time.Now()):
		return nil, errNoApproval{"the approval has expired; ask again"}
	case matching == 0:
		return nil, errNoApproval{"this statement is not the one that was approved; it changed after the ask, so ask again"}
	case unused == 0:
		return nil, errNoApproval{"this statement already ran under that approval; each approved statement runs once"}
	}
	return nil, errNoApproval{"the approval does not cover this statement"}
}

type statementAskInput struct {
	Items         []StatementItem `json:"items"`
	Summary       json.RawMessage `json:"summary"`
	Justification string          `json:"justification"`
}

// POST /api/v1/databases/{id}/statements
func (a *API) postStatementAsk(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	if !db.WritesNeedApproval() {
		writeErr(w, http.StatusBadRequest, "statements on "+db.Name+" run without approval")
		return
	}
	var in statementAskInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for i := range in.Items {
		in.Items[i].Used = false
	}
	req, err := a.store.CreateStatementAsk(r.Context(), email, db, in.Items, in.Summary, in.Justification)
	if err != nil {
		a.dbErr(w, "statement approval", err)
		return
	}
	what := "1 statement"
	if len(in.Items) != 1 {
		what = itoa(len(in.Items)) + " statements"
	}
	a.auditDB(r.Context(), "db.statement_requested", "notice", email, db.Hostname,
		fmt.Sprintf("Asked to run %s on %s (%s): %s. Waiting for an approver (request %s).",
			what, db.Name, db.Label, req.Justification, req.ID))
	a.alertRequestWaiting(r.Context(), req)
	a.alertEditorsWaiting(r.Context(), db, req)
	asks, err := a.store.StatementAsks(r.Context(), []string{req.ID})
	if err != nil || asks[req.ID] == nil {
		a.fail(w, "statement approval", err)
		return
	}
	writeJSON(w, http.StatusCreated, asks[req.ID])
}

// GET /api/v1/databases/{id}/statements/{request}
func (a *API) getStatementAsk(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	id := r.PathValue("request")
	if !uuidRE.MatchString(id) {
		writeErr(w, http.StatusNotFound, "no such approval")
		return
	}
	asks, err := a.store.StatementAsks(r.Context(), []string{id})
	if err != nil {
		a.fail(w, "statement approval", err)
		return
	}
	ask := asks[id]
	if ask == nil || ask.DatabaseID != db.ID {
		writeErr(w, http.StatusNotFound, "no such approval")
		return
	}
	var requester string
	if err := a.store.db(r.Context()).QueryRow(r.Context(),
		`SELECT requester_email FROM access_requests WHERE id = $1::uuid`, id).Scan(&requester); err != nil ||
		!strings.EqualFold(requester, email) {
		writeErr(w, http.StatusNotFound, "no such approval")
		return
	}
	if ask.State == "approved" && ask.ExpiresAt != nil && !ask.ExpiresAt.After(time.Now()) {
		ask.State = "expired"
	}
	writeJSON(w, http.StatusOK, ask)
}

// approvalWhy is what a refusal says about why db's changes wait.
func approvalWhy(db Database) string {
	if db.Label == "production" && db.Policy.WriteApproval == nil {
		return " is production"
	}
	return " asks for approval of changes"
}

// statementGate decides, in the relay, whether a run request on db may pass
// to the agent. It returns the refusal to answer with, or nil.
//
// It also sets req.RowCap from the approval the statement runs under, and
// clears whatever the browser sent there.
func (a *API) statementGate(ctx context.Context, email string, db Database, level string, req *tunnel.PGRequest) *tunnel.PGAnswer {
	req.RowCap = nil
	if req.Op != "run" || !db.WritesNeedApproval() || level == "" || level == tunnel.LevelRead {
		return nil
	}
	c := tunnel.ClassifySQL(req.SQL)
	if !tunnel.NeedsApproval(c, req.SQL) {
		return nil
	}
	// The agent refuses what its guard stops, so nothing is used up on it;
	// the person confirms first, then asks.
	if tunnel.GuardSQL(c, db.Name, req.Confirm) != nil {
		return nil
	}
	if req.Approval == "" {
		return &tunnel.PGAnswer{ID: req.ID, Type: "error", Code: tunnel.GuardNeedsApproval,
			Error: db.Name + approvalWhy(db) + ": a statement that changes it runs once someone else approves it"}
	}
	rowCap, err := a.store.UseStatementApproval(ctx, req.Approval, email, db.ID, req.SQL, req.Params)
	if err == nil {
		req.RowCap = rowCap
		return nil
	}
	var why errNoApproval
	if !errors.As(err, &why) {
		a.log.Error("checking a statement approval failed", "database", db.Name, "email", email, "error", err)
		return &tunnel.PGAnswer{ID: req.ID, Type: "error", Code: tunnel.GuardNeedsApproval,
			Error: "could not check the approval; try again"}
	}
	return &tunnel.PGAnswer{ID: req.ID, Type: "error", Code: tunnel.GuardNeedsApproval, Error: why.why}
}
