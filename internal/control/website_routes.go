package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/*
Routes for website checks (websites.go). Reading is for the roles that see
the whole fleet (admin, owner, auditor); adding, changing, removing and
checking now are for admins and owners, and adding, changing and removing
are in the audit log, since a check names servers whose agents fetch the
address.

	GET    /api/v1/websites            every check, with uptime and any open outage
	POST   /api/v1/websites            add one
	POST   /api/v1/websites/test       fetch an address once from Argus, saving nothing
	GET    /api/v1/websites/{id}       one check, its last day of rounds and its outages
	PUT    /api/v1/websites/{id}       change one
	DELETE /api/v1/websites/{id}       remove one, and its history
	POST   /api/v1/websites/{id}/check check it now
	POST   /api/v1/websites/{id}/test-alert  send a test alert to its Telegram chats now
	GET    /api/v1/websites/settings   the feature's alert settings, and the chats there are
	PUT    /api/v1/websites/settings   change them (audited)
*/

func (a *API) websiteReader(w http.ResponseWriter, r *http.Request) bool {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "website checks are for admins, owners and auditors")
		return false
	}
	return true
}

func (a *API) websiteAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can change website checks")
		return "", false
	}
	return sess.Email, true
}

func (a *API) websiteErr(w http.ResponseWriter, what string, err error) {
	var invalid ErrInvalidWebsite
	switch {
	case errors.As(err, &invalid):
		writeErr(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, ErrNoWebsite):
		writeErr(w, http.StatusNotFound, "no such website check")
	default:
		a.fail(w, what, err)
	}
}

// decorateWebsites fills in uptime, open outages and the confirming
// servers' names.
func (a *API) decorateWebsites(r *http.Request, list []WebsiteCheck) error {
	up, err := a.store.WebsiteUptimes(r.Context(), time.Now())
	if err != nil {
		return err
	}
	open, err := a.store.WebsiteOutages(r.Context(), "", 1000)
	if err != nil {
		return err
	}
	byCheck := map[string]WebsiteOutage{}
	for _, o := range open {
		byCheck[o.CheckID] = o
	}
	names := map[string]string{}
	for i := range list {
		c := &list[i]
		c.Uptime = up[c.ID]
		if o, ok := byCheck[c.ID]; ok {
			c.OpenOutage = &o
		}
		for _, id := range c.ConfirmFrom {
			n, ok := names[id]
			if !ok {
				if as, err := a.store.AssetByID(r.Context(), id); err == nil {
					n = as.Hostname
					if as.Alias != "" {
						n = as.Alias
					}
				} else {
					n = "a removed server"
				}
				names[id] = n
			}
			c.ConfirmHost = append(c.ConfirmHost, n)
		}
	}
	return nil
}

