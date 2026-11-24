//go:build !windows

package agent

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Key-value on a real server: Manage makes a bucket, Write puts and
// deletes keys, Read lists keys and reads values and their history without
// making a watcher, and NATS holds each level to that even asked directly.
func TestNATSKeyValue(t *testing.T) {
	l := newNATSLab(t)
	owner, orec, ofile := l.open(t, tunnel.LevelOwner)
	writer, wrec, _ := l.open(t, tunnel.LevelWrite)
	reader, rrec, rfile := l.open(t, tunnel.LevelRead)

	spec := &tunnel.NATSKVSpec{Bucket: "carts", History: 5, TTLS: 3600, Storage: "memory"}
	if a := natsDo(t, writer, wrec, tunnel.NATSOpKVCreate, &tunnel.NATSRequest{KVSpec: spec}); a.Type != "error" {
		t.Fatal("a writer was offered making a bucket")
	}
	if a := natsDo(t, owner, orec, tunnel.NATSOpKVCreate, &tunnel.NATSRequest{KVSpec: spec}); a.Type != "result" {
		t.Fatalf("owner create: %s", a.Error)
	}
	put := func(key, val string) tunnel.StoreAnswer {
		return natsDo(t, writer, wrec, tunnel.NATSOpKVPut, &tunnel.NATSRequest{Bucket: "carts", Key: key}, func(r *tunnel.StoreRequest) { r.Payload = val })
	}
	for _, v := range []string{`{"items":1}`, `{"items":2}`, `{"items":3}`} {
		if a := put("c1", v); a.Type != "result" {
			t.Fatalf("put: %s", a.Error)
		}
	}
	put("c2", "x")
	put("eu.c3", "y")
	if a := natsDo(t, writer, wrec, tunnel.NATSOpKVDel, &tunnel.NATSRequest{Bucket: "carts", Key: "c2"}); a.Type != "result" {
		t.Fatalf("del: %s", a.Error)
	}
	if a := natsDo(t, reader, rrec, tunnel.NATSOpKVPut, &tunnel.NATSRequest{Bucket: "carts", Key: "c1"}); a.Type != "error" {
		t.Fatal("a reader was offered a put")
	}
	for _, bad := range []string{"_kv.x", "a..b", "a b", "a*"} {
		if a := put(bad, "z"); a.Type != "error" {
			t.Errorf("key %q was put", bad)
		}
	}

	bs := natsDo(t, reader, rrec, tunnel.NATSOpKVBuckets, nil)
	buckets, _ := bs.Data.([]NATSKVBucket)
	if bs.Type != "result" || len(buckets) != 1 || buckets[0].Bucket != "carts" || buckets[0].History != 5 || buckets[0].TTLS != 3600 {
		t.Fatalf("buckets: %+v %s", bs.Data, bs.Error)
	}
	ks := natsDo(t, reader, rrec, tunnel.NATSOpKVKeys, &tunnel.NATSRequest{Bucket: "carts"})
	keys, _ := ks.Data.([]NATSKVKey)
	if ks.Type != "result" || len(keys) != 3 {
		t.Fatalf("keys: %+v %s", ks.Data, ks.Error)
	}
	byKey := map[string]NATSKVKey{}
	for _, k := range keys {
		byKey[k.Key] = k
	}
	if byKey["c1"].Kept != 3 || byKey["c1"].Op != "put" || byKey["c2"].Op != "del" {
		t.Fatalf("key states: %+v", keys)
	}
	ks = natsDo(t, reader, rrec, tunnel.NATSOpKVKeys, &tunnel.NATSRequest{Bucket: "carts", Key: "eu.*"})
	if keys, _ := ks.Data.([]NATSKVKey); len(keys) != 1 || keys[0].Key != "eu.c3" {
		t.Fatalf("filtered keys: %+v %s", ks.Data, ks.Error)
	}

	g := natsDo(t, reader, rrec, tunnel.NATSOpKVGet, &tunnel.NATSRequest{Bucket: "carts", Key: "c1"})
	m, _ := g.Data.(map[string]any)
	entries, _ := m["entries"].([]NATSKVEntry)
	if g.Type != "result" || len(entries) != 3 || entries[0].Value.Data != `{"items":3}` || entries[2].Value.Data != `{"items":1}` {
		t.Fatalf("get: %+v %s", g.Data, g.Error)
	}
	// Reading made no consumer on the bucket's stream.
	st, err := owner.js.Stream(context.Background(), "KV_carts")
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := st.Info(context.Background()); info.State.Consumers != 0 {
		t.Fatalf("reading made %d consumers", info.State.Consumers)
	}

	// NATS holds the reader even asked directly.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if kv, err := reader.js.KeyValue(ctx, "carts"); err == nil {
		if _, err := kv.Put(ctx, "c1", []byte("no")); err == nil {
			t.Fatal("NATS let a reader put a key")
		}
	}

	// Removing a bucket needs its name.
	if a := natsDo(t, owner, orec, tunnel.NATSOpKVDelete, &tunnel.NATSRequest{Bucket: "carts"}); !a.NeedsConfirm {
		t.Fatalf("delete without confirm: %+v", a)
	}
	if a := natsDo(t, owner, orec, tunnel.NATSOpKVDelete, &tunnel.NATSRequest{Bucket: "carts"}, func(r *tunnel.StoreRequest) { r.Confirm = "carts" }); a.Type != "result" {
		t.Fatalf("delete: %s", a.Error)
	}

	// The recordings hold the values read and written.
	b, _ := os.ReadFile(rfile)
	if !strings.Contains(string(b), "KV GET carts c1") {
		t.Fatal("the read is not in the reader's recording")
	}
	b, _ = os.ReadFile(ofile)
	for _, want := range []string{"KV CREATE", "KV DELETE carts"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s is not in the owner's recording", want)
		}
	}
}

