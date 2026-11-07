package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// What is never cleaned, and where a rule's extra path may point. Both are
// checked by the scan, which leaves protected files off every list, and
// again by the helper on every file, which never trusts a list.

// linuxProtected are path prefixes under which nothing is cleaned: login
// and audit records, sudo's own logs, and Argus's. A prefix matches the
// rotated copies too: /var/log/auth.log covers auth.log.1 and auth.log.2.gz.
var linuxProtected = []struct{ prefix, group string }{
	{"/var/log/auth.log", "login"},
	{"/var/log/secure", "login"},
	{"/var/log/wtmp", "login"},
	{"/var/log/btmp", "login"},
	{"/var/log/lastlog", "login"},
	{"/var/log/faillog", "login"},
	{"/var/log/tallylog", "login"},
	{"/var/log/sudo", "login"},
	{"/var/log/audit/", "audit"},
	{"/var/log/argus", "argus"},
	{"/var/lib/argus/", "argus"},
	{"/run/argus/", "argus"},
}

// protectedLinux says why p must not be cleaned, or "" when it may be.
// stateDir is the agent's own; extra are the console's added globs.
func protectedLinux(p, stateDir string, extra []string) string {
	p = filepath.Clean(p)
	for _, x := range linuxProtected {
		if strings.HasPrefix(p, x.prefix) || p+"/" == x.prefix {
			return protectedWhy(x.group)
		}
	}
	if stateDir != "" {
		sd := filepath.Clean(stateDir)
		if p == sd || strings.HasPrefix(p, sd+"/") {
			return protectedWhy("argus")
		}
	}
	if matchesGlobOrParent(p, extra) {
		return "kept by the console's Never clean list"
	}
	return ""
}

func protectedWhy(group string) string {
	switch group {
	case "login":
		return "a login record, kept as evidence"
	case "audit":
		return "the audit log, kept as evidence"
	}
	return "Argus's own files"
}

// matchesGlobOrParent reports whether p, or a folder above it, matches one
// of globs.
func matchesGlobOrParent(p string, globs []string) bool {
	for _, g := range globs {
		for q := p; ; {
			if ok, _ := filepath.Match(g, q); ok {
				return true
			}
			parent := filepath.Dir(q)
			if parent == q {
				break
			}
			q = parent
		}
	}
	return false
}

var (
	reLogGlobBad = regexp.MustCompile(`[\x00-\x1f]`)
	// linuxGlobDenied are trees a rule's extra path may never reach into,
	// whatever its name says.
	linuxGlobDenied = []string{"/etc/", "/usr/", "/bin/", "/sbin/", "/lib", "/boot/", "/dev/", "/proc/",
		"/sys/", "/run/", "/root/", "/var/lib/", "/home/"}
	windowsGlobDenied = []string{`\windows\system32`, `\windows\syswow64`, `\programdata\argus`,
		`\windows\winsxs`, `\users\`}
)

// ValidLogGlob checks a rule's extra path, to clean or to protect. It must
// be absolute, say "log" somewhere, and stay out of system trees: Also clean
// is for an application's logs, not a way to delete anything on the server.
func ValidLogGlob(g string) error {
	if g == "" || len(g) > 300 || reLogGlobBad.MatchString(g) {
		return fmt.Errorf("%q is not a path", g)
	}
	if strings.Contains(g, "..") {
		return fmt.Errorf("%q: a path cannot go up with ..", g)
	}
	if _, err := filepath.Match(g, ""); err != nil {
		return fmt.Errorf("%q is not a valid pattern", g)
	}
	if !strings.Contains(strings.ToLower(g), "log") {
		return fmt.Errorf("%q: a log path says log somewhere in it", g)
	}
	switch {
	case strings.HasPrefix(g, "/"):
		if strings.Count(strings.TrimSuffix(g, "/"), "/") < 2 {
			return fmt.Errorf("%q is too near the top of the disk", g)
		}
		for _, d := range linuxGlobDenied {
			if strings.HasPrefix(g, d) {
				return fmt.Errorf("%q is in a system folder Argus never cleans", g)
			}
		}
	case len(g) > 3 && g[1] == ':' && (g[2] == '\\' || g[2] == '/'):
		low := strings.ToLower(strings.ReplaceAll(g, "/", `\`))
		if strings.Count(strings.TrimSuffix(low, `\`), `\`) < 2 {
			return fmt.Errorf("%q is too near the top of the disk", g)
		}
		for _, d := range windowsGlobDenied {
			if strings.Contains(low, d) {
				return fmt.Errorf("%q is in a system folder Argus never cleans", g)
			}
		}
	default:
		return fmt.Errorf("%q is not an absolute path", g)
	}
	return nil
}

// reRotated is a rotated log's name: logrotate's numbers and dates, and
// the compressions it uses.
var reRotated = regexp.MustCompile(`(\.[0-9]+(\.(gz|xz|bz2|zst|lz4))?|\.(gz|xz|bz2|zst|lz4|old)|-[0-9]{8}(\.(gz|xz|bz2|zst))?)$`)

func isRotated(name string) bool { return reRotated.MatchString(name) }

// stillAsScanned says why f is not the file the scan saw, or "".
func stillAsScanned(f LogFile, size int64, mod time.Time) string {
	if !mod.Equal(f.Modified) && mod.After(f.Modified) {
		return "it changed since the scan"
	}
	lo, hi := f.Bytes-f.Bytes/10, f.Bytes+f.Bytes/10
	if size < lo || size > hi {
		return "its size changed since the scan"
	}
	return ""
}

// fileSet gathers the files of an item cleaned by deleting them.
type fileSet struct{ item LogItem }

func newFileSet(key, kind, title, root string) *fileSet {
	return &fileSet{item: LogItem{Key: key, Kind: kind, Title: title, Root: root}}
}

func (s *fileSet) add(p string, fi interface {
	Size() int64
	ModTime() time.Time
}) {
	m := fi.ModTime().UTC()
	it := &s.item
	it.Bytes += fi.Size()
	it.Count++
	if it.Oldest == nil || m.Before(*it.Oldest) {
		it.Oldest = &m
	}
	if it.Newest == nil || m.After(*it.Newest) {
		it.Newest = &m
	}
	it.Files = append(it.Files, LogFile{Path: p, Bytes: fi.Size(), Modified: m})
}

// done sorts the files oldest first, the order a cleanup keeping N days
// takes them in, and bounds the list.
func (s *fileSet) done() LogItem {
	it := s.item
	sort.Slice(it.Files, func(i, j int) bool { return it.Files[i].Modified.Before(it.Files[j].Modified) })
	if len(it.Files) > maxLogFilesPerItem {
		it.More = len(it.Files) - maxLogFilesPerItem
		it.Files = it.Files[:maxLogFilesPerItem]
	}
	return it
}

// boundReport keeps a report small enough for a heartbeat: at most
// maxLogItems items and maxLogFilesTotal files listed across them.
func boundReport(r *LogReport) {
	if len(r.Items) > maxLogItems {
		r.Items = r.Items[:maxLogItems]
	}
	left := maxLogFilesTotal
	for i := range r.Items {
		it := &r.Items[i]
		if len(it.Files) > left {
			it.More += len(it.Files) - left
			it.Files = it.Files[:left]
		}
		left -= len(it.Files)
	}
}
