//go:build !windows

package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Against a real Kafka: at Manage a topic's settings change and reset, it
// gets more partitions, its oldest messages go before the exact offsets a
// dry run worked out, and an empty group is moved and deleted; a group
// reaching past the entry is not deleted, and Write may do none of it.
func TestKafkaChangesAgainstRealKafka(t *testing.T) {
	k := startKafka(t)
	ctx := context.Background()
	topic, other := k.scope+"orders", "zz-"+k.scope+"other"
	k.topic(t, topic, 2)
	k.topic(t, other, 1)
	t.Cleanup(func() { _, _ = k.adm.DeleteTopics(ctx, other) })
	a, _ := readKafkaAdmin(k.admin)
	cl, err := kafkaClient("127.0.0.1:"+strconv.Itoa(k.port), a.User, a.Password, a.Mechanism, 10*time.Second, kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for i := range 10 {
		if err := cl.ProduceSync(ctx, &kgo.Record{Topic: topic, Partition: int32(i % 2), Value: []byte("m" + strconv.Itoa(i))}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	group, wide := k.scope+"billing", k.scope+"wide"
	commit := func(g string, os map[string]map[int32]int64) {
		var o kadm.Offsets
		for t, ps := range os {
			for p, at := range ps {
				o.AddOffset(t, p, at, -1)
			}
		}
		if res, err := k.adm.CommitOffsets(ctx, g, o); err != nil || res.Error() != nil {
			t.Fatalf("commit: %v %v", err, res.Error())
		}
	}
	commit(group, map[string]map[int32]int64{topic: {0: 1, 1: 1}})
	commit(wide, map[string]map[int32]int64{topic: {0: 1}, other: {0: 0}})
	t.Cleanup(func() { _, _ = k.adm.DeleteGroups(ctx, group, wide) })

	svc := Kafka(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: t.TempDir()})
	write := openKafka(t, k, svc, tunnel.LevelWrite, k.scope)
	defer write.close()
	owner := openKafka(t, k, svc, tunnel.LevelOwner, k.scope)
	defer owner.close()
	ch := func(op, name string, c tunnel.KafkaChange, confirm string) tunnel.StoreAnswer {
		return owner.ask(tunnel.StoreRequest{Op: op, Name: name, KafkaChange: &c, Confirm: confirm})
	}

	if a := write.ask(tunnel.StoreRequest{Op: tunnel.KafkaOpAddPartitions, Name: topic, KafkaChange: &tunnel.KafkaChange{Add: 1}}); a.Type != "error" {
		t.Fatalf("write added partitions: %+v", a)
	}
	if a := ch(tunnel.KafkaOpAlterConfigs, topic, tunnel.KafkaChange{Set: map[string]string{"retention.ms": "3600000", "cleanup.policy": "compact,delete"}}, ""); a.Type != "result" {
		t.Fatalf("alter: %+v", a)
	}
	if a := ch(tunnel.KafkaOpAlterConfigs, topic, tunnel.KafkaChange{Set: map[string]string{"unclean.leader.election.enable": "true"}}, ""); a.Type != "error" {
		t.Fatalf("a setting Argus does not offer changed: %+v", a)
	}
	cfg := func(key string) KafkaConfig {
		d := dataOf[struct{ Configs []KafkaConfig }](t, owner.ask(tunnel.StoreRequest{Op: tunnel.KafkaOpTopicConfigs, Name: topic}))
		for _, c := range d.Configs {
			if c.Key == key {
				return c
			}
		}
		t.Fatalf("no %s", key)
		return KafkaConfig{}
	}
	if c := cfg("retention.ms"); c.Value != "3600000" || c.Default || !c.Editable {
		t.Fatalf("retention.ms: %+v", c)
	}
	if a := ch(tunnel.KafkaOpAlterConfigs, topic, tunnel.KafkaChange{Reset: []string{"retention.ms"}}, ""); a.Type != "result" {
		t.Fatalf("reset: %+v", a)
	}
	if c := cfg("retention.ms"); !c.Default {
		t.Fatalf("reset left it set: %+v", c)
	}
	if c := cfg("cleanup.policy"); c.Value != "compact,delete" {
		t.Fatalf("a reset rewrote another setting: %+v", c)
	}
	if a := ch(tunnel.KafkaOpAddPartitions, topic, tunnel.KafkaChange{Add: 1}, ""); a.Type != "result" {
		t.Fatalf("add partitions: %+v", a)
	}

	// Delete the oldest: before offset 3 on partition 0.
	p0 := int32(0)
	plan := ch(tunnel.KafkaOpDeleteRecords, topic, tunnel.KafkaChange{DryRun: true, To: "offset", Partition: &p0, Offset: 3}, "")
	d := dataOf[KafkaDryRun](t, plan)
	if len(d.Plan) != 1 || d.Offsets[topic][0] != 3 || d.Unread[group] != 2 {
		t.Fatalf("dry run: %+v", d)
	}
	if a := ch(tunnel.KafkaOpDeleteRecords, topic, tunnel.KafkaChange{Offsets: d.Offsets}, ""); a.Type != "error" || !a.NeedsConfirm {
		t.Fatalf("deleted without the server's name: %+v", a)
	}
	if a := ch(tunnel.KafkaOpDeleteRecords, topic, tunnel.KafkaChange{Offsets: d.Offsets}, "cache-01"); a.Type != "result" {
		t.Fatalf("delete: %+v", a)
	}
	if !owner.recordedHas("stmt", topic+" partition 0 offset 3") {
		t.Fatal("the exact offsets are not recorded")
	}
	st, _ := k.adm.ListStartOffsets(ctx, topic)
	if o, _ := st.Lookup(topic, 0); o.Offset != 3 {
		t.Fatalf("partition 0 starts at %d", o.Offset)
	}

	// Move the group to the end, then delete it.
	g := dataOf[KafkaGroupDetail](t, ch(tunnel.KafkaOpGroup, "", tunnel.KafkaChange{Group: group}, ""))
	if g.State != "Empty" || len(g.Offsets) != 2 || g.Outside != 0 {
		t.Fatalf("group: %+v", g)
	}
	r := dataOf[KafkaDryRun](t, ch(tunnel.KafkaOpGroupReset, "", tunnel.KafkaChange{Group: group, DryRun: true, To: "latest"}, ""))
	if r.Offsets[topic][0] != 5 || r.Offsets[topic][1] != 5 {
		t.Fatalf("reset plan: %+v", r)
	}
	if a := ch(tunnel.KafkaOpGroupReset, "", tunnel.KafkaChange{Group: group, Offsets: map[string]map[int32]int64{other: {0: 0}}}, ""); a.Type != "error" {
		t.Fatalf("a group was moved on a topic outside the entry: %+v", a)
	}
	if a := ch(tunnel.KafkaOpGroupReset, "", tunnel.KafkaChange{Group: group, Offsets: r.Offsets}, ""); a.Type != "result" {
		t.Fatalf("reset: %+v", a)
	}
	if g := dataOf[KafkaGroupDetail](t, ch(tunnel.KafkaOpGroup, "", tunnel.KafkaChange{Group: group}, "")); g.Offsets[0].Now != 5 {
		t.Fatalf("after reset: %+v", g)
	}
	if a := ch(tunnel.KafkaOpDeleteGroup, "", tunnel.KafkaChange{Group: wide}, ""); a.Type != "error" || !strings.Contains(a.Error, "outside") {
		t.Fatalf("a group reaching past the entry was deleted: %+v", a)
	}
	if a := ch(tunnel.KafkaOpDeleteGroup, "", tunnel.KafkaChange{Group: group}, ""); a.Type != "result" {
		t.Fatalf("delete group: %+v", a)
	}

	// On production every change waits.
	prod, err := openStoreSession(t, svc, tunnel.KafkaService, tunnel.GrantDB{ID: "k1", Engine: tunnel.EngineKafka, Name: k.scope,
		Host: "127.0.0.1", Port: k.port, Level: tunnel.LevelOwner, ConfigPath: k.admin, ChangesNeedApproval: true}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer prod.close()
	req := tunnel.StoreRequest{Op: tunnel.KafkaOpAddPartitions, Name: topic, KafkaChange: &tunnel.KafkaChange{Add: 1}}
	if a := prod.ask(req); a.NeedsApproval == nil || a.NeedsApproval.Text != tunnel.KafkaChangeText(req) {
		t.Fatalf("production did not wait: %+v", a)
	}
}
