package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Read replicas of RDS databases (step 8 of the cloud scaling design). Adding
one is a Change-level cloud change with the usual plan, approval and apply;
removing one is Remove level, and only for a replica Argus made. Argus
never promotes a replica and never moves the application's writes.

When someone adds a replica to Argus as a database, it takes the primary's
access in Argus, capped at read: a replica takes no writes (copyReplicaAccess).
*/

type cloudReplicaCreateSpec struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Size   string `json:"size"`
	Zone   string `json:"zone,omitempty"`
	// Engine, Storage and Encrypted are read from the primary, for the plan.
	Engine    string `json:"engine,omitempty"`
	StorageGB string `json:"storageGb,omitempty"`
	SameSize  bool   `json:"sameSize,omitempty"`
}

type cloudReplicaRemoveSpec struct {
	Source string `json:"source"`
}

func isReplica(a cloud.Action) bool {
	return a == cloud.ActionReplicaCreate || a == cloud.ActionReplicaRemove
}

// rdsByName finds a database by its identifier in one account and region.
func (a *API) rdsByName(ctx context.Context, accountID, region, name string) (cloud.Resource, bool) {
	items, err := a.store.CloudResources(ctx, cloud.KindDatabase)
	if err != nil {
		return cloud.Resource{}, false
	}
	for _, it := range items {
		if it.AccountID == accountID && it.Resource.Region == region && strings.EqualFold(it.Resource.Name, name) {
			return it.Resource, true
		}
	}
	return cloud.Resource{}, false
}

// replicaProblem says why a database cannot have a read replica added from
// Argus, or "".
func replicaProblem(src cloud.Resource) string {
	engine := src.Details["engine"]
	switch {
	case src.Details["cluster"] != "" || strings.HasPrefix(engine, "aurora"):
		return src.Name + " is in an Aurora cluster; add a reader to the cluster instead"
	case cloud.ReplicaEngines[engine] == "":
		return "Argus adds read replicas of Postgres, MySQL and MariaDB, not " + engine
	case src.Details["replica_of"] != "":
		return src.Name + " is itself a replica of " + src.Details["replica_of"] + "; add one to the primary"
	case src.State != "available":
		return src.Name + " is " + src.State + "; a replica can be added once it is available"
	case src.Details["backup_days"] == "0" || src.Details["backup_days"] == "":
		return "AWS makes a replica from " + src.Name + "'s backups, which are off; turn on automated backups first"
	}
	return ""
}

