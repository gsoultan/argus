package control

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

/*
A change Argus made must not wait for the next five-minute read to show.
Root cause: after a change the whole account was read once, in the
background, and that read was dropped when another was already in flight
(which may have read the region before the change); nothing read the region
again unless something in it happened to be in a state named as settling,
which a detached disk, a grown disk or a resized group never is. The
change's region is now read at once on its own, waits for a read in flight
instead of being dropped, and is read again each settle for a while.
*/

func TestCloudChangeIsFollowedInItsRegion(t *testing.T) {
	e := storageAPI(t)
	ctx := context.Background()
	c := e.ask(t, "admin-token", e.storage("volume-attach", e.account+"-vol-spare", `,"spec":{"instance":"`+e.instance()+`"}`), http.StatusAccepted)
	if done := e.approveChange(t, c); done.State != "applied" {
		t.Fatalf("attach: %+v", done)
	}
	e.resync(t)
	acct, err := e.s.CloudAccount(ctx, e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in the fake's region is between two states, and it is still
	// read again.
	for _, r := range e.fake.Resources[storageRegion] {
		if r.Settling() {
			t.Fatalf("the test needs a region with nothing settling, found %s %s", r.ID, r.State)
		}
	}
	if got := e.a.settlingRegions(ctx, acct); !slices.Equal(got, []string{storageRegion}) {
		t.Fatalf("the region of a change just made is not read again: %v", got)
	}
	// And not for ever.
	for i := 0; i < cloudFollowReads; i++ {
		e.a.syncCloudRegions(ctx, acct, []string{storageRegion})
	}
	if got := e.a.settlingRegions(ctx, acct); len(got) != 0 {
		t.Fatalf("still followed after %d reads: %v", cloudFollowReads, got)
	}
}

func TestCloudReadAfterAChangeWaitsForTheReadInFlight(t *testing.T) {
	e := storageAPI(t)
	ctx := context.Background()
	acct, err := e.s.CloudAccount(ctx, e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := cloudReadRetry
	cloudReadRetry = 5 * time.Millisecond
	t.Cleanup(func() { cloudReadRetry = old })

	// A read is in flight.
	key := tenantIDOf(ctx) + "/" + acct.ID
	e.a.cloud.mu.Lock()
	e.a.cloud.syncing[key] = true
	e.a.cloud.mu.Unlock()
	reads := func() int {
		n := 0
		for _, c := range e.fake.CallLog() {
			if strings.HasPrefix(c, "inventory "+storageRegion+" ") {
				n++
			}
		}
		return n
	}
	before := reads()
	e.a.syncCloudAfterChange(ctx, acct, storageRegion)
	time.Sleep(40 * time.Millisecond)
	if got := reads(); got != before {
		t.Fatalf("read while another read was in flight: %d, was %d", got, before)
	}
	// It ends; the change's read happens, rather than having been dropped.
	e.a.cloud.mu.Lock()
	delete(e.a.cloud.syncing, key)
	e.a.cloud.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for reads() < before+2 { // the region on its own, then the whole account
		if time.Now().After(deadline) {
			t.Fatalf("the read after the change never happened: %d reads, was %d", reads(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
