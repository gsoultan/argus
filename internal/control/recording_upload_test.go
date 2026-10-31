package control

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

/*
Recordings an agent ships.

Without a gateway, the recording made on the host is the only one there is,
and these are the checks that stand between it and the replay an auditor
watches: it verifies against the head the session was reported with, it comes
from the host that reported it, and a session not yet reported is "later",
not "never".
*/

// castFor records a little terminal output and returns the file and its head.
func castFor(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	rec, err := recorder.New(&buf, recorder.Header{Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec.Write(recorder.Output, []byte("$ id\r\nuid=0(root)\r\n"))
	head, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), head
}

func putRecordingAs(h http.Handler, cred, id, head, format string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/report/recording/"+id, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+cred)
	r.Header.Set("X-Argus-Chain-Head", head)
	r.Header.Set("X-Argus-Format", format)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAnAgentShipsTheRecordingOfASessionItReported(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	var stored []byte
	a.storeRecording = func(_ context.Context, path, id, head string, _ time.Time) (string, error) {
		stored, _ = os.ReadFile(path)
		return "2026/10/23/" + id + ".cast", nil
	}
	host := unique("ship")
	cred, _ := enrolled(t, s, h, host)
	other := unique("ship-other")
	otherCred, _ := enrolled(t, s, h, other)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	cast, head := castFor(t)

	// Before the report lands: later, not never.
	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusConflict {
		t.Fatalf("before the report: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"`+id+`","assetHostname":"`+host+`","principal":"root","protocol":"ssh",`+
			`"state":"closed","chainHead":"`+head+`"}`); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}

	// The shared token names no host; another host's credential is not this one.
	if w := putRecordingAs(h, testReporterToken, id, head, "cast", cast); w.Code != http.StatusForbidden {
		t.Errorf("shared token: %d %s", w.Code, w.Body)
	}
	if w := putRecordingAs(h, otherCred, id, head, "cast", cast); w.Code != http.StatusForbidden {
		t.Errorf("another host: %d %s", w.Code, w.Body)
	}

	// Bytes that do not verify are refused and audited.
	tampered := bytes.Replace(cast, []byte("uid=0"), []byte("uid=1"), 1)
	if w := putRecordingAs(h, cred, id, head, "cast", tampered); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered: %d %s", w.Code, w.Body)
	}
	var n int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = 'recording.rejected' AND target = $1`, id).Scan(&n)
	if n != 1 {
		t.Errorf("recording.rejected entries: %d", n)
	}
	if stored != nil {
		t.Fatal("a recording that does not verify was stored")
	}

	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusOK {
		t.Fatalf("ship: %d %s", w.Code, w.Body)
	}
	if !bytes.Equal(stored, cast) {
		t.Error("what was stored is not what was shipped")
	}
	got, _ := s.Session(context.Background(), id)
	if got.RecordingKey == nil || *got.RecordingKey == "" {
		t.Fatal("the session has no recording key")
	}
	// A retry after a lost reply is not an error.
	stored = nil
	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusOK || stored != nil {
		t.Errorf("retry: %d, stored again %v", w.Code, stored != nil)
	}
}

// An end report and the minute's "still active" can arrive in either order
// through the agent's spool. The session must stay ended.
func TestAnEndedSessionIsNotMadeActiveAgain(t *testing.T) {
	_, s, h, _ := enrollAPI(t)
	host := unique("ended")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	report := func(state, flags string) {
		t.Helper()
		if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
			`{"id":"`+id+`","assetHostname":"`+host+`","principal":"ana","state":"`+state+
				`","riskFlags":`+flags+`}`); w.Code != http.StatusOK {
			t.Fatalf("report %s: %d %s", state, w.Code, w.Body)
		}
	}
	report("active", `[]`)
	report("terminated", `["recording-incomplete"]`)
	report("active", `[]`)
	got, _ := s.Session(context.Background(), id)
	if got.State != "terminated" || got.EndedAt == nil {
		t.Errorf("state %q, ended %v", got.State, got.EndedAt)
	}
	if len(got.RiskFlags) != 1 {
		t.Errorf("risk flags %v; a late active report replaced the final ones", got.RiskFlags)
	}
}

// With no object storage, the control plane keeps what an agent ships on its
// own disk and replays it from there. Before, an install that never set up a
// bucket -- the setup page does not ask for one -- refused every recording,
// and the console could replay none of them.
func TestARecordingIsReplayedFromThisHostWithoutObjectStorage(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)
	host := unique("local")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	cast, head := castFor(t)
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred,
		`{"id":"`+id+`","assetHostname":"`+host+`","principal":"root","protocol":"ssh",`+
			`"state":"closed","chainHead":"`+head+`"}`); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusOK {
		t.Fatalf("ship: %d %s", w.Code, w.Body)
	}

	w := do(t, h, http.MethodGet, "/api/v1/sessions/"+id+"/recording", "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if !bytes.Equal(w.Body.Bytes(), cast) {
		t.Error("the replayed recording is not the one shipped")
	}
	if v := w.Header().Get("X-Argus-Chain-Verified"); v != "intact" {
		t.Errorf("verified %q", v)
	}
}
