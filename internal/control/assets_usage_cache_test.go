package control

import (
	"context"
	"testing"
	"time"
)

// TestAssetsUsageCacheScopes: one read serves everyone who may see the same
// answer for assetsUsageFor; two people, or two organizations, never share
// one; and a read past the window asks again.
func TestAssetsUsageCacheScopes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := assetsUsageCache{now: func() time.Time { return now }}
	reads := map[string]int{}
	read := func(who string) func(context.Context) (AssetsUsage, error) {
		return func(context.Context) (AssetsUsage, error) {
			reads[who]++
			return AssetsUsage{Assets: []AssetUsageRow{{AssetID: who}}}, nil
		}
	}
	ctx := context.Background()
	other := WithTenant(ctx, &Tenant{ID: "org-b"})

	for i := 0; i < 5; i++ {
		if v, _ := c.get(ctx, "", read("fleet")); v.Assets[0].AssetID != "fleet" {
			t.Fatalf("fleet answer: %+v", v)
		}
	}
	if v, _ := c.get(ctx, "Ana@Example.com", read("ana")); v.Assets[0].AssetID != "ana" {
		t.Fatalf("a member was given another scope's answer: %+v", v)
	}
	if v, _ := c.get(ctx, "ana@example.com", read("ana")); v.Assets[0].AssetID != "ana" {
		t.Fatalf("same person, other case: %+v", v)
	}
	if v, _ := c.get(ctx, "bo@example.com", read("bo")); v.Assets[0].AssetID != "bo" {
		t.Fatalf("one member was shown another's servers: %+v", v)
	}
	if v, _ := c.get(other, "", read("fleet-b")); v.Assets[0].AssetID != "fleet-b" {
		t.Fatalf("an organization was shown another's fleet: %+v", v)
	}
	if reads["fleet"] != 1 || reads["ana"] != 1 || reads["bo"] != 1 || reads["fleet-b"] != 1 {
		t.Fatalf("reads %v, want one per scope", reads)
	}
	now = now.Add(assetsUsageFor)
	c.get(ctx, "", read("fleet"))
	if reads["fleet"] != 2 {
		t.Fatalf("an answer older than %v was served", assetsUsageFor)
	}
}

// TestAssetsUsageCacheBounded: the cache never holds more than its bound.
func TestAssetsUsageCacheBounded(t *testing.T) {
	var c assetsUsageCache
	read := func(context.Context) (AssetsUsage, error) { return AssetsUsage{}, nil }
	for i := 0; i < maxAssetsUsageKeys+50; i++ {
		c.get(context.Background(), "p"+string(rune('a'+i%26))+time.Duration(i).String(), read)
	}
	if n := len(c.entries); n > maxAssetsUsageKeys {
		t.Fatalf("%d entries, bound %d", n, maxAssetsUsageKeys)
	}
}
