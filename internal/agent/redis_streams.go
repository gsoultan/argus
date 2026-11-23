package agent

import (
	"context"
	"strconv"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A Redis stream's consumer groups, for the key editor: each group's place,
its consumers and its pending entries. Read as the person's own user, so
the ACL decides, and not recorded: it is the bookkeeping around entries,
as the key browser's names are. Changes to groups go through the edit
(XGROUP, XACK, XCLAIM), recorded and with an undo copy of the key.
*/

// StreamGroup is one consumer group of a stream.
type StreamGroup struct {
	Name          string `json:"name"`
	Consumers     int64  `json:"consumers"`
	Pending       int64  `json:"pending"`
	LastDelivered string `json:"lastDelivered"`
	// Behind is the entries after the last delivered one (Redis 7's lag);
	// nil where the server does not know it.
	Behind      *int64 `json:"behind,omitempty"`
	EntriesRead *int64 `json:"entriesRead,omitempty"`
}

// StreamConsumer is one consumer of a group.
type StreamConsumer struct {
	Name    string `json:"name"`
	Pending int64  `json:"pending"`
	IdleMS  int64  `json:"idleMs"`
	// InactiveMS is since its last successful read (Redis 7.2); nil before.
	InactiveMS *int64 `json:"inactiveMs,omitempty"`
}

// StreamPending is one entry delivered to a consumer and not acknowledged.
type StreamPending struct {
	ID         string `json:"id"`
	Consumer   string `json:"consumer"`
	IdleMS     int64  `json:"idleMs"`
	Deliveries int64  `json:"deliveries"`
}

// StreamGroupsResult is a stream's groups and, when one is named, its
// consumers and first pending entries.
type StreamGroupsResult struct {
	Groups    []StreamGroup    `json:"groups"`
	Group     string           `json:"group,omitempty"`
	Consumers []StreamConsumer `json:"consumers,omitempty"`
	Pending   []StreamPending  `json:"pending,omitempty"`
	// PendingMore says the group has more pending entries than are shown.
	PendingMore bool `json:"pendingMore,omitempty"`
}

// maxPendingShown bounds the pending entries one answer lists.
const maxPendingShown = 200

func (s *redisSession) streamGroups(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	key := s.rawKey(req)
	if key == "" || len(key) > tunnel.MaxCommand || len(req.Name) > tunnel.MaxCommand {
		return errAnswer("no stream key named")
	}
	lim := respLimit{items: 20000, budget: tunnel.MaxStoreAnswer}
	run := func(args ...string) (tunnel.RedisValue, string, error) {
		v, why, fatal := s.exec(ctx, lim, args...)
		if fatal != nil {
			return v, "", fatal
		}
		if why != nil {
			return v, why.Error(), nil
		}
		if v.T == "e" {
			return v, v.V, nil
		}
		return v, "", nil
	}
	v, why, err := run("XINFO", "GROUPS", key)
	if err != nil {
		return tunnel.StoreAnswer{}, err
	}
	if why != "" {
		if strings.Contains(why, "no such key") {
			why = "the key no longer exists"
		}
		return errAnswer("%s", why)
	}
	res := StreamGroupsResult{Groups: []StreamGroup{}}
	for _, g := range v.A {
		m := redisFields(g)
		res.Groups = append(res.Groups, StreamGroup{Name: m.str("name"), Consumers: m.num("consumers"), Pending: m.num("pending"),
			LastDelivered: m.str("last-delivered-id"), Behind: m.opt("lag"), EntriesRead: m.opt("entries-read")})
	}
	if req.Name == "" {
		return tunnel.StoreAnswer{Type: "result", Data: res}, nil
	}
	res.Group = req.Name
	v, why, err = run("XINFO", "CONSUMERS", key, req.Name)
	if err != nil {
		return tunnel.StoreAnswer{}, err
	}
	if why != "" {
		return errAnswer("%s", why)
	}
	res.Consumers = []StreamConsumer{}
	for _, c := range v.A {
		m := redisFields(c)
		res.Consumers = append(res.Consumers, StreamConsumer{Name: m.str("name"), Pending: m.num("pending"),
			IdleMS: m.num("idle"), InactiveMS: m.opt("inactive")})
	}
	v, why, err = run("XPENDING", key, req.Name, "-", "+", strconv.Itoa(maxPendingShown+1))
	if err != nil {
		return tunnel.StoreAnswer{}, err
	}
	if why != "" {
		return errAnswer("%s", why)
	}
	res.Pending = []StreamPending{}
	for _, p := range v.A {
		if p.T != "a" || len(p.A) < 4 {
			continue
		}
		if len(res.Pending) == maxPendingShown {
			res.PendingMore = true
			break
		}
		res.Pending = append(res.Pending, StreamPending{ID: p.A[0].V, Consumer: p.A[1].V, IdleMS: p.A[2].I, Deliveries: p.A[3].I})
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}

// redisFieldMap is a reply of alternating names and values.
type redisFieldMap map[string]tunnel.RedisValue

func redisFields(v tunnel.RedisValue) redisFieldMap {
	m := redisFieldMap{}
	for i := 0; i+1 < len(v.A); i += 2 {
		m[v.A[i].V] = v.A[i+1]
	}
	return m
}

func (m redisFieldMap) str(k string) string { return m[k].V }

func (m redisFieldMap) num(k string) int64 {
	v := m[k]
	if v.T == "i" {
		return v.I
	}
	n, _ := strconv.ParseInt(v.V, 10, 64)
	return n
}

func (m redisFieldMap) opt(k string) *int64 {
	v, ok := m[k]
	if !ok || v.T == "n" || (v.T != "i" && v.V == "") {
		return nil
	}
	n := m.num(k)
	return &n
}
