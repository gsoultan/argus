package tunnel

import (
	"slices"
	"testing"
)

func TestNATSSubjects(t *testing.T) {
	for s, ok := range map[string]bool{
		"orders.eu.created": true, "orders.*.created": true, "orders.>": true, ">": true, "*": true,
		"": false, "orders..x": false, ".orders": false, "orders.": false, "orders.>.x": false,
		"orders.e*": false, "orders. eu": false, "orders.>x": false,
	} {
		if ValidNATSSubject(s) != ok {
			t.Errorf("ValidNATSSubject(%q) = %v", s, !ok)
		}
	}
	for s, ok := range map[string]bool{
		"orders.eu.created": true, "orders.*": false, "$JS.API.STREAM.DELETE.X": false, "$KV.carts.c1": false,
		"_INBOX.abc": false, "_INBOX_argus_x.y": false, "$SYS.REQ.SERVER.PING": false,
	} {
		if NATSAppSubject(s) != ok {
			t.Errorf("NATSAppSubject(%q) = %v", s, !ok)
		}
	}
	for _, c := range []struct {
		f, s string
		ok   bool
	}{
		{"orders.>", "orders.eu.created", true}, {"orders.>", "orders", false}, {"orders.*", "orders.eu", true},
		{"orders.*", "orders.eu.created", false}, {"orders.*.created", "orders.us.created", true},
		{"orders.*.created", "orders.us.paid", false}, {">", "anything.at.all", true}, {"a.b", "a.b", true}, {"a.b", "a.c", false},
	} {
		if NATSSubjectMatches(c.f, c.s) != c.ok {
			t.Errorf("NATSSubjectMatches(%q, %q) = %v", c.f, c.s, !c.ok)
		}
	}
}

func TestNATSNamesAndLevels(t *testing.T) {
	for n, ok := range map[string]bool{"ORDERS": true, "orders-eu_1": true, "SYS": false, "$SYS": false, "a.b": false, "": false} {
		if ValidNATSAccount(n) != ok {
			t.Errorf("ValidNATSAccount(%q) = %v", n, !ok)
		}
	}
	if NATSStoreOps("nobody") != nil {
		t.Fatal("an unknown level was given operations")
	}
	read, owner := NATSStoreOps(LevelRead), NATSStoreOps(LevelOwner)
	for _, op := range read {
		if natsOps[op] != LevelRead {
			t.Errorf("read was given %s", op)
		}
	}
	if len(NATSStoreOps(LevelDBA)) != len(natsOps) || len(owner) != len(natsOps)-4 || slices.Contains(owner, RabbitOpNetworkSet) ||
		slices.Contains(owner, BrokerOpUserSet) || !slices.Contains(owner, BrokerOpUsers) {
		t.Fatal("admin has every operation, manage every one but the network and signing users")
	}
	if !NATSNeedsConfirm(NATSOpKVDelete, nil) {
		t.Fatal("removing a bucket needs its name")
	}
	if !NATSNeedsConfirm("stream_purge", &NATSRequest{Stream: "X"}) || NATSNeedsConfirm("stream_purge", &NATSRequest{Stream: "X", Subject: "a.b"}) ||
		!NATSNeedsConfirm("stream_delete", nil) || NATSNeedsConfirm("consumer_delete", nil) {
		t.Fatal("confirm rules")
	}
}

func TestNATSSpecs(t *testing.T) {
	good := NATSStreamSpec{Name: "ORDERS", Subjects: []string{"orders.>"}, Retention: "workqueue", Storage: "file", Replicas: 3}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]NATSStreamSpec{
		"dot in name":   {Name: "a.b", Subjects: []string{"x"}},
		"no subjects":   {Name: "A"},
		"every subject": {Name: "A", Subjects: []string{">"}},
		"$JS subject":   {Name: "A", Subjects: []string{"$JS.API.>"}},
		"inbox":         {Name: "A", Subjects: []string{"_INBOX.>"}},
		"retention":     {Name: "A", Subjects: []string{"x"}, Retention: "forever"},
		"replicas":      {Name: "A", Subjects: []string{"x"}, Replicas: 7},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := NATSConsumerSpec{Name: "invoicer", Deliver: "by_start_time", StartTime: "2026-11-23T04:30:00Z", BackoffS: []int64{5, 30}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]NATSConsumerSpec{
		"no seq":       {Name: "c", Deliver: "by_start_sequence"},
		"bad time":     {Name: "c", Deliver: "by_start_time", StartTime: "yesterday"},
		"zero backoff": {Name: "c", BackoffS: []int64{0}},
		"push to $JS":  {Name: "c", DeliverSubject: "$JS.ACK.x"},
		"group only":   {Name: "c", DeliverGroup: "g"},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNATSKeyValueRules(t *testing.T) {
	for k, ok := range map[string]bool{"c1": true, "eu.c3": true, "a/b=c_d-e": true, "_kv.x": false, "a..b": false, ".a": false, "a b": false, "a*": false, "*": false} {
		if ValidNATSKey(k, false) != ok {
			t.Errorf("ValidNATSKey(%q) = %v", k, !ok)
		}
	}
	for k, ok := range map[string]bool{"eu.*": true, ">": true, "eu.>": true, "eu.>.x": false, "e*": false} {
		if ValidNATSKey(k, true) != ok {
			t.Errorf("ValidNATSKey(%q, filter) = %v", k, !ok)
		}
	}
	if (NATSKVSpec{Bucket: "carts", History: 5}).Validate() != nil {
		t.Fatal("a good bucket was refused")
	}
	for name, k := range map[string]NATSKVSpec{"dot": {Bucket: "a.b"}, "history": {Bucket: "a", History: 65}, "storage": {Bucket: "a", Storage: "disk"}, "replicas": {Bucket: "a", Replicas: 7}} {
		if k.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if NATSStoreOps(LevelRead) == nil || slices.Contains(NATSStoreOps(LevelRead), NATSOpKVPut) || !slices.Contains(NATSStoreOps(LevelWrite), NATSOpKVPut) ||
		slices.Contains(NATSStoreOps(LevelWrite), NATSOpKVCreate) {
		t.Fatal("key-value levels")
	}
	if StoreNetworkText(EngineNATS, StoreRequest{Reach: ReachAny}) != "FIREWALL ALLOW ANY" {
		t.Fatal("network text")
	}
}
