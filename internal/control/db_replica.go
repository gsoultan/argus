package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Streaming replicas of a Postgres on a server Argus manages (cloud scaling
step 8b). Someone with DBA access to a database asks for a replica of its
server on another server Argus manages, which has the same major version of
Postgres installed and nothing in it yet. A second person approves, as for a
restore in place. Then, through the two agents:

 1. the primary is checked: anything that needs a restart (wal_level, free
    WAL senders and slots, listen_addresses) stops it here, in words
 2. the replica's agent makes the replication password and sends up only
    its SCRAM verifier
 3. the primary makes the replication role and slot with that verifier,
    and adds the replica's pg_hba.conf line and reloads if none lets it in
 4. the replica's agent copies the primary and starts as a replica
 5. the replica is added to Argus as a database of its own, with the
    primary's access there capped at read

A failure after step 3 takes the role, slot and line off the primary again.
The agents' halves are agent/pgreplica.go and agent/pgreplica_clone.go.
*/

// replicaPrincipal is the access request that stands for a replica.
const replicaPrincipal = "db:replica"

// replicaApprovalMin is how long an approval to make a replica lasts.
const replicaApprovalMin = 60

// DatabaseReplica is one replica, asked for or made.
type DatabaseReplica struct {
	ID                string     `json:"id"`
	DatabaseID        string     `json:"databaseId"`
	DBName            string     `json:"dbname"`
	PrimaryHostname   string     `json:"primaryHostname"`
	AssetID           string     `json:"assetId"`
	Hostname          string     `json:"hostname"`
	Name              string     `json:"name"`
	ReplicaAddress    string     `json:"replicaAddress"`
	PrimaryAddress    string     `json:"primaryAddress"`
	RequestedBy       string     `json:"requestedBy"`
	Reason            string     `json:"reason"`
	RequestID         *string    `json:"requestId,omitempty"`
	State             string     `json:"state"`
	Stage             string     `json:"stage"`
	Detail            string     `json:"detail"`
	Error             string     `json:"error"`
	ReplicaDatabaseID *string    `json:"replicaDatabaseId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
	// The lag watch's last reading from the primary (db_replica_watch.go).
	Streaming     bool       `json:"streaming"`
	LagS          *float64   `json:"lagS,omitempty"`
	LagBytes      *int64     `json:"lagBytes,omitempty"`
	RetainedBytes *int64     `json:"retainedBytes,omitempty"`
	WatchedAt     *time.Time `json:"watchedAt,omitempty"`
	WatchError    string     `json:"watchError,omitempty"`
	BehindSince   *time.Time `json:"behindSince,omitempty"`
	BehindAlerted bool       `json:"-"`
	// The instance Argus put in the database's read endpoint for this
	// replica, or ""; and why it is not in, when it should be
	// (db_read_endpoint.go).
	EndpointInstance string `json:"endpointInstance"`
	EndpointNote     string `json:"endpointNote"`
}

// ErrReplicaNotFound is a replica that does not exist.
var ErrReplicaNotFound = errors.New("no such replica")

const replicaCols = `r.id::text, r.database_id::text, d.dbname, pa.hostname, r.asset_id::text, ra.hostname, r.name,
	r.replica_address, r.primary_address, r.requested_by, r.reason, r.request_id::text, r.state, r.stage, r.detail,
	r.error, r.replica_database_id::text, r.created_at, r.started_at, r.finished_at,
	r.streaming, r.lag_s, r.lag_bytes, r.retained_bytes, r.watched_at, r.watch_error, r.behind_since, r.behind_alerted,
	r.endpoint_instance, r.endpoint_note`

const replicaFrom = ` FROM database_replicas r JOIN databases d ON d.id = r.database_id
	JOIN assets pa ON pa.id = d.asset_id JOIN assets ra ON ra.id = r.asset_id `

func scanReplica(row pgx.Row) (DatabaseReplica, error) {
	var r DatabaseReplica
	err := row.Scan(&r.ID, &r.DatabaseID, &r.DBName, &r.PrimaryHostname, &r.AssetID, &r.Hostname, &r.Name,
		&r.ReplicaAddress, &r.PrimaryAddress, &r.RequestedBy, &r.Reason, &r.RequestID, &r.State, &r.Stage, &r.Detail,
		&r.Error, &r.ReplicaDatabaseID, &r.CreatedAt, &r.StartedAt, &r.FinishedAt,
		&r.Streaming, &r.LagS, &r.LagBytes, &r.RetainedBytes, &r.WatchedAt, &r.WatchError, &r.BehindSince, &r.BehindAlerted,
		&r.EndpointInstance, &r.EndpointNote)
	return r, err
}

// CreateReplica records an ask. A name taken on that database is
// ErrReplicaTaken.
func (s *Store) CreateReplica(ctx context.Context, r DatabaseReplica) (DatabaseReplica, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `INSERT INTO database_replicas (database_id, asset_id, name, replica_address,
		primary_address, requested_by, reason) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7) RETURNING id::text`,
		r.DatabaseID, r.AssetID, r.Name, r.ReplicaAddress, r.PrimaryAddress, r.RequestedBy, r.Reason).Scan(&id)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return r, ErrReplicaTaken
	}
	if err != nil {
		return r, err
	}
	return s.Replica(ctx, id)
}

// ErrReplicaTaken is a replica name already used for that database.
var ErrReplicaTaken = errors.New("that database already has a replica by that name")

// Replica is one by id.
func (s *Store) Replica(ctx context.Context, id string) (DatabaseReplica, error) {
	r, err := scanReplica(s.db(ctx).QueryRow(ctx, `SELECT `+replicaCols+replicaFrom+`WHERE r.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return r, ErrReplicaNotFound
	}
	return r, err
}

