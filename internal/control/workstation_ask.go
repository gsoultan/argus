package control

import (
	"context"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

// workstationAskSeconds is how long the person at a workstation has to
// answer before the answer is no.
const workstationAskSeconds = 60

// auditAskRefused records a session the person at a workstation did not let
// open: their no is as much a part of the record as a yes.
func (a *API) auditAskRefused(ctx context.Context, email, host, what, why string) {
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action:     "session.declined_at_machine",
		Severity:   "notice",
		ActorEmail: email,
		Target:     host,
		Detail: what + " did not open: " + strings.TrimPrefix(why, tunnel.AskRefusedPrefix) +
			". Argus asked the person using the workstation first.",
	}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
}
