package control

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
A snapshot AWS finished must not stay "taking 0%" in Argus until the next
five-minute read. Root cause: the inventory was read only every five
minutes, so whatever was between two states showed where it was at the last
read. A region whose read found something settling is now read again on
its own every half minute until it settles.
*/

func TestCloudSettlingRegionIsReadAgainUntilDone(t *testing.T) {
	e := storageAPI(t)
	ctx := context.Background()
	snap := e.account + "-snap-new"
	e.fake.Resources[storageRegion] = append(e.fake.Resources[storageRegion],
		cloud.Resource{Kind: cloud.KindSnapshot, ID: snap, Name: "db before upgrade", Region: storageRegion,
			State: "pending", Size: "250 GiB", Details: map[string]string{"progress": "0%"}})
	e.resync(t)
	acct, err := e.s.CloudAccount(ctx, e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.a.settlingRegions(ctx, acct); !slices.Equal(got, []string{storageRegion}) {
		t.Fatalf("settling after a pending snapshot: %v", got)
	}

	// A failed read does not forget the region.
	e.fake.RegionErr = map[string]error{storageRegion: errors.New("throttled")}
	e.a.syncCloudRegions(ctx, acct, []string{storageRegion})
	if got := e.a.settlingRegions(ctx, acct); !slices.Equal(got, []string{storageRegion}) {
		t.Fatalf("settling after a failed read: %v", got)
	}
	e.fake.RegionErr = nil

	// AWS finishes it; the settle read alone brings that in.
	for i, r := range e.fake.Resources[storageRegion] {
		if r.ID == snap {
			e.fake.Resources[storageRegion][i].State = "completed"
			e.fake.Resources[storageRegion][i].Details = map[string]string{"progress": "100%"}
		}
	}
	before, _ := e.s.CloudAccount(ctx, e.acct.ID)
	if !e.a.syncCloudRegions(ctx, acct, e.a.settlingRegions(ctx, acct)) {
		t.Fatal("the settle read was skipped")
	}
	items, err := e.s.CloudResources(ctx, cloud.KindSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range items {
		if it.Resource.ID == snap {
			found = true
			if it.Resource.State != "completed" || it.Resource.Details["progress"] != "100%" {
				t.Fatalf("snapshot after the settle read: %s %v", it.Resource.State, it.Resource.Details)
			}
		}
	}
	if !found {
		t.Fatal("the snapshot is gone from the inventory")
	}
	if got := e.a.settlingRegions(ctx, acct); len(got) != 0 {
		t.Fatalf("still settling once done: %v", got)
	}
	// A partial read leaves the account's last full read as it was.
	after, _ := e.s.CloudAccount(ctx, e.acct.ID)
	if before.SyncedAt == nil || after.SyncedAt == nil || !before.SyncedAt.Equal(*after.SyncedAt) {
		t.Fatalf("a settle read moved the last full read: %v to %v", before.SyncedAt, after.SyncedAt)
	}
}
