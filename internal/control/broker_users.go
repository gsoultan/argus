package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The relay's half of a broker's Users tab (tunnel/brokerusers.go): the level
each operation needs, the approval a change waits for where the entry's
changes need one, and the Activity entry once the agent says it ran.

The approval is the same db:statement request SQL changes use, for the exact
text tunnel.KafkaUserText gives, which never holds the credential: it is made
in the browser after the approval and carried as a salted hash. A password
reaches the agent only for the sign-in check it is for.

"Made in Argus by …" on the Users tab comes from these Activity entries, so
no table holds broker users.
*/

// brokerUserEngines are the engines with a Users tab of this kind.
var brokerUserEngines = []string{tunnel.EngineKafka, tunnel.EngineRabbitMQ}

func brokerUserText(db Database, req tunnel.StoreRequest) string {
	return tunnel.BrokerUserText(db.Engine, db.Name, req)
}

func brokerUserProblem(db Database, req tunnel.StoreRequest) string {
	return tunnel.ValidBrokerUserChange(db.Engine, db.Name, req)
}

// brokerUserGate decides whether a broker user request may pass to the
// agent. ok is false when the request is not one of these; otherwise no is
// the refusal to answer with, or nil.
func (a *API) brokerUserGate(ctx context.Context, email string, db Database, level string, req *tunnel.StoreRequest, audit *brokerAudit) (no *tunnel.StoreAnswer, ok bool) {
	if !slices.Contains(brokerUserEngines, db.Engine) {
		req.BrokerUser = nil
		return nil, false
	}
	if db.Engine == tunnel.EngineKafka && tunnel.IsKafkaChange(req.Op) {
		req.Password, req.BrokerUser = "", nil
		return a.kafkaChangeGate(ctx, email, db, level, req), true
	}
	if db.Engine == tunnel.EngineKafka && req.Op != tunnel.KafkaOpTopicConfigs && req.Op != tunnel.KafkaOpGroup {
		req.KafkaChange = nil
	}
	need := tunnel.BrokerUserOpLevelFor(db.Engine, req.Op)
	if need == "" {
		req.Password = ""
		req.BrokerUser = nil
		return nil, false
	}
	if req.Op != tunnel.BrokerOpUserLogin {
		req.Password = ""
	}
	refuse := func(why string) (*tunnel.StoreAnswer, bool) {
		req.Password = ""
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: why}, true
	}
	if tunnel.LevelRank(level) < tunnel.LevelRank(need) {
		return refuse("this needs " + need + " or above; this session is at " + level)
	}
	if why := brokerUserProblem(db, *req); why != "" {
		return refuse(why)
	}
	if !tunnel.BrokerUserChanges(req.Op) {
		req.Approval = ""
		return nil, true
	}
	text := brokerUserText(db, *req)
	if !db.WritesNeedApproval() {
		req.Approval = ""
		audit.expect(req.ID, db, email, *req, text, "")
		return nil, true
	}
	if req.Approval == "" {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", NeedsApproval: &tunnel.RedisApprovalNeed{Text: text},
			Error: "on " + db.Hostname + approvalWhy(db) + ": this change runs once someone else approves it"}, true
	}
	if _, err := a.store.UseStatementApproval(ctx, req.Approval, email, db.ID, text, nil); err != nil {
		var na errNoApproval
		if errors.As(err, &na) {
			return refuse(na.why)
		}
		a.log.Error("broker users: checking an approval failed", "error", err)
		return refuse("the approval could not be checked; try again")
	}
	audit.expect(req.ID, db, email, *req, text, req.Approval)
	return nil, true
}

// kafkaChangeGate is the gate for a Kafka topic or group change: Manage,
// and on an entry whose changes need approval, an approval of its exact
// text. A dry run changes nothing and passes.
func (a *API) kafkaChangeGate(ctx context.Context, email string, db Database, level string, req *tunnel.StoreRequest) *tunnel.StoreAnswer {
	refuse := func(why string) *tunnel.StoreAnswer {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", Error: why}
	}
	if tunnel.LevelRank(level) < tunnel.LevelRank(tunnel.LevelOwner) {
		return refuse("this needs owner or above; this session is at " + level)
	}
	if why := tunnel.ValidKafkaChange(db.Name, *req); why != "" {
		return refuse(why)
	}
	if req.KafkaChange != nil && req.KafkaChange.DryRun {
		req.Approval = ""
		return nil
	}
	if !db.WritesNeedApproval() {
		req.Approval = ""
		return nil
	}
	text := tunnel.KafkaChangeText(*req)
	if req.Approval == "" {
		return &tunnel.StoreAnswer{ID: req.ID, Type: "error", NeedsApproval: &tunnel.RedisApprovalNeed{Text: text},
			Error: "on " + db.Hostname + approvalWhy(db) + ": this change runs once someone else approves it"}
	}
	if _, err := a.store.UseStatementApproval(ctx, req.Approval, email, db.ID, text, nil); err != nil {
		var na errNoApproval
		if errors.As(err, &na) {
			return refuse(na.why)
		}
		a.log.Error("kafka: checking an approval failed", "error", err)
		return refuse("the approval could not be checked; try again")
	}
	return nil
}

