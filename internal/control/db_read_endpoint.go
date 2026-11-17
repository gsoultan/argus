package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
A read endpoint for a managed Postgres (cloud scaling step 8c-2).

Someone who is a DBA on the database and an admin or owner of the
organization picks a target group of a network load balancer that already
exists: TCP, instance targets, on the database's port. Argus then keeps its
members: the primary, when asked to, and every replica Argus made whose
server is an instance in the same account and region. A replica that has
been behind for five minutes (the lag watch's bell) is taken out, and put
back once it catches up, so reads through the endpoint never see data much
older than a minute.

Argus records the instance it put in, and only ever takes out what it put
in: a target someone added by hand stays. Every move is audited, and runs
in the cloud as argus-replicas so CloudTrail tells it apart from a person.

A DNS name in front of the load balancer is not part of this step.
*/

// ReadEndpoint is a database's read endpoint, as the console shows it.
type ReadEndpoint struct {
	DatabaseID      string    `json:"databaseId"`
	AccountID       string    `json:"accountId"`
	AccountName     string    `json:"accountName"`
	Region          string    `json:"region"`
	TargetGroup     string    `json:"targetGroup"`
	TargetName      string    `json:"targetName"`
	IncludePrimary  bool      `json:"includePrimary"`
	PrimaryInstance string    `json:"primaryInstance"`
	Note            string    `json:"note"`
	SetBy           string    `json:"setBy"`
	SetAt           time.Time `json:"setAt"`
	// Health is each instance's health in the target group, as the cloud
	// was last read: healthy, unhealthy, initial, draining.
	Health map[string]string `json:"health"`
}

// ReadEndpointChoice is a target group the endpoint could be.
type ReadEndpointChoice struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Region      string `json:"region"`
	TargetGroup string `json:"targetGroup"`
	Name        string `json:"name"`
	Balancer    string `json:"balancer"`
	// Problem is why it cannot be picked, in words; empty when it can.
	Problem string `json:"problem,omitempty"`
}

// readEndpointView is GET's answer.
type readEndpointView struct {
	Endpoint *ReadEndpoint        `json:"endpoint"`
	Choices  []ReadEndpointChoice `json:"choices"`
	// Pending is setting it, asked and waiting for a second person.
	Pending *CloudChange `json:"pending,omitempty"`
}

/* ── Store ──────────────────────────────────────────────────────────────── */

// ErrNoReadEndpoint is a database without one.
var ErrNoReadEndpoint = errors.New("this database has no read endpoint")

func (s *Store) readEndpoint(ctx context.Context, databaseID string) (ReadEndpoint, error) {
	var e ReadEndpoint
	err := s.db(ctx).QueryRow(ctx, `SELECT e.database_id::text, e.account_id::text, a.name, e.region, e.target_group,
		e.target_name, e.include_primary, e.primary_instance, e.note, e.set_by, e.set_at
		FROM database_read_endpoints e JOIN cloud_accounts a ON a.id = e.account_id
		WHERE e.database_id = $1::uuid`, databaseID).Scan(&e.DatabaseID, &e.AccountID, &e.AccountName, &e.Region,
		&e.TargetGroup, &e.TargetName, &e.IncludePrimary, &e.PrimaryInstance, &e.Note, &e.SetBy, &e.SetAt)
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return e, ErrNoReadEndpoint
	}
	return e, err
}

