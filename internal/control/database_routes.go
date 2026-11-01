package control

import (
	"errors"
	"net/http"
	"strings"
)

/*
Routes for databases. Reading the list is filtered by who may open what;
every write is admin-or-owner and lands in the audit chain, as asset writes
do.
*/

// dbAdmin gates a database write on the admin or owner role.
func (a *API) dbAdmin(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if sess.Role != "admin" && sess.Role != "owner" {
			writeErr(w, http.StatusForbidden, "only an admin can change databases")
			return
		}
		next(w, r, sess.Email)
	}
}

func (a *API) dbErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidDatabase
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, ErrDatabaseNotFound):
		writeErr(w, http.StatusNotFound, "no such database")
	default:
		a.fail(w, what, err)
	}
}

// GET /api/v1/databases: the databases this person may open or ask for,
// with their access on each.
func (a *API) getDatabases(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var (
		list   []Database
		access map[string]DBAccess
		err    error
	)
	if assetID := r.URL.Query().Get("asset"); assetID != "" && (sess.Role == "admin" || sess.Role == "owner" || sess.Role == "auditor") {
		list, err = a.store.Databases(r.Context(), assetID)
		access = map[string]DBAccess{}
	} else {
		list, access, err = a.store.DatabasesFor(r.Context(), sess.Email, sess.Role)
	}
	if err != nil {
		a.fail(w, "databases", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": list, "access": access})
}

// GET /api/v1/databases/{id}
func (a *API) getDatabase(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	acc := DBAccess{}
	if sess.Role == "admin" || sess.Role == "owner" {
		acc.Level = "dba"
	} else {
		acc, err = a.store.DatabaseAccess(r.Context(), sess.Email, db.ID)
		if err != nil {
			a.fail(w, "database access", err)
			return
		}
		if acc.Level == "" && acc.Approvable == "" && sess.Role != "auditor" {
			writeErr(w, http.StatusNotFound, "no such database")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"database": db, "access": acc})
}

// POST /api/v1/databases
func (a *API) postDatabase(w http.ResponseWriter, r *http.Request, actor string) {
	var in Database
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	db, err := a.store.CreateDatabase(r.Context(), in, actor)
	if err != nil {
		a.dbErr(w, "create database", err)
		return
	}
	a.auditDB(r.Context(), "db.added", "notice", actor, db.Hostname,
		"Added database "+db.Name+" ("+db.Label+") on "+db.Hostname+".")
	writeJSON(w, http.StatusCreated, db)
}

// PATCH /api/v1/databases/{id}
func (a *API) patchDatabase(w http.ResponseWriter, r *http.Request, actor string) {
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	var in struct {
		Label     *string   `json:"label"`
		Host      *string   `json:"host"`
		Port      *int      `json:"port"`
		AdminUser *string   `json:"adminUser"`
		Policy    *DBPolicy `json:"policy"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Label != nil {
		db.Label = *in.Label
	}
	if in.Host != nil {
		db.Host = *in.Host
	}
	if in.Port != nil {
		db.Port = *in.Port
	}
	if in.AdminUser != nil {
		db.AdminUser = *in.AdminUser
	}
	if in.Policy != nil {
		db.Policy = *in.Policy
	}
	db, err = a.store.UpdateDatabase(r.Context(), db)
	if err != nil {
		a.dbErr(w, "update database", err)
		return
	}
	a.auditDB(r.Context(), "db.policy_changed", "notice", actor, db.Hostname,
		"Changed settings of database "+db.Name+" on "+db.Hostname+".")
	writeJSON(w, http.StatusOK, db)
}

// DELETE /api/v1/databases/{id}
func (a *API) deleteDatabase(w http.ResponseWriter, r *http.Request, actor string) {
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	if err := a.store.DeleteDatabase(r.Context(), db.ID); err != nil {
		a.dbErr(w, "delete database", err)
		return
	}
	a.auditDB(r.Context(), "db.removed", "warning", actor, db.Hostname,
		"Removed database "+db.Name+" from "+db.Hostname+". Its sessions and recordings are kept.")
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/databases/{id}/assignments
func (a *API) getDBAssignments(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "only an admin can see who may open a database")
		return
	}
	list, err := a.store.DBAssignments(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, "database assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// PUT /api/v1/databases/{id}/assignments
func (a *API) putDBAssignment(w http.ResponseWriter, r *http.Request, actor string) {
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	var in DBAssignment
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.DatabaseID = db.ID
	if err := a.store.SetDBAssignment(r.Context(), in, actor); err != nil {
		a.dbErr(w, "assign database", err)
		return
	}
	who := "person " + strings.ToLower(deref(in.UserEmail))
	if in.TeamID != nil {
		who = "team " + *in.TeamID
	}
	detail := "Gave " + who + " " + in.Level + " on database " + db.Name + " (" + db.Hostname + ")."
	if in.NeedsApproval {
		detail += " Each session needs an approved request."
	}
	a.auditDB(r.Context(), "db.assigned", "notice", actor, db.Hostname, detail)
	list, err := a.store.DBAssignments(r.Context(), db.ID)
	if err != nil {
		a.fail(w, "database assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// DELETE /api/v1/databases/{id}/assignments/{assignment}
func (a *API) deleteDBAssignment(w http.ResponseWriter, r *http.Request, actor string) {
	db, err := a.store.Database(r.Context(), r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	if err := a.store.RemoveDBAssignment(r.Context(), db.ID, r.PathValue("assignment")); err != nil {
		a.dbErr(w, "remove database assignment", err)
		return
	}
	a.auditDB(r.Context(), "db.unassigned", "notice", actor, db.Hostname,
		"Removed an assignment from database "+db.Name+" ("+db.Hostname+").")
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/databases/{id}/requests: ask for a level for a while.
func (a *API) postDatabaseRequest(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Level           string `json:"level"`
		Justification   string `json:"justification"`
		DurationMinutes int    `json:"durationMinutes"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := a.store.CreateDatabaseRequest(r.Context(), sess.Email, r.PathValue("id"), in.Level,
		in.Justification, in.DurationMinutes)
	if err != nil {
		if errors.Is(err, ErrDatabaseNotFound) {
			writeErr(w, http.StatusNotFound, "no such database")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.auditDB(r.Context(), "db.requested", "notice", sess.Email, req.AssetHostnames[0],
		"Asked for "+in.Level+" on a database for "+itoa(in.DurationMinutes)+" minutes: "+in.Justification)
	writeJSON(w, http.StatusCreated, req)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
