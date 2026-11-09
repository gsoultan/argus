package control

import (
	"context"
	"net/http"
	"strings"
)

// POST /api/v1/database-access/plan: what a grant on many databases would
// change. Writes nothing.
func (a *API) postDBAccessPlan(w http.ResponseWriter, r *http.Request, _ string) {
	var in BulkAccessInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lines, err := a.store.PlanBulkAccess(r.Context(), in)
	if err != nil {
		a.dbErr(w, "plan database access", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// POST /api/v1/database-access: give teams and people a level on many
// databases at once, and with later, on the ones added to that server later.
func (a *API) postDBAccess(w http.ResponseWriter, r *http.Request, actor string) {
	var in BulkAccessInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lines, rules, err := a.store.ApplyBulkAccess(r.Context(), in, actor)
	if err != nil {
		a.dbErr(w, "give database access", err)
		return
	}
	for _, l := range lines {
		if l.Writes() {
			a.auditDB(r.Context(), "db.assigned", "notice", actor, l.Hostname, describeGive(l, ""))
		}
	}
	for _, rule := range rules {
		a.auditDB(r.Context(), "db.rule.set", "notice", actor, rule.Hostname, describeDBRule(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "rules": rules})
}

// POST /api/v1/database-access/remove: take teams and people off many
// databases at once.
func (a *API) postDBAccessRemove(w http.ResponseWriter, r *http.Request, actor string) {
	var in BulkRemoveInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lines, err := a.store.RemoveBulkAccess(r.Context(), in)
	if err != nil {
		a.dbErr(w, "remove database access", err)
		return
	}
	for _, l := range lines {
		a.auditDB(r.Context(), "db.unassigned", "notice", actor, l.Hostname,
			"Took "+lineWho(l)+" off "+engineName(l.Engine)+" "+l.DatabaseName+" ("+l.Hostname+"); they had "+l.Before+".")
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// GET /api/v1/assets/{id}/database-access: every row on every database of a
// server, and its rules for databases added later.
func (a *API) getAssetDBAccess(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "only an admin can see who may open a database")
		return
	}
	id := r.PathValue("id")
	list, err := a.store.AssetDatabaseAccess(r.Context(), id)
	if err != nil {
		a.dbErr(w, "database access", err)
		return
	}
	rules, err := a.store.DatabaseRules(r.Context(), id)
	if err != nil {
		a.dbErr(w, "database rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": list, "rules": rules})
}

// DELETE /api/v1/assets/{id}/database-rules/{rule}?takeAway=1: stop giving
// databases added later; with takeAway, also take back what the rule gave
// that nobody has changed by hand since.
func (a *API) deleteDBRule(w http.ResponseWriter, r *http.Request, actor string) {
	takeAway := r.URL.Query().Get("takeAway") == "1" || r.URL.Query().Get("takeAway") == "true"
	rule, lines, err := a.store.RemoveDatabaseRule(r.Context(), r.PathValue("id"), r.PathValue("rule"), takeAway)
	if err != nil {
		a.dbErr(w, "remove database rule", err)
		return
	}
	a.auditDB(r.Context(), "db.rule.removed", "notice", actor, rule.Hostname,
		"Stopped giving "+dbRuleWho(rule)+" every "+rule.Kind+" added later on "+rule.Hostname+".")
	for _, l := range lines {
		a.auditDB(r.Context(), "db.unassigned", "notice", actor, l.Hostname,
			"Took "+lineWho(l)+" off "+engineName(l.Engine)+" "+l.DatabaseName+" ("+l.Hostname+") with the rule that gave it; they had "+l.Before+".")
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": rule, "lines": lines})
}

// applyRulesTo gives a database just added to whoever its server's rules
// name, and audits each row. A failure is logged, not returned: the database
// is added either way, and the console then shows who has it.
func (a *API) applyRulesTo(ctx context.Context, db Database, actor string) {
	lines, err := a.store.ApplyDatabaseRules(ctx, db.ID, actor)
	if err != nil {
		a.log.Error("database rules not applied", "database", db.ID, "error", err)
		a.auditDB(ctx, "db.rule.failed", "warning", actor, db.Hostname,
			"The rules for databases added later were not applied to "+db.Name+" ("+db.Hostname+"): "+err.Error())
		return
	}
	for _, l := range lines {
		a.auditDB(ctx, "db.assigned", "notice", actor, l.Hostname, describeGive(l, "the rule for databases added later on "+l.Hostname))
	}
}

func lineWho(l BulkAccessLine) string {
	if l.TeamID != nil {
		return "team " + l.TeamName
	}
	return "person " + strings.ToLower(deref(l.UserEmail))
}

func dbRuleWho(r DatabaseRule) string {
	if r.TeamID != nil {
		return "team " + r.TeamName
	}
	return "person " + strings.ToLower(deref(r.UserEmail))
}

// describeGive says one written line the way a single grant is audited.
func describeGive(l BulkAccessLine, by string) string {
	detail := "Gave " + lineWho(l) + " " + l.After + " on " + engineName(l.Engine) + " " + l.DatabaseName + " (" + l.Hostname + ")"
	if l.Before != "" && l.Before != l.After {
		detail += ", was " + l.Before
	}
	if by != "" {
		detail += ", by " + by
	}
	detail += "."
	if l.AfterApproval {
		detail += " Each session needs an approved request."
	}
	return detail
}

func describeDBRule(r DatabaseRule) string {
	approval := map[string]string{ApprovalByLabel: "approval as each label says", ApprovalAlways: "approval for every session",
		ApprovalNever: "no session approval"}[r.Approval]
	return "Every " + r.Kind + " added later on " + r.Hostname + " goes to " + dbRuleWho(r) + " at " + r.Level + ", " + approval + "."
}
