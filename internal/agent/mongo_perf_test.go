//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/gsoultan/argus/internal/tunnel"
)

// perf against a real MongoDB (see mongodb_test.go): refused below DBA, and
// at DBA every source is read as the person's own user, with the profiler's
// slow operations grouped by collection and plan.
func TestMongoDBPerfOnARealServer(t *testing.T) {
	m := startMongo(t)
	ctx := context.Background()
	db := m.c.Database(m.db)
	docs := make([]any, 20000)
	for i := range docs {
		docs[i] = bson.D{{Key: "customer", Value: i % 2000}, {Key: "status", Value: []string{"new", "paid", "sent"}[i%3]}, {Key: "total", Value: i % 500}}
	}
	if _, err := db.Collection("orders").InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	for _, k := range []bson.D{{{Key: "status", Value: 1}}, {{Key: "status", Value: 1}, {Key: "total", Value: 1}}} {
		if _, err := db.RunCommand(ctx, bson.D{{Key: "createIndexes", Value: "orders"},
			{Key: "indexes", Value: bson.A{bson.D{{Key: "key", Value: k}, {Key: "name", Value: indexName(k)}}}}}).Raw(); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 1}, {Key: "slowms", Value: 0}}).Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := db.Collection("orders").CountDocuments(ctx, bson.D{{Key: "customer", Value: i}}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	svc := MongoDB(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})

	read := openMongo(t, m, svc, tunnel.LevelRead, dir, 0)
	if a := read.ask(tunnel.StoreRequest{Op: tunnel.MongoOpPerf}); a.Type != "error" || !strings.Contains(a.Error, "DBA work") {
		t.Fatalf("perf at read: %+v", a)
	}
	dba := openMongo(t, m, svc, tunnel.LevelDBA, dir, 0)
	a := dba.ask(tunnel.StoreRequest{Op: tunnel.MongoOpPerf})
	if a.Type != "result" {
		t.Fatalf("perf at dba: %+v", a)
	}
	b, _ := json.Marshal(a.Data)
	var p struct {
		Server      map[string]any   `json:"server"`
		Collections []MongoCollPerf  `json:"collections"`
		Slow        []map[string]any `json:"slow"`
		Missing     []string         `json:"missing"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Missing) > 0 {
		t.Fatalf("missing: %v", p.Missing)
	}
	if p.Server["wiredTiger"] == nil || len(p.Collections) != 1 || p.Collections[0].Count != 20000 || len(p.Collections[0].Indexes) != 3 {
		t.Fatalf("perf: %s", b)
	}
	scan := false
	for _, s := range p.Slow {
		id, _ := s["_id"].(map[string]any)
		if id["plan"] == "COLLSCAN" && strings.HasSuffix(id["ns"].(string), ".orders") {
			scan = true
		}
	}
	if !scan {
		t.Fatalf("no COLLSCAN on orders among the slow operations: %s", b)
	}
	if out := os.Getenv("ARGUS_TEST_MONGODB_PERF_OUT"); out != "" {
		_ = os.WriteFile(out, b, 0o644)
	}
}

func indexName(k bson.D) string {
	var parts []string
	for _, e := range k {
		parts = append(parts, e.Key+"_1")
	}
	return strings.Join(parts, "_")
}