// brokerAudit holds the user changes sent to the agent until it answers,
// so Activity says what ran, not what was asked.
type brokerAudit struct {
	a       *API
	mu      sync.Mutex
	pending map[uint32]brokerAuditEntry
}

type brokerAuditEntry struct {
	action, email, host, detail string
}

// maxBrokerAudits bounds the changes waiting for an answer in one session.
const maxBrokerAudits = 64

func newBrokerAudit(a *API) *brokerAudit {
	return &brokerAudit{a: a, pending: map[uint32]brokerAuditEntry{}}
}

// brokerUserDetail is how an Activity entry about a broker user starts:
// the user, quoted, and the entry, so the Users tab can find its own.
func brokerUserDetail(user, dbID string) string {
	return "broker user " + strconv.Quote(user) + " in entry " + dbID + ": "
}

func (b *brokerAudit) expect(id uint32, db Database, email string, req tunnel.StoreRequest, text, approval string) {
	if b == nil || req.BrokerUser == nil {
		return
	}
	action := "broker.user.changed"
	switch {
	case req.Op == tunnel.BrokerOpUserSet && req.BrokerUser.Create:
		action = "broker.user.created"
	case req.Op == tunnel.BrokerOpUserDel:
		action = "broker.user.deleted"
	}
	detail := brokerUserDetail(req.BrokerUser.Name, db.ID) + oneLine(text)
	if approval != "" {
		detail += " (under approval " + approval + ")"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) >= maxBrokerAudits {
		// An agent that never answers: the oldest go unrecorded rather than
		// the map growing. Its session recording still holds them.
		for k := range b.pending {
			delete(b.pending, k)
			break
		}
	}
	b.pending[id] = brokerAuditEntry{action: action, email: email, host: db.Hostname, detail: detail}
}

// answer records the change an answer is for, when it ran.
func (b *brokerAudit) answer(ctx context.Context, p []byte) {
	if b == nil {
		return
	}
	var ans struct {
		ID   uint32 `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(p, &ans) != nil {
		return
	}
	b.mu.Lock()
	e, ok := b.pending[ans.ID]
	delete(b.pending, ans.ID)
	b.mu.Unlock()
	if ok && ans.Type == "result" {
		b.a.auditDB(ctx, e.action, "notice", e.email, e.host, e.detail+".")
	}
}

// BrokerUserEvent is one Activity entry about a broker user of an entry.
type BrokerUserEvent struct {
	User   string    `json:"user"`
	Action string    `json:"action"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
}

// BrokerUserEvents are the newest Activity entries about an entry's broker
// users, at most limit.
func (s *Store) BrokerUserEvents(ctx context.Context, host, dbID string, limit int) ([]BrokerUserEvent, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT at, action, actor_email, detail FROM audit_events
		 WHERE target = $1 AND action IN ('broker.user.created', 'broker.user.changed', 'broker.user.deleted')
		 ORDER BY seq DESC LIMIT 2000`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BrokerUserEvent{}
	for rows.Next() {
		var e BrokerUserEvent
		var detail string
		if err := rows.Scan(&e.At, &e.Action, &e.By, &detail); err != nil {
			return nil, err
		}
		rest, ok := strings.CutPrefix(detail, "broker user ")
		if !ok {
			continue
		}
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			continue
		}
		user, _ := strconv.Unquote(q)
		tail, ok := strings.CutPrefix(rest[len(q):], " in entry "+dbID+": ")
		if !ok {
			continue
		}
		e.User, e.Text, e.At = user, tail, e.At.UTC()
		if len(out) < limit {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// getBrokerUserEvents answers the Users tab's "Made" column and a user's
// recent changes.
func (a *API) getBrokerUserEvents(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	ev, err := a.store.BrokerUserEvents(r.Context(), db.Hostname, db.ID, 500)
	if err != nil {
		a.fail(w, "broker user activity", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev})
}
