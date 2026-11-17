package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/tunnel"
)

// A read replica of an RDS database: refused where AWS would refuse it,
// asked like any Change-level change on Production, made with deletion
// protection, and once someone adds it to Argus it takes the primary's
// access there, capped at read. Only a replica Argus made can be removed.
func TestCloudReadReplicas(t *testing.T) {
	e := changeAPI(t)
	ctx := context.Background()
	region := "ap-southeast-3"
	addr := func(name string) string { return name + ".abc123." + region + ".rds.amazonaws.com" }
	db := func(name, state string, d map[string]string) cloud.Resource {
		base := map[string]string{"engine": "postgres", "version": "16.4", "address": addr(name), "port": "5432",
			"backup_days": "7", "storage_gb": "500", "encrypted": "yes"}
		for k, v := range d {
			base[k] = v
		}
		return cloud.Resource{Kind: cloud.KindDatabase, ID: "arn:aws:rds:" + region + ":" + e.account + ":db:" + name, Name: name,
			Region: region, Zone: region + "a", State: state, Size: "db.r6i.large", Details: base}
	}
	e.fake.Resources[cloud.GlobalRegion] = append(e.fake.Resources[cloud.GlobalRegion],
		db("orders-db", "available", nil),
		db("nobackup-db", "available", map[string]string{"backup_days": "0"}),
		db("aurora-1", "available", map[string]string{"engine": "aurora-postgresql", "cluster": "aurora"}),
		db("theirs-replica", "available", map[string]string{"replica_of": "orders-db"}),
	)
	e.resync(t)
	body := func(action, extra string) string {
		return `{"accountId":"` + e.acct.ID + `","region":"` + region + `","action":"` + action + `"` + extra + `}`
	}
	create := func(spec string, want int) CloudChange {
		return e.ask(t, "admin-token", body("replica-create", `,"reason":"reads are busy","spec":`+spec), want)
	}

	// Refused before anything is asked.
	create(`{"source":"nobackup-db","name":"nb-r1"}`, http.StatusConflict)
	create(`{"source":"aurora-1","name":"a-r1"}`, http.StatusConflict)
	create(`{"source":"theirs-replica","name":"chain-1"}`, http.StatusConflict)
	create(`{"source":"missing-db","name":"m-r1"}`, http.StatusNotFound)
	create(`{"source":"orders-db","name":"orders-db"}`, http.StatusBadRequest)
	create(`{"source":"orders-db","name":"bad--name"}`, http.StatusBadRequest)
	create(`{"source":"orders-db","name":"theirs-replica"}`, http.StatusConflict)
	create(`{"source":"orders-db","name":"orders-r1","zone":"us-east-1a"}`, http.StatusBadRequest)

	// Production: a second person approves, and it runs as the one who asked.
	c := create(`{"source":"orders-db","name":"orders-db-r1","zone":"ap-southeast-3b"}`, http.StatusAccepted)
	if c.State != "awaiting_approval" || c.Level != cloud.LevelChange ||
		!strings.Contains(c.Summary, "Add read replica orders-db-r1 of orders-db (Postgres 16.4), db.r6i.large like the primary, in ap-southeast-3b") ||
		!strings.Contains(c.Summary, "Deletion protection on. Writes stay on orders-db") {
		t.Fatalf("plan: %+v", c)
	}
	done := e.decide(t, "approver-token", c.ID, true)
	wantARN := "arn:aws:rds:" + region + ":123456789012:db:orders-db-r1"
	if done.State != "applied" || done.ResourceID != wantARN {
		t.Fatalf("applied: %+v", done)
	}
	found := false
	for _, call := range e.fake.CallLog() {
		found = found || call == "replica-create orders-db orders-db-r1 "+e.admin
	}
	if !found {
		t.Fatalf("not made as the person who asked: %v", e.fake.CallLog())
	}
	e.resync(t)
	rep, err := e.a.cloudResource(ctx, e.acct.ID, region, cloud.KindDatabase, wantARN)
	if err != nil || rep.Tags[cloud.ReplicaTag] != "orders-db" || rep.Details["deletion_protection"] != "yes" || rep.Tags["argus:change"] != c.ID {
		t.Fatalf("the replica as read: %+v %v", rep, err)
	}
	items, _ := e.s.CloudResources(ctx, cloud.KindDatabase)
	for _, it := range items {
		if it.Resource.ID == wantARN && !it.MadeByArgus {
			t.Fatal("Argus made the replica, and the list should say so")
		}
	}

	// The primary is in Argus through a server's PgBouncer, with a team at
	// write and a person at dba. Adding the replica copies both, the write
	// capped at read.
	asset, err := e.s.CreateAsset(ctx, AssetInput{Hostname: "pgb-" + e.account, Kind: "server"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(context.Background(), `DELETE FROM assets WHERE id::text = $1`, asset.ID) })
	primary, err := e.s.CreateDatabase(ctx, Database{AssetID: asset.ID, Engine: "postgres", Host: addr("orders-db"), Port: 5432, Name: "orders"}, e.admin)
	if err != nil {
		t.Fatal(err)
	}
	var team string
	if err := e.s.pool.QueryRow(ctx, `INSERT INTO teams (name) VALUES ($1) RETURNING id::text`, "orders-"+e.account).Scan(&team); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(context.Background(), `DELETE FROM teams WHERE id::text = $1`, team) })
	dba := "dba-" + e.account + "@example.com"
	for _, g := range []DBAssignment{{DatabaseID: primary.ID, TeamID: &team, Level: tunnel.LevelWrite},
		{DatabaseID: primary.ID, UserEmail: &dba, Level: tunnel.LevelDBA}} {
		if err := e.s.SetDBAssignment(ctx, g, e.admin); err != nil {
			t.Fatal(err)
		}
	}
	// The replica's endpoint appears once AWS has made it.
	g := e.fake.Resources[cloud.GlobalRegion]
	for i, r := range g {
		if r.Name == "orders-db-r1" {
			g[i].State = "available"
			g[i].Details["address"] = addr("orders-db-r1")
		}
	}
	e.resync(t)
	// One server keeps one entry per port and name: the replica through the
	// same PgBouncer is refused rather than repointing the primary's entry.
	add := func(on string, want int) *httptest.ResponseRecorder {
		w := do(t, e.h, http.MethodPost, "/api/v1/databases", "admin-token",
			fmt.Sprintf(`{"assetId":%q,"engine":"postgres","host":%q,"port":5432,"dbname":"orders"}`, on, addr("orders-db-r1")))
		if w.Code != want {
			t.Fatalf("add the replica: %d %s, want %d", w.Code, w.Body, want)
		}
		return w
	}
	add(asset.ID, http.StatusBadRequest)
	if again, _ := e.s.Database(ctx, primary.ID); again.Host != addr("orders-db") {
		t.Fatalf("the primary's entry moved to %s", again.Host)
	}
	other, err := e.s.CreateAsset(ctx, AssetInput{Hostname: "pgb2-" + e.account, Kind: "server"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.s.pool.Exec(context.Background(), `DELETE FROM assets WHERE id::text = $1`, other.ID) })
	w := add(other.ID, http.StatusCreated)
	var added Database
	_ = json.Unmarshal(w.Body.Bytes(), &added)
	got, _ := e.s.DBAssignments(ctx, added.ID)
	levels := map[string]string{}
	for _, g := range got {
		levels[assignKey(g)] = g.Level
	}
	if levels["t:"+team] != tunnel.LevelRead || levels["u:"+dba] != tunnel.LevelDBA || len(got) != 2 {
		t.Fatalf("the replica's access: %v", levels)
	}

	// Removing: only a replica Argus made, at Remove level.
	e.ask(t, "admin-token", body("replica-remove", `,"resourceId":"`+db("theirs-replica", "", nil).ID+`"`), http.StatusConflict)
	e.ask(t, "admin-token", body("replica-remove", `,"resourceId":"`+db("orders-db", "", nil).ID+`"`), http.StatusConflict)
	x := e.ask(t, "admin-token", body("replica-remove", `,"resourceId":"`+wantARN+`","reason":"sale over"`), http.StatusAccepted)
	if x.Level != cloud.LevelRemove || !strings.Contains(x.Summary, "Remove read replica orders-db-r1 of orders-db") {
		t.Fatalf("remove plan: %+v", x)
	}
	if d := e.decide(t, "approver-token", x.ID, true); d.State != "applied" {
		t.Fatalf("removed: %+v", d)
	}
}
