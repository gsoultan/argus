package control

import (
	"context"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

// MongoDB entries: what the control plane will save.
func TestMongoDBEntries(t *testing.T) {
	_, s, _, pg, admin := dbFixture(t)
	ctx := context.Background()
	m, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMongoDB, Name: "shop"}, admin)
	if err != nil {
		t.Fatalf("mongodb: %v", err)
	}
	if m.Host != "127.0.0.1" || m.Port != 27017 || m.ConfigPath != "" || m.Label != "production" || m.AdminUser != "" {
		t.Fatalf("mongodb defaults: %+v", m)
	}
	if _, err := s.CreateDatabase(ctx, Database{AssetID: pg.AssetID, Engine: tunnel.EngineMongoDB, Name: "billing",
		Host: "/tmp/mongodb-27017.sock", ConfigPath: "/etc/argus/mongodb-shop.conf"}, admin); err != nil {
		t.Fatalf("socket and admin file: %v", err)
	}
	for _, bad := range []Database{
		{Engine: tunnel.EngineMongoDB, Name: "admin"},
		{Engine: tunnel.EngineMongoDB, Name: "local"},
		{Engine: tunnel.EngineMongoDB, Name: "a.b"},
		{Engine: tunnel.EngineMongoDB, Name: "a b"},
		{Engine: tunnel.EngineMongoDB, Name: "x", Host: "mongo.internal"},
		{Engine: tunnel.EngineMongoDB, Name: "y", ConfigPath: "/etc/shadow"},
		{Engine: tunnel.EngineMongoDB, Name: "z", ConfigPath: "/etc/../root/mongo.conf"},
		{Engine: tunnel.EngineMongoDB, Name: tunnel.TestDBPrefix + "x"},
	} {
		bad.AssetID = pg.AssetID
		if _, err := s.CreateDatabase(ctx, bad, admin); err == nil {
			t.Errorf("saved %+v", bad)
		} else if !errorsAs(err, new(ErrInvalidDatabase)) {
			t.Errorf("%+v: not an invalid-entry error: %v", bad, err)
		}
	}
}
