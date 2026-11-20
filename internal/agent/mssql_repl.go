package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
SQL Server replication, as the admin login: what the database's
availability group, publications and log shipping look like, and the fixed
changes in tunnel/mssqlrepl.go. A change is recorded with its placeholders
and run with them filled in, so no password reaches the recording.
*/

// MSReplication answers ms.replication.
type MSReplication struct {
	Server       string `json:"server"`
	Edition      string `json:"edition"`
	HadrEnabled  bool   `json:"hadrEnabled"`
	Recovery     string `json:"recovery"`
	Distributor  bool   `json:"distributor"`
	Distribution string `json:"distribution,omitempty"`
	// ReplDir is where a snapshot folder would go by default.
	ReplDir    string `json:"replDir,omitempty"`
	Published  bool   `json:"published"`
	Subscribed bool   `json:"subscribed"`

	AG *MSAG `json:"ag,omitempty"`
	// Groups are the availability groups this replica is primary of, that
	// the database could be added to.
	Groups []MSGroup `json:"groups,omitempty"`

	Publications []MSPublication `json:"publications,omitempty"`
	Tables       []MSReplTable   `json:"tables,omitempty"`
	Databases    []string        `json:"databases,omitempty"`

	LogShipping *MSLogShipping `json:"logShipping,omitempty"`
}

// MSGroup is an availability group on this server.
type MSGroup struct {
	Name        string `json:"name"`
	ClusterType string `json:"clusterType"`
}

// MSAG is the availability group the database is in.
type MSAG struct {
	Group       string      `json:"group"`
	ClusterType string      `json:"clusterType"`
	LocalRole   string      `json:"localRole"`
	Replicas    []MSReplica `json:"replicas"`
}

// MSReplica is one replica of the database's group, and the database's
// state there.
type MSReplica struct {
	Server    string `json:"server"`
	Local     bool   `json:"local,omitempty"`
	Role      string `json:"role"`
	Mode      string `json:"mode"`
	Failover  string `json:"failover"`
	Health    string `json:"health"`
	Connected string `json:"connected"`
	SyncState string `json:"syncState"`
	Suspended string `json:"suspended,omitempty"`
	// SendKB and RedoKB are the log not yet sent to, or not yet redone on,
	// the replica.
	SendKB     int64  `json:"sendKb"`
	RedoKB     int64  `json:"redoKb"`
	LastCommit string `json:"lastCommit,omitempty"`
	// LagS is how far behind the primary's last commit this replica's is.
	LagS *int64 `json:"lagS,omitempty"`
}

// MSPublication is one of the database's publications.
type MSPublication struct {
	Name          string           `json:"name"`
	Kind          string           `json:"kind"`
	Active        bool             `json:"active"`
	Articles      []MSArticle      `json:"articles"`
	Subscriptions []MSSubscription `json:"subscriptions"`
	Snapshot      *MSAgentRun      `json:"snapshot,omitempty"`
}

// MSArticle is a published table.
type MSArticle struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
	Table  string `json:"table"`
}

// MSSubscription is a subscription to a publication.
type MSSubscription struct {
	Subscriber string      `json:"subscriber"`
	DB         string      `json:"db"`
	Status     string      `json:"status"`
	Agent      *MSAgentRun `json:"agent,omitempty"`
}

// MSAgentRun is a replication agent's last word.
type MSAgentRun struct {
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	At        string `json:"at,omitempty"`
	LatencyMS int64  `json:"latencyMs,omitempty"`
}

// MSReplTable is a table that could be published.
type MSReplTable struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// PK: transactional replication publishes only tables with one.
	PK bool `json:"pk"`
}

// MSLogShipping is the log shipping the database takes part in.
type MSLogShipping struct {
	Role string `json:"role"`
	// As primary.
	LastBackup     string   `json:"lastBackup,omitempty"`
	LastBackupFile string   `json:"lastBackupFile,omitempty"`
	BackupAlertMin int      `json:"backupAlertMin,omitempty"`
	Secondaries    []string `json:"secondaries,omitempty"`
	// As secondary.
	Primary         string `json:"primary,omitempty"`
	LastCopied      string `json:"lastCopied,omitempty"`
	LastRestored    string `json:"lastRestored,omitempty"`
	RestoreLagMin   int    `json:"restoreLagMin,omitempty"`
	RestoreAlertMin int    `json:"restoreAlertMin,omitempty"`
}

