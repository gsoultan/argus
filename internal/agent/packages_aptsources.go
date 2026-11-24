package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

/*
apt's sources after a release upgrade, as far as PostgreSQL's own repository
(apt.postgresql.org, "PGDG") and the files the upgrader leaves are concerned.

Two things are left behind that make a person ask:

  - Files in sources.list.d apt will not read: pgdg.list.migrate (the
    upgrader's copy from turning pgdg.list into pgdg.sources), and the
    .distUpgrade copies of the old ones. apt prints "Notice: Ignoring file
    'pgdg.list.migrate' ... as it has an invalid filename extension" on every
    update. They are harmless; Argus offers to move them aside, but only
    where the real file they copy is there, never the only copy of a source.

  - The PGDG source turned off ("Enabled: no", or a commented line) or still
    naming the old release's suite (noble-pgdg on resolute): then nothing
    from apt.postgresql.org updates. Argus offers to point it at this
    release's suite and turn it on, and puts it back if apt.postgresql.org
    does not serve that suite yet.
*/

// AptSourcesFix is what the check found that an apt_sources_fix run can
// put right.
type AptSourcesFix struct {
	// Leftovers are files apt ignores in sources.list.d.
	Leftovers []AptLeftover `json:"leftovers,omitempty"`
	// PGDG is apt.postgresql.org turned off or on another release.
	PGDG *PGDGSource `json:"pgdg,omitempty"`
	// Fingerprint names what a run would change, so it changes nothing if
	// the files moved on since the console showed them.
	Fingerprint string `json:"fingerprint"`
}

// AptLeftover is one file apt does not read.
type AptLeftover struct {
	Name string `json:"name"`
	// Noticed is apt printing a notice about it on every update; the rest
	// it skips silently (.distUpgrade, .save).
	Noticed bool `json:"noticed,omitempty"`
	// Movable is the real file it copies being there; Why says why not.
	Movable bool   `json:"movable"`
	Why     string `json:"why,omitempty"`
}

// PGDGSource is apt.postgresql.org's entry when it is off or on another
// release.
type PGDGSource struct {
	// File is the sources file, under /etc/apt.
	File string `json:"file"`
	// Disabled is "Enabled: no" or a commented line; Suites what it names;
	// Want the suite for this release.
	Disabled bool     `json:"disabled,omitempty"`
	Suites   []string `json:"suites"`
	Want     string   `json:"want"`
}

// aptIgnoredSilently is apt's Dir::Ignore-Files-Silently: files it skips
// without a notice.
var aptIgnoredSilently = regexp.MustCompile(`~$|\.disabled$|\.bak$|\.dpkg-[a-z]+$|\.ucf-[a-z]+$|\.save$|\.orig$|\.distUpgrade$`)

// aptLeftoverSilently are the silent ones a release upgrade leaves, which
// are moved with the noticed ones.
var aptLeftoverSilently = regexp.MustCompile(`\.distUpgrade$|\.save$`)

// aptBackupDir is where a run moves what it takes out of apt's way.
var aptBackupDir = "/var/backups/argus-apt-sources"

// aptSourcesFix reads sources.list.d. nil when there is nothing to fix.
// pgdgUsed is whether anything installed came from apt.postgresql.org.
func aptSourcesFix(osr map[string]string, pgdgUsed bool) *AptSourcesFix {
	dir := filepath.Join(aptSourcesRoot, "sources.list.d")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	f := &AptSourcesFix{}
	real := map[string]bool{}
	for _, e := range ents {
		if e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".list") || strings.HasSuffix(e.Name(), ".sources")) {
			real[strings.SplitN(e.Name(), ".", 2)[0]] = true
		}
	}
	noticed := false
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasSuffix(n, ".list") || strings.HasSuffix(n, ".sources") {
			continue
		}
		l := AptLeftover{Name: n, Noticed: !aptIgnoredSilently.MatchString(n)}
		if !l.Noticed && !aptLeftoverSilently.MatchString(n) {
			continue
		}
		noticed = noticed || l.Noticed
		base := strings.SplitN(n, ".", 2)[0]
		if real[base] {
			l.Movable = true
		} else {
			l.Why = "no " + base + ".sources or " + base + ".list beside it, so it may be the only copy of that source"
		}
		f.Leftovers = append(f.Leftovers, l)
	}
	if !noticed {
		f.Leftovers = nil
	}
	if pgdgUsed {
		f.PGDG = pgdgProblem(dir, osr["VERSION_CODENAME"])
	}
	if len(f.Leftovers) == 0 && f.PGDG == nil {
		return nil
	}
	f.Fingerprint = f.fingerprint()
	return f
}

