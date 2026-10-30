package control

import (
	"errors"
	"net/http"
	"strings"
)

// Teams are managed by an administrator or owner and read by anyone who sees
// the whole fleet: an auditor asking "who could reach pay-01" needs the teams
// as much as the per-person assignments.

func (a *API) getTeams(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing teams requires the admin, owner or auditor role")
		return
	}
	teams, err := a.store.Teams(r.Context())
	if err != nil {
		a.failTeam(w, "teams", err)
		return
	}
	writeJSON(w, http.StatusOK, teams)
}

func (a *API) postTeam(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "making a team", "admin", "owner") {
		return
	}
	var in struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := a.store.CreateTeam(r.Context(), in.Name, in.Members, sess.Email)
	if err != nil {
		a.failTeam(w, "create team", err)
		return
	}
	team, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	detail := "Made the team " + team.Name + " (" + id + ")"
	if len(team.Members) > 0 {
		detail += " with " + strings.Join(team.Members, ", ")
	}
	a.auditAsset(r, AuditEvent{
		Action: "team.created", Severity: "notice", ActorEmail: sess.Email,
		Target: team.Name, Detail: detail + ".",
	})
	writeJSON(w, http.StatusOK, team)
}

func (a *API) patchTeam(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "renaming a team", "admin", "owner") {
		return
	}
	id := r.PathValue("id")
	before, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.RenameTeam(r.Context(), id, in.Name); err != nil {
		a.failTeam(w, "rename team", err)
		return
	}
	team, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "team.renamed", Severity: "info", ActorEmail: sess.Email,
		Target: team.Name, Detail: "Renamed the team " + before.Name + " to " + team.Name + " (" + id + ").",
	})
	writeJSON(w, http.StatusOK, team)
}

// deleteTeam removes a team. The audit entry lists the people and servers it
// held, because after the cascade nothing else in the database does.
func (a *API) deleteTeam(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "deleting a team", "admin", "owner") {
		return
	}
	id := r.PathValue("id")
	team, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	if err := a.store.DeleteTeam(r.Context(), id); err != nil {
		a.failTeam(w, "delete team", err)
		return
	}
	hosts := make([]string, 0, len(team.Servers))
	for _, s := range team.Servers {
		hosts = append(hosts, s.Hostname)
	}
	detail := "Deleted the team " + team.Name + " (" + id + ")."
	if len(team.Members) > 0 {
		detail += " Members: " + strings.Join(team.Members, ", ") + "."
	}
	if len(hosts) > 0 {
		detail += " It no longer gives anyone access to " + strings.Join(hosts, ", ") + "."
	}
	a.auditAsset(r, AuditEvent{
		Action: "team.deleted", Severity: "notice", ActorEmail: sess.Email,
		Target: team.Name, Detail: detail,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *API) putTeamMember(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing who is in a team", "admin", "owner") {
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if err := a.store.AddTeamMember(r.Context(), id, in.Email, sess.Email); err != nil {
		a.failTeam(w, "add team member", err)
		return
	}
	team, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	a.auditAsset(r, AuditEvent{
		Action: "team.member_added", Severity: "notice", ActorEmail: sess.Email,
		Target: team.Name,
		Detail: email + " joined the team " + team.Name + " (" + id + ")" + teamReach(team) + ".",
	})
	writeJSON(w, http.StatusOK, team)
}

func (a *API) deleteTeamMember(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "changing who is in a team", "admin", "owner") {
		return
	}
	id := r.PathValue("id")
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if err := a.store.RemoveTeamMember(r.Context(), id, email); err != nil {
		a.failTeam(w, "remove team member", err)
		return
	}
	team, err := a.store.TeamByID(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "team.member_removed", Severity: "notice", ActorEmail: sess.Email,
		Target: team.Name,
		Detail: email + " left the team " + team.Name + " (" + id + ")" + teamReach(team) + ".",
	})
	writeJSON(w, http.StatusOK, team)
}

// teamReach names the servers a membership change gives or takes away, so the
// audit entry answers "what could they reach" without a second lookup.
func teamReach(t Team) string {
	if len(t.Servers) == 0 {
		return ", which gives access to no servers"
	}
	hosts := make([]string, 0, len(t.Servers))
	for _, s := range t.Servers {
		hosts = append(hosts, s.Hostname+" as "+describePrincipals(s.Principals))
	}
	return ", which gives access to " + strings.Join(hosts, "; ")
}

func (a *API) getTeamAssignments(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing which teams can reach a host requires the admin, owner or auditor role")
		return
	}
	list, err := a.store.TeamAssignments(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failTeam(w, "team assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// putTeamAssignment gives a team access to an asset, or takes it away when
// the principal list is empty.
func (a *API) putTeamAssignment(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "giving a team a server", "admin", "owner") {
		return
	}
	id := r.PathValue("id")
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	var in struct {
		TeamID     string   `json:"teamId"`
		Principals []string `json:"principals"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	team, err := a.store.TeamByID(r.Context(), in.TeamID)
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	if err := a.store.SetTeamAssignment(r.Context(), id, in.TeamID, in.Principals, sess.Email); err != nil {
		a.failTeam(w, "assign team", err)
		return
	}
	action, detail := "asset.team_assigned", "Everyone in the team "+team.Name+" ("+team.ID+
		") may now open a session on "+asset.Hostname+" as "+describePrincipals(dedupe(in.Principals))+"."
	if len(dedupe(in.Principals)) == 0 {
		action, detail = "asset.team_unassigned", "The team "+team.Name+" ("+team.ID+
			") no longer has access to "+asset.Hostname+"."
	}
	a.auditAsset(r, AuditEvent{
		Action: action, Severity: "notice", ActorEmail: sess.Email,
		Target: asset.Hostname, Detail: detail,
	})
	list, err := a.store.TeamAssignments(r.Context(), id)
	if err != nil {
		a.failTeam(w, "team assignments", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) deleteTeamAssignment(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !a.privileged(w, sess, "taking a server from a team", "admin", "owner") {
		return
	}
	id := r.PathValue("id")
	asset, err := a.store.AssetByID(r.Context(), id)
	if err != nil {
		a.failAsset(w, "asset", err)
		return
	}
	team, err := a.store.TeamByID(r.Context(), r.PathValue("team"))
	if err != nil {
		a.failTeam(w, "team", err)
		return
	}
	if err := a.store.RemoveTeamAssignment(r.Context(), id, team.ID); err != nil {
		a.failTeam(w, "remove team assignment", err)
		return
	}
	a.auditAsset(r, AuditEvent{
		Action: "asset.team_unassigned", Severity: "notice", ActorEmail: sess.Email,
		Target: asset.Hostname,
		Detail: "The team " + team.Name + " (" + team.ID + ") no longer has access to " + asset.Hostname + ".",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (a *API) failTeam(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, ErrTeamNotFound) {
		writeErr(w, http.StatusNotFound, "no such team")
		return
	}
	a.failAsset(w, what, err)
}