const msReplTables = 500

// msAgentStatus words a replication agent's runstatus.
func msAgentStatus(n int) string {
	return map[int]string{1: "started", 2: "succeeded", 3: "in progress", 4: "idle", 5: "retrying", 6: "failed"}[n]
}

// replOp runs one replication operation for a DBA session.
func (s *mssqlSession) replOp(ctx context.Context, req tunnel.PGRequest) error {
	op := req.Admin
	text, err := tunnel.MSSQLReplSQL(op, s.db.Name)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	if op.Preview {
		b, _ := json.Marshal(PGAdminDone{SQL: text})
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b})
	}
	start := time.Now()
	limit := 90 * time.Second
	if op.Kind == tunnel.AdminMSReplDistributor || op.Kind == tunnel.AdminMSAGAdd {
		limit = 10 * time.Minute
	}
	actx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	admin, err := mssqlAdminConn(actx, s.db.Host, s.db.Port, s.db.ConfigPath)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: err.Error()})
	}
	defer admin.close()
	c, err := admin.db.Conn(actx)
	if err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}
	defer c.Close()
	if _, err := c.ExecContext(actx, "USE "+tunnel.MSSQLIdent(s.db.Name)); err != nil {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
	}
	r := op.MSRepl
	if r == nil {
		r = &tunnel.MSSQLReplSpec{}
	}

	if !op.Mutates() {
		data, err := msReplicationOf(actx, c)
		if err != nil {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
		}
		b, _ := json.Marshal(data)
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, MS: time.Since(start).Milliseconds()})
	}

	if why := msReplReach(actx, c, op.Kind, r); why != "" {
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: why})
	}
	// Recorded with its placeholders, before it runs.
	if err := s.rec.write(tunnel.PGRecord{Kind: "admin", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	run := strings.NewReplacer(
		tunnel.MSAdminLogin, tunnel.MSSQLString(admin.user),
		tunnel.MSAdminPassword, tunnel.MSSQLString(admin.pw),
		tunnel.MSSubPassword, tunnel.MSSQLString(r.Password),
	).Replace(text)
	// Availability group DDL runs from master.
	switch op.Kind {
	case tunnel.AdminMSAGAdd, tunnel.AdminMSAGRemove, tunnel.AdminMSAGFailover, tunnel.AdminMSAGSuspend, tunnel.AdminMSAGResume:
		if _, err := c.ExecContext(actx, "USE master"); err != nil {
			return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: mssqlMessage(err)})
		}
	}
	// Dropping the database's last publication, or turning publishing
	// off, clears its log for replication, which a running Log Reader
	// Agent holds: stop the agent first, as SSMS does.
	if msEndsLogReading(actx, c, op.Kind, r) {
		msStopLogReader(actx, c)
	}
	// One batch, outside a transaction: the replication procedures and
	// ALTER AVAILABILITY GROUP refuse one.
	_, err = c.ExecContext(actx, run)
	if err != nil && msLogReaderHeld(err) {
		// The agent's connection can outlive its job by a moment.
		select {
		case <-actx.Done():
		case <-time.After(5 * time.Second):
			_, err = c.ExecContext(actx, run)
		}
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		msg := msAllMessages(err)
		// SQL Server quotes a failing statement back now and then; never
		// with a password in it.
		for _, pw := range []string{admin.pw, r.Password} {
			if pw != "" {
				msg = strings.ReplaceAll(msg, pw, tunnel.PasswordRedacted)
			}
		}
		if errors.Is(actx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("stopped after %s (%s)", limit, msg)
		}
		if rerr := s.rec.write(tunnel.PGRecord{Kind: "error", Request: req.ID, Error: msg, MS: elapsed}); rerr != nil {
			return rerr
		}
		return s.send(tunnel.PGAnswer{ID: req.ID, Type: "error", Error: msg, MS: elapsed})
	}
	tag := msTag([]string{text})
	if err := s.rec.write(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, MS: elapsed}); err != nil {
		return err
	}
	b, _ := json.Marshal(PGAdminDone{SQL: text, Tag: tag})
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "admin", Admin: b, Tag: tag, MS: elapsed})
}

