package control

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

/*
Version skew between a fleet and its control plane.

Reporters are deployed separately and will be different versions -- ordinary
mid-upgrade, not an error. Refusing a message over one unrecognised field threw
away everything else in it, three times, invisibly from both ends.
*/

type capturingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLogger) Warn(msg string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(strings.Trim(sprint(a), "\n")))
	}
	c.lines = append(c.lines, b.String())
}

func sprint(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func report(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/report/heartbeat",
		bytes.NewBufferString(body))
}

type heartbeatIn struct {
	Hostname       string `json:"hostname"`
	ActiveSessions int    `json:"active_sessions"`
}

// A field this build does not know must not cost the whole message.
func TestAnUnknownFieldDoesNotDiscardTheMessage(t *testing.T) {
	warnedFields = map[string]struct{}{}
	var in heartbeatIn
	log := &capturingLogger{}

	err := decodeReport(report(`{
		"hostname": "db-01",
		"active_sessions": 3,
		"exec_tracing": true,
		"something_from_the_future": {"nested": 1}
	}`), &in, log, "report/heartbeat")
	if err != nil {
		t.Fatalf("a newer reporter was refused: %v", err)
	}
	if in.Hostname != "db-01" || in.ActiveSessions != 3 {
		t.Errorf("the fields this build understands were lost: %+v", in)
	}
}

// And it must be said out loud, because silence is what let this run for a week.
func TestAnUnknownFieldIsReportedOnce(t *testing.T) {
	warnedFields = map[string]struct{}{}
	log := &capturingLogger{}
	for i := 0; i < 5; i++ {
		var in heartbeatIn
		if err := decodeReport(report(`{"hostname":"db-01","exec_tracing":true}`),
			&in, log, "report/heartbeat"); err != nil {
			t.Fatal(err)
		}
	}
	if len(log.lines) != 1 {
		t.Fatalf("logged %d times, want exactly 1 -- a gateway reports on every "+
			"session and would bury this in its own noise", len(log.lines))
	}
	if !strings.Contains(log.lines[0], "exec_tracing") {
		t.Errorf("the log should name the field: %s", log.lines[0])
	}
}

// Malformed JSON is still an error; tolerance is for unknown fields, not rubbish.
func TestMalformedJSONIsStillRefused(t *testing.T) {
	warnedFields = map[string]struct{}{}
	var in heartbeatIn
	if err := decodeReport(report(`{"hostname": `), &in, &capturingLogger{},
		"report/heartbeat"); err == nil {
		t.Error("truncated JSON was accepted")
	}
}

// A body with nothing unexpected logs nothing.
func TestAKnownBodyIsSilent(t *testing.T) {
	warnedFields = map[string]struct{}{}
	log := &capturingLogger{}
	var in heartbeatIn
	if err := decodeReport(report(`{"hostname":"db-01","active_sessions":1}`),
		&in, log, "report/heartbeat"); err != nil {
		t.Fatal(err)
	}
	if len(log.lines) != 0 {
		t.Errorf("logged for a body it fully understood: %v", log.lines)
	}
}

// The console stays strict: it ships with this build, so an unknown field there
// is a typo or a stale client and should fail where someone will see it.
func TestTheConsoleDecoderIsStillStrict(t *testing.T) {
	var in heartbeatIn
	err := decode(httptest.NewRequest(http.MethodPost, "/api/v1/policy",
		bytes.NewBufferString(`{"hostname":"db-01","typo_field":1}`)), &in)
	if err == nil {
		t.Error("the console decoder accepted an unknown field")
	}
}

// Field names come from the json tags, including embedded structs.
func TestUnknownFieldsUnderstandsTags(t *testing.T) {
	type inner struct {
		B string `json:"b"`
	}
	type outer struct {
		inner
		A       string `json:"a,omitempty"`
		Skipped string `json:"-"`
		NoTag   string
	}
	got := unknownFields([]byte(`{"a":"1","b":"2","NoTag":"3","surprise":"4"}`), &outer{})
	if len(got) != 1 || got[0] != "surprise" {
		t.Errorf("unknown = %v, want [surprise]", got)
	}
}
