package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/*
The app the phone downloads: the deployment's own build when one is
configured, the project's release of this version otherwise, and a plain
answer when there is neither.
*/

func androidAPK(t *testing.T, a *API) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/install/argus-android.apk", nil)
	w := httptest.NewRecorder()
	a.getAndroidAPK(w, r)
	return w
}

func TestTheConfiguredAppIsServedAsAnAPK(t *testing.T) {
	a := installAPI()
	a.AndroidAPK = filepath.Join(t.TempDir(), "argus.apk")
	if err := os.WriteFile(a.AndroidAPK, []byte("PK\x03\x04 the app"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := androidAPK(t, a)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.android.package-archive" {
		t.Fatalf("content type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "argus-android.apk") {
		t.Fatalf("content disposition %q", cd)
	}
	if w.Body.String() != "PK\x03\x04 the app" {
		t.Fatalf("body %q", w.Body)
	}
}

func TestAMissingConfiguredAppIsNotFoundNotAnError(t *testing.T) {
	a := installAPI()
	a.AndroidAPK = filepath.Join(t.TempDir(), "gone.apk")
	if w := androidAPK(t, a); w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestWithoutAConfiguredAppThePhoneIsSentToTheRelease(t *testing.T) {
	a := installAPI()
	a.Version = "v2.1.0"
	w := androidAPK(t, a)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != DefaultAgentReleasesURL+"/v2.1.0/argus-android.apk" {
		t.Fatalf("location %q", loc)
	}

	// A mirror, like the agent packages.
	a.AgentReleasesURL = "https://mirror.example/argus/"
	if loc := androidAPK(t, a).Header().Get("Location"); loc != "https://mirror.example/argus/v2.1.0/argus-android.apk" {
		t.Fatalf("mirror location %q", loc)
	}

	// A newer release the watcher found wins, as it does for the install script.
	a.Releases = &ReleaseWatcher{latest: &AgentRelease{Version: "2.2.0"}}
	if loc := androidAPK(t, a).Header().Get("Location"); !strings.HasSuffix(loc, "/v2.2.0/argus-android.apk") {
		t.Fatalf("newer release location %q", loc)
	}
}

func TestABuildFromSourceSaysItHasNoAppToOffer(t *testing.T) {
	a := installAPI()
	a.Version = "dev"
	w := androidAPK(t, a)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "android_apk") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}
