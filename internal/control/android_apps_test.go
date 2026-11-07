package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func getAndroidApps(t *testing.T, h http.Handler, path string) ServerAndroidApps {
	t.Helper()
	w := do(t, h, http.MethodGet, path, "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	var got ServerAndroidApps
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestADevicesInstalledAppsReachTheAppsTab(t *testing.T) {
	s, h, _ := reportAPI(t)
	asset, cred := enrolledAsset(t, s, h)
	path := "/api/v1/assets/" + asset.ID + "/android-apps"

	if got := getAndroidApps(t, h, path); got.Reported || len(got.Apps) != 0 {
		t.Fatalf("before a report: %+v", got)
	}

	facts := `{"hostname":"` + asset.Hostname + `","os":"Android 14","android_apps":{` +
		`"checked_at":"2026-10-11T02:14:00Z","apps":[` +
		`{"package":"com.android.chrome","label":"Chrome","version":"131.0","source":"play"},` +
		`{"package":"com.android.settings","label":"Settings","version":"14","source":"preinstalled"},` +
		`{"package":"  ","label":"blank id dropped"},` +
		`{"package":"com.evil.app","label":"Sideloaded","source":"nonsense-source"}]}}`
	if w := do(t, h, http.MethodPost, "/api/v1/report/facts", cred, facts); w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}

	got := getAndroidApps(t, h, path)
	if !got.Reported || got.CheckedAt == nil {
		t.Fatalf("after a report: %+v", got)
	}
	if len(got.Apps) != 3 {
		t.Fatalf("got %d apps; want the blank-id one dropped, 3 kept: %+v", len(got.Apps), got.Apps)
	}
	if got.Apps[0].Package != "com.android.chrome" || got.Apps[0].Source != "play" {
		t.Errorf("first app %+v", got.Apps[0])
	}
	// An unknown source is dropped to empty rather than carried through.
	for _, a := range got.Apps {
		if a.Package == "com.evil.app" && a.Source != "" {
			t.Errorf("an unknown source %q was kept", a.Source)
		}
	}
}

func TestAndroidAppsReportIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"checked_at":"2026-10-11T02:14:00Z","apps":[`)
	for i := 0; i < maxAndroidApps+50; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"package":"com.app.n`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(`"}`)
	}
	b.WriteString("]}")
	var apps AndroidApps
	if err := json.Unmarshal([]byte(b.String()), &apps); err != nil {
		t.Fatal(err)
	}
	bounded := apps.bounded()
	if len(bounded.Apps) != maxAndroidApps {
		t.Fatalf("bounded to %d apps; want the cap %d", len(bounded.Apps), maxAndroidApps)
	}
	// A nil report stays nil, so an older app does not wipe the row.
	var nilApps *AndroidApps
	if nilApps.bounded() != nil {
		t.Error("a nil report did not stay nil")
	}
}