func (a *API) getWebsites(w http.ResponseWriter, r *http.Request) {
	if !a.websiteReader(w, r) {
		return
	}
	list, err := a.store.WebsiteChecks(r.Context())
	if err == nil {
		err = a.decorateWebsites(r, list)
	}
	if err != nil {
		a.fail(w, "website checks", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *API) getWebsite(w http.ResponseWriter, r *http.Request) {
	if !a.websiteReader(w, r) {
		return
	}
	id := r.PathValue("id")
	c, err := a.store.WebsiteCheck(r.Context(), id)
	if err != nil {
		a.websiteErr(w, "website check", err)
		return
	}
	list := []WebsiteCheck{c}
	if err := a.decorateWebsites(r, list); err != nil {
		a.fail(w, "website check", err)
		return
	}
	rounds, err := a.store.WebsiteRounds(r.Context(), id, time.Now().Add(-24*time.Hour))
	if err != nil {
		a.fail(w, "website rounds", err)
		return
	}
	outages, err := a.store.WebsiteOutages(r.Context(), id, 50)
	if err != nil {
		a.fail(w, "website outages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"check": list[0], "rounds": rounds, "outages": outages})
}

// websiteSummary is a check's settings in words, for the audit log.
func websiteSummary(c WebsiteCheck) string {
	parts := []string{c.URL, fmt.Sprintf("every %d min", c.EverySeconds/60), fmt.Sprintf("timeout %ds", c.TimeoutSeconds)}
	if c.ExpectStatus != 0 {
		parts = append(parts, fmt.Sprintf("expects %d", c.ExpectStatus))
	}
	if c.Keyword != "" {
		parts = append(parts, fmt.Sprintf("looks for %q", c.Keyword))
	}
	if len(c.ConfirmHost) > 0 {
		parts = append(parts, "confirmed from "+strings.Join(c.ConfirmHost, ", "))
	}
	if c.Paused {
		parts = append(parts, "paused")
	}
	switch c.Telegram {
	case TelegramOff:
		parts = append(parts, "no Telegram")
	case TelegramChats:
		parts = append(parts, "Telegram to "+strings.Join(c.TelegramChats, ", "))
	}
	return strings.Join(parts, ", ")
}

func (a *API) auditWebsite(r *http.Request, action, severity, actor string, c WebsiteCheck, detail string) {
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: action, Severity: severity, ActorEmail: actor,
		Target: "website " + c.Name, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

func (a *API) postWebsite(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in WebsiteInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.store.CreateWebsiteCheck(r.Context(), in, actor)
	if err != nil {
		a.websiteErr(w, "add website check", err)
		return
	}
	list := []WebsiteCheck{c}
	_ = a.decorateWebsites(r, list)
	a.auditWebsite(r, "website.add", "notice", actor, list[0], "Added a website check: "+websiteSummary(list[0])+".")
	writeJSON(w, http.StatusCreated, list[0])
}

func (a *API) putWebsite(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in WebsiteInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, after, err := a.store.UpdateWebsiteCheck(r.Context(), r.PathValue("id"), in)
	if err != nil {
		a.websiteErr(w, "change website check", err)
		return
	}
	list := []WebsiteCheck{before, after}
	_ = a.decorateWebsites(r, list)
	a.auditWebsite(r, "website.change", "notice", actor, list[1],
		"Changed a website check from "+websiteSummary(list[0])+" to "+websiteSummary(list[1])+".")
	writeJSON(w, http.StatusOK, list[1])
}

func (a *API) deleteWebsite(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	c, err := a.store.DeleteWebsiteCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		a.websiteErr(w, "remove website check", err)
		return
	}
	a.closeAlerts(r.Context(), websiteSubject(c.ID), "The check was removed by "+actor)
	a.auditWebsite(r, "website.remove", "warn", actor, c, "Removed the website check of "+c.URL+" and its history.")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postWebsiteCheckNow(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.websiteAdmin(w, r); !ok {
		return
	}
	if err := a.store.CheckWebsiteNow(r.Context(), r.PathValue("id")); err != nil {
		a.websiteErr(w, "check website now", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// postWebsiteTest fetches an address once from this control server, for
// the form's Test button. It saves nothing and asks no server.
func (a *API) postWebsiteTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.websiteAdmin(w, r); !ok {
		return
	}
	var in WebsiteInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.ConfirmFrom = nil
	if err := in.normalise(); err != nil {
		a.websiteErr(w, "test website", err)
		return
	}
	res := a.probeSite(r.Context(), in.grant())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// websiteSettingsView is the feature's settings with what they choose from.
type websiteSettingsView struct {
	WebsiteAlertSettings
	// Chats are the chats Telegram is set up for in Settings, Alerts.
	Chats []string `json:"chats"`
	// Unavailable says why Telegram cannot be used here; empty when it can.
	Unavailable string `json:"unavailable,omitempty"`
}

func (a *API) websiteSettingsView(r *http.Request, v WebsiteAlertSettings) websiteSettingsView {
	out := websiteSettingsView{WebsiteAlertSettings: v, Chats: a.SignInAlerts.TelegramChats()}
	if out.Chats == nil {
		out.Chats = []string{}
	}
	_, why := a.websiteTelegramChats(r.Context(), WebsiteCheck{Telegram: TelegramChats, TelegramChats: out.Chats})
	out.Unavailable = why
	return out
}

func (a *API) getWebsiteSettings(w http.ResponseWriter, r *http.Request) {
	if !a.websiteReader(w, r) {
		return
	}
	v, err := a.store.WebsiteAlertSettings(r.Context())
	if err != nil {
		a.fail(w, "website alert settings", err)
		return
	}
	writeJSON(w, http.StatusOK, a.websiteSettingsView(r, v))
}

func (a *API) putWebsiteSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Telegram      bool     `json:"telegram"`
		TelegramChats []string `json:"telegramChats"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	before, _ := a.store.WebsiteAlertSettings(r.Context())
	v, err := a.store.SetWebsiteAlertSettings(r.Context(), WebsiteAlertSettings{Telegram: in.Telegram, TelegramChats: in.TelegramChats}, actor)
	if err != nil {
		a.websiteErr(w, "website alert settings", err)
		return
	}
	say := func(s WebsiteAlertSettings) string {
		if !s.Telegram {
			return "off"
		}
		if len(s.TelegramChats) == 0 {
			return "on, to every configured chat"
		}
		return "on, to " + strings.Join(s.TelegramChats, ", ")
	}
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{Action: "website.alerts", Severity: "notice", ActorEmail: actor,
		Target: "websites", Detail: "Changed Telegram for website alerts from " + say(before) + " to " + say(v) + "."}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", "website.alerts")
	}
	writeJSON(w, http.StatusOK, a.websiteSettingsView(r, v))
}

// postWebsiteTestAlert sends a test message to the check's Telegram chats
// now and says where it went, or why it went nowhere.
func (a *API) postWebsiteTestAlert(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.websiteAdmin(w, r)
	if !ok {
		return
	}
	c, err := a.store.WebsiteCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		a.websiteErr(w, "test website alert", err)
		return
	}
	chats, why := a.websiteTelegramChats(r.Context(), c)
	if len(chats) == 0 {
		writeErr(w, http.StatusConflict, why)
		return
	}
	text := websiteTelegramText("🧪", "Test: "+c.Name+" alerts reach this chat",
		"Sent by "+actor+" from Argus. When "+c.URL+" goes down, and when it is back, it is said here.")
	if err := a.SignInAlerts.TelegramNow(r.Context(), chats, text, "Open in Argus",
		a.websiteConsoleLink("/websites?check="+c.ID)); err != nil {
		writeErr(w, http.StatusBadGateway, "Telegram did not take it: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}
