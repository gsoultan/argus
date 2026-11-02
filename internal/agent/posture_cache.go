package agent

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

/*
The heartbeat carries posture, and used to get it by scanning the host every
30 seconds: sshd_config, passwd, shadow, and every key in every account's
authorized_keys parsed and fingerprinted. On a host with fifty accounts that is
a millisecond of CPU and 400 KB of garbage, twice a minute, to learn on almost
every beat that nothing changed.

PostureCache answers from the last scan while nothing the scan read has
changed, and checks that by stat-ing the same files: the sshd configuration and
every file it includes, passwd, shadow, each account's .ssh directory and both
authorized_keys files. A key added, removed or edited changes a size or a
modification time, and a file created or renamed into .ssh changes the
directory's, so the heartbeat still reports it on the next beat -- as quickly
as the full scan did. MaxAge bounds what a stat cannot see (accounts read from
a directory service rather than a file), after which it scans regardless.
*/

// PostureCache shares one posture scan between the callers that need it.
type PostureCache struct {
	cfg    ScanConfig
	maxAge time.Duration
	now    func() time.Time

	mu    sync.Mutex
	have  bool
	p     Posture
	err   error
	at    time.Time
	stamp []byte
	// paths are the files a scan reads beyond sshd's: passwd, shadow, and
	// each account's .ssh and key files, worked out once per scan.
	paths []string
	scans int
}

// NewPostureCache scans with cfg. A zero maxAge means five minutes.
func NewPostureCache(cfg ScanConfig, maxAge time.Duration) *PostureCache {
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	return &PostureCache{cfg: cfg, maxAge: maxAge, now: time.Now}
}

// Scan returns the host's posture, from the last scan when nothing it read
// has changed since.
func (c *PostureCache) Scan() (Posture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.have && now.Sub(c.at) < c.maxAge && bytes.Equal(c.inputsLocked(), c.stamp) {
		p := c.p
		// Checked now: every input was seen unchanged at this moment.
		p.CheckedAt = now.UTC()
		return p, c.err
	}
	return c.rescanLocked(now)
}

// Rescan scans the host whatever the cache holds.
func (c *PostureCache) Rescan() (Posture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rescanLocked(c.now())
}

// Scans reports how many full scans have run.
func (c *PostureCache) Scans() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scans
}

func (c *PostureCache) rescanLocked(now time.Time) (Posture, error) {
	// The homes come first and the stamp after, both before the scan reads
	// anything: a change landing mid-scan then differs from the stamp, and
	// the next call scans again rather than keeping a half-old answer.
	c.paths = scanPaths(c.cfg)
	c.stamp = c.inputsLocked()
	c.p, c.err = Scan(c.cfg)
	c.at, c.have = now, true
	c.scans++
	return c.p, c.err
}

// inputsLocked fingerprints every file a scan reads.
func (c *PostureCache) inputsLocked() []byte {
	b := make([]byte, 0, len(c.stamp))
	stat := func(path string) {
		b = append(b, path...)
		b = append(b, 0)
		fi, err := os.Stat(path)
		if err != nil {
			b = append(b, 'x')
			return
		}
		b = binary.LittleEndian.AppendUint64(b, uint64(fi.Size()))
		b = binary.LittleEndian.AppendUint64(b, uint64(fi.ModTime().UnixNano()))
		b = binary.LittleEndian.AppendUint32(b, uint32(fi.Mode()))
	}
	for _, f := range sshdConfigFiles(c.cfg.SSHDConfig) {
		stat(f)
	}
	for _, p := range c.paths {
		stat(p)
	}
	return b
}

// scanPaths lists the files beyond sshd's that a scan reads.
func scanPaths(cfg ScanConfig) []string {
	out := []string{cfg.PasswdFile, cfg.ShadowFile}
	accounts, _ := readAccounts(cfg.PasswdFile)
	for _, a := range accounts {
		ssh := filepath.Join(a.home, ".ssh")
		out = append(out, ssh, filepath.Join(ssh, "authorized_keys"), filepath.Join(ssh, "authorized_keys2"))
	}
	if adminKeysFile != "" {
		out = append(out, adminKeysFile)
	}
	return out
}

// sshdConfigFiles is path and every file it includes, as readSSHDSettings
// follows them, in the order it would read them.
func sshdConfigFiles(path string) []string {
	var out []string
	var walk func(string, int)
	walk = func(p string, depth int) {
		if depth > 8 {
			return
		}
		out = append(out, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			i := strings.IndexAny(line, " \t=")
			if i < 0 || !strings.EqualFold(line[:i], "include") {
				continue
			}
			for _, pattern := range strings.Fields(strings.Trim(strings.TrimLeft(line[i:], " \t="), `"`)) {
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join(filepath.Dir(sshdConfigPath), pattern)
				}
				// The directory too, so a file dropped into sshd_config.d
				// is seen even before it is listed.
				out = append(out, filepath.Dir(pattern))
				matches, _ := filepath.Glob(pattern)
				sort.Strings(matches)
				for _, m := range matches {
					walk(m, depth+1)
				}
			}
		}
	}
	walk(path, 0)
	return out
}