// msReplReach refuses a change that does not fit where the database is:
// adding it to a group this replica is not primary of, failing over from
// the primary, publishing with no distributor, and so on. SQL Server would
// refuse most of these too, in fewer words.
func msReplReach(ctx context.Context, c *sql.Conn, kind string, r *tunnel.MSSQLReplSpec) string {
	one := func(q string, args ...any) (int, error) {
		var n int
		err := c.QueryRowContext(ctx, q, args...).Scan(&n)
		return n, err
	}
	check := func(q string, why string, args ...any) string {
		n, err := one(q, args...)
		if err != nil {
			return mssqlMessage(err)
		}
		if n == 0 {
			return why
		}
		return ""
	}
	inGroup := `SELECT COUNT(*) FROM sys.availability_databases_cluster adc JOIN sys.availability_groups ag ON ag.group_id = adc.group_id
	  WHERE adc.database_name = DB_NAME() AND ag.name = @p1`
	localRole := `SELECT COUNT(*) FROM sys.availability_groups ag JOIN sys.dm_hadr_availability_replica_states rs
	  ON rs.group_id = ag.group_id AND rs.is_local = 1 WHERE ag.name = @p1 AND rs.role_desc = @p2`
	switch kind {
	case tunnel.AdminMSAGAdd:
		if n, err := one(`SELECT CAST(ISNULL(SERVERPROPERTY('IsHadrEnabled'), 0) AS int)`); err != nil || n == 0 {
			return "Always On availability groups are not enabled on this server"
		}
		if why := check(localRole, "this server is not the primary of "+r.Group, r.Group, "PRIMARY"); why != "" {
			return why
		}
		if why := check(`SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.availability_databases_cluster WHERE database_name = DB_NAME()) THEN 0 ELSE 1 END`,
			"the database is in an availability group already"); why != "" {
			return why
		}
		return check(`SELECT CASE WHEN recovery_model_desc = 'FULL' THEN 1 ELSE 0 END FROM sys.databases WHERE database_id = DB_ID()`,
			"an availability group needs the full recovery model; change it in Settings first")
	case tunnel.AdminMSAGRemove:
		return check(inGroup, "the database is not in "+r.Group, r.Group)
	case tunnel.AdminMSAGSuspend, tunnel.AdminMSAGResume:
		return check(`SELECT COUNT(*) FROM sys.dm_hadr_database_replica_states WHERE database_id = DB_ID() AND is_local = 1`,
			"the database is not in an availability group")
	case tunnel.AdminMSAGFailover:
		if why := check(inGroup, "the database is not in "+r.Group, r.Group); why != "" {
			return why
		}
		return check(localRole, "fail over from the replica that is to become primary: this one is not a secondary of "+r.Group, r.Group, "SECONDARY")
	case tunnel.AdminMSReplDistributor:
		return check(`SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.servers WHERE name = 'repl_distributor') THEN 0 ELSE 1 END`,
			"this server has a distributor already")
	case tunnel.AdminMSReplPublish:
		if r.Enabled != nil && *r.Enabled {
			return check(`SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.servers WHERE name = 'repl_distributor') THEN 1 ELSE 0 END`,
				"this server has no distributor; set one up first")
		}
	case tunnel.AdminMSReplPubAdd, tunnel.AdminMSReplArticleAdd:
		if why := check(`SELECT CAST(is_published AS int) FROM sys.databases WHERE database_id = DB_ID()`,
			"publishing is off for this database; turn it on first"); why != "" {
			return why
		}
		if kind == tunnel.AdminMSReplArticleAdd {
			return check(`SELECT COUNT(*) FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id WHERE s.name = @p1 AND t.name = @p2`,
				"there is no table "+r.Schema+"."+r.Table+" in this database", r.Schema, r.Table)
		}
	case tunnel.AdminMSReplSubAdd:
		if r.Subscriber == "" {
			return check(`SELECT COUNT(*) FROM sys.databases WHERE name = @p1`,
				"there is no database "+r.SubscriberDB+" on this server to subscribe; create it first", r.SubscriberDB)
		}
	case tunnel.AdminMSLSBackup:
		return check(`SELECT COUNT(*) FROM msdb.dbo.log_shipping_primary_databases WHERE primary_database = DB_NAME()`,
			"the database is not a log shipping primary on this server")
	case tunnel.AdminMSLSCopy, tunnel.AdminMSLSRestore:
		return check(`SELECT COUNT(*) FROM msdb.dbo.log_shipping_secondary_databases WHERE secondary_database = DB_NAME()`,
			"the database is not a log shipping secondary on this server")
	}
	return ""
}

