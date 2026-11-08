package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
What is installed on an Android device, as its app last reported it.

The app lists the device's packages with the package manager -- read-only,
and nothing a secret lives in: a package id, a label, a version and where it
came from are visible to any app on the device already. Here the report is
bounded on the way in (the app is credentialed, but it is still input) and
served to the device page's Apps tab, its "what runs here".
*/

// AndroidApps mirrors agent.AndroidApps, in the agent's spelling.
type AndroidApps struct {
	CheckedAt time.Time    `json:"checked_at"`
	Apps      []AndroidApp `json:"apps"`
}

// AndroidApp is one installed app, as the agent spells it.
type AndroidApp struct {
	Package string `json:"package"`
	Label   string `json:"label,omitempty"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
	// Disabled is a preinstalled app disabled for the device's user: still
	// on the device, not usable, and restorable.
	Disabled bool `json:"disabled,omitempty"`
}

const (
	maxAndroidApps    = 1000
	maxAndroidAppText = 256
)

// androidAppSources is where an app may say it came from. Anything else is
// dropped to empty, so a newer app cannot write a source the console does not
// know into the list.
var androidAppSources = map[string]bool{"play": true, "preinstalled": true, "sideloaded": true}

// bounded caps the report an agent sends: a device with thousands of packages
// does not fill the row, and each string is trimmed. Nil stays nil, so an
// older app leaves what the row holds.
func (a *AndroidApps) bounded() *AndroidApps {
	if a == nil {
		return nil
	}
	out := &AndroidApps{CheckedAt: a.CheckedAt, Apps: []AndroidApp{}}
	for _, app := range a.Apps {
		if len(out.Apps) >= maxAndroidApps {
			break
		}
		pkg := clampText(app.Package, maxAndroidAppText)
		if pkg == "" {
			continue
		}
		src := app.Source
		if !androidAppSources[src] {
			src = ""
		}
		out.Apps = append(out.Apps, AndroidApp{
			Package:  pkg,
			Label:    clampText(app.Label, maxAndroidAppText),
			Version:  clampText(app.Version, maxAndroidAppText),
			Source:   src,
			Disabled: app.Disabled,
		})
	}
	return out
}

func clampText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ServerAndroidApps is what the device page's Apps tab shows.
type ServerAndroidApps struct {
	// Reported is false when the device has not sent a list: it is older than
	// app reporting, or has not checked in.
	Reported  bool               `json:"reported"`
	CheckedAt *time.Time         `json:"checkedAt"`
	Apps      []ServerAndroidApp `json:"apps"`
}

// ServerAndroidApp is one installed app on the device page. The action column
// and the source let the app-install work hang controls off each row later.
type ServerAndroidApp struct {
	Package string `json:"package"`
	Label   string `json:"label"`
	Version string `json:"version"`
	Source  string `json:"source"`
	// Disabled is a preinstalled app disabled for the device's user.
	Disabled bool `json:"disabled,omitempty"`
}

// AndroidAppsFor reads what the app on device id reported installed.
func (s *Store) AndroidAppsFor(ctx context.Context, id string) (ServerAndroidApps, error) {
	var raw []byte
	err := s.db(ctx).QueryRow(ctx, `
		SELECT ag.android_apps
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT android_apps FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.id::text = $1 AND a.archived_at IS NULL`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerAndroidApps{}, ErrAssetNotFound
	}
	if err != nil {
		return ServerAndroidApps{}, err
	}
	out := ServerAndroidApps{Apps: []ServerAndroidApp{}}
	var inv AndroidApps
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &inv) != nil {
		return out, nil // not reported, or a report that does not parse
	}
	out.Reported = true
	if !inv.CheckedAt.IsZero() {
		at := inv.CheckedAt
		out.CheckedAt = &at
	}
	for _, a := range inv.Apps {
		out.Apps = append(out.Apps, ServerAndroidApp{
			Package: a.Package, Label: a.Label, Version: a.Version, Source: a.Source, Disabled: a.Disabled,
		})
	}
	return out, nil
}

// getAndroidApps lists what is installed on a device. Readable by whoever sees
// the whole fleet, like the server applications list.
func (a *API) getAndroidApps(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden,
			"seeing what is installed on a device requires the admin, owner or auditor role")
		return
	}
	out, err := a.store.AndroidAppsFor(r.Context(), r.PathValue("id"))
	if err != nil {
		a.failAsset(w, "android apps", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