func (a *API) planReplica(r *http.Request, m cloudManaged, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	if _, ok := m.manager.(cloud.Replicas); !ok {
		return ch, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot add read replicas from Argus yet")
	}
	if err := a.storageMissing(m, in.Action); err != nil {
		return ch, http.StatusConflict, err
	}
	ctx := r.Context()
	if in.Action == cloud.ActionReplicaRemove {
		rep, err := a.cloudResource(ctx, m.acct.ID, in.Region, cloud.KindDatabase, in.ResourceID)
		if err != nil {
			return ch, http.StatusNotFound, errors.New("Argus has not read that database in " + in.Region)
		}
		if rep.Details["replica_of"] == "" {
			return ch, http.StatusConflict, errors.New(rep.Name + " is not a read replica; Argus removes only replicas")
		}
		if !a.cloudMadeByArgus(ctx, m.acct.ID, rep.ID, cloud.ActionReplicaCreate) || rep.Tags[cloud.ReplicaTag] == "" {
			return ch, http.StatusConflict, errors.New("Argus removes only replicas it made; remove " + rep.Name + " in the AWS console")
		}
		if rep.State == "deleting" {
			return ch, http.StatusConflict, errors.New(rep.Name + " is already being deleted")
		}
		ch.Spec, _ = json.Marshal(cloudReplicaRemoveSpec{Source: rep.Details["replica_of"]})
		ch.ResourceID, ch.ResourceName = rep.ID, rep.Name
		ch.Environment = cloudEnvOf(rep.Tags, m.acct.Environment)
		ch.Summary = fmt.Sprintf("Remove read replica %s of %s. Reads sent to it stop working; %s and its data are not touched",
			rep.Name, rep.Details["replica_of"], rep.Details["replica_of"])
		return ch, http.StatusOK, nil
	}
	var s cloudReplicaCreateSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, http.StatusBadRequest, errors.New("a read replica takes {source, name, size}")
	}
	src, ok := a.rdsByName(ctx, m.acct.ID, in.Region, strings.TrimSpace(s.Source))
	if !ok {
		return ch, http.StatusNotFound, errors.New("Argus has not read a database named " + s.Source + " in " + in.Region)
	}
	if p := replicaProblem(src); p != "" {
		return ch, http.StatusConflict, errors.New(p)
	}
	s.Source, s.Name, s.Size, s.Zone = src.Name, strings.TrimSpace(s.Name), strings.TrimSpace(s.Size), strings.TrimSpace(s.Zone)
	if s.Size == "" {
		s.Size = src.Size
	}
	spec := cloud.ReplicaSpec{Region: in.Region, Source: s.Source, Name: s.Name, Size: s.Size, Zone: s.Zone}
	if err := spec.Check(); err != nil {
		return ch, http.StatusBadRequest, err
	}
	if _, taken := a.rdsByName(ctx, m.acct.ID, in.Region, s.Name); taken {
		return ch, http.StatusConflict, errors.New(in.Region + " already has a database named " + s.Name)
	}
	s.Engine, s.StorageGB, s.SameSize = src.Details["engine"], src.Details["storage_gb"], s.Size == src.Size
	ch.Spec, _ = json.Marshal(s)
	ch.ResourceName = s.Name
	ch.Environment = cloudEnvOf(src.Tags, m.acct.Environment)
	ch.Summary = replicaWords(s, src)
	return ch, http.StatusOK, nil
}

// replicaWords is a replica's plan in a sentence.
func replicaWords(s cloudReplicaCreateSpec, src cloud.Resource) string {
	where := "a zone AWS picks"
	if s.Zone != "" {
		where = s.Zone
	}
	w := fmt.Sprintf("Add read replica %s of %s (%s %s), %s", s.Name, s.Source, cloud.ReplicaEngines[s.Engine], src.Details["version"], s.Size)
	if s.SameSize {
		w += " like the primary"
	}
	w += ", in " + where
	if s.StorageGB != "" {
		w += ", with its own " + s.StorageGB + " GiB of storage"
	}
	w += ". Deletion protection on. Writes stay on " + s.Source + "; AWS bills the replica like a database of its own"
	return w
}

func (a *API) applyReplica(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	rp, ok := m.manager.(cloud.Replicas)
	if !ok {
		return cloud.Result{}, errors.New(m.acct.Provider + " cannot add read replicas from Argus yet")
	}
	actor := cloud.Actor(ch.RequestedBy)
	if ch.Action == cloud.ActionReplicaRemove {
		res, err := rp.RemoveReplica(ctx, m.target, actor, ch.Region, ch.ResourceName)
		if err == nil {
			res.Detail = "AWS is deleting " + ch.ResourceName
		}
		return res, err
	}
	var s cloudReplicaCreateSpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	res, err := rp.CreateReplica(ctx, m.target, actor, cloud.ReplicaSpec{Region: ch.Region, Source: s.Source, Name: s.Name, Size: s.Size,
		Zone: s.Zone, Tags: map[string]string{"argus:change": ch.ID, "argus:launched-by": ch.RequestedBy}})
	if err == nil {
		res.Detail = "AWS is making " + s.Name + " from " + s.Source + "'s latest backup; it takes some minutes, longer for a large database"
	}
	return res, err
}

