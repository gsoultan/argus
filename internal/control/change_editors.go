package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/jackc/pgx/v5"
)

/*
Who may approve a change on a database.

A database whose changes need approval (WritesNeedApproval) names who decides
them in DBPolicy.WriteApprovers:

	"approvers" (or unset)  admins, owners and approvers, as for every request
	"editors"               those, and anyone who may change this database:
	                        Write, Owner or DBA on it now, given directly or
	                        through a team

An editor decides only db:statement requests for that database, never their
own (DecideRequest refuses self-approval for everyone), and their access must
stand on its own: a grant that needs approval to open a session counts only
while that approval is live, which is what DatabaseAccess already reports.
*/

const (
	// ApproversDefault is the admins, owners and approvers of the organization.
	ApproversDefault = "approvers"
	// ApproversEditors adds the database's editors.
	ApproversEditors = "editors"
)

// ChangeApprovers is who decides d's change requests.
func (d Database) ChangeApprovers() string {
	if d.Policy.WriteApprovers == ApproversEditors {
		return ApproversEditors
	}
	return ApproversDefault
}

// EditorsApprove is whether d's editors may decide its change requests.
func (d Database) EditorsApprove() bool {
	return d.WritesNeedApproval() && d.ChangeApprovers() == ApproversEditors
}

// isEditor is whether level may change a database.
func isEditor(level string) bool {
	return tunnel.LevelRank(level) >= tunnel.LevelRank(tunnel.LevelWrite)
}

