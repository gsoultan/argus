package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
)

/*
Reading what a gateway or an agent sends.

Reporters are deployed separately from the control plane and will be different
versions of it -- that is the ordinary state of a fleet mid-upgrade, not an
error. So an unknown field is a newer sender talking to an older receiver, and
refusing the message on account of it throws away everything else in the
message too.

That happened three times before anyone noticed, because it is invisible from
both ends. The sender spools and retries forever. The receiver logs one bad
request among many. Nothing says "a field you do not understand is costing you
every session report from this gateway".

  - terminated sessions carried terminatedBy, which the Session type lacked, so
    every administrative terminate went unrecorded
  - heartbeats carried exec_tracing, which the handler lacked, so every agent
    on the fleet went stale and stayed there for a week
  - and the probe state that "require eBPF for root" depends on could not reach
    the control plane at all, which is why that switch could not be enforced

So: take the fields we understand, keep the message, and say plainly what was
not understood. A field this build cannot store is data loss either way; the
difference is whether it costs one field or the whole report, and whether
anyone finds out.
*/

// decodeReport reads a reporter's body, keeping what this build understands.
//
// The log line is the point. Unknown fields are not an error, but they are not
// nothing either: they mean this control plane is older than the fleet
// reporting to it, and somebody should know before the next feature quietly
// depends on a field that never arrives.
func decodeReport(r *http.Request, v any, log logger, endpoint string) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return errors.New("invalid JSON: " + err.Error())
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.New("invalid JSON: " + err.Error())
	}
	if unknown := unknownFields(body, v); len(unknown) > 0 && log != nil {
		warnUnknownOnce(log, endpoint, unknown)
	}
	return nil
}

// logger is the slice of *slog.Logger this file needs, so the helper can be
// tested without one.
type logger interface {
	Warn(msg string, args ...any)
}

// unknownFields lists the top-level keys in body that v has no field for.
//
// Top level only. A nested unknown is a smaller problem -- it costs one field
// inside a structure the receiver already understands -- and walking arbitrary
// nesting here would cost more than it explains.
func unknownFields(body []byte, v any) []string {
	var raw map[string]jsonSkip
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil // not an object; nothing to compare against
	}
	known := knownFieldNames(reflect.TypeOf(v))
	var out []string
	for k := range raw {
		if _, ok := known[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// jsonSkip accepts any value and keeps none of it.
//
// This scan needs the keys and nothing else. map[string]json.RawMessage copied
// every value's bytes to answer that -- RawMessage's UnmarshalJSON appends
// them -- on every report from every gateway, for a log line emitted at most
// once per endpoint and field set.
type jsonSkip struct{}

func (jsonSkip) UnmarshalJSON([]byte) error { return nil }

// knownFieldNames is jsonFieldNames, computed once per type.
//
// The answer is a property of a struct this build compiled, so it cannot change
// between requests, and the reflect walk that produced it ran on every one.
//
// Unbounded is fine here where it would not be elsewhere in this file: the key
// is a reflect.Type from this package's own handlers, six of them, not anything
// a reporter can supply. The returned map is shared, so it is read and never
// written.
var knownFields sync.Map // reflect.Type -> map[string]struct{}

func knownFieldNames(t reflect.Type) map[string]struct{} {
	if cached, ok := knownFields.Load(t); ok {
		return cached.(map[string]struct{})
	}
	names := jsonFieldNames(t)
	knownFields.Store(t, names)
	return names
}

// jsonFieldNames collects the names a type accepts, following embedded structs.
func jsonFieldNames(t reflect.Type) map[string]struct{} {
	for t != nil && (t.Kind() == reflect.Ptr || t.Kind() == reflect.Interface) {
		t = t.Elem()
	}
	names := map[string]struct{}{}
	if t == nil || t.Kind() != reflect.Struct {
		return names
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		if f.Anonymous && name == f.Name {
			// Embedded without a tag: its fields are promoted into this one.
			for k := range jsonFieldNames(f.Type) {
				names[k] = struct{}{}
			}
			continue
		}
		names[name] = struct{}{}
	}
	return names
}

// maxWarnedFieldSets bounds how many distinct endpoint/field-set combinations
// are remembered.
//
// The key is built from field names a reporter chose, so without a bound this
// is a map an authenticated reporter can grow from the outside, one line of
// JSON at a time, for as long as the control plane runs.
const maxWarnedFieldSets = 1024

// maxWarnedKeyFields bounds how much of one report's unknown-field list reaches
// the key and the log line. A 1 MiB body can carry thousands of names, and
// neither a map key nor a log field wants all of them.
const maxWarnedKeyFields = 16

// warnedFields keeps the log to one line per endpoint and field set.
//
// A gateway reports on every session. Without this, a single unknown field
// would produce a line per report and bury itself in its own noise.
//
// Emptied wholesale at the cap rather than evicted entry by entry: this decides
// only whether a Warn repeats, so the cheapest policy that cannot grow is the
// right one. A repeat after a reset is the honest outcome anyway -- the skew it
// reports is still there.
var (
	warnedMu     sync.Mutex
	warnedFields = map[string]struct{}{}
)

func warnUnknownOnce(log logger, endpoint string, unknown []string) {
	named, extra := unknown, 0
	if len(named) > maxWarnedKeyFields {
		named, extra = named[:maxWarnedKeyFields], len(named)-maxWarnedKeyFields
	}
	key := endpoint + "\x00" + strings.Join(named, ",")
	if extra > 0 {
		key += "\x00+" + strconv.Itoa(extra)
	}

	warnedMu.Lock()
	if _, seen := warnedFields[key]; seen {
		warnedMu.Unlock()
		return
	}
	if len(warnedFields) >= maxWarnedFieldSets {
		clear(warnedFields)
	}
	warnedFields[key] = struct{}{}
	warnedMu.Unlock()

	fields := strings.Join(named, ", ")
	if extra > 0 {
		fields += ", and " + strconv.Itoa(extra) + " more"
	}
	log.Warn("a reporter sent fields this build does not understand",
		"endpoint", endpoint, "fields", fields,
		"detail", "the rest of the message was kept; this control plane is "+
			"older than something reporting to it, and whatever those fields "+
			"carry is not being stored")
}