// fingerprint hashes what a run would touch, contents included.
func (f *AptSourcesFix) fingerprint() string {
	h := sha256.New()
	for _, l := range f.Leftovers {
		if l.Movable {
			h.Write([]byte("move " + l.Name + "\n"))
		}
	}
	if f.PGDG != nil {
		b, _ := os.ReadFile(filepath.Join(aptSourcesRoot, f.PGDG.File))
		s := sha256.Sum256(b)
		h.Write([]byte("pgdg " + f.PGDG.File + " " + f.PGDG.Want + " " + hex.EncodeToString(s[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pgdgHost is apt.postgresql.org, or its archive.
func pgdgHost(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "apt.postgresql.org" || h == "apt-archive.postgresql.org"
}

// pgdgEntry is one apt.postgresql.org entry.
type pgdgEntry struct {
	file    string
	enabled bool
	suites  []string
}

// pgdgProblem is the PGDG entry to fix, nil when one is on and names this
// release (or the server has none at all, which is a person's choice).
func pgdgProblem(dir, codename string) *PGDGSource {
	if codename == "" {
		return nil
	}
	want := codename + "-pgdg"
	var found []pgdgEntry
	files, _ := filepath.Glob(filepath.Join(dir, "*.sources"))
	lists, _ := filepath.Glob(filepath.Join(dir, "*.list"))
	sort.Strings(files)
	sort.Strings(lists)
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err == nil {
			found = append(found, pgdgDeb822(rel(p), b)...)
		}
	}
	for _, p := range append(lists, filepath.Join(aptSourcesRoot, "sources.list")) {
		b, err := os.ReadFile(p)
		if err == nil {
			found = append(found, pgdgOneLine(rel(p), b)...)
		}
	}
	for _, e := range found {
		if e.enabled && pgdgSuiteFits(e.suites, codename) {
			return nil
		}
	}
	if len(found) == 0 {
		return nil
	}
	e := found[0]
	return &PGDGSource{File: e.file, Disabled: !e.enabled, Suites: e.suites, Want: want}
}

func rel(p string) string {
	r, err := filepath.Rel(aptSourcesRoot, p)
	if err != nil {
		return p
	}
	// Shown as it is under /etc/apt, whatever the separator here.
	return filepath.ToSlash(r)
}

// pgdgSuiteFits is a suite of this release: resolute-pgdg, or its testing
// and snapshot suites.
func pgdgSuiteFits(suites []string, codename string) bool {
	for _, s := range suites {
		if strings.HasPrefix(s, codename+"-pgdg") {
			return true
		}
	}
	return false
}

// pgdgDeb822 reads the PGDG stanzas of a .sources file.
func pgdgDeb822(file string, b []byte) []pgdgEntry {
	var out []pgdgEntry
	for _, st := range reStanzaGap.Split(string(b), -1) {
		var uris, suites []string
		enabled := true
		for _, l := range strings.Split(st, "\n") {
			if strings.HasPrefix(l, "#") || strings.HasPrefix(l, " ") {
				continue
			}
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "uris":
				uris = strings.Fields(v)
			case "suites":
				suites = strings.Fields(v)
			case "enabled":
				enabled = !strings.EqualFold(strings.TrimSpace(v), "no")
			}
		}
		for _, u := range uris {
			if pgdgHost(u) {
				out = append(out, pgdgEntry{file: file, enabled: enabled, suites: suites})
				break
			}
		}
	}
	return out
}

var reStanzaGap = regexp.MustCompile(`\n[ \t]*\n`)

// pgdgOneLine reads the PGDG lines of a .list file, commented ones as off.
func pgdgOneLine(file string, b []byte) []pgdgEntry {
	var out []pgdgEntry
	for _, raw := range strings.Split(string(b), "\n") {
		on, uri, suite, ok := parseDebLine(raw)
		if ok && pgdgHost(uri) {
			out = append(out, pgdgEntry{file: file, enabled: on, suites: []string{suite}})
		}
	}
	return out
}

// parseDebLine reads "deb [opts] uri suite comps", commented or not
// (Argus's and the upgrader's marks included).
func parseDebLine(raw string) (on bool, uri, suite string, ok bool) {
	line := strings.TrimSpace(raw)
	on = true
	if strings.HasPrefix(line, "#") {
		on = false
		if i := strings.Index(line, ": deb"); strings.HasPrefix(line, disabledMark) && i > 0 {
			line = line[i+2:]
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
	}
	if c := strings.Index(line, " #"); c > 0 {
		line = line[:c]
	}
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != "deb" {
		return false, "", "", false
	}
	i := 1
	if strings.HasPrefix(f[1], "[") {
		for i < len(f) && !strings.HasSuffix(f[i], "]") {
			i++
		}
		i++
	}
	if i+1 >= len(f) {
		return false, "", "", false
	}
	return on, f[i], f[i+1], true
}

// pgdgSuiteFor moves a PGDG suite to this release: noble-pgdg becomes
// resolute-pgdg, noble-pgdg-testing resolute-pgdg-testing.
func pgdgSuiteFor(suite, codename string) string {
	if i := strings.Index(suite, "-pgdg"); i > 0 {
		return codename + suite[i:]
	}
	return codename + "-pgdg"
}

// fixPGDGDeb822 points the PGDG stanzas at this release and turns them on.
func fixPGDGDeb822(b []byte, codename string) []byte {
	stanzas := reStanzaGap.Split(string(b), -1)
	for si, st := range stanzas {
		if len(pgdgDeb822("", []byte(st))) == 0 {
			continue
		}
		lines := strings.Split(st, "\n")
		keep := lines[:0]
		for _, l := range lines {
			k, v, ok := strings.Cut(l, ":")
			if ok && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, " ") {
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "enabled":
					continue
				case "suites":
					s := strings.Fields(v)
					for i := range s {
						s[i] = pgdgSuiteFor(s[i], codename)
					}
					l = "Suites: " + strings.Join(s, " ")
				}
			}
			keep = append(keep, l)
		}
		stanzas[si] = strings.Join(keep, "\n")
	}
	return []byte(strings.Join(stanzas, "\n\n"))
}

// fixPGDGOneLine does the same to a .list file's PGDG lines.
func fixPGDGOneLine(b []byte, codename string) []byte {
	lines := strings.Split(string(b), "\n")
	for i, raw := range lines {
		_, uri, suite, ok := parseDebLine(raw)
		if !ok || !pgdgHost(uri) {
			continue
		}
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, disabledMark) {
			if j := strings.Index(line, ": deb"); j > 0 {
				line = line[j+2:]
			}
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		if c := strings.Index(line, " #"); c > 0 {
			line = line[:c]
		}
		f := strings.Fields(line)
		for k := range f {
			if f[k] == suite {
				f[k] = pgdgSuiteFor(suite, codename)
				break
			}
		}
		lines[i] = strings.Join(f, " ")
	}
	return []byte(strings.Join(lines, "\n"))
}