// setReadEndpoint makes or replaces one. A new target group starts with
// nothing of Argus's in it; the caller has taken out what was.
func (s *Store) setReadEndpoint(ctx context.Context, e ReadEndpoint) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO database_read_endpoints (database_id, account_id, region, target_group,
		target_name, include_primary, set_by) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT (database_id) DO UPDATE SET account_id = EXCLUDED.account_id, region = EXCLUDED.region,
		target_group = EXCLUDED.target_group, target_name = EXCLUDED.target_name,
		include_primary = EXCLUDED.include_primary, set_by = EXCLUDED.set_by, set_at = now(), note = ''`,
		e.DatabaseID, e.AccountID, e.Region, e.TargetGroup, e.TargetName, e.IncludePrimary, e.SetBy)
	return err
}

func (s *Store) deleteReadEndpoint(ctx context.Context, databaseID string) error {
	if _, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET endpoint_instance = '', endpoint_note = ''
		WHERE database_id = $1::uuid`, databaseID); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM database_read_endpoints WHERE database_id = $1::uuid`, databaseID)
	return err
}

func (s *Store) setEndpointPrimary(ctx context.Context, databaseID, instance, note string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_read_endpoints SET primary_instance = $2, note = $3
		WHERE database_id = $1::uuid`, databaseID, instance, clip(note, 500))
	return err
}

