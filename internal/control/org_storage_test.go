package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tunnel"
)

// azuriteKey is Azurite's published development account key.
const azuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// settingsSpy is a settings store that records what it was asked to save.
type settingsSpy struct {
	cur   Settings
	saved []Settings
}

func (s *settingsSpy) Settings() (Settings, error) { return s.cur, nil }
func (s *settingsSpy) SaveSettings(_ context.Context, in Settings, _ string) ([]string, error) {
	s.saved = append(s.saved, in)
	return nil, nil
}
func (s *settingsSpy) TestDatabase(context.Context, DatabaseSettings) (DatabaseCheck, error) {
	return DatabaseCheck{}, nil
}
func (s *settingsSpy) TestStorage(context.Context, StorageSettings) error { return nil }
func (s *settingsSpy) TestEmail(context.Context, EmailSettings) error     { return nil }
func (s *settingsSpy) TestCloud(context.Context, string, AWSIdentitySettings) (CloudIdentityCheck, error) {
	return CloudIdentityCheck{}, nil
}

// An organization other than the default one keeps its recordings where it
// chooses, with its own key sealed in its own schema; what it kept before
// still replays and is copied across; moving back copies them back; and its
// admins cannot change, or read, the control plane's storage.
func TestAnOrganizationKeepsItsRecordingsInItsOwnStorage(t *testing.T) {
	endpoint := os.Getenv("ARGUS_TEST_AZURITE")
	if endpoint == "" {
		t.Skip("ARGUS_TEST_AZURITE is not set")
	}
	h := newTenantHarness(t)
	key, err := tunnel.GrantKeyFromSecret("a-test-signing-secret-that-is-long-enough-0123")
	if err != nil {
		t.Fatal(err)
	}
	h.api.Tunnels = &TunnelHub{GrantKey: key}
	platformDir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.api.SetRecordingDir(platformDir)
	h.api.AgentKeepRecordings = 7 * 24 * time.Hour

	tn, owner, pw := newTenant(t, h.store)
	ctx := WithTenant(context.Background(), tn)
	c := h.signIn(t, tn.Slug, owner, pw)
	call := func(cl *http.Client, method, path string, body any) (int, map[string]any, string) {
		t.Helper()
		b := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			b = string(raw)
		}
		code, text := h.call(t, cl, "", method, path, b)
		out := map[string]any{}
		_ = json.Unmarshal([]byte(text), &out)
		return code, out, text
	}

	// A recording kept before the organization had storage of its own.
	src := filepath.Join(t.TempDir(), "old.cast")
	if err := os.WriteFile(src, []byte("an older recording\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldKey, err := storage.ForTenant(platformDir, tn.ID).Upload(ctx, src, "11111111-1111-1111-1111-111111111111", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	opEmail := unique("op") + "@northwind.id"
	if _, err := h.store.CreateAccount(ctx, opEmail, "Op", "operator", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	op := h.signIn(t, tn.Slug, opEmail, "correct horse battery staple")
	if code, _, _ := call(op, "GET", "/api/v1/recording-storage", nil); code != http.StatusForbidden {
		t.Errorf("an operator reading recording storage = %d, want 403", code)
	}

	container := "argus-org-" + strings.ToLower(tn.ID)
	azure := map[string]any{"enabled": true, "provider": "azure", "endpoint": endpoint, "bucket": container,
		"accessKey": "devstoreaccount1", "secretKey": azuriteKey, "useSsl": false}

	// A wrong key is refused before anything is saved.
	bad := map[string]any{}
	for k, v := range azure {
		bad[k] = v
	}
	bad["secretKey"] = "d3Jvbmc="
	if code, out, _ := call(c, "PUT", "/api/v1/recording-storage", map[string]any{"storage": bad}); code != http.StatusBadRequest {
		t.Errorf("a wrong key = %d %v, want 400", code, out)
	}
	if rows, _ := h.store.RecordingStorages(ctx); len(rows) != 0 {
		t.Fatalf("a destination that failed its check was saved: %+v", rows)
	}
	if code, out, _ := call(c, "PUT", "/api/v1/recording-storage", map[string]any{"storage": azure,
		"archive": map[string]any{"afterDays": 0, "storageClass": "GLACIER"}}); code != http.StatusBadRequest {
		t.Errorf("an S3 class on Azure = %d %v, want 400", code, out)
	}

	if code, out, _ := call(c, "POST", "/api/v1/recording-storage/test", azure); code != http.StatusOK {
		t.Fatalf("test = %d %v", code, out)
	}
	code, out, text := call(c, "PUT", "/api/v1/recording-storage", map[string]any{"storage": azure,
		"archive": map[string]any{"afterDays": 90, "storageClass": "Cool"}, "agentKeepRecordingsDays": 3})
	if code != http.StatusOK {
		t.Fatalf("put = %d %s", code, text)
	}
	if strings.Contains(text, azuriteKey) {
		t.Fatal("the key came back out")
	}
	st, _ := out["storage"].(map[string]any)
	if st["secretKeySet"] != true || st["bucket"] != container {
		t.Errorf("view after saving = %v", out)
	}
	var sealed []byte
	if err := h.store.db(ctx).QueryRow(ctx, `SELECT secret_sealed FROM recording_storages`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(azuriteKey)) {
		t.Fatal("the key is kept in the clear")
	}
	// Sealed for this organization only.
	if _, err := h.api.openStorageSecret(context.Background(), sealed); err == nil {
		t.Error("the default organization can open another organization's storage key")
	}

	if got := h.api.keepRecordingsFor(ctx); got != 3*24*time.Hour {
		t.Errorf("servers keep recordings for %v, want 72h", got)
	}
	if got := h.api.archiveAfterFor(ctx); got != 90*24*time.Hour {
		t.Errorf("archive after %v, want 90 days", got)
	}
	if got := h.api.archiveClassFor(ctx); got != "Cool" {
		t.Errorf("archive class %q", got)
	}
	if got := h.api.keepRecordingsFor(context.Background()); got != 7*24*time.Hour {
		t.Errorf("the default organization's servers keep recordings for %v", got)
	}

	read := func(st storage.Store, key string) string {
		t.Helper()
		r, err := st.Get(ctx, key)
		if err != nil {
			return "error: " + err.Error()
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}

	// Before the copy, the older recording replays from where it was.
	if got := read(h.api.recordingsFor(ctx), oldKey); got != "an older recording\n" {
		t.Fatalf("an older recording before the copy: %q", got)
	}
	// New ones go to the organization's own storage, not the control plane's.
	src2 := filepath.Join(t.TempDir(), "new.cast")
	if err := os.WriteFile(src2, []byte("a newer recording\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newKey, err := h.api.recordingsFor(ctx).Upload(ctx, src2, "22222222-2222-2222-2222-222222222222", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	own, err := storage.Open(storage.Config{Provider: storage.ProviderAzure, Endpoint: endpoint, Bucket: container,
		AccessKey: "devstoreaccount1", SecretKey: azuriteKey}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Release(own)
	if got := read(own, newKey); got != "a newer recording\n" {
		t.Fatalf("a new recording in the organization's storage: %q", got)
	}
	if _, err := platformDir.Get(ctx, newKey); err == nil {
		t.Error("a new recording went to the control plane's storage too")
	}

	h.api.copyOrgRecordings(ctx)
	if got := read(own, oldKey); got != "an older recording\n" {
		t.Fatalf("the older recording was not copied into the organization's storage: %q", got)
	}
	if cp := h.api.orgCopies.get(tn.ID); cp == nil || cp.Copied != 1 || cp.Failed != 0 {
		t.Errorf("copy status = %+v", cp)
	}

	// Moving back to the control plane's storage: the organization's own is
	// retired, read from until copied back, then forgotten.
	code, out, text = call(c, "PUT", "/api/v1/recording-storage", map[string]any{
		"storage": map[string]any{"enabled": false}, "archive": map[string]any{"afterDays": 0}})
	if code != http.StatusOK || out["moving"] != float64(1) {
		t.Fatalf("move back = %d %s", code, text)
	}
	if got := read(h.api.recordingsFor(ctx), newKey); got != "a newer recording\n" {
		t.Fatalf("a recording in the retired storage does not replay: %q", got)
	}
	h.api.copyOrgRecordings(ctx)
	if got := read(platformDir, newKey); got != "a newer recording\n" {
		t.Fatalf("the newer recording was not copied back: %q", got)
	}
	if rows, err := h.store.RecordingStorages(ctx); err != nil || len(rows) != 0 {
		t.Errorf("after the copy back the organization still has %d destinations (%v)", len(rows), err)
	}

	evs, err := h.store.AuditEvents(ctx, 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range evs {
		seen[e.Action]++
	}
	if seen["recording_storage.changed"] != 2 || seen["recordings.copied"] != 2 {
		t.Errorf("audit = %v", seen)
	}
}

// An organization's admins neither read nor change where the control plane
// keeps recordings: Settings shows them nothing of it and keeps it as
// configured whatever they send.
func TestAnotherOrganizationCannotChangeTheControlPlanesStorage(t *testing.T) {
	h := newTenantHarness(t)
	spy := &settingsSpy{cur: Settings{AgentKeepRecordingsDays: 7,
		Storage: StorageSettings{Enabled: true, Endpoint: "s3.platform.example", Bucket: "platform-recordings",
			AccessKey: "PLATFORMKEY", SecretKeySet: true}}}
	h.api.Settings = spy
	tn, owner, pw := newTenant(t, h.store)
	c := h.signIn(t, tn.Slug, owner, pw)
	call := func(cl *http.Client, method, path string, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		if body == nil {
			raw = nil
		}
		return h.call(t, cl, "", method, path, string(raw))
	}

	code, text := call(c, "GET", "/api/v1/settings", nil)
	if code != http.StatusOK || strings.Contains(text, "platform-recordings") || strings.Contains(text, "PLATFORMKEY") {
		t.Errorf("settings as another organization = %d %s", code, text)
	}
	sent := spy.cur
	sent.Storage = StorageSettings{Enabled: true, Endpoint: "evil.example", Bucket: "theirs", AccessKey: "X"}
	sent.AgentKeepRecordingsDays = -1
	sent.Archive = &RecordingArchiveSettings{AfterDays: -1}
	if code, text := call(c, "PUT", "/api/v1/settings", sent); code != http.StatusOK {
		t.Fatalf("put settings = %d %s", code, text)
	}
	if got := spy.saved[len(spy.saved)-1]; got.Storage.Bucket != "platform-recordings" ||
		got.Storage.Endpoint != "s3.platform.example" || got.AgentKeepRecordingsDays != 7 || got.Archive != nil {
		t.Errorf("another organization changed the control plane's storage: %+v keep %d archive %v",
			got.Storage, got.AgentKeepRecordingsDays, got.Archive)
	}
	if code, _ := call(c, "POST", "/api/v1/settings/test-storage", StorageSettings{}); code != http.StatusForbidden {
		t.Errorf("testing the control plane's storage from another organization = %d, want 403", code)
	}
}
