package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Saved queries, for the database workspace. See migration 043.

	GET    /api/v1/databases/{id}/saved-queries         mine and shared
	POST   /api/v1/databases/{id}/saved-queries         save
	PUT    /api/v1/databases/{id}/saved-queries/{q}     rename, change, share (author only)
	DELETE /api/v1/databases/{id}/saved-queries/{q}     remove (author or admin)

Anyone who may open the database may list and save; a saved query is only
text, so it grants nothing. Reading a shared query needs the same access to
the same database, so a shared query never reaches someone who could not
have typed it there.
*/

// SavedQuery is one kept query.
type SavedQuery struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Name      string    `json:"name"`
	SQL       string    `json:"sql"`
	Shared    bool      `json:"shared"`
	Mine      bool      `json:"mine"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const (
	maxSavedPerPerson = 200
	maxSavedListed    = 500
	maxSavedSQL       = 100000
	maxSavedName      = 120
)

var errSavedNotFound = errors.New("no such saved query")

func (q *SavedQuery) check() error {
	q.Name = strings.TrimSpace(q.Name)
	switch {
	case q.Name == "" || len([]rune(q.Name)) > maxSavedName:
		return ErrInvalidDatabase{fmt.Sprintf("a saved query needs a name of 1 to %d characters", maxSavedName)}
	case strings.TrimSpace(q.SQL) == "":
		return ErrInvalidDatabase{"there is no query to save"}
	case len(q.SQL) > maxSavedSQL:
		return ErrInvalidDatabase{fmt.Sprintf("a saved query is at most %d characters", maxSavedSQL)}
	}
	return nil
}

// SavedQueries lists email's own queries on db and those others shared.
func (s *Store) SavedQueries(ctx context.Context, dbID, email string) ([]SavedQuery, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, owner, name, sql, shared, created_at, updated_at
		FROM saved_queries WHERE database_id = $1::uuid AND (owner = $2 OR shared)
		ORDER BY owner = $2 DESC, lower(name), created_at LIMIT $3`, dbID, email, maxSavedListed)
	if err != nil {
		return nil, fmt.Errorf("saved queries: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SavedQuery, error) {
		var q SavedQuery
		err := r.Scan(&q.ID, &q.Owner, &q.Name, &q.SQL, &q.Shared, &q.CreatedAt, &q.UpdatedAt)
		q.Mine = q.Owner == email
		return q, err
	})
}

// SaveQuery keeps a new query for email.
func (s *Store) SaveQuery(ctx context.Context, dbID, email string, q SavedQuery) (SavedQuery, error) {
	if err := q.check(); err != nil {
		return q, err
	}
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM saved_queries WHERE database_id = $1::uuid AND owner = $2`,
		dbID, email).Scan(&n); err != nil {
		return q, fmt.Errorf("save query: %w", err)
	}
	if n >= maxSavedPerPerson {
		return q, ErrInvalidDatabase{fmt.Sprintf("you have %d saved queries on this database; remove one first", n)}
	}
	err := s.db(ctx).QueryRow(ctx, `INSERT INTO saved_queries (database_id, owner, name, sql, shared)
		VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text, created_at, updated_at`,
		dbID, email, q.Name, q.SQL, q.Shared).Scan(&q.ID, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return q, fmt.Errorf("save query: %w", err)
	}
	q.Owner, q.Mine = email, true
	return q, nil
}

// UpdateSavedQuery changes one of email's own queries.
func (s *Store) UpdateSavedQuery(ctx context.Context, dbID, email string, q SavedQuery) (SavedQuery, error) {
	if err := q.check(); err != nil {
		return q, err
	}
	err := s.db(ctx).QueryRow(ctx, `UPDATE saved_queries SET name = $4, sql = $5, shared = $6, updated_at = now()
		WHERE id = $1::uuid AND database_id = $2::uuid AND owner = $3 RETURNING created_at, updated_at`,
		q.ID, dbID, email, q.Name, q.SQL, q.Shared).Scan(&q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || isBadUUID(err) {
		return q, errSavedNotFound
	}
	if err != nil {
		return q, fmt.Errorf("update saved query: %w", err)
	}
	q.Owner, q.Mine = email, true
	return q, nil
}

// SavedQuery reads one query on db, whoever owns it.
func (s *Store) SavedQuery(ctx context.Context, dbID, id string) (SavedQuery, error) {
	var q SavedQuery
	err := s.db(ctx).QueryRow(ctx, `SELECT id::text, owner, name, sql, shared, created_at, updated_at
		FROM saved_queries WHERE id = $1::uuid AND database_id = $2::uuid`, id, dbID).
		Scan(&q.ID, &q.Owner, &q.Name, &q.SQL, &q.Shared, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || isBadUUID(err) {
		return q, errSavedNotFound
	}
	return q, err
}

// DeleteSavedQuery removes one query.
func (s *Store) DeleteSavedQuery(ctx context.Context, dbID, id string) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM saved_queries WHERE id = $1::uuid AND database_id = $2::uuid`, id, dbID)
	return err
}

func isBadUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

// savedErr answers a saved-query failure.
func (a *API) savedErr(w http.ResponseWriter, err error) {
	var invalid ErrInvalidDatabase
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, errSavedNotFound):
		writeErr(w, http.StatusNotFound, "no such saved query, or it is not yours")
	default:
		a.fail(w, "saved query", err)
	}
}

// GET /api/v1/databases/{id}/saved-queries
func (a *API) getSavedQueries(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	list, err := a.store.SavedQueries(r.Context(), db.ID, email)
	if err != nil {
		a.savedErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queries": list})
}

type savedInput struct {
	Name   *string `json:"name"`
	SQL    *string `json:"sql"`
	Shared *bool   `json:"shared"`
}

// POST /api/v1/databases/{id}/saved-queries
func (a *API) postSavedQuery(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	var in savedInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q := SavedQuery{Name: deref(in.Name), SQL: deref(in.SQL), Shared: in.Shared != nil && *in.Shared}
	q, err := a.store.SaveQuery(r.Context(), db.ID, email, q)
	if err != nil {
		a.savedErr(w, err)
		return
	}
	a.auditDB(r.Context(), "db.query_saved", "info", email, db.Hostname,
		fmt.Sprintf("Saved query %q on %s%s.", q.Name, db.Name, sharedSuffix(q.Shared)))
	writeJSON(w, http.StatusCreated, q)
}

// PUT /api/v1/databases/{id}/saved-queries/{q}
func (a *API) putSavedQuery(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	var in savedInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, err := a.store.SavedQuery(r.Context(), db.ID, r.PathValue("q"))
	if err == nil && cur.Owner != email {
		err = errSavedNotFound
	}
	if err != nil {
		a.savedErr(w, err)
		return
	}
	next := cur
	if in.Name != nil {
		next.Name = *in.Name
	}
	if in.SQL != nil {
		next.SQL = *in.SQL
	}
	if in.Shared != nil {
		next.Shared = *in.Shared
	}
	next, err = a.store.UpdateSavedQuery(r.Context(), db.ID, email, next)
	if err != nil {
		a.savedErr(w, err)
		return
	}
	what := "Changed"
	switch {
	case next.Shared && !cur.Shared:
		what = "Shared"
	case !next.Shared && cur.Shared:
		what = "Stopped sharing"
	}
	a.auditDB(r.Context(), "db.query_saved", "info", email, db.Hostname,
		fmt.Sprintf("%s saved query %q on %s.", what, next.Name, db.Name))
	writeJSON(w, http.StatusOK, next)
}

// DELETE /api/v1/databases/{id}/saved-queries/{q}
func (a *API) deleteSavedQuery(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	cur, err := a.store.SavedQuery(r.Context(), db.ID, r.PathValue("q"))
	if err == nil && cur.Owner != email && !(cur.Shared && a.isAdmin(r)) {
		err = errSavedNotFound
	}
	if err != nil {
		a.savedErr(w, err)
		return
	}
	if err := a.store.DeleteSavedQuery(r.Context(), db.ID, cur.ID); err != nil {
		a.savedErr(w, err)
		return
	}
	by := ""
	if cur.Owner != email {
		by = " (saved by " + cur.Owner + ")"
	}
	a.auditDB(r.Context(), "db.query_removed", "info", email, db.Hostname,
		fmt.Sprintf("Removed saved query %q on %s%s.", cur.Name, db.Name, by))
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

func (a *API) isAdmin(r *http.Request) bool {
	sess, ok := a.authenticate(r)
	return ok && (sess.Role == "admin" || sess.Role == "owner")
}

func sharedSuffix(shared bool) string {
	if shared {
		return ", shared with everyone who can open it"
	}
	return ""
}