// The Network tab changes the firewall for the client port only, at Admin,
// and refuses what would need nats-server's own config changed.
func TestNATSNetwork(t *testing.T) {
	oldAddrs := hostAddresses
	hostAddresses = func() []RedisAddress { return nil }
	t.Cleanup(func() { hostAddresses = oldAddrs })
	l := newNATSLab(t)
	if a := natsDo(t, func() *natsSession { s, _, _ := l.open(t, tunnel.LevelOwner); return s }(), nil, tunnel.RabbitOpNetwork, nil); a.Type != "error" {
		t.Fatal("Manage was offered the network")
	}
	s, rec, file := l.open(t, tunnel.LevelDBA)
	if !slices.Contains(s.ready().Ops, tunnel.RabbitOpNetworkSet) {
		t.Fatalf("ops %v", s.ready().Ops)
	}

	// The real server listens on the loopback: nothing to open from here.
	var log []string
	fw := &fakeRedisFirewall{active: true}
	s.f = &natsFactory{cfg: StoreConfig{Firewall: func(ctx context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
		if !r.Check {
			log = append(log, "firewall "+r.Reach)
		}
		return fw.do(ctx, r)
	}}}
	a := natsDo(t, s, rec, tunnel.RabbitOpNetwork, nil)
	if n, _ := a.Data.(NATSNetwork); a.Type != "result" || n.Bind != "127.0.0.1" || n.Reach != tunnel.ReachLocal {
		t.Fatalf("view: %+v %s", a.Data, a.Error)
	}
	sel := func(r *tunnel.StoreRequest) {
		r.Reach, r.Addresses, r.Sources = tunnel.ReachSelected, []string{"10.0.0.183"}, []string{"10.0.0.61"}
	}
	if a := natsDo(t, s, rec, tunnel.RabbitOpNetworkSet, nil, sel); a.Type != "error" || !strings.Contains(a.Error, "nats-server.conf") || len(log) != 0 {
		t.Fatalf("loopback: %v %+v", log, a)
	}

	// A server listening on every address is limited by the firewall.
	s.f.httpGet = func(context.Context, string) ([]byte, error) {
		return []byte(`{"host":"0.0.0.0","port":` + strconv.Itoa(l.port) + `}`), nil
	}
	if a := natsDo(t, s, rec, tunnel.RabbitOpNetworkSet, nil, sel); a.Type != "result" || !slices.Equal(log, []string{"firewall selected"}) ||
		a.Data.(NATSNetworkResult).Network.Reach != tunnel.ReachSelected {
		t.Fatalf("selected: %v %+v", log, a)
	}
	if a := natsDo(t, s, rec, tunnel.RabbitOpNetworkSet, nil, func(r *tunnel.StoreRequest) { r.Reach = tunnel.ReachAny }); a.NeedsApproval == nil || a.NeedsApproval.Text != "FIREWALL ALLOW ANY" {
		t.Fatalf("anywhere: %+v", a)
	}
	// A monitoring port that answers for another server is not believed.
	s.f.httpGet = func(context.Context, string) ([]byte, error) { return []byte(`{"host":"0.0.0.0","port":4999}`), nil }
	if a := natsDo(t, s, rec, tunnel.RabbitOpNetwork, nil); a.Type != "error" || !strings.Contains(a.Error, "not") {
		t.Fatalf("other server: %+v", a)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "FIREWALL") {
		t.Fatal("the change is not in the recording")
	}
}