// ReplicaByRequest is the replica an approval request stands for.
func (s *Store) ReplicaByRequest(ctx context.Context, requestID string) (DatabaseReplica, error) {
	r, err := scanReplica(s.db(ctx).QueryRow(ctx, `SELECT `+replicaCols+replicaFrom+`WHERE r.request_id = $1::uuid`, requestID))
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return r, ErrReplicaNotFound
	}
	return r, err
}

// Replicas lists a database's replicas, newest first.
func (s *Store) Replicas(ctx context.Context, databaseID string) ([]DatabaseReplica, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+replicaCols+replicaFrom+`WHERE r.database_id = $1::uuid ORDER BY r.created_at DESC`, databaseID)
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

// replicaBusyOn says whether a replica is asked for or being made on asset.
func (s *Store) replicaBusyOn(ctx context.Context, assetID string) (bool, error) {
	var n int
	err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM database_replicas WHERE asset_id = $1::uuid
		AND state IN ('pending', 'running', 'done')`, assetID).Scan(&n)
	return n > 0, err
}

func (s *Store) linkReplicaRequest(ctx context.Context, id, requestID string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET request_id = $2::uuid WHERE id = $1::uuid`, id, requestID)
	return err
}

// setReplica moves a replica on. A state of running stamps started_at; done,
// failed, denied and cancelled stamp finished_at.
func (s *Store) setReplica(ctx context.Context, id, state, stage, detail, errText string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET state = $2, stage = $3, detail = $4, error = $5,
		started_at = CASE WHEN $2 = 'running' THEN coalesce(started_at, now()) ELSE started_at END,
		finished_at = CASE WHEN $2 IN ('done', 'failed', 'denied', 'cancelled') THEN now() ELSE finished_at END
		WHERE id = $1::uuid`, id, state, stage, clip(detail, 500), clip(errText, 2000))
	return err
}

// claimReplica moves a pending replica to running, once.
func (s *Store) claimReplica(ctx context.Context, id string) (bool, error) {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET state = 'running', stage = 'checking', started_at = now()
		WHERE id = $1::uuid AND state = 'pending'`, id)
	return tag.RowsAffected() == 1, err
}

