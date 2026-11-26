package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
perf: what the MongoDB Performance panel needs to say why this database is
heavy. Read-only, DBA only (the relay and this agent both refuse it below),
and run as the person's own user: at DBA that user has dbOwner on the
database and clusterMonitor, which is exactly what these reads need
(serverStatus, $currentOp, $collStats, $indexStats, the profiler's level and
system.profile). Nothing is recorded, as nothing is changed.

Each source is read on its own: one MongoDB refuses is named in Missing and
the rest still answers. The analysis is the console's
(web/src/lib/mongoPerformance).
*/

// MongoPerf answers perf.
type MongoPerf struct {
	Version string `json:"version"`
	// Server is serverStatus, cut to the parts the advice reads.
	Server bson.M `json:"server,omitempty"`
	// DB is dbStats for this database.
	DB bson.M `json:"db,omitempty"`
	// Collections are $collStats (storage and counts) with $indexStats
	// (each index's key, size and use since it was last loaded).
	Collections []MongoCollPerf `json:"collections"`
	// Profile is this database's profiler level and slowms.
	Profile bson.M `json:"profile,omitempty"`
	// Slow are system.profile's operations grouped by collection, kind and
	// plan, the slowest first; empty when the profiler is off.
	Slow []bson.M `json:"slow"`
	// Running are this database's operations running 5 seconds or more.
	Running []bson.M `json:"running"`
	Missing []string `json:"missing,omitempty"`
}

// MongoCollPerf is one collection's storage and indexes.
type MongoCollPerf struct {
	Name    string   `json:"name"`
	Count   int64    `json:"count"`
	Size    int64    `json:"size"`
	Storage int64    `json:"storage"`
	Free    int64    `json:"free"`
	Indexes []bson.M `json:"indexes"`
	// IndexSize is each index's size in bytes, by name.
	IndexSize bson.M `json:"indexSize,omitempty"`
}

// mongoPerfCollections caps how many collections are read.
const mongoPerfCollections = 200

func (s *mongoSession) perf(ctx context.Context) (tunnel.StoreAnswer, error) {
	if s.db.Level != tunnel.LevelDBA {
		return errAnswer("reading the server's performance statistics is DBA work; this session is at %s", s.db.Level)
	}
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	p := readMongoPerf(actx, s.client, s.db.Name)
	p.Version = s.version
	return tunnel.StoreAnswer{Type: "result", Data: p}, nil
}