// DatabaseEditors lists, in order, everyone who may change database dbID now.
func (s *Store) DatabaseEditors(ctx context.Context, dbID string) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT DISTINCT lower(g.user_email) FROM database_assignments g
		WHERE g.database_id = $1::uuid AND g.user_email IS NOT NULL
		UNION
		SELECT DISTINCT lower(m.user_email) FROM database_assignments g
		JOIN team_members m ON m.team_id = g.team_id
		WHERE g.database_id = $1::uuid`, dbID)
	if err != nil {
		return nil, fmt.Errorf("database editors: %w", err)
	}
	var people []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return nil, err
		}
		people = append(people, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range people {
		acc, err := s.DatabaseAccess(ctx, e, dbID)
		if err != nil {
			return nil, err
		}
		if isEditor(acc.Level) {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out, nil
}

// EditedDatabases lists the databases whose change requests email may decide
// as an editor.
func (s *Store) EditedDatabases(ctx context.Context, email string) ([]string, error) {
	email = strings.ToLower(email)
	rows, err := s.db(ctx).Query(ctx, `
		SELECT DISTINCT g.database_id::text FROM database_assignments g
		JOIN databases d ON d.id = g.database_id
		WHERE d.policy->>'writeApprovers' = 'editors'
		  AND (lower(g.user_email) = $1
		       OR g.team_id IN (SELECT team_id FROM team_members WHERE lower(user_email) = $1))`, email)
	if err != nil {
		return nil, fmt.Errorf("edited databases: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []string{}
	for _, id := range ids {
		db, err := s.Database(ctx, id)
		if err != nil {
			if errors.Is(err, ErrDatabaseNotFound) {
				continue
			}
			return nil, err
		}
		if !db.EditorsApprove() {
			continue
		}
		acc, err := s.DatabaseAccess(ctx, email, id)
		if err != nil {
			return nil, err
		}
		if isEditor(acc.Level) {
			out = append(out, id)
		}
	}
	return out, nil
}

// requestDatabase is the principal of request id and the database it is on,
// if any.
func (s *Store) requestDatabase(ctx context.Context, id string) (principal, dbID string, err error) {
	if !uuidRE.MatchString(id) {
		return "", "", fmt.Errorf("request not found")
	}
	var db *string
	err = s.db(ctx).QueryRow(ctx,
		`SELECT principal, database_id::text FROM access_requests WHERE id = $1::uuid`, id).Scan(&principal, &db)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("request not found")
	}
	if err != nil {
		return "", "", err
	}
	if db != nil {
		dbID = *db
	}
	return principal, dbID, nil
}

// decidesAsEditor says whether email, whose role does not decide requests,
// may decide request id as an editor of its database, and names the database.
func (a *API) decidesAsEditor(ctx context.Context, email, id string) (Database, bool, error) {
	principal, dbID, err := a.store.requestDatabase(ctx, id)
	if err != nil || principal != statementPrincipal || dbID == "" {
		return Database{}, false, err
	}
	db, err := a.store.Database(ctx, dbID)
	if err != nil {
		if errors.Is(err, ErrDatabaseNotFound) {
			return Database{}, false, nil
		}
		return Database{}, false, err
	}
	if !db.EditorsApprove() {
		return db, false, nil
	}
	acc, err := a.store.DatabaseAccess(ctx, email, db.ID)
	if err != nil {
		return db, false, err
	}
	return db, isEditor(acc.Level), nil
}

// alertEditorsWaiting tells db's editors, other than the asker, that a change
// waits for one of them. Deciders are told as for every request.
func (a *API) alertEditorsWaiting(ctx context.Context, db Database, req AccessRequest) {
	if !db.EditorsApprove() {
		return
	}
	editors, err := a.store.DatabaseEditors(ctx, db.ID)
	if err != nil {
		a.log.Error("could not list a database's editors", "database", db.ID, "error", err)
		return
	}
	for _, e := range editors {
		if strings.EqualFold(e, req.RequesterEmail) {
			continue
		}
		// A decider of the organization is told already, as a decider.
		if u, err := a.store.UserRole(ctx, e); err == nil && decidesRequests(u) {
			continue
		}
		a.raiseAlert(ctx, ConsoleAlert{
			Kind: AlertApproval, Severity: "warning", Audience: AudiencePerson, Recipient: e,
			Actor:  req.RequesterEmail,
			Title:  req.RequesterEmail + " asks you to review a change on " + db.Name,
			Detail: req.Justification + " You can approve it as an editor of " + db.Name + ".",
			Link:   "/approvals",
		})
	}
}

// UserRole is the organization role of the account email, or "" when there
// is none.
func (s *Store) UserRole(ctx context.Context, email string) (string, error) {
	var role string
	err := s.db(ctx).QueryRow(ctx, `SELECT role FROM users WHERE lower(email) = lower($1)`, email).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// approversWords is who decides db's changes, in words.
func approversWords(db Database) string {
	if db.ChangeApprovers() == ApproversEditors {
		return "admins, approvers or another editor of " + db.Name
	}
	return "admins and approvers"
}

// auditApprovalChange records a change to whether db's changes wait, or who
// decides them, apart from the routine settings entry: turning approval off
// is a loosening, and is recorded as a warning.
func (a *API) auditApprovalChange(ctx context.Context, actor string, before, after Database) {
	was, now := before.WritesNeedApproval(), after.WritesNeedApproval()
	if was != now {
		if now {
			a.auditDB(ctx, "db.write_approval_on", "notice", actor, after.Hostname,
				"Changes on "+after.Name+" now need a second person's approval ("+approversWords(after)+").")
		} else {
			a.auditDB(ctx, "db.write_approval_off", "warning", actor, after.Hostname,
				"Changes on "+after.Name+" no longer need approval: they run as soon as someone with Write sends them.")
		}
		return
	}
	if now && before.ChangeApprovers() != after.ChangeApprovers() {
		a.auditDB(ctx, "db.write_approvers_changed", "notice", actor, after.Hostname,
			"Changes on "+after.Name+" are now decided by "+approversWords(after)+".")
	}
}

// ChangeApproval is how a database's changes are approved, for the console:
// the Safety card, Give access, the prompt before a change and the asker's
// wait.
type ChangeApproval struct {
	// Needed is whether a change waits for someone else.
	Needed bool `json:"needed"`
	// Default is whether it would by its label alone.
	Default bool `json:"default"`
	// Approvers is "approvers" or "editors".
	Approvers string `json:"approvers"`
	// Editors are who may change it now, when Approvers is "editors".
	Editors []string `json:"editors"`
}

// GET /api/v1/databases/{id}/approval
func (a *API) getChangeApproval(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	out := ChangeApproval{Needed: db.WritesNeedApproval(), Default: db.Label == "production",
		Approvers: db.ChangeApprovers(), Editors: []string{}}
	if out.Approvers == ApproversEditors {
		editors, err := a.store.DatabaseEditors(r.Context(), db.ID)
		if err != nil {
			a.fail(w, "approval", err)
			return
		}
		out.Editors = editors
	}
	writeJSON(w, http.StatusOK, out)
}

// hidePreviews takes the affected rows out of the change requests in reqs
// whose database the reader cannot read: they decide on the counts, and the
// values stay with the people who could see them anyway.
func (a *API) hidePreviews(ctx context.Context, email, role string, reqs []AccessRequest) error {
	levels := map[string]string{}
	for i := range reqs {
		st := reqs[i].Statement
		if st == nil || len(st.Summary) == 0 || !strings.Contains(string(st.Summary), `"preview"`) {
			continue
		}
		level, seen := levels[st.DatabaseID]
		if !seen {
			var err error
			if level, err = a.dbLevelOf(ctx, email, role, st.DatabaseID); err != nil {
				return err
			}
			levels[st.DatabaseID] = level
		}
		if level != "" {
			continue
		}
		var sum map[string]json.RawMessage
		if json.Unmarshal(st.Summary, &sum) != nil {
			st.Summary = json.RawMessage(`{}`)
			continue
		}
		delete(sum, "preview")
		b, _ := json.Marshal(sum)
		st.Summary = b
	}
	return nil
}
