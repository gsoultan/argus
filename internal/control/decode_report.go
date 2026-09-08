package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
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
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil // not an object; nothing to compare against
	}
	known := jsonFieldNames(reflect.TypeOf(v))
	var out []string
	for k := range raw {
		if _, ok := known[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
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

// warnedFields keeps the log to one line per endpoint and field set.
//
// A gateway reports on every session. Without this, a single unknown field
// would produce a line per report and bury itself in its own noise.
var warnedFields sync.Map

func warnUnknownOnce(log logger, endpoint string, unknown []string) {
	key := endpoint + "\x00" + strings.Join(unknown, ",")
	if _, seen := warnedFields.LoadOrStore(key, struct{}{}); seen {
		return
	}
	log.Warn("a reporter sent fields this build does not understand",
		"endpoint", endpoint, "fields", strings.Join(unknown, ", "),
		"detail", "the rest of the message was kept; this control plane is "+
			"older than something reporting to it, and whatever those fields "+
			"carry is not being stored")
}
