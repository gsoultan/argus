package control

import (
	"context"
	"errors"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The relay's half of a MongoDB's change of which machines can connect
(tunnel/mongonetwork.go): it is DBA work, it is audited, and it waits for
approval where the database's changes do, and always when it opens the
database to anywhere. The approval is for the change's exact text,
tunnel.MongoNetworkText, which the agent asks for too.
*/

// mongoGate decides whether a MongoDB request may pass to the agent. It
// returns the refusal to answer with, or nil.
func (a *API) mongoGate(ctx context.Context, email string, db Database, level string, req *tunnel.StoreRequest) *tunnel.StoreAnswer {
	switch req.Op {
	case tunnel.MongoOpNetwork, tunnel.MongoOpNetworkSet:
	default:
		req.Approval = ""
		return nil
	}
	if level != tunnel.LevelDBA {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error",
			Error: "changing which machines can connect is DBA work; this session is at " + level}
	}
	if req.Op == tunnel.MongoOpNetwork {
		req.Approval = ""
		return nil
	}
	op := tunnel.MongoNetOp(*req)
	if err := tunnel.ValidatePGNetwork(&op); err != nil {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: err.Error()}
	}
	req.Sources = op.Sources
	text := tunnel.MongoNetworkText(*req)
	always := req.Reach == tunnel.ReachAny
	if !db.WritesNeedApproval() && !always {
		req.Approval = ""
		a.auditDB(ctx, "db.network_change", "notice", email, db.Hostname,
			"Changed which machines can connect to "+db.Noun()+" "+db.Hostname+": "+oneLine(text)+".")
		return nil
	}
	if req.Approval == "" {
		why := "on " + db.Hostname + approvalWhy(db)
		if always && !db.WritesNeedApproval() {
			why = "opening " + db.Hostname + " to anywhere"
		}
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", NeedsApproval: &tunnel.RedisApprovalNeed{Text: text},
			Error: why + ": this change runs once someone else approves it"}
	}
	if _, err := a.store.UseStatementApproval(ctx, req.Approval, email, db.ID, text, nil); err != nil {
		var no errNoApproval
		if errors.As(err, &no) {
			return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: no.why}
		}
		a.log.Error("mongodb: checking an approval failed", "error", err)
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: "the approval could not be checked; try again"}
	}
	a.auditDB(ctx, "db.network_change", "notice", email, db.Hostname,
		"Changed which machines can connect to "+db.Noun()+" "+db.Hostname+" under approval "+req.Approval+": "+oneLine(text)+".")
	return nil
}
