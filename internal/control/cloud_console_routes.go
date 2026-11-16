package control

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
An instance's console output: what it printed while booting, which is how
to see why a server that does not answer is stuck (a disk that would not
mount, a kernel panic, a full root disk).

Read with the manage role, so only the people who may change the account
see it: a boot log can hold more than it should. Every read is audited.
*/

// maxConsoleText is the most of the output kept: the end, where a stuck
// boot stops.
const maxConsoleText = 64 << 10

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// cleanConsole keeps the end of a console's text, without terminal escapes
// or control characters other than line breaks and tabs.
func cleanConsole(s string) string {
	s = reANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' || unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, s)
	if len(s) > maxConsoleText {
		s = s[len(s)-maxConsoleText:]
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}

func (a *API) getCloudConsoleOutput(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudChanger(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	m, code, err := a.cloudManagedAccount(r.Context(), q.Get("accountId"))
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	cs, ok := m.manager.(cloud.Consoler)
	if !ok {
		writeErr(w, http.StatusNotImplemented, m.acct.Provider+" cannot read console output yet")
		return
	}
	if err := a.missingFor(m, []string{"ec2:GetConsoleOutput"}); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	inst, err := a.store.CloudInstance(r.Context(), m.acct.ID, q.Get("region"), q.Get("resourceId"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	out, err := cs.ConsoleOutput(r.Context(), m.target, cloud.Actor(actor), inst.Region, inst.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "AWS did not give the console output: "+err.Error())
		return
	}
	out.Text = cleanConsole(out.Text)
	a.auditAsset(r, AuditEvent{Action: "cloud.console_output.read", Severity: "info", ActorEmail: actor,
		Target: "aws " + m.acct.Name + " " + inst.Region + " " + inst.Name + " (" + inst.ID + ")",
		Detail: "Read the instance's console output"})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
