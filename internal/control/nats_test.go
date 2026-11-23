package control

import (
	"context"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// NATS entries: one account of a server in operator mode, on the server
// itself.
func TestNATSEntries(t *testing.T) {
	_, s, _, pg, admin := dbFixture(t)
	ctx := context.Background()
	n, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineNATS, Name: "ORDERS"}, admin)
	if err != nil {
		t.Fatalf("nats: %v", err)
	}
	if n.Name != "ORDERS" || n.Host != "127.0.0.1" || n.Port != 4222 || n.ConfigPath != "" || !n.Broker() {
		t.Fatalf("nats defaults: %+v", n)
	}
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineNATS, Name: "PAYMENTS",
		Port: 4223, ConfigPath: "/etc/argus/nats-payments.conf"}, admin); err != nil {
		t.Fatalf("a port and a key file: %v", err)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineNATS},
		{Engine: tunnel.EngineNATS, Name: "SYS"},
		{Engine: tunnel.EngineNATS, Name: "orders.eu"},
		{Engine: tunnel.EngineNATS, Name: "X", Host: "nats.internal"},
		{Engine: tunnel.EngineNATS, Name: "Y", ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineNATS, Name: "Z", Port: 70000},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
}
