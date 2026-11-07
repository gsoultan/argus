package control

import (
	"context"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Kafka entries: what the control plane will save.
func TestKafkaEntries(t *testing.T) {
	_, s, _, pg, admin := dbFixture(t)
	ctx := context.Background()
	k, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineKafka}, admin)
	if err != nil {
		t.Fatalf("kafka: %v", err)
	}
	if k.Name != "*" || k.Host != "127.0.0.1" || k.Port != 9092 || k.ConfigPath != "" || k.AdminUser != "" {
		t.Fatalf("kafka defaults: %+v", k)
	}
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineKafka, Name: "orders.",
		Port: 9094, ConfigPath: "/etc/argus/kafka-orders.conf"}, admin); err != nil {
		t.Fatalf("a prefix and an admin file: %v", err)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineKafka, Name: "orders/*"},
		{Engine: tunnel.EngineKafka, Name: "a b"},
		{Engine: tunnel.EngineKafka, Name: "x", Host: "kafka.internal"},
		{Engine: tunnel.EngineKafka, Name: "y", ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineKafka, Name: "z", ConfigPath: "/etc/../root/kafka.conf"},
		{Engine: tunnel.EngineKafka, Name: "w", Port: 70000},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
}
