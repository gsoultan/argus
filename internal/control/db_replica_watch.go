package control

import (
	"context"
	"fmt"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The lag watch on streaming replicas Argus made (cloud scaling step 8c).

Once a minute the primary's agent is asked how each of its replicas is doing:
whether it streams, how far behind it is, and how much WAL its slot keeps on
the primary. A replica more than a minute behind, or not streaming at all,
for five minutes rings the bell once; it rings again, as news, when the
replica catches up. Asking the primary is the honest view: a replica that has
gone away cannot say so itself, and the slot it leaves keeps WAL on the
primary until its disk fills.
*/

const (
	// ReplicaWatchInterval is how often the primaries are asked.
	ReplicaWatchInterval = time.Minute
	// replicaBehindS is how far behind counts as behind, in seconds.
	replicaBehindS = 60
	// replicaBehindFor is how long a replica stays behind before the bell rings.
	replicaBehindFor = 5 * time.Minute
)

// RunReplicaWatch watches every organization's replicas until ctx ends.
func (a *API) RunReplicaWatch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.WatchReplicas(tctx, time.Now().UTC())
		})
		if err != nil {
			a.log.Error("replica watch", "error", err)
		}
	}
}

// replicaWatch is one reading, as stored.
type replicaWatch struct {
	Streaming     bool
	LagS          *float64
	LagBytes      *int64
	RetainedBytes *int64
	BehindSince   *time.Time
	Alerted       bool
}

// WatchReplicas reads every finished replica's primary once, for the
// organization on ctx.
func (a *API) WatchReplicas(ctx context.Context, now time.Time) {
	reps, err := a.store.replicasToWatch(ctx)
	if err != nil {
		a.log.Error("replica watch: cannot list replicas", "error", err)
		return
	}
	for _, rep := range reps {
		a.watchReplica(ctx, rep, now)
	}
	// With the readings in, the read endpoints follow them.
	a.TendReadEndpoints(ctx)
}

func (a *API) watchReplica(ctx context.Context, rep DatabaseReplica, now time.Time) {
	primary, err := a.store.Database(ctx, rep.DatabaseID)
	if err != nil {
		return
	}
	w := replicaWatch{BehindSince: rep.BehindSince, Alerted: rep.BehindAlerted}
	var st tunnel.PGReplicaStatus
	if !primary.TunnelConnected {
		// Not knowing is not behind: the agent's own alerts cover a server
		// that went quiet. The last reading stays, marked as old.
		_ = a.store.setReplicaWatchError(ctx, rep.ID, "the agent on "+primary.Hostname+" is not connected")
		return
	}
	sys := rep
	sys.RequestedBy = "system"
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = a.primaryOp(octx, sys, primary, tunnel.ReplicaStatus, "", &st)
	cancel()
	if err != nil {
		_ = a.store.setReplicaWatchError(ctx, rep.ID, err.Error())
		return
	}
	w.Streaming = st.Streaming
	if st.ReplayLagS != nil || st.Streaming {
		lag := st.Lag()
		w.LagS = &lag
	}
	if st.LagBytes >= 0 {
		w.LagBytes = &st.LagBytes
	}
	if st.SlotExists {
		w.RetainedBytes = &st.RetainedBytes
	}
	lag := st.Lag()
	behind := lag < 0 || lag > replicaBehindS
	switch {
	case behind && w.BehindSince == nil:
		w.BehindSince = &now
	case !behind:
		w.BehindSince = nil
	}
	subject := "replica-lag:" + rep.ID
	link := "/databases/" + primary.ID
	if behind && !w.Alerted && now.Sub(*w.BehindSince) >= replicaBehindFor {
		w.Alerted = true
		title, detail := replicaBehindText(rep, st, now.Sub(*w.BehindSince))
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertBackup, Severity: "warning", Audience: AudienceAdmins,
			Title: title, Detail: detail, Link: link, Subject: subject})
		a.auditDB(ctx, "db.replica_behind", "warning", "system", rep.PrimaryHostname, title+". "+detail)
	}
	if !behind && w.Alerted {
		w.Alerted = false
		a.closeAlerts(ctx, subject, "Caught up")
		title := fmt.Sprintf("The replica on %s caught up with %s", rep.Hostname, rep.PrimaryHostname)
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertBackup, Severity: "info", Audience: AudienceAdmins,
			Title: title, Detail: fmt.Sprintf("%s (%s) streams again and is %s behind.", rep.Hostname, rep.Name, lagWords(lag)), Link: link})
		a.auditDB(ctx, "db.replica_caught_up", "notice", "system", rep.PrimaryHostname, title+".")
	}
	if err := a.store.setReplicaWatch(ctx, rep.ID, w, now); err != nil {
		a.log.Error("replica watch: cannot record", "replica", rep.ID, "error", err)
	}
}

