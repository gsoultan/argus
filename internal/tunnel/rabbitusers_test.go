package tunnel

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRabbitUserChanges(t *testing.T) {
	hash := base64.StdEncoding.EncodeToString(make([]byte, 36))
	perms := &RabbitPerms{Configure: "^inv\\.", Write: "^inv\\.", Read: ".*"}
	set := func(u BrokerUser) StoreRequest { return StoreRequest{Op: BrokerOpUserSet, BrokerUser: &u} }
	ok := set(BrokerUser{Name: "invoices", Create: true, PasswordHash: hash, Tags: &[]string{}, Perms: perms})
	if why := ValidRabbitUserChange("/orders", ok); why != "" {
		t.Fatalf("a good create was refused: %s", why)
	}
	for name, req := range map[string]StoreRequest{
		"no password":     set(BrokerUser{Name: "invoices", Create: true, Perms: perms}),
		"not a hash":      set(BrokerUser{Name: "invoices", Create: true, PasswordHash: "pw", Perms: perms}),
		"argus user":      set(BrokerUser{Name: "argus-ana-dba", Perms: perms}),
		"admin tag":       set(BrokerUser{Name: "invoices", Tags: &[]string{"administrator"}}),
		"policymaker tag": set(BrokerUser{Name: "invoices", Tags: &[]string{"policymaker"}}),
		"bad regex":       set(BrokerUser{Name: "invoices", Perms: &RabbitPerms{Configure: "(", Write: "", Read: ""}}),
		"no permissions":  set(BrokerUser{Name: "invoices", Create: true, PasswordHash: hash}),
		"nothing":         set(BrokerUser{Name: "invoices"}),
		"login no pw":     {Op: BrokerOpUserLogin, BrokerUser: &BrokerUser{Name: "invoices"}},
	} {
		if ValidRabbitUserChange("/orders", req) == "" {
			t.Errorf("%s passed", name)
		}
	}
	text := RabbitUserText("/orders", ok)
	if strings.Contains(text, hash) || !strings.Contains(text, "password_hash [redacted]") || !strings.Contains(text, `on vhost "/orders"`) {
		t.Fatalf("text: %s", text)
	}
	if BrokerUserOpLevelFor(EngineRabbitMQ, BrokerOpUsers) != LevelDBA || BrokerUserOpLevelFor(EngineKafka, BrokerOpUsers) != LevelOwner ||
		BrokerUserOpLevelFor(EngineRabbitMQ, "peek") != "" {
		t.Fatal("levels")
	}
}

func TestKafkaChangeRules(t *testing.T) {
	for name, req := range map[string]StoreRequest{
		"outside":       {Op: KafkaOpAddPartitions, Name: "payments", KafkaChange: &KafkaChange{Add: 1}},
		"zero":          {Op: KafkaOpAddPartitions, Name: "orders.eu", KafkaChange: &KafkaChange{Add: 0}},
		"unknown key":   {Op: KafkaOpAlterConfigs, Name: "orders.eu", KafkaChange: &KafkaChange{Set: map[string]string{"follower.replication.throttled.replicas": "*"}}},
		"bad value":     {Op: KafkaOpAlterConfigs, Name: "orders.eu", KafkaChange: &KafkaChange{Set: map[string]string{"cleanup.policy": "never"}}},
		"no offsets":    {Op: KafkaOpDeleteRecords, Name: "orders.eu", KafkaChange: &KafkaChange{}},
		"other topic":   {Op: KafkaOpDeleteRecords, Name: "orders.eu", KafkaChange: &KafkaChange{Offsets: map[string]map[int32]int64{"orders.us": {0: 1}}}},
		"reset outside": {Op: KafkaOpGroupReset, KafkaChange: &KafkaChange{Group: "g", Offsets: map[string]map[int32]int64{"payments": {0: 1}}}},
		"shift deletes": {Op: KafkaOpDeleteRecords, Name: "orders.eu", KafkaChange: &KafkaChange{DryRun: true, To: "shift", By: -1}},
		"group no name": {Op: KafkaOpDeleteGroup, KafkaChange: &KafkaChange{}},
	} {
		if ValidKafkaChange("orders.", req) == "" {
			t.Errorf("%s passed", name)
		}
	}
	ok := StoreRequest{Op: KafkaOpGroupReset, KafkaChange: &KafkaChange{Group: "billing", Offsets: map[string]map[int32]int64{"orders.eu": {1: 40, 0: 12}}}}
	if why := ValidKafkaChange("orders.", ok); why != "" {
		t.Fatal(why)
	}
	if got := KafkaChangeText(ok); got != "MOVE CONSUMER GROUP \"billing\" TO\n  orders.eu partition 0 offset 12\n  orders.eu partition 1 offset 40" {
		t.Fatalf("text: %q", got)
	}
}
