package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Stranded packages: what is installed that no enabled apt source offers
// any more, such as everything from a third-party source that was turned
// off. A release upgrade cannot carry them to the new release, and Ubuntu's
// upgrader refuses to remove some of them (its deny list protects
// postgresql-*), so it stops before upgrading anything. Argus lists them
// before the upgrade and sorts out the ones it can show nothing uses, which
// the console then offers to remove through the usual previewed removal.

// StrandedPackages is what the check found.
type StrandedPackages struct {
	// Count is how many installed packages no source offers.
	Count int `json:"count"`
	// Unused are the ones Argus can show nothing uses: no database on the
	// server has their extension, no preload setting loads their library,
	// and no package that stays needs them.
	Unused []string `json:"unused,omitempty"`
	// Kept are the rest, each with why Argus leaves it.
	Kept []StrandedKept `json:"kept,omitempty"`
	// Protected is how many of all of them the upgrader will not remove.
	Protected int `json:"protected,omitempty"`
}

// StrandedKept is a stranded package Argus will not offer to remove.
type StrandedKept struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

const (
	maxStrandedUnused = 1000
	maxStrandedKept   = 200
)

// pgClusterDir is where PostgreSQL's clusters are set up; tests move it.
// The package database is dpkgInfoDir (software_managers.go).
var pgClusterDir = "/etc/postgresql"

// reUpgraderDenied is the part of Ubuntu's removal deny list that stops
// upgrades on database servers.
var reUpgraderDenied = regexp.MustCompile(`^postgresql-.*[0-9][0-9]`)

// stranded works the list out. nil when the server has nothing stranded or
// apt could not say.
func (c PackageChecker) stranded(ctx context.Context) *StrandedPackages {
	out, code, err := output(ctx, c.Run, aptEnv, "apt", "list", "--installed")
	if err != nil || code != 0 {
		return nil
	}
	local, auto := parseAptInstalledLocal(out)
	if len(local) == 0 {
		return nil
	}
	if b, code, err := output(ctx, c.Run, aptEnv, "apt-mark", "showauto"); err == nil && code == 0 {
		for _, n := range strings.Fields(string(b)) {
			auto[n] = true
		}
	}
	deps, provides := map[string][][]string{}, map[string][]string{}
	if b, code, err := output(ctx, c.Run, nil, "dpkg-query", "-W", "-f",
		"${Package}\t${db:Status-Abbrev}\t${Pre-Depends}, ${Depends}\t${Provides}\n"); err == nil && code == 0 {
		deps, provides = parseDpkgDepends(b)
	}
	pg := c.postgresUse(ctx)
	return sortStranded(local, auto, deps, provides, pg, packageFiles)
}

// parseAptInstalledLocal reads apt list --installed for packages only the
// local package database knows ("[installed,local]"), and which of all of
// them were installed as dependencies.
func parseAptInstalledLocal(b []byte) (local []string, auto map[string]bool) {
	auto = map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, "/")
		if !ok || strings.HasPrefix(line, "WARNING") || strings.HasPrefix(line, "Listing") {
			continue
		}
		i := strings.LastIndex(rest, "[")
		if i < 0 {
			continue
		}
		flags := strings.Split(strings.Trim(rest[i:], "[] "), ",")
		if hasString(flags, "automatic") {
			auto[name] = true
		}
		if hasString(flags, "local") {
			local = append(local, name)
		}
	}
	return sortedUnique(local), auto
}

// parseDpkgDepends reads each installed package's dependencies, as groups
// of alternatives, and what virtual names packages provide.
func parseDpkgDepends(b []byte) (deps map[string][][]string, provides map[string][]string) {
	deps, provides = map[string][][]string{}, map[string][]string{}
	bare := func(s string) string {
		s = strings.TrimSpace(s)
		if i := strings.IndexAny(s, " ("); i >= 0 {
			s = s[:i]
		}
		s, _, _ = strings.Cut(s, ":")
		return s
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 || !strings.HasPrefix(f[1], "ii") {
			continue
		}
		pkg := f[0]
		deps[pkg] = nil
		for _, group := range strings.Split(f[2], ",") {
			var alts []string
			for _, a := range strings.Split(group, "|") {
				if n := bare(a); n != "" {
					alts = append(alts, n)
				}
			}
			if len(alts) > 0 {
				deps[pkg] = append(deps[pkg], alts)
			}
		}
		for _, p := range strings.Split(f[3], ",") {
			if n := bare(p); n != "" {
				provides[n] = append(provides[n], pkg)
			}
		}
	}
	return deps, provides
}

