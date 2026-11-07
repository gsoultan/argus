package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/apk/apktest"
	"github.com/gsoultan/argus/internal/storage"
)

/*
An APK added to the library and installed on a device: what the file says
about itself is read and shown first, the device fetches it on its own
credential for that run only, and what may never be uninstalled or disabled
is refused before anything reaches the device.
*/

func uploadFile(t *testing.T, h http.Handler, name string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(body)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/software/files?note=field+app", &b)
	r.Header.Set("Authorization", "Bearer admin-token")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func deviceBeat(t *testing.T, h http.Handler, cred, host string, st *PackageRunStatus) *PackageOrders {
	t.Helper()
	b := PackageBeat{Reason: "an Android device changes apps from its Apps tab", Run: st}
	raw, _ := json.Marshal(map[string]any{"hostname": host, "version": "1.0.0", "active_sessions": 0, "packages": b})
	w := do(t, h, http.MethodPost, "/api/v1/report/heartbeat", cred, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	var out struct {
		Packages *PackageOrders `json:"packages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.Packages
}

func TestAnAPKFromTheLibraryInstallsOnADevice(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetRecordingDir(dir)
	ctx := context.Background()

	// The library reads the file and keeps it once.
	file, cert := apktest.Signed(t, "com.impack.survey", 240, "2.4.0", "Field Survey")
	w := uploadFile(t, h, "survey-2.4.0.apk", file)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	var f SoftwareFile
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if f.App == nil || f.App.Package != "com.impack.survey" || f.App.VersionCode != 240 || f.Name != "Field Survey" ||
		f.App.Signer != apktest.SHA256(cert) || f.SHA256 != apktest.SHA256(file) || f.Note != "field app" {
		t.Fatalf("%+v %+v", f, f.App)
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM software_files WHERE sha256 = $1`, f.SHA256) })
	if w := uploadFile(t, h, "again.apk", file); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), f.ID) {
		t.Errorf("the same file again: %d %s", w.Code, w.Body)
	}
	// Another build of the same app, signed by someone else, says so.
	other, _ := apktest.Signed(t, "com.impack.survey", 241, "2.4.1", "Field Survey")
	w = uploadFile(t, h, "survey-2.4.1.apk", other)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "Signed by a different key than survey-2.4.0.apk") {
		t.Errorf("another signer: %d %s", w.Code, w.Body)
	}
	var otherFile SoftwareFile
	_ = json.Unmarshal(w.Body.Bytes(), &otherFile)
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM software_files WHERE id = $1::uuid`, otherFile.ID) })
	for name, body := range map[string][]byte{
		"setup.exe":    []byte("MZ"),
		"notes.txt":    []byte("hello"),
		"unsigned.apk": apktest.Zip(t, map[string][]byte{"AndroidManifest.xml": apktest.Manifest("com.a.b", 1, "1", "")}),
		"fake.apk":     []byte("not a zip"),
	} {
		if w := uploadFile(t, h, name, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}

	// A device, its app new enough to install.
	host := unique("pixel") + ".example"
	cred, _ := enrolled(t, s, h, host)
	device, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.80", Kind: "android",
		Principals: []string{DeviceControl}, ControlMode: "attended"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
		_ = s.ArchiveAsset(ctx, device.ID)
	})
	runs := "/api/v1/assets/" + device.ID + "/android-apps/runs"
	facts := `{"hostname":"` + host + `","os":"Android 15","system":{"engine":"attended","can_install_apps":true},` +
		`"android_apps":{"checked_at":"2026-11-07T10:00:00Z","apps":[` +
		`{"package":"com.android.chrome","label":"Chrome","version":"131","source":"preinstalled"},` +
		`{"package":"com.whatsapp","label":"WhatsApp","version":"2.24","source":"play"}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("facts: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, runs, "admin-token", `{"action":"install","fileId":"`+f.ID+`"}`); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "too old") {
		t.Errorf("before the app said it can: %d %s", w.Code, w.Body)
	}
	deviceBeat(t, h, cred, host, nil)

	// What is refused before anything reaches the device.
	for body, want := range map[string]string{
		`{"action":"uninstall","package":"com.gsoultan.argus"}`: "how Argus reaches this device",
		`{"action":"uninstall","package":"com.android.chrome"}`: "came with the device",
		`{"action":"disable","package":"com.android.chrome"}`:   "needs the unattended engine",
		`{"action":"uninstall","package":"com.not.there"}`:      "not installed",
		`{"action":"uninstall","package":"x; reboot"}`:          "package name",
		`{"action":"install","fileId":"` + f.ID[:8] + `"}`:      "no such file",
	} {
		if w := do(t, h, http.MethodPost, runs, "admin-token", body); w.Code < 400 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}

	// Install: the order names the file, its hash and what it is.
	w = do(t, h, http.MethodPost, runs, "admin-token", `{"action":"install","fileId":"`+f.ID+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	var run PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if e := lastAuditOn(t, s, host); e.Action != "software.app_requested" || !strings.Contains(e.Detail, "Android asks the person") {
		t.Errorf("audit %+v", e)
	}
	o := deviceBeat(t, h, cred, host, nil)
	if o == nil || o.Run == nil || o.Run.App == nil || o.Run.App.File == nil || o.Run.App.File.SHA256 != f.SHA256 {
		t.Fatalf("order %+v", o)
	}

	// The device runs it through the agent's own code, fetching through the
	// control plane on its credential.
	inst := &recordingInstaller{}
	fetch := func(cred string) agent.AppFetch {
		return func(_ context.Context, path string, dst io.Writer, limit int64) (int64, error) {
			w := do(t, h, http.MethodGet, path, cred, "")
			if w.Code != http.StatusOK {
				return 0, &httpErr{w.Code, w.Body.String()}
			}
			return io.Copy(dst, io.LimitReader(w.Body, limit+1))
		}
	}
	// Another device's credential cannot fetch it.
	otherCred, _ := enrolled(t, s, h, unique("other")+".example")
	if _, err := fetch(otherCred)(ctx, o.Run.App.File.Path, io.Discard, 1<<30); err == nil {
		t.Error("another device fetched the file")
	}
	raw, _ := json.Marshal(o.Run)
	var req agent.PackageRunRequest
	_ = json.Unmarshal(raw, &req)
	ar := &agent.AppRuns{Dir: t.TempDir(), Fetch: fetch(cred), Installer: inst, Attended: func() bool { return true }}
	done := make(chan struct{})
	ar.Start(ctx, req, func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the device run did not end")
	}
	st := ar.Status()
	if st.State != agent.RunDone || inst.calls != "install com.impack.survey" {
		t.Fatalf("device: %+v %q", st, inst.calls)
	}
	raw, _ = json.Marshal(st)
	var cst PackageRunStatus
	_ = json.Unmarshal(raw, &cst)
	deviceBeat(t, h, cred, host, &cst)
	got, err := s.PackageRunByID(ctx, run.ID)
	if err != nil || got.State != RunDone || got.SoftwareFileName != "Field Survey" {
		t.Fatalf("%+v %v", got, err)
	}
	// Ended: the file is no longer handed out for it.
	if w := do(t, h, http.MethodGet, o.Run.App.File.Path, cred, ""); w.Code != http.StatusGone {
		t.Errorf("after the run: %d", w.Code)
	}

	// Removing it from the library deletes the file, not the app.
	if w := do(t, h, http.MethodDelete, "/api/v1/software/files/"+f.ID, "admin-token", ""); w.Code != http.StatusNoContent {
		t.Errorf("remove: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, runs, "admin-token", `{"action":"install","fileId":"`+f.ID+`"}`); w.Code != http.StatusNotFound {
		t.Errorf("install a removed file: %d %s", w.Code, w.Body)
	}
}

type recordingInstaller struct{ calls string }

func (r *recordingInstaller) Install(_, pkg string) error { r.calls = "install " + pkg; return nil }
func (r *recordingInstaller) Uninstall(pkg string) error  { r.calls = "uninstall " + pkg; return nil }
func (r *recordingInstaller) Disable(pkg string) error    { r.calls = "disable " + pkg; return nil }
func (r *recordingInstaller) Restore(pkg string) error    { r.calls = "restore " + pkg; return nil }

type httpErr struct {
	code int
	body string
}

func (e *httpErr) Error() string { return e.body }
