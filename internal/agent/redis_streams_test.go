//go:build !windows

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// A stream's consumer groups are read at Read and changed through the edit
// at Write: made, moved, a consumer's entries acknowledged and claimed, a
// consumer and a group removed, each recorded, with an undo copy that puts
// the groups back too.
func TestRedisStreamGroups(t *testing.T) {
	srv := startRedis(t)
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/rec", 0o700)
	svc := redisSvc(dir)
	for _, id := range []string{"1-0", "2-0", "3-0", "4-0", "5-0"} {
		srv.admin(t, "XADD", "orders", id, "n", id)
	}
	srv.admin(t, "XGROUP", "CREATE", "orders", "billing", "0")
	srv.admin(t, "XREADGROUP", "GROUP", "billing", "worker-1", "COUNT", "2", "STREAMS", "orders", ">")

	r := openRedis(t, srv, svc, tunnel.LevelRead, dir+"/rec")
	var res StreamGroupsResult
	data(t, r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "orders"}), &res)
	if len(res.Groups) != 1 || res.Groups[0].Name != "billing" || res.Groups[0].Pending != 2 || res.Groups[0].LastDelivered != "2-0" ||
		res.Groups[0].Behind == nil || *res.Groups[0].Behind != 3 {
		t.Fatalf("groups: %+v", res.Groups)
	}
	data(t, r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "orders", Name: "billing"}), &res)
	if len(res.Consumers) != 1 || res.Consumers[0].Name != "worker-1" || res.Consumers[0].Pending != 2 ||
		len(res.Pending) != 2 || res.Pending[0].ID != "1-0" || res.Pending[0].Deliveries != 1 {
		t.Fatalf("group: %+v", res)
	}
	if r.recorded("stmt", "XINFO") {
		t.Fatal("reading the groups is recorded")
	}
	if a := r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "orders", Edits: [][]string{{"XACK", "orders", "billing", "1-0"}}}); a.Type != "error" {
		t.Fatalf("Read acknowledged: %+v", a)
	}
	if a := r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "nope"}); a.Type != "error" {
		t.Fatalf("a missing key: %+v", a)
	}

	w := openRedis(t, srv, svc, tunnel.LevelWrite, dir+"/rec")
	a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "orders", Undo: true, Edits: [][]string{
		{"XGROUP", "CREATE", "orders", "audit", "$"},
		{"XACK", "orders", "billing", "1-0"},
		{"XCLAIM", "orders", "billing", "worker-2", "0", "2-0"},
		{"XGROUP", "DELCONSUMER", "orders", "billing", "worker-1"},
		{"XGROUP", "SETID", "orders", "billing", "4-0"},
	}})
	if a.Type != "result" {
		t.Fatalf("edit: %+v", a)
	}
	var er RedisEditResult
	data(t, a, &er)
	if !w.recorded("stmt", "XCLAIM orders billing worker-2 0 2-0") {
		t.Fatal("the claim is not recorded")
	}
	data(t, r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "orders", Name: "billing"}), &res)
	if len(res.Groups) != 2 || len(res.Consumers) != 1 || res.Consumers[0].Name != "worker-2" || len(res.Pending) != 1 || res.Pending[0].ID != "2-0" {
		t.Fatalf("after: %+v", res)
	}
	for _, g := range res.Groups {
		if g.Name == "billing" && g.LastDelivered != "4-0" {
			t.Fatalf("not moved: %+v", g)
		}
	}
	// Only this key's groups, and no other subcommand.
	for _, bad := range [][]string{{"XGROUP", "CREATE", "other", "g", "$"}, {"XGROUP", "HELP", "orders", "x"}, {"XREADGROUP", "orders", "g", "c"}} {
		if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "orders", Edits: [][]string{bad}}); a.Type != "error" {
			t.Fatalf("%v ran: %+v", bad, a)
		}
	}
	// Undo puts the groups back as they were.
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpUndo, UndoID: er.UndoID}); a.Type != "result" {
		t.Fatalf("undo: %+v", a)
	}
	data(t, r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "orders", Name: "billing"}), &res)
	if len(res.Groups) != 1 || len(res.Pending) != 2 || res.Consumers[0].Name != "worker-1" {
		t.Fatalf("after undo: %+v", res)
	}
	if a := w.ask(tunnel.StoreRequest{Op: tunnel.RedisOpEdit, Key: "orders", Edits: [][]string{{"XGROUP", "DESTROY", "orders", "billing"}}}); a.Type != "result" {
		t.Fatalf("destroy: %+v", a)
	}
	data(t, r.ask(tunnel.StoreRequest{Op: tunnel.RedisOpStreamGroups, Key: "orders"}), &res)
	if len(res.Groups) != 0 || !strings.Contains(srv.admin(t, "TYPE", "orders").V, "stream") {
		t.Fatalf("after destroy: %+v", res)
	}
}