func readMongoPerf(ctx context.Context, c *mongo.Client, name string) MongoPerf {
	p := MongoPerf{Collections: []MongoCollPerf{}, Slow: []bson.M{}, Running: []bson.M{}}
	miss := func(what string, err error) bool {
		if err != nil {
			p.Missing = append(p.Missing, what+": "+mongoErrText(err, 0))
			return true
		}
		return false
	}
	db := c.Database(name)

	var st bson.M
	if !miss("server status", c.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1},
		{Key: "repl", Value: 0}, {Key: "locks", Value: 0}, {Key: "tcmalloc", Value: 0}}).Decode(&st)) {
		p.Server = bson.M{}
		for _, k := range []string{"uptime", "connections", "opcounters", "globalLock", "mem", "wiredTiger", "metrics", "opLatencies"} {
			if v, ok := st[k]; ok {
				p.Server[k] = trimServerStatus(k, v)
			}
		}
	}
	var ds bson.M
	if !miss("database stats", db.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&ds)) {
		p.DB = ds
	}

	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "type", Value: "collection"}})
	if !miss("collections", err) {
		if len(names) > mongoPerfCollections {
			names = names[:mongoPerfCollections]
		}
		for _, n := range names {
			if strings.HasPrefix(n, "system.") {
				continue
			}
			cp := MongoCollPerf{Name: n, Indexes: []bson.M{}}
			var cs []bson.M
			cur, err := db.Collection(n).Aggregate(ctx, mongo.Pipeline{{{Key: "$collStats", Value: bson.D{
				{Key: "storageStats", Value: bson.D{}}}}}})
			if err == nil {
				err = cur.All(ctx, &cs)
			}
			if miss("$collStats of "+n, err) {
				continue
			}
			if len(cs) > 0 {
				if ss, ok := asM(cs[0]["storageStats"]); ok {
					cp.Count, cp.Size, cp.Storage = num(ss["count"]), num(ss["size"]), num(ss["storageSize"])
					cp.Free = num(ss["freeStorageSize"])
					if is, ok := asM(ss["indexSizes"]); ok {
						cp.IndexSize = is
					}
				}
			}
			cur, err = db.Collection(n).Aggregate(ctx, mongo.Pipeline{{{Key: "$indexStats", Value: bson.D{}}}})
			if err == nil {
				err = cur.All(ctx, &cp.Indexes)
			}
			miss("$indexStats of "+n, err)
			for _, ix := range cp.Indexes {
				// host and the full spec say nothing the advice needs.
				delete(ix, "host")
				if spec, ok := asM(ix["spec"]); ok {
					ix["unique"] = spec["unique"] == true
					ix["ttl"] = spec["expireAfterSeconds"] != nil
					ix["partial"] = spec["partialFilterExpression"] != nil
					delete(ix, "spec")
				}
			}
			p.Collections = append(p.Collections, cp)
		}
	}

	var prof bson.M
	if !miss("profiler", db.RunCommand(ctx, bson.D{{Key: "profile", Value: -1}}).Decode(&prof)) {
		p.Profile = bson.M{"was": prof["was"], "slowms": prof["slowms"]}
		if num(prof["was"]) > 0 {
			cur, err := db.Collection("system.profile").Aggregate(ctx, mongo.Pipeline{
				// Not the database itself, not the profiler's own collection,
				// and not this read's own $collStats and $indexStats.
				{{Key: "$match", Value: bson.D{
					{Key: "ns", Value: bson.D{{Key: "$nin", Value: bson.A{name, name + ".system.profile"}}}},
					{Key: "command.pipeline.0.$collStats", Value: bson.D{{Key: "$exists", Value: false}}},
					{Key: "command.pipeline.0.$indexStats", Value: bson.D{{Key: "$exists", Value: false}}},
				}}},
				{{Key: "$group", Value: bson.D{
					{Key: "_id", Value: bson.D{{Key: "ns", Value: "$ns"}, {Key: "op", Value: "$op"}, {Key: "plan", Value: "$planSummary"},
						{Key: "shape", Value: "$queryHash"}}},
					{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
					{Key: "totalMs", Value: bson.D{{Key: "$sum", Value: "$millis"}}},
					{Key: "maxMs", Value: bson.D{{Key: "$max", Value: "$millis"}}},
					{Key: "examined", Value: bson.D{{Key: "$sum", Value: "$docsExamined"}}},
					{Key: "keys", Value: bson.D{{Key: "$sum", Value: "$keysExamined"}}},
					{Key: "returned", Value: bson.D{{Key: "$sum", Value: "$nreturned"}}},
					{Key: "sorts", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$hasSortStage", 1, 0}}}}}},
					{Key: "example", Value: bson.D{{Key: "$first", Value: bson.D{{Key: "filter", Value: "$command.filter"},
						{Key: "sort", Value: "$command.sort"}, {Key: "pipeline", Value: "$command.pipeline"},
						{Key: "q", Value: "$command.q"}}}}},
				}}},
				{{Key: "$sort", Value: bson.D{{Key: "totalMs", Value: -1}}}},
				{{Key: "$limit", Value: 25}},
			})
			if err == nil {
				err = cur.All(ctx, &p.Slow)
			}
			miss("slow operations", err)
		}
	}

	cur, err := c.Database("admin").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$currentOp", Value: bson.D{{Key: "allUsers", Value: true}, {Key: "idleSessions", Value: false}}}},
		{{Key: "$match", Value: bson.D{{Key: "ns", Value: bson.D{{Key: "$regex", Value: "^" + regexQuote(name) + `\.`}}},
			{Key: "secs_running", Value: bson.D{{Key: "$gte", Value: 5}}}}}},
		{{Key: "$project", Value: bson.D{{Key: "opid", Value: 1}, {Key: "op", Value: 1}, {Key: "ns", Value: 1},
			{Key: "secs_running", Value: 1}, {Key: "planSummary", Value: 1}, {Key: "waitingForLock", Value: 1}}}},
		{{Key: "$limit", Value: 50}},
	})
	if err == nil {
		err = cur.All(ctx, &p.Running)
	}
	miss("running operations", err)
	return p
}

// trimServerStatus keeps from a serverStatus section only what the advice
// reads, so the answer stays small.
func trimServerStatus(section string, v any) any {
	m, ok := asM(v)
	if !ok {
		return v
	}
	pick := func(src bson.M, keys ...string) bson.M {
		out := bson.M{}
		for _, k := range keys {
			if x, ok := src[k]; ok {
				out[k] = x
			}
		}
		return out
	}
	switch section {
	case "wiredTiger":
		c, _ := asM(m["cache"])
		t, _ := asM(m["concurrentTransactions"])
		return bson.M{"cache": pick(c, "bytes currently in the cache", "maximum bytes configured", "tracked dirty bytes in the cache",
			"pages read into cache", "pages requested from the cache", "pages evicted by application threads",
			"application threads page read from disk to cache count"),
			"concurrentTransactions": t}
	case "metrics":
		out := bson.M{}
		if q, ok := asM(m["queryExecutor"]); ok {
			out["queryExecutor"] = pick(q, "scanned", "scannedObjects", "collectionScans")
		}
		if d, ok := asM(m["document"]); ok {
			out["document"] = d
		}
		if o, ok := asM(m["operation"]); ok {
			out["operation"] = pick(o, "scanAndOrder", "writeConflicts")
		}
		if c, ok := asM(m["cursor"]); ok {
			out["cursor"] = pick(c, "timedOut", "open")
		}
		return out
	case "globalLock":
		return pick(m, "currentQueue", "activeClients")
	}
	return m
}

func num(v any) int64 {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// asM is a document as a map: the v2 driver decodes nested documents
// inside a bson.M as bson.D.
func asM(v any) (bson.M, bool) {
	switch x := v.(type) {
	case bson.M:
		return x, true
	case bson.D:
		m := make(bson.M, len(x))
		for _, e := range x {
			m[e.Key] = e.Value
		}
		return m, true
	}
	return nil, false
}

// regexQuote escapes a database name for a $regex.
func regexQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
