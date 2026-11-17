package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

// A read endpoint: a target group on the database's port that Argus keeps
// to the primary and the replicas that are not behind. A replica five
// minutes behind is taken out, and put back when it catches up; removing
// the endpoint takes out only what Argus put in.
func TestDatabaseReadEndpoint(t *testing.T) {
	e := networkAPI(t)
	ctx := context.Background()
	pub, key, _ := ed25519.GenerateKey(nil)
	e.a.Tunnels = &TunnelHub{GrantKey: key}
	f := &replicaAgents{t: t, pub: pub}
	e.a.openAgent = f.open

	primaryAsset, _ := enrolledAsset(t, e.s, e.h)
	replicaAsset, _ := enrolledAsset(t, e.s, e.h)
	if _, err := e.s.pool.Exec(ctx, `UPDATE agent_credentials SET tunnel_seen_at = now()
		WHERE lower(hostname) IN (lower($1), lower($2))`, primaryAsset.Hostname, replicaAsset.Hostname); err != nil {
		t.Fatal(err)
	}
	pInst, rInst, byHand := e.account+"-i-pg1", e.account+"-i-pg2", e.account+"-i-hand"
	inst := func(id, name string) cloud.Resource {
		return cloud.Resource{Kind: cloud.KindInstance, ID: id, Name: name, Region: storageRegion, State: "running",
			Network: "vpc-1", Size: "r6i.large", Tags: map[string]string{"Name": name}}
	}
	e.fake.Resources[storageRegion] = append(e.fake.Resources[storageRegion],
		inst(pInst, primaryAsset.Hostname), inst(rInst, replicaAsset.Hostname), inst(byHand, "reporting-01"),
		cloud.Resource{Kind: cloud.KindTargetGroup, ID: "arn:tg/pg-read", Name: "pg-read", Region: storageRegion, Network: "vpc-1",
			Details: map[string]string{"targetType": "instance", "port": "5432", "protocol": "TCP"},
			Links: []cloud.Ref{{Kind: cloud.KindLoadBalancer, ID: "arn:lb/pg", Name: "pg"},
				{Kind: cloud.KindInstance, ID: byHand, Name: "healthy"}}},
		cloud.Resource{Kind: cloud.KindTargetGroup, ID: "arn:tg/pg-ip", Name: "pg-ip", Region: storageRegion, Network: "vpc-1",
			Details: map[string]string{"targetType": "ip", "port": "5432", "protocol": "TCP"}},
	)
	e.resync(t)

	primary, err := e.s.CreateDatabase(ctx, Database{AssetID: primaryAsset.ID, Engine: "postgres", Host: "/var/run/postgresql",
		Port: 5432, Name: "orders"}, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.pool.Exec(ctx, `INSERT INTO database_replicas (database_id, asset_id, name, replica_address,
		primary_address, requested_by, reason, state) VALUES ($1::uuid, $2::uuid, 'orders_r1', '10.0.1.20', '10.0.1.10',
		'dba@example.com', 'reads', 'done')`, primary.ID, replicaAsset.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The watch reads every replica in the test database: leave none.
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM console_alerts WHERE subject IN
			(SELECT 'replica-lag:' || id::text FROM database_replicas WHERE database_id = $1::uuid)
			OR (kind = 'backup' AND title LIKE '%' || $2 || '%')`, primary.ID, replicaAsset.Hostname)
		_, _ = e.s.pool.Exec(ctx, `DELETE FROM databases WHERE id = $1::uuid`, primary.ID)
	})
	path := "/api/v1/databases/" + primary.ID + "/read-endpoint"
	view := func() readEndpointView {
		t.Helper()
		w := do(t, e.h, http.MethodGet, path, "admin-token", "")
		if w.Code != http.StatusOK {
			t.Fatalf("get: %d %s", w.Code, w.Body)
		}
		var v readEndpointView
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	replica := func() DatabaseReplica {
		t.Helper()
		reps, err := e.s.Replicas(ctx, primary.ID)
		if err != nil || len(reps) != 1 {
			t.Fatalf("replicas: %v %d", err, len(reps))
		}
		return reps[0]
	}
	members := func() []string {
		t.Helper()
		var out []string
		for _, r := range e.fake.Resources[storageRegion] {
			if r.ID == "arn:tg/pg-read" {
				for _, l := range r.Links {
					if l.Kind == cloud.KindInstance {
						out = append(out, l.ID)
					}
				}
			}
		}
		slices.Sort(out)
		return out
	}
	want := func(ids ...string) {
		t.Helper()
		slices.Sort(ids)
		if got := members(); !slices.Equal(got, ids) {
			t.Fatalf("target group holds %v, want %v", got, ids)
		}
	}

	// The choices: the target group on 5432 first; one sending to IPs is
	// shown with why not; the web one on 8080 is not offered at all.
	v := view()
	if v.Endpoint != nil || len(v.Choices) != 2 || v.Choices[0].TargetGroup != "arn:tg/pg-read" || v.Choices[0].Problem != "" ||
		v.Choices[0].Balancer != "pg" || !strings.Contains(v.Choices[1].Problem, "ip targets") {
		t.Fatalf("choices: %+v", v)
	}

	put := func(token, tg string, primaryIn bool, code int) ReadEndpoint {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"accountId": e.acct.ID, "region": storageRegion, "targetGroup": tg, "includePrimary": primaryIn})
		w := do(t, e.h, http.MethodPut, path, token, string(body))
		if w.Code != code {
			t.Fatalf("put %s: %d %s, want %d", tg, w.Code, w.Body, code)
		}
		var ep ReadEndpoint
		_ = json.Unmarshal(w.Body.Bytes(), &ep)
		return ep
	}
	put("operator-token", "arn:tg/pg-read", true, http.StatusForbidden)
	put("admin-token", "arn:tg/pg-ip", true, http.StatusBadRequest)
	put("admin-token", "arn:tg/web", true, http.StatusBadRequest)

	// Set: the primary and the replica go in, as argus-replicas.
	ep := put("admin-token", "arn:tg/pg-read", true, http.StatusOK)
	if ep.PrimaryInstance != pInst || ep.TargetName != "pg-read" || ep.Note != "" || ep.SetBy != e.admin {
		t.Fatalf("set: %+v", ep)
	}
	e.called(t, "target-register arn:tg/pg-read "+pInst+" argus-replicas")
	e.called(t, "target-register arn:tg/pg-read "+rInst+" argus-replicas")
	if r := replica(); r.EndpointInstance != rInst || r.EndpointNote != "" {
		t.Fatalf("replica not in: %+v", r)
	}
	want(byHand, pInst, rInst)

	// The replica stops streaming: still in for five minutes, out after.
	now := time.Now().UTC()
	f.status = tunnel.PGReplicaStatus{LagBytes: -1, SlotExists: true}
	e.a.WatchReplicas(ctx, now)
	want(byHand, pInst, rInst)
	e.a.WatchReplicas(ctx, now.Add(6*time.Minute))
	if r := replica(); r.EndpointInstance != "" || r.EndpointNote != "out while it is behind" {
		t.Fatalf("replica not out: %+v", r)
	}
	want(byHand, pInst)
	e.called(t, "target-deregister arn:tg/pg-read "+rInst+" argus-replicas")
	// It stays out, without asking the cloud again.
	calls := len(e.fake.CallLog())
	e.a.WatchReplicas(ctx, now.Add(7*time.Minute))
	if len(e.fake.CallLog()) != calls {
		t.Fatalf("asked the cloud again: %v", e.fake.CallLog()[calls:])
	}

	// It catches up: back in.
	zero := 0.0
	f.status = tunnel.PGReplicaStatus{Streaming: true, State: "streaming", ReplayLagS: &zero, SlotExists: true, SlotActive: true}
	e.a.WatchReplicas(ctx, now.Add(8*time.Minute))
	if r := replica(); r.EndpointInstance != rInst || r.EndpointNote != "" {
		t.Fatalf("replica not back: %+v", r)
	}
	want(byHand, pInst, rInst)

	// Leaving the primary out takes it out.
	if ep := put("admin-token", "arn:tg/pg-read", false, http.StatusOK); ep.PrimaryInstance != "" {
		t.Fatalf("primary still in: %+v", ep)
	}
	want(byHand, rInst)
	var n int
	_ = e.s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'db.read_endpoint_%' AND target = $1`,
		primary.Hostname).Scan(&n)
	if n < 6 {
		t.Fatalf("audited %d moves", n)
	}

	// Removing: what Argus put in comes out; the one put in by hand stays.
	if w := do(t, e.h, http.MethodDelete, path, "operator-token", ""); w.Code != http.StatusForbidden {
		t.Fatalf("operator delete: %d", w.Code)
	}
	if w := do(t, e.h, http.MethodDelete, path, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	want(byHand)
	if v := view(); v.Endpoint != nil {
		t.Fatalf("still there: %+v", v.Endpoint)
	}
	if r := replica(); r.EndpointInstance != "" {
		t.Fatalf("replica still marked in: %+v", r)
	}
}
