package agent

import (
	"strings"
	"testing"
)

func TestParseAptInstalledLocal(t *testing.T) {
	out := []byte("Listing...\n" +
		"postgresql-18/now 18.7-1.pgdg24.04+2 amd64 [installed,local]\n" +
		"libgdal39/now 3.11.0-1.pgdg24.04+1 amd64 [installed,automatic,local]\n" +
		"bash/noble,now 5.2.21-2ubuntu4 amd64 [installed]\n" +
		"libc6/noble-updates,now 2.39-0ubuntu8.6 amd64 [installed,automatic]\n")
	local, auto := parseAptInstalledLocal(out)
	if strings.Join(local, ",") != "libgdal39,postgresql-18" || !auto["libgdal39"] || !auto["libc6"] || auto["bash"] {
		t.Fatalf("local %v auto %v", local, auto)
	}
}

// The server of 2026-11-15: PostgreSQL 18 runs with one database, and the
// apt.postgresql.org source is off, so everything from it is stranded.
func TestSortStranded(t *testing.T) {
	local := []string{"libgdal39", "pgagent", "postgresql-11", "postgresql-17", "postgresql-17-postgis-3",
		"postgresql-18", "postgresql-18-jit", "postgresql-18-mobilitydb", "postgresql-18-mobilitydb-dbgsym",
		"postgresql-18-pgaudit", "postgresql-18-postgis-3", "postgresql-18-postgis-3-scripts",
		"postgresql-18-repmgr", "postgresql-client-15", "postgresql-client-18", "postgresql-common",
		"postgresql-server-dev-15", "postgresql-18-timescaledb", "mystery-tool"}
	deps, provides := parseDpkgDepends([]byte(strings.Join([]string{
		"libgdal39\tii \t, libc6\t",
		"pgagent\tii \t, postgresql-client-common, libpq5\t",
		"postgresql-11\tii \t, postgresql-common\t",
		"postgresql-17\tii \t, postgresql-common\t",
		"postgresql-17-postgis-3\tii \t, postgresql-17, libgdal39\t",
		"postgresql-18\tii \t, postgresql-common, postgresql-client-18\tpostgresql-contrib-18",
		"postgresql-18-jit\tii \t, postgresql-18\t",
		"postgresql-18-mobilitydb\tii \t, postgresql-18-postgis-3\t",
		"postgresql-18-mobilitydb-dbgsym\tii \t, postgresql-18-mobilitydb (= 1.3.1)\t",
		"postgresql-18-pgaudit\tii \t, postgresql-18\t",
		"postgresql-18-postgis-3\tii \t, postgresql-18, postgresql-18-postgis-3-scripts, libgdal39\t",
		"postgresql-18-postgis-3-scripts\tii \t, \t",
		"postgresql-18-repmgr\tii \t, postgresql-18\t",
		"postgresql-18-timescaledb\tii \t, postgresql-18\t",
		"postgresql-client-15\tii \t, postgresql-client-common\t",
		"postgresql-client-18\tii \t, postgresql-client-common\t",
		"postgresql-common\tii \t, adduser\t",
		"postgresql-client-common\tii \t, \t",
		"postgresql-server-dev-15\tii \t, postgresql-client-15\t",
		"mystery-tool\tii \t, \t",
		"libc6\tii \t, \t",
		"my-app\tii \t, mystery-tool | something-else\t",
	}, "\n")))
	files := map[string][]string{
		"pgagent":                         {"/usr/bin/pgagent", "/usr/share/postgresql/18/extension/pgagent.control"},
		"postgresql-18":                   {"/usr/lib/postgresql/18/bin/postgres"},
		"postgresql-18-mobilitydb":        {"/usr/lib/postgresql/18/lib/libMobilityDB-1.3.so", "/usr/share/postgresql/18/extension/mobilitydb.control"},
		"postgresql-18-pgaudit":           {"/usr/lib/postgresql/18/lib/pgaudit.so", "/usr/share/postgresql/18/extension/pgaudit.control"},
		"postgresql-18-postgis-3":         {"/usr/lib/postgresql/18/lib/postgis-3.so"},
		"postgresql-18-postgis-3-scripts": {"/usr/share/postgresql/18/extension/postgis.control"},
		"postgresql-18-repmgr":            {"/usr/lib/postgresql/18/bin/repmgrd", "/usr/share/postgresql/18/extension/repmgr.control"},
		"postgresql-18-timescaledb":       {"/usr/lib/postgresql/18/lib/timescaledb.so", "/usr/share/postgresql/18/extension/timescaledb.control"},
		"postgresql-17-postgis-3":         {"/usr/share/postgresql/17/extension/postgis.control"},
	}
	pg := pgUse{clusters: map[string]bool{"18": true},
		extensions: map[string]map[string]string{"18": {"pgagent": "athena_db", "pg_trgm": "athena_db"}},
		libraries:  map[string]map[string]bool{"18": {"pg_stat_statements": true, "timescaledb": true}},
		unasked:    map[string]string{}}
	got := sortStranded(local, map[string]bool{"libgdal39": true, "mystery-tool": true}, deps, provides, pg, func(p string) []string { return files[p] })
	want := "libgdal39,postgresql-11,postgresql-17,postgresql-17-postgis-3,postgresql-18-mobilitydb," +
		"postgresql-18-mobilitydb-dbgsym,postgresql-18-pgaudit,postgresql-18-postgis-3,postgresql-18-postgis-3-scripts," +
		"postgresql-client-15,postgresql-server-dev-15"
	if strings.Join(got.Unused, ",") != want {
		t.Errorf("unused\n got %s\nwant %s", strings.Join(got.Unused, ","), want)
	}
	kept := map[string]string{}
	for _, k := range got.Kept {
		kept[k.Name] = k.Why
	}
	for name, why := range map[string]string{
		"pgagent":                   "database athena_db uses its pgagent extension",
		"postgresql-18":             "PostgreSQL 18 runs here",
		"postgresql-18-jit":         "PostgreSQL 18 runs here",
		"postgresql-client-18":      "PostgreSQL 18 runs here",
		"postgresql-common":         "part of PostgreSQL itself",
		"postgresql-18-timescaledb": "preloads its timescaledb library",
		"postgresql-18-repmgr":      "repmgrd",
		"mystery-tool":              "my-app needs it",
	} {
		if !strings.Contains(kept[name], why) {
			t.Errorf("%s kept for %q, want %q", name, kept[name], why)
		}
	}
	if got.Count != len(local) || got.Protected != 15 {
		t.Errorf("count %d protected %d", got.Count, got.Protected)
	}
}

// A cluster Argus could not ask keeps every extension of its version.
func TestSortStrandedUnaskedKeepsExtensions(t *testing.T) {
	pg := pgUse{clusters: map[string]bool{"18": true}, extensions: map[string]map[string]string{},
		libraries: map[string]map[string]bool{}, unasked: map[string]string{"18": "Argus could not ask PostgreSQL 18"}}
	got := sortStranded([]string{"postgresql-18-postgis-3-scripts"}, nil, nil, nil, pg, func(string) []string {
		return []string{"/usr/share/postgresql/18/extension/postgis.control"}
	})
	if len(got.Unused) != 0 || len(got.Kept) != 1 || !strings.Contains(got.Kept[0].Why, "could not ask") {
		t.Fatalf("%+v", got)
	}
}
