package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

/*
Monitoring groups, and who sees a check (migration 104).

Admins, owners and auditors see every check, as before. Anyone else sees the
checks they were given, or a team of theirs was given, either one check at a
time or a whole group at once. can_operate adds "check now" and pause; adding,
changing and removing checks stays with admins and owners.

Someone outside the fleet roles sees a check limited: its name, address,
state, uptime and outages, but not what it sends (body, header names), where
it alerts (Telegram) or which servers confirm it, since those say things about
the fleet that the grant does not.

	GET    /api/v1/check-groups               every group, with how many checks are in it
	POST   /api/v1/check-groups               add one
	PUT    /api/v1/check-groups/{id}          rename one
	DELETE /api/v1/check-groups/{id}          remove one (its checks stay, ungrouped)
	GET    /api/v1/check-groups/{id}/viewers  who sees every check in it
	PUT    /api/v1/check-groups/{id}/viewers  replace that list
	PUT    /api/v1/checks/{id}/group          move a check into a group, or out
	GET    /api/v1/checks/{id}/viewers        who sees one check, besides through its group
	PUT    /api/v1/checks/{id}/viewers        replace that list
	POST   /api/v1/websites/{id}/pause        pause or resume a check

Changing who sees what is for admins and owners, and is in the audit log.
*/

// CheckGroup is a named set of checks, shown together on Monitoring.
type CheckGroup struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Checks int    `json:"checks"`
}

// CheckViewer is one person or team given a check or a group.
type CheckViewer struct {
	// Email is a person; TeamID and TeamName a team. Exactly one is set.
	Email      string `json:"email,omitempty"`
	TeamID     string `json:"teamId,omitempty"`
	TeamName   string `json:"teamName,omitempty"`
	CanOperate bool   `json:"canOperate"`
}

// ErrNoCheckGroup is a group that does not exist in this organization.
var ErrNoCheckGroup = errors.New("no such group")

// ErrInvalidCheckAccess is a request the console should have refused.
type ErrInvalidCheckAccess struct{ Reason string }

func (e ErrInvalidCheckAccess) Error() string { return e.Reason }

// CheckGroups lists every group by name.
func (s *Store) CheckGroups(ctx context.Context) ([]CheckGroup, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT g.id::text, g.name, (SELECT count(*) FROM checks c WHERE c.group_id = g.id)
		FROM check_groups g ORDER BY lower(g.name), g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckGroup{}
	for rows.Next() {
		var g CheckGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Checks); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func cleanGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 60 {
		return "", ErrInvalidCheckAccess{"A group's name is 1 to 60 characters."}
	}
	return name, nil
}

func groupNameErr(err error) error {
	if err != nil && isUniqueViolation(err) {
		return ErrInvalidCheckAccess{"There is already a group with that name."}
	}
	return err
}

// CreateCheckGroup adds a group.
func (s *Store) CreateCheckGroup(ctx context.Context, name, by string) (CheckGroup, error) {
	name, err := cleanGroupName(name)
	if err != nil {
		return CheckGroup{}, err
	}
	g := CheckGroup{Name: name}
	err = s.db(ctx).QueryRow(ctx, `INSERT INTO check_groups (name, created_by) VALUES ($1, $2) RETURNING id::text`,
		name, by).Scan(&g.ID)
	return g, groupNameErr(err)
}

// RenameCheckGroup renames a group and returns its old name.
func (s *Store) RenameCheckGroup(ctx context.Context, id, name string) (string, error) {
	name, err := cleanGroupName(name)
	if err != nil {
		return "", err
	}
	var old string
	err = s.db(ctx).QueryRow(ctx, `UPDATE check_groups g SET name = $2 FROM check_groups o
		WHERE g.id::text = $1 AND o.id = g.id RETURNING o.name`, id, name).Scan(&old)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoCheckGroup
	}
	return old, groupNameErr(err)
}