// packageFiles is the files a package installed, from dpkg's own list.
func packageFiles(pkg string) []string {
	for _, name := range []string{pkg + ".list", pkg + ":amd64.list", pkg + ":arm64.list", pkg + ":all.list"} {
		b, err := os.ReadFile(filepath.Join(dpkgInfoDir, name))
		if err == nil {
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dpkgInfoDir, pkg+":*.list")); len(m) > 0 {
		if b, err := os.ReadFile(m[0]); err == nil {
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
	}
	return nil
}

// pgUse is what the server's PostgreSQL clusters use.
type pgUse struct {
	// clusters are the major versions with a cluster set up.
	clusters map[string]bool
	// extensions are the extensions some database has, by major version;
	// a version missing here could not be asked.
	extensions map[string]map[string]string
	// libraries are what preload settings load, by major version.
	libraries map[string]map[string]bool
	// unasked says why a version with a cluster could not be asked.
	unasked map[string]string
}

// postgresUse asks every PostgreSQL cluster on the server which extensions
// its databases have and which libraries it preloads.
func (c PackageChecker) postgresUse(ctx context.Context) pgUse {
	u := pgUse{clusters: map[string]bool{}, extensions: map[string]map[string]string{},
		libraries: map[string]map[string]bool{}, unasked: map[string]string{}}
	confs, _ := filepath.Glob(filepath.Join(pgClusterDir, "*", "*", "postgresql.conf"))
	for _, conf := range confs {
		ver := filepath.Base(filepath.Dir(filepath.Dir(conf)))
		u.clusters[ver] = true
		port := pgConfPort(conf)
		psql := func(db, sql string) ([]string, error) {
			b, code, err := output(ctx, c.Run, nil, "runuser", "-u", "postgres", "--", "psql", "-X", "-A", "-t", "-q",
				"-p", port, "-d", db, "-c", sql)
			if err == nil && code != 0 {
				err = fmt.Errorf("psql exited %d: %s", code, tail(b, 200))
			}
			if err != nil {
				return nil, err
			}
			return strings.Fields(strings.TrimSpace(string(b))), nil
		}
		dbs, err := psql("postgres", "select datname from pg_database where datallowconn")
		if err != nil {
			u.unasked[ver] = fmt.Sprintf("Argus could not ask PostgreSQL %s (port %s) what it uses: %v", ver, port, err)
			continue
		}
		exts := u.extensions[ver]
		if exts == nil {
			exts = map[string]string{}
			u.extensions[ver] = exts
		}
		for _, db := range dbs {
			names, err := psql(db, "select extname from pg_extension")
			if err != nil {
				u.unasked[ver] = fmt.Sprintf("Argus could not ask database %s on PostgreSQL %s what it uses: %v", db, ver, err)
				break
			}
			for _, n := range names {
				if exts[n] == "" {
					exts[n] = db
				}
			}
		}
		if _, bad := u.unasked[ver]; bad {
			delete(u.extensions, ver)
			continue
		}
		libs, err := psql("postgres", "select string_agg(setting, ',') from pg_settings where name in "+
			"('shared_preload_libraries','session_preload_libraries','local_preload_libraries')")
		if err != nil {
			u.unasked[ver] = fmt.Sprintf("Argus could not ask PostgreSQL %s what it preloads: %v", ver, err)
			delete(u.extensions, ver)
			continue
		}
		set := u.libraries[ver]
		if set == nil {
			set = map[string]bool{}
			u.libraries[ver] = set
		}
		for _, l := range strings.Split(strings.Join(libs, ""), ",") {
			l = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "$libdir/"), ".so")
			if l != "" {
				set[filepath.Base(l)] = true
			}
		}
	}
	return u
}

var rePgPort = regexp.MustCompile(`(?m)^\s*port\s*=\s*'?(\d+)`)

