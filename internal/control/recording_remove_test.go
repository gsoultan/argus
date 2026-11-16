package control

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/storage"
)

// storedRecording reports a closed session for a fresh host, ships its
// recording into a directory store, and returns the session id, the host and
// the directory.
func storedRecording(t *testing.T) (*API, *Store, http.Handler, string, string, string) {
	t.Helper()
	a, s, h, _ := enrollAPI(t)
	root := t.TempDir()
	dir, err := storage.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	a.SetStorage(nil)
	a.SetRecordingDir(dir)
	host := unique("crm")
	cred, _ := enrolled(t, s, h, host)
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id::text = $1`, id)
	})
	cast, head := castFor(t)
	report := `{"id":"` + id + `","assetHostname":"` + host + `","principal":"sys-admin","protocol":"ssh",` +
		`"origin":"direct","state":"closed","chainHead":"` + head + `","recordingBytes":269400000}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, report); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	if w := putRecordingAs(h, cred, id, head, "cast", cast); w.Code != http.StatusOK {
		t.Fatalf("ship: %d %s", w.Code, w.Body)
	}
	return a, s, h, id, host, root
}

func storedFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// An admin removes a recording: the file goes, the session stays and says
// who removed it and why, the replay answers with that rather than "missing",
// and the removal is in the audit chain.
func TestAnAdminRemovesARecordingAndTheRemovalIsRecorded(t *testing.T) {
	_, s, h, id, host, root := storedRecording(t)
	if len(storedFiles(t, root)) != 1 {
		t.Fatalf("expected one stored file, have %v", storedFiles(t, root))
	}

	body := `{"confirm":"` + strings.ToUpper(host) + `","reason":"a CI deploy copied 100 MB through it"}`
	w := do(t, h, http.MethodDelete, "/api/v1/sessions/"+id+"/recording", "admin-token", body)
	if w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if files := storedFiles(t, root); len(files) != 0 {
		t.Errorf("the stored file is still there: %v", files)
	}

	sess, err := s.Session(context.Background(), id)
	if err != nil || sess == nil {
		t.Fatalf("the session itself went: %v", err)
	}
	if sess.RecordingKey != nil || sess.RecordingRemovedAt == nil ||
		sess.RecordingRemovedBy == nil || *sess.RecordingRemovedBy == "" ||
		sess.RecordingRemovedReason == nil || !strings.Contains(*sess.RecordingRemovedReason, "CI deploy") {
		t.Errorf("the session does not say the recording was removed: %+v", sess)
	}
	if sess.ChainHead == nil || *sess.ChainHead == "" || sess.RecordingBytes != 269400000 {
		t.Errorf("the session lost what it recorded about itself: %+v", sess)
	}

	n, err := s.AuditCountFor(context.Background(), "recording.removed", host)
	if err != nil || n != 1 {
		t.Errorf("audit entries for the removal: %d (%v)", n, err)
	}

	r := do(t, h, http.MethodGet, "/api/v1/sessions/"+id+"/recording", "admin-token", "")
	if r.Code != http.StatusGone || !strings.Contains(r.Body.String(), "CI deploy") {
		t.Errorf("replay after removal: %d %s", r.Code, r.Body)
	}

	// A second removal is refused, not repeated in the audit.
	if w := do(t, h, http.MethodDelete, "/api/v1/sessions/"+id+"/recording", "admin-token", body); w.Code != http.StatusConflict {
		t.Errorf("second removal: %d %s", w.Code, w.Body)
	}
}

// What is refused: someone who is not an admin, a host name that does not
// match, and no reason. Each leaves the recording where it was.
func TestRemovingARecordingIsRefusedWithoutAnAdminAConfirmationAndAReason(t *testing.T) {
	a, s, h, id, host, root := storedRecording(t)
	operator, _ := newAccount(t, s, "operator")
	a.UserTokens["operator-token"] = operator
	path := "/api/v1/sessions/" + id + "/recording"

	cases := []struct {
		name, token, body string
		want              int
	}{
		{"an operator", "operator-token", `{"confirm":"` + host + `","reason":"space"}`, http.StatusForbidden},
		{"the wrong host", "admin-token", `{"confirm":"another-host","reason":"space"}`, http.StatusBadRequest},
		{"no reason", "admin-token", `{"confirm":"` + host + `","reason":"  "}`, http.StatusBadRequest},
		{"nobody", "", `{"confirm":"` + host + `","reason":"space"}`, http.StatusUnauthorized},
	}
	for _, c := range cases {
		if w := do(t, h, http.MethodDelete, path, c.token, c.body); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
	}
	if len(storedFiles(t, root)) != 1 {
		t.Error("a refused removal touched the stored file")
	}
	sess, _ := s.Session(context.Background(), id)
	if sess == nil || sess.RecordingKey == nil || sess.RecordingRemovedAt != nil {
		t.Errorf("a refused removal changed the session: %+v", sess)
	}
	if n, _ := s.AuditCountFor(context.Background(), "recording.removed", host); n != 0 {
		t.Errorf("a refused removal was audited as a removal: %d", n)
	}
}

// The agent that shipped it may ship it again, after a lost reply. A removed
// recording is answered as done and not stored again, and a later report
// naming its old key does not bring it back.
func TestARemovedRecordingIsNotStoredAgain(t *testing.T) {
	_, s, h, id, host, root := storedRecording(t)
	before, _ := s.Session(context.Background(), id)
	oldKey := *before.RecordingKey
	if w := do(t, h, http.MethodDelete, "/api/v1/sessions/"+id+"/recording", "admin-token",
		`{"confirm":"`+host+`","reason":"space"}`); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}

	cred, _ := enrolled(t, s, h, host)
	cast, _ := castFor(t)
	if w := putRecordingAs(h, cred, id, *before.ChainHead, "cast", cast); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "removed") {
		t.Errorf("a re-shipped removed recording: %d %s", w.Code, w.Body)
	}
	if files := storedFiles(t, root); len(files) != 0 {
		t.Errorf("the removed recording was stored again: %v", files)
	}

	report := `{"id":"` + id + `","assetHostname":"` + host + `","principal":"sys-admin",` +
		`"protocol":"ssh","state":"closed","chainHead":"` + *before.ChainHead + `","recordingPath":"` + oldKey + `"}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/session", cred, report); w.Code != http.StatusOK {
		t.Fatalf("report again: %d %s", w.Code, w.Body)
	}
	after, _ := s.Session(context.Background(), id)
	if after.RecordingKey != nil {
		t.Errorf("the session points at the removed recording again: %q", *after.RecordingKey)
	}
}