// DeleteCheckGroup removes a group, and who was given it; its checks stay,
// in no group.
func (s *Store) DeleteCheckGroup(ctx context.Context, id string) (string, error) {
	var name string
	err := s.db(ctx).QueryRow(ctx, `DELETE FROM check_groups WHERE id::text = $1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoCheckGroup
	}
	return name, err
}

// SetCheckGroup moves a check into a group, or out of every group when
// groupID is empty.
func (s *Store) SetCheckGroup(ctx context.Context, checkID, groupID string) error {
	var g any
	if groupID != "" {
		var ok bool
		if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM check_groups WHERE id::text = $1)`, groupID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrNoCheckGroup
		}
		g = groupID
	}
	tag, err := s.db(ctx).Exec(ctx, `UPDATE checks SET group_id = $2::uuid, updated_at = now() WHERE id::text = $1`, checkID, g)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoWebsite
	}
	return nil
}

// CheckViewers is who was given a check (target "check") or a group
// ("group"), people first.
func (s *Store) CheckViewers(ctx context.Context, target, id string) ([]CheckViewer, error) {
	col, err := viewerColumn(target)
	if err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT COALESCE(v.user_email, ''), COALESCE(v.team_id::text, ''), COALESCE(t.name, ''), v.can_operate
		FROM check_viewers v LEFT JOIN teams t ON t.id = v.team_id
		WHERE v.`+col+`::text = $1
		ORDER BY v.user_email IS NULL, v.user_email, lower(t.name)`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckViewer{}
	for rows.Next() {
		var v CheckViewer
		if err := rows.Scan(&v.Email, &v.TeamID, &v.TeamName, &v.CanOperate); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func viewerColumn(target string) (string, error) {
	switch target {
	case "check":
		return "check_id", nil
	case "group":
		return "group_id", nil
	}
	return "", fmt.Errorf("unknown check access target %q", target)
}

// SetCheckViewers replaces who was given a check or a group, and returns
// the list as it was and as it is now.
func (s *Store) SetCheckViewers(ctx context.Context, target, id string, in []CheckViewer, by string) (before, after []CheckViewer, err error) {
	col, err := viewerColumn(target)
	if err != nil {
		return nil, nil, err
	}
	if len(in) > 200 {
		return nil, nil, ErrInvalidCheckAccess{"At most 200 people and teams can be given one check or group."}
	}
	table := "checks"
	missing := error(ErrNoWebsite)
	if target == "group" {
		table, missing = "check_groups", ErrNoCheckGroup
	}
	var ok bool
	if err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id::text = $1)`, id).Scan(&ok); err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, missing
	}
	before, err = s.CheckViewers(ctx, target, id)
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM check_viewers WHERE `+col+`::text = $1`, id); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, v := range in {
		email := strings.ToLower(strings.TrimSpace(v.Email))
		team := strings.TrimSpace(v.TeamID)
		if (email == "") == (team == "") {
			return nil, nil, ErrInvalidCheckAccess{"Each entry is one person or one team."}
		}
		key := email + "\x00" + team
		if seen[key] {
			continue
		}
		seen[key] = true
		if email != "" {
			var known bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`, email).Scan(&known); err != nil {
				return nil, nil, err
			}
			if !known {
				return nil, nil, ErrInvalidCheckAccess{email + " is not a person in this organization."}
			}
			_, err = tx.Exec(ctx, `INSERT INTO check_viewers (`+col+`, user_email, can_operate, granted_by) VALUES ($1::uuid, $2, $3, $4)`,
				id, email, v.CanOperate, by)
		} else {
			var known bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM teams WHERE id::text = $1)`, team).Scan(&known); err != nil {
				return nil, nil, err
			}
			if !known {
				return nil, nil, ErrInvalidCheckAccess{"One of the teams no longer exists."}
			}
			_, err = tx.Exec(ctx, `INSERT INTO check_viewers (`+col+`, team_id, can_operate, granted_by) VALUES ($1::uuid, $2::uuid, $3, $4)`,
				id, team, v.CanOperate, by)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	after, err = s.CheckViewers(ctx, target, id)
	return before, after, err
}

// visibleChecksSQL is every check email was given, directly or through a
// team, one check at a time or through its group, with whether any of those
// grants lets them operate it. $1 is the lower-cased email.
const visibleChecksSQL = `SELECT c.id::text, bool_or(v.can_operate)
	FROM checks c
	JOIN check_viewers v ON v.check_id = c.id OR (v.group_id IS NOT NULL AND v.group_id = c.group_id)
	WHERE v.user_email = $1 OR v.team_id IN (SELECT team_id FROM team_members WHERE user_email = $1)
	GROUP BY c.id`

// VisibleChecks is every check email was given, mapped to whether they may
// operate it.
func (s *Store) VisibleChecks(ctx context.Context, email string) (map[string]bool, error) {
	rows, err := s.db(ctx).Query(ctx, visibleChecksSQL, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var op bool
		if err := rows.Scan(&id, &op); err != nil {
			return nil, err
		}
		out[id] = op
	}
	return out, rows.Err()
}

// SeesSomeCheck is whether email was given any check, for the menu.
func (s *Store) SeesSomeCheck(ctx context.Context, email string) (bool, error) {
	var ok bool
	err := s.db(ctx).QueryRow(ctx, `SELECT EXISTS (`+visibleChecksSQL+`)`, strings.ToLower(strings.TrimSpace(email))).Scan(&ok)
	return ok, err
}

// CheckWatchers is every person given a check, through a team or a group
// included, for its alerts.
func (s *Store) CheckWatchers(ctx context.Context, checkID string) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT DISTINCT e FROM (
		SELECT v.user_email AS e FROM check_viewers v JOIN checks c ON c.id::text = $1
		  WHERE v.user_email IS NOT NULL AND (v.check_id = c.id OR v.group_id = c.group_id)
		UNION
		SELECT m.user_email FROM check_viewers v JOIN checks c ON c.id::text = $1
		  JOIN team_members m ON m.team_id = v.team_id
		  WHERE v.check_id = c.id OR v.group_id = c.group_id
	) w ORDER BY e`, checkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetWebsitePaused pauses or resumes a check. A resumed check is due now.