func (s *Store) setReplicaDatabase(ctx context.Context, id, dbID string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET replica_database_id = $2::uuid WHERE id = $1::uuid`, id, dbID)
	return err
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// GET /api/v1/databases/{id}/replicas
func (a *API) getReplicas(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	out, err := a.store.Replicas(r.Context(), db.ID)
	if err != nil {
		a.fail(w, "replicas", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/v1/databases/{id}/replicas: ask for a replica of db's server on
// another server. It waits for an approver.
func (a *API) postReplica(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	var in struct {
		AssetID        string `json:"assetId"`
		Name           string `json:"name"`
		ReplicaAddress string `json:"replicaAddress"`
		PrimaryAddress string `json:"primaryAddress"`
		Reason         string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	in.Name, in.Reason = strings.TrimSpace(in.Name), clip(strings.TrimSpace(in.Reason), 500)
	if in.Reason == "" {
		writeErr(w, http.StatusBadRequest, "say why, for the approver")
		return
	}
	if !tunnel.LocalPGHost(db.Host) {
		writeErr(w, http.StatusConflict, db.Name+" is reached through another server's PgBouncer; Argus copies only a Postgres it reaches on its own server")
		return
	}
	if err := (tunnel.PGReplica{Name: in.Name}).Check(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := a.store.AssetByID(ctx, in.AssetID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such server")
		return
	}
	if target.ID == db.AssetID {
		writeErr(w, http.StatusBadRequest, "a replica goes on another server than the primary")
		return
	}
	primaryAsset, err := a.store.AssetByID(ctx, db.AssetID)
	if err != nil {
		a.fail(w, "replica", err)
		return
	}
	if in.ReplicaAddress == "" {
		in.ReplicaAddress = target.Address
	}
	if in.PrimaryAddress == "" {
		in.PrimaryAddress = primaryAsset.Address
	}
	if _, err := netip.ParseAddr(in.ReplicaAddress); err != nil {
		writeErr(w, http.StatusBadRequest, "give the replica server's IP address as "+db.Hostname+" sees it")
		return
	}
	probe := tunnel.GrantReplica{PGReplica: tunnel.PGReplica{Name: in.Name}, PrimaryHost: in.PrimaryAddress,
		PrimaryPort: db.Port, PrimaryVersion: 16}
	if err := probe.Validate(tunnel.BackupJobReplicaClone); err != nil {
		writeErr(w, http.StatusBadRequest, "give the address "+target.Hostname+" reaches "+db.Hostname+" at")
		return
	}
	if busy, err := a.store.replicaBusyOn(ctx, target.ID); err != nil {
		a.fail(w, "replica", err)
		return
	} else if busy {
		writeErr(w, http.StatusConflict, target.Hostname+" is already a replica, or one is being made there")
		return
	}
	if !db.TunnelConnected || !target.TunnelConnected {
		writeErr(w, http.StatusConflict, "the agents on both servers must be connected")
		return
	}
	row, err := a.store.CreateReplica(ctx, DatabaseReplica{DatabaseID: db.ID, AssetID: target.ID, Name: in.Name,
		ReplicaAddress: in.ReplicaAddress, PrimaryAddress: in.PrimaryAddress, RequestedBy: email, Reason: in.Reason})
	if errors.Is(err, ErrReplicaTaken) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "replica", err)
		return
	}
	req, err := a.store.CreateRequest(ctx, AccessRequest{RequesterEmail: email,
		AssetHostnames: []string{db.Hostname, target.Hostname}, Principal: replicaPrincipal,
		Justification:   fmt.Sprintf("Copy %s's Postgres to %s as a streaming replica (%s): %s", db.Hostname, target.Hostname, in.Name, in.Reason),
		DurationMinutes: replicaApprovalMin})
	if err != nil {
		_ = a.store.setReplica(ctx, row.ID, "failed", "approval", "", err.Error())
		a.fail(w, "replica request", err)
		return
	}
	if err := a.store.linkReplicaRequest(ctx, row.ID, req.ID); err != nil {
		a.fail(w, "replica request", err)
		return
	}
	a.auditDB(ctx, "db.replica_requested", "notice", email, db.Hostname,
		fmt.Sprintf("Asked for a streaming replica of %s's Postgres on %s (%s): %s. Waiting for an approver (request %s).",
			db.Hostname, target.Hostname, in.Name, in.Reason, req.ID))
	if _, ok := a.approveByPolicy(ctx, req, db.Label,
		"Made a streaming replica of "+db.Hostname+"'s Postgres on "+target.Hostname); !ok {
		a.alertRequestWaiting(ctx, req)
	}
	row, _ = a.store.Replica(ctx, row.ID)
	writeJSON(w, http.StatusAccepted, row)
}

// replicaDecided runs a replica once its request is approved, or records
// that it was not. Called from the decision and cancel routes.
func (a *API) replicaDecided(ctx context.Context, requestID, decision, by, note string) {
	rep, err := a.store.ReplicaByRequest(ctx, requestID)
	if err != nil || rep.State != "pending" {
		return
	}
	switch decision {
	case "approved":
		a.auditDB(ctx, "db.replica_approved", "notice", by, rep.PrimaryHostname,
			fmt.Sprintf("Approved a replica of %s's Postgres on %s, asked by %s.", rep.PrimaryHostname, rep.Hostname, rep.RequestedBy))
		go a.RunReplica(context.WithoutCancel(ctx), rep)
	case "cancelled":
		_ = a.store.setReplica(ctx, rep.ID, "cancelled", "approval", "", "cancelled by "+by+" before it was decided")
	default:
		why := "denied by " + by
		if note != "" {
			why += ": " + note
		}
		_ = a.store.setReplica(ctx, rep.ID, "denied", "approval", "", why)
	}
}

/* ── The run ─────────────────────────────────────────────────────────────── */

// openAgentFunc opens a stream to an agent; a test plays the agents.
type openAgentFunc func(ctx context.Context, host string, hd tunnel.Header) (tunnel.Stream, tunnel.Answer, error)

func (a *API) agentOpener() openAgentFunc {
	if a.openAgent != nil {
		return a.openAgent
	}
	return a.Tunnels.OpenWith
}

// replicaJob runs one job on db's server: the grant, the stream, its
// frames. onProgress sees progress and may write to the stream through
// send.
func (a *API) replicaJob(ctx context.Context, email string, db Database, b tunnel.GrantBackup,
	onProgress func(p tunnel.BackupProgress, send func(tunnel.BackupRequest) error)) (tunnel.BackupResult, error) {
	grant, _, err := a.backupGrant(ctx, email, db, b)
	if err != nil {
		return tunnel.BackupResult{}, err
	}
	octx, cancel := context.WithTimeout(ctx, backupOpenWait)
	st, ans, err := a.agentOpener()(octx, db.agent(), tunnel.Header{Service: tunnel.BackupService, Grant: grant})
	cancel()
	switch {
	case errors.Is(err, ErrNoTunnel):
		return tunnel.BackupResult{}, fmt.Errorf("the agent on %s is not connected", db.Hostname)
	case tunnel.IsRefused(err):
		return tunnel.BackupResult{}, fmt.Errorf("the agent on %s refused: %s", db.Hostname, ans.Error)
	case err != nil:
		return tunnel.BackupResult{}, fmt.Errorf("could not reach the agent on %s: %w", db.Hostname, err)
	}
	defer st.Close()
	var wmu sync.Mutex
	send := func(req tunnel.BackupRequest) error {
		wmu.Lock()
		defer wmu.Unlock()
		return tunnel.WriteFrame(st, tunnel.FrameBackupRequest, tunnel.EncodeBackupMessage(req))
	}
	defer cancelOnDone(ctx, st, &wmu)()
	return backupFrames(st, func(p tunnel.BackupProgress) {
		if onProgress != nil {
			onProgress(p, send)
		}
	}, nil)
}

// primaryOp runs one replica operation on the primary and reads its answer
// into out.
func (a *API) primaryOp(ctx context.Context, rep DatabaseReplica, primary Database, op, verifier string, out any) error {
	res, err := a.replicaJob(ctx, rep.RequestedBy, primary, tunnel.GrantBackup{Job: tunnel.BackupJobReplicaPrimary,
		Replica: &tunnel.GrantReplica{Op: op, PGReplica: tunnel.PGReplica{Name: rep.Name, Address: rep.ReplicaAddress,
			Verifier: verifier, By: rep.RequestedBy}}, Reason: "replica " + rep.Name}, nil)
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	if out != nil && len(res.Replica) > 0 {
		return json.Unmarshal(res.Replica, out)
	}
	return nil
}

// RunReplica makes an approved replica. It returns when it has ended.
func (a *API) RunReplica(ctx context.Context, rep DatabaseReplica) {
	if ok, err := a.store.claimReplica(ctx, rep.ID); err != nil || !ok {
		return
	}
	fail := func(stage string, err error) {
		_ = a.store.setReplica(ctx, rep.ID, "failed", stage, "", err.Error())
		a.auditDB(ctx, "db.replica_failed", "warning", rep.RequestedBy, rep.PrimaryHostname,
			fmt.Sprintf("The replica of %s's Postgres on %s (%s) failed while %s: %v", rep.PrimaryHostname, rep.Hostname, rep.Name, stage, err))
	}
	primary, err := a.store.Database(ctx, rep.DatabaseID)
	if err != nil {
		fail("starting", err)
		return
	}
	agent, err := a.store.AgentNameForAsset(ctx, rep.AssetID)
	if err != nil {
		fail("starting", fmt.Errorf("%s has no agent: %w", rep.Hostname, err))
		return
	}
	// The replica's Postgres, as the agent there reaches it: the same
	// socket and port as the primary's, which a server set up the same way
	// has.
	there := Database{ID: rep.ID, AssetID: rep.AssetID, Hostname: rep.Hostname, Engine: "postgres", Host: primary.Host,
		Port: primary.Port, Name: "postgres", AdminUser: primary.AdminUser, AgentHost: agent}

	// 1. The primary's settings.
	var check tunnel.PGReplicaCheck
	if err := a.primaryOp(ctx, rep, primary, tunnel.ReplicaCheck, "", &check); err != nil {
		fail("checking", err)
		return
	}
	var blocking []string
	for _, m := range check.Missing {
		if !strings.Contains(m, "pg_hba.conf") {
			blocking = append(blocking, m)
		}
	}
	if len(blocking) > 0 {
		fail("checking", fmt.Errorf("%s needs these first, and Argus never restarts a primary: %s", rep.PrimaryHostname, strings.Join(blocking, "; ")))
		return
	}

	// 2-4. The clone, which stops at the verifier for the primary to be
	// prepared.
	prepared := false
	var prepErr error
	_ = a.store.setReplica(ctx, rep.ID, "running", "copying", "", "")
	res, err := a.replicaJob(ctx, rep.RequestedBy, there, tunnel.GrantBackup{Job: tunnel.BackupJobReplicaClone,
		Replica: &tunnel.GrantReplica{PGReplica: tunnel.PGReplica{Name: rep.Name}, PrimaryHost: rep.PrimaryAddress,
			PrimaryPort: primary.Port, PrimaryVersion: check.VersionNum / 10000}, Reason: "replica " + rep.Name},
		func(p tunnel.BackupProgress, send func(tunnel.BackupRequest) error) {
			if p.Stage != tunnel.ReplicaStageVerifier {
				// pg_basebackup's own lines come without a stage.
				stage := p.Stage
				if stage == "" {
					stage = "copying"
				}
				_ = a.store.setReplica(ctx, rep.ID, "running", stage, p.Detail, "")
				return
			}
			if prepared || prepErr != nil {
				return
			}
			_ = a.store.setReplica(ctx, rep.ID, "running", "preparing", "the replication role and slot on "+rep.PrimaryHostname, "")
			if prepErr = a.primaryOp(ctx, rep, primary, tunnel.ReplicaPrepare, p.Detail, nil); prepErr == nil && !check.HBAOK {
				prepErr = a.primaryOp(ctx, rep, primary, tunnel.ReplicaAllow, "", nil)
			}
			if prepErr != nil {
				_ = send(tunnel.BackupRequest{Op: "cancel"})
				return
			}
			prepared = true
			_ = send(tunnel.BackupRequest{Op: "go"})
		})
	if err == nil && !res.OK {
		err = errors.New(res.Error)
	}
	if prepErr != nil {
		err = fmt.Errorf("preparing %s: %w", rep.PrimaryHostname, prepErr)
	}
	if err != nil {
		if prepared || prepErr != nil {
			if derr := a.primaryOp(context.WithoutCancel(ctx), rep, primary, tunnel.ReplicaDrop, "", nil); derr != nil {
				err = fmt.Errorf("%w; taking the role and slot off %s again failed too: %v", err, rep.PrimaryHostname, derr)
			}
		}
		stage := res.Stage
		if stage == "" {
			stage = "copying"
		}
		fail(stage, err)
		return
	}
	var cloned tunnel.PGReplicaCloned
	_ = json.Unmarshal(res.Replica, &cloned)

	// 5. Into Argus, with the primary's access capped at read.
	added, err := a.store.CreateDatabase(ctx, Database{AssetID: rep.AssetID, Engine: "postgres", Host: primary.Host,
		Port: primary.Port, Name: primary.Name, AdminUser: primary.AdminUser, Label: primary.Label}, rep.RequestedBy)
	if err != nil {
		fail("adding to Argus", fmt.Errorf("the replica is streaming, but adding it to Argus failed: %w", err))
		return
	}
	_ = a.store.setReplicaDatabase(ctx, rep.ID, added.ID)
	given := a.copyAccess(ctx, primary.ID, added, rep.RequestedBy)
	detail := fmt.Sprintf("Postgres %d, streaming from %s; the server's own empty data directory is kept at %s", cloned.Version/10000,
		rep.PrimaryHostname, cloned.Kept)
	_ = a.store.setReplica(ctx, rep.ID, "done", "done", detail, "")
	msg := fmt.Sprintf("%s's Postgres is now a streaming replica of %s's (%s), added to Argus as %s on %s.",
		rep.Hostname, rep.PrimaryHostname, rep.Name, primary.Name, rep.Hostname)
	if len(given) > 0 {
		msg += " It took " + primary.Name + "'s access, capped at read: " + strings.Join(given, ", ") + "."
	}
	a.auditDB(ctx, "db.replica_done", "notice", rep.RequestedBy, rep.Hostname, msg)
}