func pgConfPort(conf string) string {
	b, err := os.ReadFile(conf)
	if err == nil {
		if m := rePgPort.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return "5432"
}

var (
	rePgVersioned = regexp.MustCompile(`^postgresql-(?:client-|server-dev-|plperl-|plpython3-|pltcl-)?(\d+)(?:$|-)`)
	rePgExtFile   = regexp.MustCompile(`^/usr/share/postgresql/(\d+)/extension/([^/]+)\.control$`)
	rePgLibFile   = regexp.MustCompile(`^/usr/lib/postgresql/(\d+)/lib/(?:bitcode/)?([^/]+)\.so$`)
	rePgProgram   = regexp.MustCompile(`^/usr/(?:s?bin|lib/postgresql/\d+/bin)/[^/]+$|^/(?:usr/)?lib/systemd/system/[^/]+$`)
)

// sortStranded decides, for each stranded package, whether Argus can show
// nothing uses it.
func sortStranded(local []string, auto map[string]bool, deps map[string][][]string, provides map[string][]string,
	pg pgUse, files func(string) []string) *StrandedPackages {
	out := &StrandedPackages{Count: len(local)}
	why := map[string]string{}
	newestClient := ""
	for _, p := range local {
		if m := rePgVersioned.FindStringSubmatch(p); m != nil && strings.HasPrefix(p, "postgresql-client-") &&
			versionNewer(m[1], newestClient) {
			newestClient = m[1]
		}
	}
	for _, p := range local {
		if reUpgraderDenied.MatchString(p) {
			out.Protected++
		}
		if w := strandedInUse(p, auto[p], newestClient, pg, files); w != "" {
			why[p] = w
		}
	}
	// What stays keeps what it depends on: a package that stays and needs
	// one of these, with no alternative outside them, keeps it too.
	removing := map[string]bool{}
	for _, p := range local {
		if why[p] == "" {
			removing[p] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for pkg, groups := range deps {
			if removing[pkg] {
				continue
			}
			for _, alts := range groups {
				var inside []string
				outside := false
				for _, a := range alts {
					cands := []string{a}
					if _, real := deps[a]; !real {
						cands = provides[a]
					}
					for _, c := range cands {
						if removing[c] {
							inside = append(inside, c)
						} else if _, installed := deps[c]; installed {
							outside = true
						}
					}
				}
				if outside {
					continue
				}
				for _, c := range inside {
					delete(removing, c)
					why[c] = pkg + " needs it, and " + pkg + " stays"
					changed = true
				}
			}
		}
	}
	for _, p := range local {
		if removing[p] {
			if len(out.Unused) < maxStrandedUnused {
				out.Unused = append(out.Unused, p)
			}
		} else if len(out.Kept) < maxStrandedKept {
			out.Kept = append(out.Kept, StrandedKept{Name: p, Why: why[p]})
		}
	}
	sort.Strings(out.Unused)
	return out
}

// strandedInUse is why a stranded package counts as used, or "" when
// nothing on the server uses it (before what other packages need).
func strandedInUse(p string, automatic bool, newestClient string, pg pgUse, files func(string) []string) string {
	switch {
	case strings.HasSuffix(p, "-dbgsym") || strings.HasSuffix(p, "-dbg"):
		return "" // debugging symbols: nothing runs them
	case strings.HasSuffix(p, "-doc"):
		return ""
	case p == "postgresql-common" || p == "postgresql-client-common" || p == "postgresql" ||
		p == "postgresql-client" || p == "postgresql-contrib":
		if len(pg.clusters) > 0 {
			return "part of PostgreSQL itself, which runs here"
		}
	}
	ver := ""
	if m := rePgVersioned.FindStringSubmatch(p); m != nil {
		ver = m[1]
	}
	fs := files(p)
	var exts, libs []string
	extVer := ver
	program := ""
	for _, f := range fs {
		if m := rePgExtFile.FindStringSubmatch(f); m != nil {
			exts, extVer = append(exts, m[2]), m[1]
			if pg.clusters[m[1]] {
				if db := pg.extensions[m[1]][m[2]]; db != "" {
					return fmt.Sprintf("database %s uses its %s extension", db, m[2])
				}
				if w := pg.unasked[m[1]]; w != "" {
					return w
				}
			}
		}
		if m := rePgLibFile.FindStringSubmatch(f); m != nil {
			libs = append(libs, m[2])
			if pg.clusters[m[1]] && pg.libraries[m[1]][m[2]] {
				return fmt.Sprintf("PostgreSQL %s preloads its %s library", m[1], m[2])
			}
		}
		if program == "" && rePgProgram.MatchString(f) {
			program = f
		}
	}
	switch {
	case ver != "" && !pg.clusters[ver] && len(exts) == 0:
		// A PostgreSQL version with no cluster: nothing runs on it.
		if strings.HasPrefix(p, "postgresql-client-") && ver == newestClient {
			return "the newest PostgreSQL client, which psql runs"
		}
		return ""
	case ver != "" && pg.clusters[ver] && (p == "postgresql-"+ver || strings.HasPrefix(p, "postgresql-client-") ||
		p == "postgresql-"+ver+"-jit"):
		return "PostgreSQL " + ver + " runs here"
	case len(exts) > 0 && !pg.clusters[extVer] && ver != "":
		return ""
	case len(exts) > 0:
		if program != "" && !pg.clusters[extVer] {
			return ""
		}
		if program != "" {
			return "it installs " + program + ", which Argus cannot tell is unused"
		}
		return "" // its extensions are in no database
	case ver != "" && pg.clusters[ver] && len(libs) > 0 && program == "":
		return "" // libraries nothing preloads, and no extension
	case ver != "" && pg.clusters[ver] && program != "":
		return "it installs " + program + ", which Argus cannot tell is unused"
	case ver != "" && pg.clusters[ver]:
		return "" // scripts or data for PostgreSQL that nothing loads
	case automatic && program == "":
		return "" // installed only for other packages; kept if one that stays needs it
	}
	return "Argus cannot tell whether anything uses it"
}

// versionNewer compares PostgreSQL major versions.
func versionNewer(a, b string) bool {
	if b == "" {
		return true
	}
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}
