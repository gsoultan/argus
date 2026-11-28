package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gsoultan/caduceus"
)

/*
Ending the sessions an agent no longer has.

A login through sshd is recorded by the server's agent, which reports it
running every minute and once more when it ends. An agent that restarts
under a login never reports that end: the new agent does not know the
session, and the row stayed "active" -- shown on Live, counted, and offered
for Watch live, which the agent then refused. Silence turned it grey after
three minutes and closed it after a day.

So every heartbeat says which sessions the agent is recording now, and the
control plane ends, at once, each session that agent reported and no longer
has. One the login carried on under a new id is ended as carried on, naming
that id, and Watch live follows it there.
*/

// AgentRecording is what an agent is recording now, as of At on its clock.
type AgentRecording struct {
	At       time.Time               `json:"at"`
	Sessions []AgentRecordingSession `json:"sessions"`
}

// AgentRecordingSession is one session an agent is recording, and the ids
// the same login had before an agent restart.
type AgentRecordingSession struct {
	ID        string   `json:"id"`
	Continues []string `json:"continues,omitempty"`
}

// agentRosterMax bounds a heartbeat's list; past it nothing is ended, since
// a list cut short would end sessions that are running.
const agentRosterMax = 4096

// agentRosterGrace keeps a session that started just before the beat was
// taken out of it: its row can exist before the beat that lists it arrives.
const agentRosterGrace = 30 * time.Second

// Why a session the agent no longer has was ended.
const (
	agentLostReason = "the agent on this server no longer has this session: it restarted " +
		"during it, or the session's end did not reach Argus"
	agentContinuedReason = "the login carried on as session %s after the agent on this server restarted"
)

// EndSessionsAgentLost ends each session reporter ("agent:<host>") reported
// as running, that began before rec.At and is not in rec. Each one's end is
// the last time it was reported, marked inferred. Returns what it ended.
func (s *Store) EndSessionsAgentLost(ctx context.Context, reporter string,
	rec AgentRecording) ([]Session, error) {

	if len(rec.Sessions) > agentRosterMax || rec.At.IsZero() {
		return nil, nil
	}
	// A beat replayed late is as old as it says; one from the future is
	// taken as now.
	at := rec.At
	if now := time.Now(); at.After(now) {
		at = now
	}
	at = at.Add(-agentRosterGrace)

	ids := make([]string, 0, len(rec.Sessions))
	var olds, news []string
	for _, r := range rec.Sessions {
		if r.ID == "" {
			continue
		}
		ids = append(ids, r.ID)
		for _, old := range r.Continues {
			if old != "" {
				olds, news = append(olds, old), append(news, r.ID)
			}
		}
	}

	rows, err := s.db(ctx).Query(ctx, `
		WITH carried(old_id, new_id) AS (
			SELECT * FROM unnest($4::text[], $5::text[])
		)
		UPDATE sessions s
		   SET state              = 'closed',
		       ended_at           = s.last_reported_at,
		       end_inferred       = true,
		       termination_reason = COALESCE(s.termination_reason,
		           (SELECT format($6, c.new_id) FROM carried c
		             WHERE c.old_id = s.id::text LIMIT 1),
		           $7)
		 WHERE s.state = 'active'
		   AND s.reported_by = $1
		   AND s.started_at < $2
		   AND NOT (s.id::text = ANY($3::text[]))
		RETURNING s.id::text, s.user_email, s.asset_hostname, s.principal, s.started_at,
		          s.last_reported_at, COALESCE(s.termination_reason, '')`,
		reporter, at, ids, olds, news,
		// format() takes %s, as Sprintf does.
		agentContinuedReason, agentLostReason)
	if err != nil {
		return nil, fmt.Errorf("end sessions the agent lost: %w", err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var v Session
		var why string
		if err := rows.Scan(&v.ID, &v.UserEmail, &v.AssetHostname, &v.Principal,
			&v.StartedAt, &v.LastReportedAt, &why); err != nil {
			return nil, err
		}
		v.State, v.EndInferred = "closed", true
		end := v.LastReportedAt
		v.EndedAt, v.TerminationReason = &end, &why
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		s.notifyChanged(ctx)
	}
	return out, nil
}

// ContinuedAs is the running session that carries id on after an agent
// restart, "" for none: the session an agent recorded the rest of the login
// under, following each restart in turn.
func (s *Store) ContinuedAs(ctx context.Context, id string) (string, error) {
	cur := id
	for range 8 {
		var next string
		err := s.db(ctx).QueryRow(ctx, `
			SELECT n.id::text FROM sessions o JOIN sessions n
			    ON n.asset_hostname = o.asset_hostname AND n.principal = o.principal
			   AND n.origin_reason LIKE 'continues session ' || o.id::text || ' %'
			 WHERE o.id::text = $1
			 ORDER BY n.started_at DESC LIMIT 1`, cur).Scan(&next)
		if err != nil {
			if errors.Is(err, caduceus.ErrNoRows) {
				break
			}
			return "", err
		}
		cur = next
	}
	if cur == id {
		return "", nil
	}
	return cur, nil
}

// endAgentLost ends what the agent behind this heartbeat no longer has, and
// audits each one: a privileged session ending is an audit event whoever
// decided it.
func (a *API) endAgentLost(ctx context.Context, host string, rec AgentRecording) {
	ended, err := a.store.EndSessionsAgentLost(ctx, "agent:"+host, rec)
	if err != nil {
		a.log.Error("heartbeat: cannot end sessions the agent no longer has", "host", host, "error", err)
		return
	}
	for _, v := range ended {
		a.log.Info("ended a session the agent no longer has", "session", v.ID, "host", host,
			"why", *v.TerminationReason)
		if _, err := a.store.AppendAudit(ctx, AuditEvent{
			Action:     "session.end_inferred",
			Severity:   "notice",
			ActorEmail: v.UserEmail,
			Target:     v.AssetHostname,
			Detail: fmt.Sprintf("Session %s as %s ended: %s. Last reported %s.", v.ID, v.Principal,
				*v.TerminationReason, v.LastReportedAt.UTC().Format("2006-01-02 15:04:05 MST")),
		}); err != nil {
			a.log.Error("audit append failed", "error", err)
		}
	}
}
