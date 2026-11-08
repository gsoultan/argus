package control

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/storage"
)

/*
An MSI added to the library and installed on a Windows server: the file is
checked to be one and kept with its arguments, the server must say it can,
production waits for a try elsewhere, and the agent is handed the file for
that run only.
*/

func uploadWinFile(t *testing.T, h http.Handler, name, args string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(body)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/software/files?name=Field+Reports&args="+args, &b)
	r.Header.Set("Authorization", "Bearer admin-token")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func windowsBeat(t *testing.T, h http.Handler, cred, host string, st *PackageRunStatus) *PackageOrders {
	t.Helper()
	b := PackageBeat{Reason: "operating system updates from the console are for Linux servers", Run: st,
		Installs: []string{"remove", "script", "file_install", "nonsense"}}
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

func TestAnMSIFromTheLibraryInstallsOnAWindowsServer(t *testing.T) {
	a, s, h, _ := enrollAPI(t)
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetRecordingDir(dir)
	ctx := context.Background()

	msi := append(append([]byte{}, msiMagic...), []byte(unique("msi-body"))...)
	w := uploadWinFile(t, h, "field-reports-1.0.msi", "ALLUSERS=1+REBOOT=ReallySuppress", msi)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	var f SoftwareFile
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM software_files WHERE id = $1::uuid`, f.ID) })
	if f.Platform != "windows" || f.Kind != "msi" || f.Name != "Field Reports" || f.Windows == nil ||
		strings.Join(f.Windows.Args, " ") != "ALLUSERS=1 REBOOT=ReallySuppress" || len(f.Warnings) != 1 {
		t.Fatalf("%+v %+v", f, f.Windows)
	}
	for _, c := range []struct{ name, args string }{
		{"bad.msi", "TARGETDIR=%22C:%5Cx%22"},
		{"setup.exe", "%2FS"},      // not a PE image
		{"fake.msi", "ALLUSERS=1"}, // not a compound file
	} {
		body := msi
		if c.name != "bad.msi" {
			body = []byte("MZ not really")
		}
		if w := uploadWinFile(t, h, c.name, c.args, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}

	// A Windows server, its agent new enough to install.
	host := unique("win") + ".example"
	cred, _ := enrolled(t, s, h, host)
	server, err := s.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.90", Protocol: "rdp",
		Principals: []string{"Administrator"}, Tags: []string{"production"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM package_runs WHERE lower(hostname) = lower($1)`, host)
		_ = s.ArchiveAsset(ctx, server.ID)
	})
	runs := "/api/v1/assets/" + server.ID + "/software/file-runs"
	body := `{"fileId":"` + f.ID + `"}`
	if w := do(t, h, http.MethodPost, runs, "admin-token", body); w.Code != http.StatusConflict {
		t.Errorf("before the agent said it can: %d %s", w.Code, w.Body)
	}
	windowsBeat(t, h, cred, host, nil)
	var view PackagesView
	if w := do(t, h, http.MethodGet, "/api/v1/assets/"+server.ID+"/packages", "admin-token", ""); w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &view)
	}
	if strings.Join(view.Installs, ",") != "remove,script,file_install" || view.Supported {
		t.Errorf("view installs = %v supported = %v", view.Installs, view.Supported)
	}

	// Production waits for a try outside it.
	if w := do(t, h, http.MethodPost, runs, "admin-token", body); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "outside production") {
		t.Errorf("production first: %d %s", w.Code, w.Body)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE assets SET tags = '{}' WHERE id = $1::uuid`, server.ID); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodPost, runs, "admin-token", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	var run PackageRun
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	if run.Mode != PackagesFileInstall || len(run.Items) != 1 || run.Items[0].Label != "Field Reports" {
		t.Errorf("run %+v", run)
	}
	if e := lastAuditOn(t, s, host); e.Action != "software.file_requested" || !strings.Contains(e.Detail, f.SHA256) {
		t.Errorf("audit %+v", e)
	}

	// The order names the file; the agent fetches it on its credential.
	o := windowsBeat(t, h, cred, host, nil)
	if o == nil || o.Run == nil || o.Run.File == nil || o.Run.File.SHA256 != f.SHA256 || o.Run.File.Kind != "msi" ||
		strings.Join(o.Run.File.Args, " ") != "ALLUSERS=1 REBOOT=ReallySuppress" {
		t.Fatalf("order %+v", o)
	}
	if w := do(t, h, http.MethodGet, o.Run.File.Path, cred, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), msi) {
		t.Errorf("fetch: %d", w.Code)
	}
	now := time.Now().UTC()
	windowsBeat(t, h, cred, host, &PackageRunStatus{ID: run.ID, State: "done", StartedAt: &now, FinishedAt: &now,
		Message: "Field Reports finished: 1 package added", Upgraded: []PackageChange{{Name: "Field Reports", To: "1.0.0"}}})
	got, err := s.PackageRunByID(ctx, run.ID)
	if err != nil || got.State != RunDone || got.SoftwareFileName != "Field Reports" {
		t.Fatalf("%+v %v", got, err)
	}
	if w := do(t, h, http.MethodGet, o.Run.File.Path, cred, ""); w.Code != http.StatusGone {
		t.Errorf("after the run: %d", w.Code)
	}
	if tried, err := s.fileTriedOutsideProduction(ctx, f.ID); err != nil || !tried {
		t.Errorf("tried = %v %v", tried, err)
	}

	// An Android package is not a Windows installer, and a Linux server
	// does not take MSI files.
	if w := do(t, h, http.MethodPost, runs, "admin-token", `{"fileId":"00000000-0000-4000-8000-000000000000"}`); w.Code != http.StatusNotFound {
		t.Errorf("no such file: %d %s", w.Code, w.Body)
	}
}

func TestWindowsInstallerArguments(t *testing.T) {
	for kind, ok := range map[string][]string{
		"msi": {"ALLUSERS=1", "INSTALLDIR=C:\\Apps\\Acme", "SERVER=reports.internal:443"},
		"exe": {"/S", "/VERYSILENT", "--quiet", "/norestart", "/D=C:\\x"},
	} {
		for _, a := range ok {
			if _, err := parseWinArgs(kind, a); err != nil {
				t.Errorf("%s %q: %v", kind, a, err)
			}
		}
	}
	for kind, bad := range map[string][]string{
		"msi": {"allusers=1", `X="a b"`, "/qn"},
		"exe": {"/S&calc", "$(x)", "`id`", "|", ";"},
	} {
		for _, a := range bad {
			if _, err := parseWinArgs(kind, a); err == nil {
				t.Errorf("%s %q was accepted", kind, a)
			}
		}
	}
}
