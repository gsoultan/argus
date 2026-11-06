package control

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

/*
The Argus app for Android, handed out by the control plane.

The console's add-a-device page points the phone at
/api/v1/install/argus-android.apk, like the install script for a server.
Where the APK comes from is the deployment's choice: a file configured in
android_apk (a build of the app signed with the deployment's own key), or
the project's release of the control plane's own version, which is what a
server's install script does. Nothing on the route needs a sign-in -- the
phone has none yet -- and nothing on it is a secret: the app is the same
for every Argus, and it is the code it scans that links it to this one.
*/

// androidAPKName is the release asset, and the file the console names.
const androidAPKName = "argus-android.apk"

// getAndroidAPK serves the app, or sends the phone to the release that
// matches this control plane.
func (a *API) getAndroidAPK(w http.ResponseWriter, r *http.Request) {
	if a.AndroidAPK != "" {
		f, err := os.Open(a.AndroidAPK)
		if err != nil {
			a.log.Error("the Android app cannot be read", "path", a.AndroidAPK, "error", err)
			writeErr(w, http.StatusNotFound, "the Android app is not available on this Argus")
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			writeErr(w, http.StatusNotFound, "the Android app is not available on this Argus")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		w.Header().Set("Content-Disposition", `attachment; filename="`+androidAPKName+`"`)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, filepath.Base(a.AndroidAPK), st.ModTime(), f)
		return
	}
	offer, _, _ := a.agentTarget()
	if offer == "" {
		writeErr(w, http.StatusNotFound,
			"the Android app is not available: this Argus is a build from source with no release "+
				"to point at; set android_apk to a built app")
		return
	}
	base := a.AgentReleasesURL
	if base == "" {
		base = DefaultAgentReleasesURL
	}
	http.Redirect(w, r, strings.TrimRight(base, "/")+"/v"+offer+"/"+androidAPKName, http.StatusFound)
}
