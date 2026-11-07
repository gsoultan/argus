package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/storage"
)

/*
A library installer put on many servers at once: one at a time, outside
production first, the ones that cannot take it named and left out.
*/

func TestABatchGoesOneServerAtATimeOutsideProductionFirst(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetRecordingDir(dir)
	ctx := context.Background()

	msi := append(append([]byte{}, msiMagic...), []byte(unique("batch-msi"))...)
	w := uploadWinFile(t, h, "agent.msi", "", msi)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	var f SoftwareFile
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM software_files WHERE id = $1::uuid`, f.ID) })

	type srv struct {
		id, host, cred string
	}
	add := func(prefix, protocol string, tags []string, windows bool) srv {
		host := unique(prefix) + ".example"
		cred, _ := enrolled(t, s, h, host)
		as, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.91", Protocol: protocol,
			Principals: []string{"Administrator"}, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = s.pool.Exec(ctx, `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
			_ = s.ArchiveAsset(ctx, as.ID)
		})
		if windows {
			windowsBeat(t, h, cred, host, nil)
		}
		return srv{as.ID, host, cred}
	}
	prod := add("win-prod", "rdp", []string{"production"}, true)
	test := add("win-test", "rdp", nil, true)
	linux := add("lin", "ssh", nil, false)

	batch := func(ids ...string) (int, BatchResult, string) {
		raw, _ := json.Marshal(map[string]any{"kind": "file", "fileId": f.ID, "assetIds": ids})
		w := do(t, h, http.MethodPost, "/api/v1/software/batches", "admin-token", string(raw))
		var res BatchResult
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res, w.Body.String()
	}

	// Production alone, never tried elsewhere: refused, and says why.
	if code, _, body := batch(prod.id); code != http.StatusConflict || !strings.Contains(body, "outside production") {
		t.Errorf("production alone: %d %s", code, body)
	}

	// Production listed first still goes second; Linux is left out.
	code, res, body := batch(prod.id, linux.id, test.id)
	if code != http.StatusAccepted {
		t.Fatalf("batch: %d %s", code, body)
	}
	if len(res.Runs) != 2 || res.Runs[0].AssetHostname != test.host || res.Runs[1].AssetHostname != prod.host ||
		res.Runs[0].State != RunQueued || res.Runs[1].State != RunWaiting || res.Runs[0].BatchID == nil {
		t.Fatalf("runs %+v", res.Runs)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Hostname != linux.host || !strings.Contains(res.Skipped[0].Reason, "not a Windows server") {
		t.Errorf("skipped %+v", res.Skipped)
	}
	if e := lastAuditOn(t, s, f.Name); e.Action != "software.batch_requested" || !strings.Contains(e.Detail, "one at a time") || !strings.Contains(e.Detail, linux.host) {
		t.Errorf("audit %+v", e)
	}
	// A server already busy is left out of the next batch.
	if code, _, body := batch(test.id); code != http.StatusConflict || !strings.Contains(body, "already has a change") {
		t.Errorf("busy: %d %s", code, body)
	}

	// The first one finishing starts the next.
	o := windowsBeat(t, h, test.cred, test.host, nil)
	if o == nil || o.Run == nil || o.Run.File == nil {
		t.Fatalf("order %+v", o)
	}
	if o := windowsBeat(t, h, prod.cred, prod.host, nil); o != nil && o.Run != nil {
		t.Fatalf("production started before the test server finished: %+v", o.Run)
	}
	now := time.Now().UTC()
	windowsBeat(t, h, test.cred, test.host, &PackageRunStatus{ID: res.Runs[0].ID, State: "done", StartedAt: &now, FinishedAt: &now})
	if o := windowsBeat(t, h, prod.cred, prod.host, nil); o == nil || o.Run == nil || o.Run.ID != res.Runs[1].ID {
		t.Fatalf("production did not start after: %+v", o)
	}
}
