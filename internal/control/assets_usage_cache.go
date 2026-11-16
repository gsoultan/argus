package control

import (
	"context"
	"strings"
	"sync"
	"time"
)

/*
The Assets cards' usage, read once per organization and scope every
assetsUsageFor however many consoles ask.

Agents report usage about once a minute, and the cards ask every 15 s so a
report shows within about that. Without this every open Assets page would
be a query every 15 s; with it, a fleet-wide view is one query per
organization per assetsUsageFor, and each Member or Approver one per person.

The key is never the role alone. The whole-fleet roles share "fleet" in their
organization; everyone else is keyed by their own lowercased email, because
what they see is what was given to them (heldAssignmentsSQL). Keyed by role,
one member would be shown another's servers.

The cost of sharing: a change to who holds a server -- taken off a team, an
assignment removed -- reaches that person's cards up to assetsUsageFor late.
The figures are three numbers and running-low warnings, and the list of
servers itself is not cached, so the server disappears from the page at once.
*/

// assetsUsageFor is how long one read of the cards' usage is shared.
var assetsUsageFor = 10 * time.Second

// maxAssetsUsageKeys bounds the cache; a key is an organization and a
// person, so past this the expired ones go, and failing that, all of them.
const maxAssetsUsageKeys = 4096

type assetsUsageEntry struct {
	mu  sync.Mutex // held while the read runs, so one read serves a burst
	at  time.Time
	val AssetsUsage
	ok  bool
}

type assetsUsageCache struct {
	mu      sync.Mutex
	entries map[string]*assetsUsageEntry
	now     func() time.Time
}

func (c *assetsUsageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// assetsUsageKey is who an answer may be shared with: the organization,
// then "fleet" or the person.
func assetsUsageKey(ctx context.Context, scope string) string {
	who := "fleet"
	if scope != "" {
		who = "person\x00" + strings.ToLower(scope)
	}
	return tenantIDOf(ctx) + "\x00" + who
}

// get answers from a read no older than assetsUsageFor, or reads.
func (c *assetsUsageCache) get(ctx context.Context, scope string,
	read func(context.Context) (AssetsUsage, error)) (AssetsUsage, error) {
	key := assetsUsageKey(ctx, scope)
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*assetsUsageEntry{}
	}
	e := c.entries[key]
	if e == nil {
		if len(c.entries) >= maxAssetsUsageKeys {
			c.evictLocked()
		}
		e = &assetsUsageEntry{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ok && c.clock().Sub(e.at) < assetsUsageFor {
		return e.val, nil
	}
	v, err := read(ctx)
	if err != nil {
		return v, err
	}
	e.val, e.at, e.ok = v, c.clock(), true
	return v, nil
}

// evictLocked drops expired entries, or every entry when none had expired.
// Called with c.mu held.
func (c *assetsUsageCache) evictLocked() {
	now := c.clock()
	for k, e := range c.entries {
		if e.mu.TryLock() {
			stale := !e.ok || now.Sub(e.at) >= assetsUsageFor
			e.mu.Unlock()
			if stale {
				delete(c.entries, k)
			}
		}
	}
	if len(c.entries) >= maxAssetsUsageKeys {
		c.entries = map[string]*assetsUsageEntry{}
	}
}