// replicaBehindText words a replica that has been behind for d.
func replicaBehindText(rep DatabaseReplica, st tunnel.PGReplicaStatus, d time.Duration) (string, string) {
	mins := int(d.Round(time.Minute) / time.Minute)
	var title, detail string
	if !st.Streaming {
		title = fmt.Sprintf("The replica on %s has not streamed from %s for %d minutes", rep.Hostname, rep.PrimaryHostname, mins)
		detail = fmt.Sprintf("%s (%s) is not connected to %s", rep.Hostname, rep.Name, rep.PrimaryHostname)
		if st.State != "" {
			detail = fmt.Sprintf("%s (%s) is connected but %s, not streaming", rep.Hostname, rep.Name, st.State)
		}
		if st.SlotExists {
			detail += fmt.Sprintf(". Its slot keeps %s of WAL on %s, and that grows until the replica is back or removed", bytesWords(st.RetainedBytes), rep.PrimaryHostname)
		}
		return title, detail + "."
	}
	title = fmt.Sprintf("The replica on %s is %s behind %s", rep.Hostname, lagWords(st.Lag()), rep.PrimaryHostname)
	detail = fmt.Sprintf("%s (%s) has been more than a minute behind for %d minutes", rep.Hostname, rep.Name, mins)
	if st.LagBytes > 0 {
		detail += fmt.Sprintf(", with %s of changes still to apply", bytesWords(st.LagBytes))
	}
	return title, detail + ". Reads from it see old data."
}

// lagWords is a lag in seconds in words: "4 s", "3 min", "2 h 5 min".
func lagWords(s float64) string {
	n := int(s)
	switch {
	case n < 60:
		return fmt.Sprintf("%d s", n)
	case n < 3600:
		return fmt.Sprintf("%d min", n/60)
	default:
		return fmt.Sprintf("%d h %d min", n/3600, n%3600/60)
	}
}

// bytesWords is a size in words, in binary units.
func bytesWords(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

/* ── Store ──────────────────────────────────────────────────────────────── */

// replicasToWatch is every replica that was made and still stands.
func (s *Store) replicasToWatch(ctx context.Context) ([]DatabaseReplica, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+replicaCols+replicaFrom+`WHERE r.state = 'done' ORDER BY r.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DatabaseReplica{}
	for rows.Next() {
		r, err := scanReplica(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) setReplicaWatch(ctx context.Context, id string, w replicaWatch, now time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET streaming = $2, lag_s = $3, lag_bytes = $4,
		retained_bytes = $5, watched_at = $6, watch_error = '', behind_since = $7, behind_alerted = $8
		WHERE id = $1::uuid`, id, w.Streaming, w.LagS, w.LagBytes, w.RetainedBytes, now, w.BehindSince, w.Alerted)
	return err
}

// setReplicaWatchError records that the primary could not be asked; the
// last reading stays, with its time.
func (s *Store) setReplicaWatchError(ctx context.Context, id, msg string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET watch_error = $2 WHERE id = $1::uuid`, id, clip(msg, 500))
	return err
}