func (s *Store) setReplicaEndpoint(ctx context.Context, id, instance, note string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE database_replicas SET endpoint_instance = $2, endpoint_note = $3
		WHERE id = $1::uuid`, id, instance, clip(note, 500))
	return err
}

// readEndpointDatabases is every database with a read endpoint.
func (s *Store) readEndpointDatabases(ctx context.Context) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT database_id::text FROM database_read_endpoints ORDER BY set_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

/* ── Routes ──────────────────────────────────────────────────────────────── */

// GET /api/v1/databases/{id}/read-endpoint: the endpoint, if any, and the
// target groups it could be.
func (a *API) getReadEndpoint(w http.ResponseWriter, r *http.Request, _ string, db Database, _ string) {
	ctx := r.Context()
	view := readEndpointView{Choices: []ReadEndpointChoice{}}
	e, err := a.store.readEndpoint(ctx, db.ID)
	switch {
	case err == nil:
		e.Health = a.targetHealth(ctx, e.AccountID, e.Region, e.TargetGroup)
		view.Endpoint = &e
	case !errors.Is(err, ErrNoReadEndpoint):
		a.fail(w, "read endpoint", err)
		return
	}
	view.Choices, err = a.readEndpointChoices(ctx, db)
	if err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	view.Pending, err = a.pendingReadEndpoint(ctx, db.ID)
	if err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// pendingReadEndpoint is the change setting db's endpoint that waits for
// approval, or nil.
func (a *API) pendingReadEndpoint(ctx context.Context, databaseID string) (*CloudChange, error) {
	list, err := a.store.CloudChanges(ctx, true, "", "", 500)
	if err != nil {
		return nil, err
	}
	for _, ch := range list {
		var s readEndpointSpec
		if ch.Action == cloud.ActionReadEndpoint && json.Unmarshal(ch.Spec, &s) == nil && s.DatabaseID == databaseID {
			return &ch, nil
		}
	}
	return nil, nil
}

// readEndpointChoices is every target group on db's port in a connected
// account, each with why it cannot be used, if it cannot.
func (a *API) readEndpointChoices(ctx context.Context, db Database) ([]ReadEndpointChoice, error) {
	accts, err := a.store.CloudAccounts(ctx)
	if err != nil {
		return nil, err
	}
	access := map[string]string{}
	for _, c := range accts {
		access[c.ID] = c.Access
	}
	items, err := a.store.CloudResources(ctx, cloud.KindTargetGroup)
	if err != nil {
		return nil, err
	}
	out := []ReadEndpointChoice{}
	for _, it := range items {
		g := it.Resource
		if g.Details["port"] != strconv.Itoa(db.Port) {
			continue
		}
		c := ReadEndpointChoice{AccountID: it.AccountID, AccountName: it.AccountName, Region: g.Region,
			TargetGroup: g.ID, Name: g.Name}
		for _, l := range g.Links {
			if l.Kind == cloud.KindLoadBalancer {
				c.Balancer = orWord(l.Name, l.ID)
				break
			}
		}
		c.Problem = targetGroupProblem(g, db.Port)
		if c.Problem == "" && access[it.AccountID] != "manage" {
			c.Problem = it.AccountName + " is connected read only"
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Problem == "") != (out[j].Problem == "") {
			return out[i].Problem == ""
		}
		return out[i].AccountName+out[i].Region+out[i].Name < out[j].AccountName+out[j].Region+out[j].Name
	})
	return out, nil
}

// targetGroupProblem says why g cannot carry a database on port, or "".
func targetGroupProblem(g cloud.Resource, port int) string {
	if t := g.Details["targetType"]; t != "" && t != "instance" {
		return "it sends to " + t + " targets; Argus registers instances"
	}
	if p := g.Details["protocol"]; p != "" && p != "TCP" {
		return "it speaks " + p + "; a database needs TCP"
	}
	if g.Details["port"] != strconv.Itoa(port) {
		return fmt.Sprintf("it listens on %s, not the database's port %d", orWord(g.Details["port"], "no port"), port)
	}
	return ""
}

// readEndpointSpec is a cloud change that sets a read endpoint, for an
// account where it waits for a second person.
type readEndpointSpec struct {
	DatabaseID     string `json:"databaseId"`
	Database       string `json:"database"`
	Hostname       string `json:"hostname"`
	IncludePrimary bool   `json:"includePrimary"`
}

// PUT /api/v1/databases/{id}/read-endpoint: make a target group db's read
// endpoint. A DBA on db who is an admin or owner of the organization. On an
// account whose changes wait for approval (production), it is asked as a
// cloud change and set once someone else approves it in Approvals.
func (a *API) putReadEndpoint(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	sess, _ := a.authenticate(r)
	if !a.privileged(w, sess, "setting a read endpoint", "admin", "owner") {
		return
	}
	var in struct {
		AccountID      string `json:"accountId"`
		Region         string `json:"region"`
		TargetGroup    string `json:"targetGroup"`
		IncludePrimary bool   `json:"includePrimary"`
		Reason         string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	m, code, err := a.cloudManagedAccount(ctx, in.AccountID)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	g, code, err := a.readEndpointTarget(ctx, m, in.Region, in.TargetGroup, db)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	if cloudNeedsApproval(m.acct.Environment, cloud.LevelOf(cloud.ActionReadEndpoint)) {
		a.askReadEndpoint(w, r, m, g, db, email, in.IncludePrimary, in.Reason)
		return
	}
	if err := a.setReadEndpointNow(ctx, m, g, db, in.IncludePrimary, email, ""); err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	e, err := a.store.readEndpoint(ctx, db.ID)
	if err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	e.Health = a.targetHealth(ctx, e.AccountID, e.Region, e.TargetGroup)
	writeJSON(w, http.StatusOK, e)
}

// readEndpointTarget is the target group a read endpoint would be, checked.
func (a *API) readEndpointTarget(ctx context.Context, m cloudManaged, region, id string, db Database) (cloud.Resource, int, error) {
	if _, ok := m.manager.(cloud.Network); !ok {
		return cloud.Resource{}, http.StatusNotImplemented, errors.New(m.acct.Provider + " cannot change load balancers yet")
	}
	g, err := a.store.CloudResourceOf(ctx, m.acct.ID, region, cloud.KindTargetGroup, id)
	if err != nil {
		return g, http.StatusBadRequest, errors.New("no target group " + id + " in " + m.acct.Name + " " + region + " as last read")
	}
	if p := targetGroupProblem(g, db.Port); p != "" {
		return g, http.StatusBadRequest, errors.New(g.Name + " cannot be a read endpoint for " + db.Name + ": " + p)
	}
	return g, http.StatusOK, nil
}

// askReadEndpoint records setting the endpoint as a cloud change waiting
// for someone other than the person who asked.
func (a *API) askReadEndpoint(w http.ResponseWriter, r *http.Request, m cloudManaged, g cloud.Resource, db Database,
	email string, includePrimary bool, reason string) {
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		writeErr(w, http.StatusBadRequest, "keep the reason under 500 characters")
		return
	}
	spec, _ := json.Marshal(readEndpointSpec{DatabaseID: db.ID, Database: db.Name, Hostname: db.Hostname, IncludePrimary: includePrimary})
	with := "its replicas that are not behind"
	if includePrimary {
		with = db.Hostname + " and " + with
	}
	ch := CloudChange{AccountID: m.acct.ID, Region: g.Region, Action: cloud.ActionReadEndpoint,
		Level: cloud.LevelOf(cloud.ActionReadEndpoint), Environment: m.acct.Environment, ResourceID: g.ID, ResourceName: g.Name,
		Spec: spec, Reason: reason, RequestedBy: email, State: "awaiting_approval",
		Summary: fmt.Sprintf("Make %s the read endpoint of %s on %s: Argus keeps %s in it", g.Name, db.Name, db.Hostname, with)}
	ch, err := a.store.CreateCloudChange(r.Context(), ch)
	if errors.Is(err, ErrCloudChangeBusy) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	detail := ch.Summary + ". Waits for approval (" + ch.Environment + ")."
	if ch.Reason != "" {
		detail += " Reason: " + ch.Reason
	}
	a.auditCloudChange(r, ch, "requested", email, detail)
	writeJSON(w, http.StatusAccepted, ch)
}

// applyReadEndpoint sets the endpoint an approved change asked for, after
// checking the database and target group again.
func (a *API) applyReadEndpoint(ctx context.Context, m cloudManaged, ch CloudChange) (cloud.Result, error) {
	var s readEndpointSpec
	if err := json.Unmarshal(ch.Spec, &s); err != nil {
		return cloud.Result{}, err
	}
	db, err := a.store.Database(ctx, s.DatabaseID)
	if err != nil {
		return cloud.Result{}, fmt.Errorf("database %s: %w", s.Database, err)
	}
	g, _, err := a.readEndpointTarget(ctx, m, ch.Region, ch.ResourceID, db)
	if err != nil {
		return cloud.Result{}, err
	}
	approved := ""
	if ch.DecidedBy != nil {
		approved = *ch.DecidedBy
	}
	if err := a.setReadEndpointNow(ctx, m, g, db, s.IncludePrimary, ch.RequestedBy, approved); err != nil {
		return cloud.Result{}, err
	}
	res := cloud.Result{ResourceID: g.ID, Detail: "Argus keeps " + g.Name + " from now on"}
	if e, err := a.store.readEndpoint(ctx, db.ID); err == nil && e.Note != "" {
		res.Detail += "; " + e.Note
	}
	return res, nil
}

// setReadEndpointNow makes g db's read endpoint and puts its members in.
// approvedBy is the second person, when one approved it.
func (a *API) setReadEndpointNow(ctx context.Context, m cloudManaged, g cloud.Resource, db Database, includePrimary bool,
	by, approvedBy string) error {
	// A different target group: take out what Argus put in the old one.
	old, err := a.store.readEndpoint(ctx, db.ID)
	if err == nil && (old.TargetGroup != g.ID || old.AccountID != m.acct.ID || old.Region != g.Region) {
		a.clearReadEndpoint(ctx, old, db)
	} else if err != nil && !errors.Is(err, ErrNoReadEndpoint) {
		return err
	}
	e := ReadEndpoint{DatabaseID: db.ID, AccountID: m.acct.ID, Region: g.Region, TargetGroup: g.ID,
		TargetName: g.Name, IncludePrimary: includePrimary, SetBy: by}
	if err := a.store.setReadEndpoint(ctx, e); err != nil {
		return err
	}
	with := "its replicas"
	if includePrimary {
		with = "the primary and its replicas"
	}
	detail := fmt.Sprintf("Read endpoint for %s: target group %s in %s %s, with %s that are not behind", db.Name, g.Name, m.acct.Name, g.Region, with)
	if approvedBy != "" {
		detail += "; approved by " + approvedBy
	}
	a.auditDB(ctx, "db.read_endpoint_set", "notice", by, db.Hostname, detail)
	a.tendReadEndpoint(ctx, db)
	return nil
}

// DELETE /api/v1/databases/{id}/read-endpoint: take out what Argus put in
// and stop keeping it.
func (a *API) deleteReadEndpoint(w http.ResponseWriter, r *http.Request, email string, db Database, _ string) {
	sess, _ := a.authenticate(r)
	if !a.privileged(w, sess, "removing a read endpoint", "admin", "owner") {
		return
	}
	ctx := r.Context()
	e, err := a.store.readEndpoint(ctx, db.ID)
	if errors.Is(err, ErrNoReadEndpoint) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	if left := a.clearReadEndpoint(ctx, e, db); left != "" {
		writeErr(w, http.StatusBadGateway, left)
		return
	}
	if err := a.store.deleteReadEndpoint(ctx, db.ID); err != nil {
		a.fail(w, "read endpoint", err)
		return
	}
	a.auditDB(ctx, "db.read_endpoint_removed", "notice", email, db.Hostname,
		fmt.Sprintf("Read endpoint for %s removed; Argus no longer keeps target group %s", db.Name, e.TargetName))
	w.WriteHeader(http.StatusNoContent)
}

/* ── Keeping it ─────────────────────────────────────────────────────────── */

// endpointCloud is what keeping an endpoint needs from its account.
type endpointCloud struct {
	m  cloudManaged
	nw cloud.Network
}

func (a *API) endpointCloud(ctx context.Context, e ReadEndpoint) (endpointCloud, error) {
	m, _, err := a.cloudManagedAccount(ctx, e.AccountID)
	if err != nil {
		return endpointCloud{}, err
	}
	nw, ok := m.manager.(cloud.Network)
	if !ok {
		return endpointCloud{}, errors.New(m.acct.Provider + " cannot change load balancers yet")
	}
	return endpointCloud{m: m, nw: nw}, nil
}

// clearReadEndpoint takes out everything Argus put in e's target group and
// says what it could not, or "".
func (a *API) clearReadEndpoint(ctx context.Context, e ReadEndpoint, db Database) string {
	ec, err := a.endpointCloud(ctx, e)
	if err != nil {
		return err.Error()
	}
	var left []string
	out := func(instance, what string) bool {
		if _, err := ec.nw.DeregisterTarget(ctx, ec.m.target, cloud.ReplicaActor, e.Region, e.TargetGroup, instance, int32(db.Port)); err != nil {
			left = append(left, what+": "+err.Error())
			return false
		}
		return true
	}
	if e.PrimaryInstance != "" && out(e.PrimaryInstance, db.Hostname) {
		_ = a.store.setEndpointPrimary(ctx, db.ID, "", "")
	}
	reps, err := a.store.Replicas(ctx, db.ID)
	if err != nil {
		return err.Error()
	}
	for _, rep := range reps {
		if rep.EndpointInstance != "" && out(rep.EndpointInstance, rep.Hostname) {
			_ = a.store.setReplicaEndpoint(ctx, rep.ID, "", "")
		}
	}
	return strings.Join(left, "; ")
}

// TendReadEndpoints keeps every read endpoint of the organization on ctx.
// The lag watch calls it after its readings.
func (a *API) TendReadEndpoints(ctx context.Context) {
	ids, err := a.store.readEndpointDatabases(ctx)
	if err != nil {
		a.log.Error("read endpoints: cannot list", "error", err)
		return
	}
	for _, id := range ids {
		db, err := a.store.Database(ctx, id)
		if err != nil {
			continue
		}
		a.tendReadEndpoint(ctx, db)
	}
}

// tendReadEndpoint puts in and takes out db's members so the target group
// holds what it should: the primary if asked, and each replica that stands
// and is not behind.
func (a *API) tendReadEndpoint(ctx context.Context, db Database) {
	e, err := a.store.readEndpoint(ctx, db.ID)
	if err != nil {
		return
	}
	reps, err := a.store.Replicas(ctx, db.ID)
	if err != nil {
		return
	}
	ec, cerr := a.endpointCloud(ctx, e)
	if cerr != nil {
		_ = a.store.setEndpointPrimary(ctx, db.ID, e.PrimaryInstance, cerr.Error())
		return
	}
	instances, err := a.store.AgentInstances(ctx)
	if err != nil {
		return
	}
	port := int32(db.Port)
	// instanceOf is the instance in e's account and region a server runs
	// as, or why there is none.
	instanceOf := func(assetID, host string) (string, string) {
		it, err := a.assetInstance(ctx, assetID, instances)
		if err != nil {
			return "", err.Error()
		}
		if it == nil {
			return "", host + " is not matched to an instance in a connected account"
		}
		if it.AccountID != e.AccountID || it.Resource.Region != e.Region {
			return "", fmt.Sprintf("%s runs in %s %s, not %s %s, where the target group is", host, it.AccountName,
				it.Resource.Region, e.AccountName, e.Region)
		}
		return it.Resource.ID, ""
	}
	// move puts an instance in or takes it out; it answers the instance
	// now in (or "") and a note when the move failed.
	move := func(in bool, instance, host, why string) (string, string) {
		if in {
			if _, err := ec.nw.RegisterTarget(ctx, ec.m.target, cloud.ReplicaActor, e.Region, e.TargetGroup, instance, port); err != nil {
				return "", "could not put " + host + " in: " + err.Error()
			}
			a.auditDB(ctx, "db.read_endpoint_in", "notice", string(cloud.ReplicaActor), db.Hostname,
				fmt.Sprintf("%s (%s) put in read endpoint %s of %s%s", host, instance, e.TargetName, db.Name, why))
			return instance, ""
		}
		if _, err := ec.nw.DeregisterTarget(ctx, ec.m.target, cloud.ReplicaActor, e.Region, e.TargetGroup, instance, port); err != nil {
			return instance, "could not take " + host + " out: " + err.Error()
		}
		a.auditDB(ctx, "db.read_endpoint_out", "notice", string(cloud.ReplicaActor), db.Hostname,
			fmt.Sprintf("%s (%s) taken out of read endpoint %s of %s%s", host, instance, e.TargetName, db.Name, why))
		return "", ""
	}

	// The primary.
	inst, note := e.PrimaryInstance, ""
	switch {
	case e.IncludePrimary && inst == "":
		var id string
		if id, note = instanceOf(db.AssetID, db.Hostname); id != "" {
			inst, note = move(true, id, db.Hostname, "")
		}
	case !e.IncludePrimary && inst != "":
		inst, note = move(false, inst, db.Hostname, ": the primary is no longer included")
	}
	if inst != e.PrimaryInstance || note != e.Note {
		_ = a.store.setEndpointPrimary(ctx, db.ID, inst, note)
	}

	// The replicas.
	for _, rep := range reps {
		want := rep.State == "done" && !rep.BehindAlerted
		inst, note := rep.EndpointInstance, ""
		switch {
		case want && inst == "":
			why := ""
			if rep.WatchedAt != nil && rep.LagS != nil {
				why = ": it is " + lagWords(*rep.LagS) + " behind"
			}
			var id string
			if id, note = instanceOf(rep.AssetID, rep.Hostname); id != "" {
				inst, note = move(true, id, rep.Hostname, why)
			}
		case !want && inst != "":
			why := ": it is no longer a replica"
			if rep.State == "done" {
				why = ": it has been behind for more than five minutes"
			}
			inst, note = move(false, inst, rep.Hostname, why)
			if inst == "" && rep.State == "done" {
				note = "out while it is behind"
			}
		case !want && rep.State == "done":
			note = "out while it is behind"
		}
		if inst != rep.EndpointInstance || note != rep.EndpointNote {
			_ = a.store.setReplicaEndpoint(ctx, rep.ID, inst, note)
		}
	}
}
