package control

import (
	"context"
	"errors"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The relay's half of managing a Redis (see tunnel/redis_manage.go): the level
an operation needs, and the approval a bulk, configuration or user change
waits for where the entry's changes need one.

An approval is the same db:statement request SQL changes use: the ask's one
item is the exact text tunnel.RedisApproval gives, and its row cap is the
most keys a bulk change may touch. The agent refuses such a change without
an approval id too, so the API and the agent refuse the same thing.
*/

// redisGate decides whether a Redis request may pass to the agent. It
// returns the refusal to answer with, or nil.
func (a *API) redisGate(ctx context.Context, email string, db Database, level string, req *tunnel.StoreRequest) *tunnel.StoreAnswer {
	// A password reaches the agent only for the sign-in check it is for.
	if req.Op != tunnel.RedisOpACLLogin {
		req.Password = ""
	}
	need := tunnel.RedisOpLevel(req.Op)
	if need == "" {
		req.Approval = ""
		return nil
	}
	if tunnel.LevelRank(level) < tunnel.LevelRank(need) {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error",
			Error: "this needs " + need + " or above; this session is at " + level}
	}
	ask, waits := tunnel.RedisApproval(*req)
	if !waits || !db.WritesNeedApproval() {
		req.Approval = ""
		if waits {
			a.auditDB(ctx, "db.redis_change", "notice", email, db.Hostname,
				"Changed "+db.Noun()+" "+db.Hostname+": "+oneLine(ask.Text)+".")
		}
		return nil
	}
	if req.Approval == "" {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", NeedsApproval: &ask,
			Error: "on " + db.Hostname + approvalWhy(db) + ": this change runs once someone else approves it"}
	}
	rowCap, err := a.store.UseStatementApproval(ctx, req.Approval, email, db.ID, ask.Text, nil)
	if err != nil {
		var no errNoApproval
		if errors.As(err, &no) {
			return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: no.why}
		}
		a.log.Error("redis: checking an approval failed", "error", err)
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: "the approval could not be checked; try again"}
	}
	if req.Op == tunnel.RedisOpBulk {
		if rowCap == nil {
			return &tunnel.StoreAnswer{ID: req.ID, Type: "error",
				Error: "that approval names no number of keys; count and ask again"}
		}
		// The approved count, never more, whatever the browser sent.
		req.Max = min(req.Max, *rowCap)
	}
	a.auditDB(ctx, "db.redis_change", "notice", email, db.Hostname,
		"Changed "+db.Noun()+" "+db.Hostname+" under approval "+req.Approval+": "+oneLine(ask.Text)+".")
	return nil
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", "; ") }

// approvalEngines are the engines whose changes can wait for approval:
// Postgres's statements, and a Redis's bulk, configuration and user changes.
var approvalEngines = []string{tunnel.EnginePostgres, tunnel.EngineRedis}
