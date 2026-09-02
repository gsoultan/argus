// Package ratelimit throttles authentication attempts.
//
// Two properties matter more than the algorithm:
//
//   - The table is bounded. It is keyed by client address, which is chosen by
//     whoever is connecting, so an unbounded map is itself the denial of
//     service it was added to prevent.
//   - Failures are counted separately and far more tightly than successes.
//     Someone with valid credentials reconnecting is normal traffic; someone
//     producing failures at the same rate is not, and a single limit tuned to
//     tolerate the former barely inconveniences the latter.
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// Limit describes an allowance.
type Limit struct {
	// Rate is how many events are permitted per Window once the burst is spent.
	Rate int
	// Window the Rate applies over.
	Window time.Duration
	// Burst absorbs legitimate clustering — a browser opening several requests
	// on page load, or an operator retrying immediately after a typo.
	Burst int
}

// Limiter is a bounded set of token buckets keyed by client.
type Limiter struct {
	limit   Limit
	maxKeys int

	mu      sync.Mutex
	buckets map[string]*entry
	// LRU ordering so eviction drops the least recently seen client rather
	// than an arbitrary one. Evicting an active attacker's entry would reset
	// their allowance, which is exactly the wrong choice.
	order *list.List

	now func() time.Time // injectable for tests
}

type entry struct {
	tokens   float64
	lastSeen time.Time
	elem     *list.Element
	key      string
}

// DefaultMaxKeys bounds memory at roughly a few MB while comfortably covering
// the number of distinct addresses a real deployment sees.
const DefaultMaxKeys = 50_000

// New builds a limiter.
func New(limit Limit, maxKeys int) *Limiter {
	if limit.Burst <= 0 {
		limit.Burst = limit.Rate
	}
	if limit.Window <= 0 {
		limit.Window = time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &Limiter{
		limit:   limit,
		maxKeys: maxKeys,
		buckets: make(map[string]*entry),
		order:   list.New(),
		now:     time.Now,
	}
}

// Allow consumes one token for key, reporting whether the event may proceed and
// how long to wait if not.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e, exists := l.buckets[key]
	if !exists {
		l.evictIfFullLocked()
		e = &entry{tokens: float64(l.limit.Burst), lastSeen: now, key: key}
		e.elem = l.order.PushFront(e)
		l.buckets[key] = e
	} else {
		// Refill in proportion to elapsed time.
		refill := now.Sub(e.lastSeen).Seconds() *
			(float64(l.limit.Rate) / l.limit.Window.Seconds())
		e.tokens = minFloat(e.tokens+refill, float64(l.limit.Burst))
		e.lastSeen = now
		l.order.MoveToFront(e.elem)
	}

	if e.tokens < 1 {
		// How long until one token is available.
		perToken := l.limit.Window.Seconds() / float64(l.limit.Rate)
		wait := time.Duration((1 - e.tokens) * perToken * float64(time.Second))
		return false, wait
	}

	e.tokens--
	return true, 0
}

// Reset clears a key's history, used after a successful authentication so a
// user who mistyped once is not throttled for the rest of the window.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.buckets[key]; ok {
		e.tokens = float64(l.limit.Burst)
	}
}

// Len reports how many keys are tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// evictIfFullLocked drops the least recently seen entry. Caller holds the lock.
func (l *Limiter) evictIfFullLocked() {
	if len(l.buckets) < l.maxKeys {
		return
	}
	oldest := l.order.Back()
	if oldest == nil {
		return
	}
	e := oldest.Value.(*entry)
	l.order.Remove(oldest)
	delete(l.buckets, e.key)
}

// Sweep drops entries that have been idle long enough to have fully refilled,
// since they are indistinguishable from absent ones.
func (l *Limiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	idle := l.limit.Window * 2
	cutoff := l.now().Add(-idle)
	removed := 0

	for elem := l.order.Back(); elem != nil; {
		prev := elem.Prev()
		e := elem.Value.(*entry)
		if e.lastSeen.After(cutoff) {
			// The list is LRU-ordered, so everything ahead of this is newer.
			break
		}
		l.order.Remove(elem)
		delete(l.buckets, e.key)
		removed++
		elem = prev
	}
	return removed
}

// StartSweeper runs Sweep periodically until stop is closed.
func (l *Limiter) StartSweeper(every time.Duration, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.Sweep()
			case <-stop:
				return
			}
		}
	}()
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