func (s *Store) SetWebsitePaused(ctx context.Context, id string, paused bool) (WebsiteCheck, error) {
	c, err := scanWebsite(s.db(ctx).QueryRow(ctx, `UPDATE checks SET paused = $2, updated_at = now(),
		next_at = CASE WHEN $2 THEN next_at ELSE now() END
		WHERE id::text = $1 AND kind = 'website' RETURNING `+websiteCols, id, paused))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNoWebsite
	}
	return c, err
}

// checkAccessItems is the Monitoring section of what a person may reach.
func checkAccessItems(ctx context.Context, s *Store, email string) ([]AccessItem, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT c.id::text, c.name, COALESCE(g.name, ''), bool_or(v.can_operate),
		bool_or(v.user_email IS NOT NULL), COALESCE(array_agg(DISTINCT t.name) FILTER (WHERE t.name IS NOT NULL), '{}')
		FROM checks c
		LEFT JOIN check_groups g ON g.id = c.group_id
		JOIN check_viewers v ON v.check_id = c.id OR (v.group_id IS NOT NULL AND v.group_id = c.group_id)
		LEFT JOIN teams t ON t.id = v.team_id
		WHERE v.user_email = $1 OR v.team_id IN (SELECT team_id FROM team_members WHERE user_email = $1)
		GROUP BY c.id, c.name, g.name ORDER BY lower(c.name)`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessItem
	for rows.Next() {
		var it AccessItem
		var op bool
		if err := rows.Scan(&it.ID, &it.Name, &it.Where, &op, &it.Own, &it.Teams); err != nil {
			return nil, err
		}
		it.What = "see it"
		if op {
			it.What = "see it, check it now and pause it"
		}
		sort.Strings(it.Teams)
		out = append(out, it)
	}
	return out, rows.Err()
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// limitWebsite leaves out what a check sends, where it alerts and which
// servers confirm it, for someone outside the fleet roles.
func limitWebsite(c *WebsiteCheck) {
	c.Limited = true
	c.Body = ""
	c.HeaderNames = []string{}
	c.Telegram, c.TelegramBot, c.TelegramChats = "", "", []string{}
	c.ConfirmFrom, c.ConfirmHost = []string{}, []string{}
	c.CreatedBy = ""
}

func (a *API) checkAccessErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidCheckAccess
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, ErrNoCheckGroup):
		writeErr(w, http.StatusNotFound, "no such group")
	default:
		a.websiteErr(w, what, err)
	}
}

func (a *API) auditCheckAccess(r *http.Request, action, actor, target, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: action, Severity: "notice", ActorEmail: actor,
		Target: target, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

func (a *API) getCheckGroups(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	list, err := a.store.CheckGroups(r.Context())
	if err != nil {
		a.fail(w, "check groups", err)
		return
	}
	if !seesWholeFleet(sess.Role) {
		// Only the groups of checks they can see, counting only those.
		seen, err := a.store.VisibleChecks(r.Context(), sess.Email)
		if err != nil {
			a.fail(w, "check groups", err)
			return
		}
		checks, err := a.store.WebsiteChecks(r.Context())
		if err != nil {
			a.fail(w, "check groups", err)
			return
		}
		count := map[string]int{}
		for _, c := range checks {
			if _, ok := seen[c.ID]; ok && c.GroupID != "" {
				count[c.GroupID]++
			}
		}
		mine := []CheckGroup{}
		for _, g := range list {
			if count[g.ID] > 0 {
				g.Checks = count[g.ID]
				mine = append(mine, g)
			}
		}
		list = mine
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) postCheckGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	g, err := a.store.CreateCheckGroup(r.Context(), in.Name, actor)
	if err != nil {
		a.checkAccessErr(w, "add group", err)
		return
	}
	a.auditCheckAccess(r, "check.group.add", actor, "monitoring group "+g.Name, "Added the Monitoring group "+g.Name+".")
	writeJSON(w, http.StatusCreated, g)
}

func (a *API) putCheckGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	old, err := a.store.RenameCheckGroup(r.Context(), r.PathValue("id"), in.Name)
	if err != nil {
		a.checkAccessErr(w, "rename group", err)
		return
	}
	name := strings.TrimSpace(in.Name)
	a.auditCheckAccess(r, "check.group.rename", actor, "monitoring group "+name, "Renamed the Monitoring group "+old+" to "+name+".")
	writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id"), "name": name})
}

func (a *API) deleteCheckGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	name, err := a.store.DeleteCheckGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		a.checkAccessErr(w, "remove group", err)
		return
	}
	a.auditCheckAccess(r, "check.group.remove", actor, "monitoring group "+name,
		"Removed the Monitoring group "+name+". Its checks stay, in no group, and whoever was given the group no longer sees them through it.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putCheckGroupOf(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		GroupID string `json:"groupId"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.store.WebsiteCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		a.websiteErr(w, "move check", err)
		return
	}
	if err := a.store.SetCheckGroup(r.Context(), c.ID, in.GroupID); err != nil {
		a.checkAccessErr(w, "move check", err)
		return
	}
	detail := "Moved the check " + c.Name + " out of its group."
	if in.GroupID != "" {
		groups, _ := a.store.CheckGroups(r.Context())
		for _, g := range groups {
			if g.ID == in.GroupID {
				detail = "Moved the check " + c.Name + " into the group " + g.Name + "."
			}
		}
	}
	a.auditCheckAccess(r, "check.group.move", actor, "website "+c.Name, detail+" Whoever was given a group sees the checks in it.")
	w.WriteHeader(http.StatusNoContent)
}