// msReplicationOf reads the database's replication. Each part reads what
// the server has; a part this server does not have is left out.
func msReplicationOf(ctx context.Context, c *sql.Conn) (*MSReplication, error) {
	out := &MSReplication{}
	var hadr, dist, published, subscribed int
	var replDir, distDB sql.NullString
	err := c.QueryRowContext(ctx, `SELECT @@SERVERNAME, CAST(SERVERPROPERTY('Edition') AS nvarchar(128)),
	  CAST(ISNULL(SERVERPROPERTY('IsHadrEnabled'), 0) AS int), d.recovery_model_desc,
	  CASE WHEN EXISTS (SELECT 1 FROM sys.servers WHERE name = 'repl_distributor') THEN 1 ELSE 0 END,
	  (SELECT TOP (1) name FROM sys.databases WHERE is_distributor = 1),
	  CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(4000)),
	  CAST(d.is_published AS int), CAST(d.is_subscribed AS int)
	FROM sys.databases d WHERE d.database_id = DB_ID()`).
		Scan(&out.Server, &out.Edition, &hadr, &out.Recovery, &dist, &distDB, &replDir, &published, &subscribed)
	if err != nil {
		return nil, err
	}
	out.HadrEnabled, out.Distributor, out.Published, out.Subscribed = hadr == 1, dist == 1, published == 1, subscribed == 1
	out.Distribution = distDB.String
	if replDir.Valid && replDir.String != "" {
		d := strings.TrimRight(replDir.String, `/\`)
		sep := "/"
		if strings.Contains(d, `\`) {
			sep = `\`
		}
		if i := strings.LastIndexAny(d, `/\`); i > 0 && strings.EqualFold(d[i+1:], "data") {
			d = d[:i]
		}
		out.ReplDir = d + sep + "ReplData"
	}
	if out.HadrEnabled {
		if err := msAGOf(ctx, c, out); err != nil {
			return nil, err
		}
	}
	if err := msPublicationsOf(ctx, c, out); err != nil {
		return nil, err
	}
	if err := msLogShippingOf(ctx, c, out); err != nil {
		return nil, err
	}
	return out, nil
}

func msAGOf(ctx context.Context, c *sql.Conn, out *MSReplication) error {
	rows, err := c.QueryContext(ctx, `SELECT ag.name, ISNULL(ag.cluster_type_desc, ''), ar.replica_server_name, CAST(ISNULL(rs.is_local, 0) AS int),
	  ISNULL(rs.role_desc, ''), ar.availability_mode_desc, ar.failover_mode_desc, ISNULL(rs.synchronization_health_desc, ''),
	  ISNULL(rs.connected_state_desc, ''), ISNULL(drs.synchronization_state_desc, ''), ISNULL(drs.suspend_reason_desc, ''),
	  CAST(ISNULL(drs.is_suspended, 0) AS int), ISNULL(drs.log_send_queue_size, 0), ISNULL(drs.redo_queue_size, 0), drs.last_commit_time
	FROM sys.availability_databases_cluster adc
	JOIN sys.availability_groups ag ON ag.group_id = adc.group_id
	JOIN sys.availability_replicas ar ON ar.group_id = ag.group_id
	LEFT JOIN sys.dm_hadr_availability_replica_states rs ON rs.replica_id = ar.replica_id
	LEFT JOIN sys.dm_hadr_database_replica_states drs ON drs.replica_id = ar.replica_id AND drs.group_database_id = adc.group_database_id
	WHERE adc.database_name = DB_NAME()
	ORDER BY CASE WHEN rs.role_desc = 'PRIMARY' THEN 0 ELSE 1 END, ar.replica_server_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var primary *time.Time
	var commits []*time.Time
	for rows.Next() {
		var g, ct string
		var rep MSReplica
		var local, suspended int
		var last sql.NullTime
		if err := rows.Scan(&g, &ct, &rep.Server, &local, &rep.Role, &rep.Mode, &rep.Failover, &rep.Health, &rep.Connected,
			&rep.SyncState, &rep.Suspended, &suspended, &rep.SendKB, &rep.RedoKB, &last); err != nil {
			return err
		}
		if out.AG == nil {
			out.AG = &MSAG{Group: g, ClusterType: ct}
		}
		rep.Local = local == 1
		if suspended == 0 {
			rep.Suspended = ""
		} else if rep.Suspended == "" {
			rep.Suspended = "suspended"
		}
		if rep.Local {
			out.AG.LocalRole = rep.Role
		}
		rep.LastCommit = msTime(last)
		var t *time.Time
		if last.Valid {
			tt := last.Time
			t = &tt
			if rep.Role == "PRIMARY" {
				primary = t
			}
		}
		commits = append(commits, t)
		out.AG.Replicas = append(out.AG.Replicas, rep)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if out.AG != nil && primary != nil {
		for i := range out.AG.Replicas {
			if out.AG.Replicas[i].Role != "PRIMARY" && commits[i] != nil {
				lag := int64(primary.Sub(*commits[i]).Seconds())
				if lag < 0 {
					lag = 0
				}
				out.AG.Replicas[i].LagS = &lag
			}
		}
	}
	if out.AG == nil {
		rows, err := c.QueryContext(ctx, `SELECT ag.name, ISNULL(ag.cluster_type_desc, '') FROM sys.availability_groups ag
		  JOIN sys.dm_hadr_availability_replica_states rs ON rs.group_id = ag.group_id AND rs.is_local = 1
		  WHERE rs.role_desc = 'PRIMARY' ORDER BY ag.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g MSGroup
			if err := rows.Scan(&g.Name, &g.ClusterType); err != nil {
				return err
			}
			out.Groups = append(out.Groups, g)
		}
		return rows.Err()
	}
	return nil
}

func msPublicationsOf(ctx context.Context, c *sql.Conn, out *MSReplication) error {
	// What could be published, and where to.
	rows, err := c.QueryContext(ctx, `SELECT TOP (@p1) s.name, t.name,
	  CASE WHEN EXISTS (SELECT 1 FROM sys.indexes i WHERE i.object_id = t.object_id AND i.is_primary_key = 1) THEN 1 ELSE 0 END
	FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id WHERE t.is_ms_shipped = 0 ORDER BY s.name, t.name`, msReplTables)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t MSReplTable
		var pk int
		if err := rows.Scan(&t.Schema, &t.Name, &pk); err != nil {
			rows.Close()
			return err
		}
		t.PK = pk == 1
		out.Tables = append(out.Tables, t)
	}
	rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT name FROM sys.databases WHERE database_id > 4 AND database_id <> DB_ID() AND is_distributor = 0 ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		out.Databases = append(out.Databases, n)
	}
	rows.Close()

	var has int
	if err := c.QueryRowContext(ctx, `SELECT CASE WHEN OBJECT_ID('dbo.syspublications') IS NULL THEN 0 ELSE 1 END`).Scan(&has); err != nil || has == 0 {
		return err
	}
	byID := map[int]int{}
	rows, err = c.QueryContext(ctx, `SELECT pubid, name, repl_freq, status FROM dbo.syspublications ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, freq, status int
		var p MSPublication
		if err := rows.Scan(&id, &p.Name, &freq, &status); err != nil {
			rows.Close()
			return err
		}
		p.Kind, p.Active = "transactional", status == 1
		if freq == 1 {
			p.Kind = "snapshot"
		}
		p.Articles, p.Subscriptions = []MSArticle{}, []MSSubscription{}
		byID[id] = len(out.Publications)
		out.Publications = append(out.Publications, p)
	}
	rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT pubid, name, ISNULL(OBJECT_SCHEMA_NAME(objid), ''), ISNULL(OBJECT_NAME(objid), '') FROM dbo.sysarticles ORDER BY name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		var a MSArticle
		if err := rows.Scan(&id, &a.Name, &a.Schema, &a.Table); err != nil {
			rows.Close()
			return err
		}
		if i, ok := byID[id]; ok {
			out.Publications[i].Articles = append(out.Publications[i].Articles, a)
		}
	}
	rows.Close()
	rows, err = c.QueryContext(ctx, `SELECT DISTINCT a.pubid, s.srvname, s.dest_db, s.status FROM dbo.syssubscriptions s
	  JOIN dbo.sysarticles a ON a.artid = s.artid WHERE s.srvid >= 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, status int
		var sub MSSubscription
		if err := rows.Scan(&id, &sub.Subscriber, &sub.DB, &status); err != nil {
			rows.Close()
			return err
		}
		sub.Status = map[int]string{0: "inactive", 1: "subscribed", 2: "active"}[status]
		if i, ok := byID[id]; ok {
			out.Publications[i].Subscriptions = append(out.Publications[i].Subscriptions, sub)
		}
	}
	rows.Close()
	if out.Distribution != "" {
		msAgentRuns(ctx, c, out)
	}
	return nil
}

// msAgentRuns adds each agent's last run, from the distribution database
// on this server. A distributor elsewhere is not read.
func msAgentRuns(ctx context.Context, c *sql.Conn, out *MSReplication) {
	d := tunnel.MSSQLIdent(out.Distribution)
	find := func(pub string) *MSPublication {
		for i := range out.Publications {
			if out.Publications[i].Name == pub {
				return &out.Publications[i]
			}
		}
		return nil
	}
	// Matched on the publication and the subscriber's database: which
	// server an agent delivers to is recorded differently across versions.
	rows, err := c.QueryContext(ctx, `SELECT a.publication, a.subscriber_db, ISNULL(h.runstatus, 0),
	  ISNULL(h.comments, ''), h.time, ISNULL(h.delivery_latency, 0)
	FROM `+d+`.dbo.MSdistribution_agents a
	OUTER APPLY (SELECT TOP (1) runstatus, comments, time, delivery_latency FROM `+d+`.dbo.MSdistribution_history
	  WHERE agent_id = a.id ORDER BY time DESC) h
	WHERE a.publisher_db = DB_NAME()`)
	if err == nil {
		for rows.Next() {
			var pub, db, msg string
			var st int
			var at sql.NullTime
			var lat int64
			if rows.Scan(&pub, &db, &st, &msg, &at, &lat) != nil {
				break
			}
			p := find(pub)
			if p == nil {
				continue
			}
			for i := range p.Subscriptions {
				s := &p.Subscriptions[i]
				if s.DB == db {
					s.Agent = &MSAgentRun{Status: msAgentStatus(st), Message: msg, At: msTime(at), LatencyMS: lat}
				}
			}
		}
		rows.Close()
	}
	rows, err = c.QueryContext(ctx, `SELECT a.publication, ISNULL(h.runstatus, 0), ISNULL(h.comments, ''), h.time
	FROM `+d+`.dbo.MSsnapshot_agents a
	OUTER APPLY (SELECT TOP (1) runstatus, comments, time FROM `+d+`.dbo.MSsnapshot_history WHERE agent_id = a.id ORDER BY time DESC) h
	WHERE a.publisher_db = DB_NAME()`)
	if err == nil {
		for rows.Next() {
			var pub, msg string
			var st int
			var at sql.NullTime
			if rows.Scan(&pub, &st, &msg, &at) != nil {
				break
			}
			if p := find(pub); p != nil {
				p.Snapshot = &MSAgentRun{Status: msAgentStatus(st), Message: msg, At: msTime(at)}
			}
		}
		rows.Close()
	}
}

func msLogShippingOf(ctx context.Context, c *sql.Conn, out *MSReplication) error {
	var file sql.NullString
	var last sql.NullTime
	var alert sql.NullInt64
	err := c.QueryRowContext(ctx, `SELECT TOP (1) m.last_backup_file, m.last_backup_date, m.backup_threshold
	FROM msdb.dbo.log_shipping_primary_databases p
	LEFT JOIN msdb.dbo.log_shipping_monitor_primary m ON m.primary_id = p.primary_id
	WHERE p.primary_database = DB_NAME()`).Scan(&file, &last, &alert)
	switch {
	case err == nil:
		ls := &MSLogShipping{Role: "primary", LastBackup: msTime(last), LastBackupFile: file.String, BackupAlertMin: int(alert.Int64)}
		rows, err := c.QueryContext(ctx, `SELECT ps.secondary_server + N'.' + ps.secondary_database FROM msdb.dbo.log_shipping_primary_secondaries ps
		  JOIN msdb.dbo.log_shipping_primary_databases p ON p.primary_id = ps.primary_id WHERE p.primary_database = DB_NAME()`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			ls.Secondaries = append(ls.Secondaries, s)
		}
		rows.Close()
		out.LogShipping = ls
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	var primary string
	var copied, restored sql.NullTime
	var lag, ralert sql.NullInt64
	err = c.QueryRowContext(ctx, `SELECT TOP (1) s.primary_server + N'.' + s.primary_database, m.last_copied_date, m.last_restored_date,
	  m.last_restored_latency, m.restore_threshold
	FROM msdb.dbo.log_shipping_secondary_databases d
	JOIN msdb.dbo.log_shipping_secondary s ON s.secondary_id = d.secondary_id
	LEFT JOIN msdb.dbo.log_shipping_monitor_secondary m ON m.secondary_id = s.secondary_id AND m.secondary_database = d.secondary_database
	WHERE d.secondary_database = DB_NAME()`).Scan(&primary, &copied, &restored, &lag, &ralert)
	switch {
	case err == nil:
		out.LogShipping = &MSLogShipping{Role: "secondary", Primary: primary, LastCopied: msTime(copied), LastRestored: msTime(restored),
			RestoreLagMin: int(lag.Int64), RestoreAlertMin: int(ralert.Int64)}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	return nil
}

// msLogReaderHeld says SQL Server refused because a Log Reader Agent is
// connected to the database's log (error 18752).
func msLogReaderHeld(err error) bool {
	var me mssql.Error
	return errors.As(err, &me) && me.Number == 18752
}

// msEndsLogReading says op ends the database's log reading: it drops the
// last publication, or turns publishing off.
func msEndsLogReading(ctx context.Context, c *sql.Conn, kind string, r *tunnel.MSSQLReplSpec) bool {
	switch kind {
	case tunnel.AdminMSReplPublish:
		return r.Enabled != nil && !*r.Enabled
	case tunnel.AdminMSReplPubDrop:
		var n int
		if c.QueryRowContext(ctx, `SELECT CASE WHEN OBJECT_ID('dbo.syspublications') IS NULL THEN 0 ELSE (SELECT COUNT(*) FROM dbo.syspublications) END`).Scan(&n) != nil {
			return false
		}
		return n <= 1
	}
	return false
}

// msStopLogReader stops the database's Log Reader Agent job, on a
// distributor on this server, and waits for it to stop. It says whether
// there was one to stop.
func msStopLogReader(ctx context.Context, c *sql.Conn) bool {
	var dist sql.NullString
	if c.QueryRowContext(ctx, `SELECT TOP (1) name FROM sys.databases WHERE is_distributor = 1`).Scan(&dist) != nil || !dist.Valid {
		return false
	}
	var job sql.NullString
	if c.QueryRowContext(ctx, `SELECT TOP (1) CAST(CAST(job_id AS uniqueidentifier) AS nvarchar(36)) FROM `+tunnel.MSSQLIdent(dist.String)+
		`.dbo.MSlogreader_agents WHERE publisher_db = DB_NAME()`).Scan(&job) != nil || !job.Valid {
		return false
	}
	running := func() bool {
		var n int
		_ = c.QueryRowContext(ctx, `SELECT COUNT(*) FROM msdb.dbo.sysjobactivity WHERE job_id = @p1
		  AND start_execution_date IS NOT NULL AND stop_execution_date IS NULL
		  AND session_id = (SELECT MAX(session_id) FROM msdb.dbo.syssessions)`, job.String).Scan(&n)
		return n > 0
	}
	if running() {
		_, _ = c.ExecContext(ctx, `EXEC msdb.dbo.sp_stop_job @job_id = @p1`, job.String)
	}
	for i := 0; i < 30 && running(); i++ {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	// The agent's connection can outlive its job by a moment.
	time.Sleep(2 * time.Second)
	return true
}