// cloudMadeByArgus is whether an applied change of kind act made resource
// id in this provider account.
func (a *API) cloudMadeByArgus(ctx context.Context, accountID, id string, act cloud.Action) bool {
	var made bool
	err := a.store.db(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cloud_changes c
		JOIN cloud_accounts ca ON ca.id = c.account_id
		JOIN cloud_accounts me ON me.provider = ca.provider AND me.account_id = ca.account_id
		WHERE me.id::text = $1 AND c.resource_id = $2 AND c.action = $3 AND c.state = 'applied')`,
		accountID, id, string(act)).Scan(&made)
	return err == nil && made
}

// replicaPrimary is the RDS primary whose replica answers at host, when
// Argus has read both: the replica first, then its primary.
func (a *API) replicaPrimary(ctx context.Context, host string) (rep, primary cloud.Resource, ok bool) {
	if host == "" {
		return rep, primary, false
	}
	items, err := a.store.CloudResources(ctx, cloud.KindDatabase)
	if err != nil {
		return rep, primary, false
	}
	var acct string
	for _, it := range items {
		if strings.EqualFold(it.Resource.Details["address"], host) && it.Resource.Details["replica_of"] != "" {
			rep, acct, ok = it.Resource, it.AccountID, true
		}
	}
	if !ok {
		return rep, primary, false
	}
	for _, it := range items {
		if it.AccountID == acct && it.Resource.Region == rep.Region && it.Resource.Name == rep.Details["replica_of"] {
			return rep, it.Resource, it.Resource.Details["address"] != ""
		}
	}
	return rep, primary, false
}

// replicaLevel is the most a replica's access may be: it takes no writes,
// so write and owner become read; dba stays, for its activity and locks.
func replicaLevel(level string) string {
	if level == tunnel.LevelDBA || tunnel.LevelRank(level) <= tunnel.LevelRank(tunnel.LevelRead) {
		return level
	}
	return tunnel.LevelRead
}

// copyReplicaAccess gives a database just added to Argus, when it is an RDS
// read replica of a database already in Argus, the primary's access there,
// capped at read. Nobody who could not open the primary can open it.
func (a *API) copyReplicaAccess(ctx context.Context, db Database, actor string) {
	rep, primary, ok := a.replicaPrimary(ctx, db.Host)
	if !ok {
		return
	}
	var fromID, fromHost string
	err := a.store.db(ctx).QueryRow(ctx, `SELECT d.id::text, a.hostname FROM databases d JOIN assets a ON a.id = d.asset_id
		WHERE lower(d.host) = lower($1) AND d.port = $2 AND d.dbname = $3 AND d.engine = $4 AND d.id::text <> $5
		ORDER BY d.created_at LIMIT 1`, primary.Details["address"], db.Port, db.Name, db.Engine, db.ID).Scan(&fromID, &fromHost)
	if err != nil {
		return
	}
	from, err := a.store.DBAssignments(ctx, fromID)
	if err != nil {
		a.log.Error("replica access not copied", "database", db.ID, "error", err)
		return
	}
	have, _ := a.store.DBAssignments(ctx, db.ID)
	taken := map[string]bool{}
	for _, g := range have {
		taken[assignKey(g)] = true
	}
	var given []string
	for _, g := range from {
		if taken[assignKey(g)] {
			continue
		}
		g.ID, g.DatabaseID, g.Level = "", db.ID, replicaLevel(g.Level)
		if err := a.store.SetDBAssignment(ctx, g, actor); err != nil {
			a.log.Error("replica access not copied", "database", db.ID, "error", err)
			continue
		}
		given = append(given, assignWho(g)+" "+g.Level)
	}
	if len(given) > 0 {
		a.auditDB(ctx, "db.assigned", "notice", actor, db.Hostname, fmt.Sprintf("%s (%s) is a read replica of %s, so it took %s's access in Argus, capped at read: %s.",
			db.Name, rep.Name, primary.Name, primary.Name, strings.Join(given, ", ")))
	}
}

func assignKey(g DBAssignment) string {
	if g.TeamID != nil {
		return "t:" + *g.TeamID
	}
	return "u:" + strings.ToLower(deref(g.UserEmail))
}

func assignWho(g DBAssignment) string {
	if g.TeamID != nil {
		return "team " + g.TeamName
	}
	return strings.ToLower(deref(g.UserEmail))
}