func viewerWords(list []CheckViewer) string {
	if len(list) == 0 {
		return "nobody besides admins, owners and auditors"
	}
	parts := make([]string, 0, len(list))
	for _, v := range list {
		who := v.Email
		if v.TeamID != "" {
			who = "team " + v.TeamName
		}
		if v.CanOperate {
			who += " (can check now and pause)"
		}
		parts = append(parts, who)
	}
	return strings.Join(parts, ", ")
}

func (a *API) getViewers(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.websiteAdmin(w, r); !ok {
			return
		}
		list, err := a.store.CheckViewers(r.Context(), target, r.PathValue("id"))
		if err != nil {
			a.checkAccessErr(w, "who sees it", err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

func (a *API) putViewers(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.websiteAdmin(w, r)
		if !ok {
			return
		}
		var in []CheckViewer
		if err := decode(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		id := r.PathValue("id")
		var what string
		if target == "check" {
			c, err := a.store.WebsiteCheck(r.Context(), id)
			if err != nil {
				a.websiteErr(w, "who sees it", err)
				return
			}
			what = "website " + c.Name
		}
		before, after, err := a.store.SetCheckViewers(r.Context(), target, id, in, actor)
		if err != nil {
			a.checkAccessErr(w, "who sees it", err)
			return
		}
		if target == "group" {
			groups, _ := a.store.CheckGroups(r.Context())
			for _, g := range groups {
				if g.ID == id {
					what = "monitoring group " + g.Name
				}
			}
		}
		a.auditCheckAccess(r, "check.viewers", actor, what,
			"Changed who sees the "+what+" from "+viewerWords(before)+" to "+viewerWords(after)+".")
		writeJSON(w, http.StatusOK, after)
	}
}

func (a *API) postWebsitePause(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id := r.PathValue("id")
	if !a.operatesCheck(w, r, sess.Role, sess.Email, id) {
		return
	}
	var in struct {
		Paused bool `json:"paused"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.store.SetWebsitePaused(r.Context(), id, in.Paused)
	if err != nil {
		a.websiteErr(w, "pause website check", err)
		return
	}
	verb := "Resumed"
	if in.Paused {
		verb = "Paused"
	}
	a.auditWebsite(r, "website.pause", "notice", sess.Email, c, verb+" the website check of "+c.URL+".")
	w.WriteHeader(http.StatusNoContent)
}

// operatesCheck is whether this person may check id now and pause it: an
// admin or owner, or someone given it with can_operate. Someone who cannot
// see it at all is told it does not exist.
func (a *API) operatesCheck(w http.ResponseWriter, r *http.Request, role, email, id string) bool {
	if role == "admin" || role == "owner" {
		return true
	}
	seen, err := a.store.VisibleChecks(r.Context(), email)
	if err != nil {
		a.fail(w, "website check", err)
		return false
	}
	op, ok := seen[id]
	switch {
	case !ok && !seesWholeFleet(role):
		writeErr(w, http.StatusNotFound, "no such website check")
		return false
	case !op:
		writeErr(w, http.StatusForbidden, "you can see this check but not check it now or pause it")
		return false
	}
	return true
}

// seesChecks is whether Monitoring has anything for this person.
func (a *API) seesChecks(r *http.Request, role, email string) bool {
	if seesWholeFleet(role) {
		return true
	}
	ok, err := a.store.SeesSomeCheck(r.Context(), email)
	if err != nil {
		a.log.Warn("could not tell whether someone was given a check", "email", email, "error", err)
	}
	return ok
}

// alertCheckWatchers tells everyone given a check, outside the fleet roles
// (who are told already), what al tells the admins, in their own bell.
func (a *API) alertCheckWatchers(ctx context.Context, checkID string, al ConsoleAlert) {
	watchers, err := a.store.CheckWatchers(ctx, checkID)
	if err != nil {
		a.log.Warn("could not list who was given a check", "check", checkID, "error", err)
		return
	}
	for _, e := range watchers {
		role, err := a.store.UserRole(ctx, e)
		if err != nil || role == "" || readsAdminAlerts(role) {
			continue
		}
		p := al
		p.Audience, p.Recipient = AudiencePerson, e
		a.raiseAlert(ctx, p)
	}
}
