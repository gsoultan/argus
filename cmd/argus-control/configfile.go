package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

/*
Where the configuration lives.

The packaged service may write to /var/lib/argus-control and nowhere else
(ProtectSystem=strict), and the configuration is now something it writes: first
by the setup page, then by the console's Settings page. So the packaged unit
reads /var/lib/argus-control/control.yaml, with its secrets and certificates beside it.

Not /var/lib/argus: that is the agent's, and the agent's unit makes it root's
whenever it starts, which on a host carrying both would lock the control plane
out of its own configuration.

A deployment from before that has its settings in /etc/argus/control.yaml.
That file goes on being read for as long as the managed one does not exist; the
first save from the console writes the managed one, and from then on it wins.
The package still installs /etc/argus/control.yaml, holding only comments, so a
package manager upgrading an edited one keeps it rather than renaming it away.
*/

const (
	packagedStateDir = "/var/lib/argus-control"
	legacyConfigPath = "/etc/argus/control.yaml"
)

// errNotSetUp is what a subcommand gets on a control plane nobody has set up.
var errNotSetUp = errors.New("argus-control is not set up yet: start it " +
	"(systemctl enable --now argus-control) and finish setup at https://<this host>:8080/setup")

// defaultConfigPath is the --config default: the managed file where the
// packaged layout exists, the working directory otherwise.
func defaultConfigPath() string {
	if runtime.GOOS == "linux" {
		if info, err := os.Stat(packagedStateDir); err == nil && info.IsDir() {
			return filepath.Join(packagedStateDir, "control.yaml")
		}
	}
	return "control.yaml"
}

// configLocation says which file to read, and where to write.
type configLocation struct {
	// Path is the file to read. Empty means there is none: run setup.
	Path string
	// Managed is where setup and the Settings page write. Its directory holds
	// the secrets and certificates they write too.
	Managed string
}

func (l configLocation) dataDir() string { return filepath.Dir(l.Managed) }
func (l configLocation) secretsDir() string {
	return filepath.Join(l.dataDir(), "secrets")
}
func (l configLocation) pki() tlsconfig.ManagedPKI {
	return tlsconfig.ManagedPKI{Dir: filepath.Join(l.dataDir(), "tls")}
}

// locateConfig finds the configuration for the --config path given.
func locateConfig(configured string) (configLocation, error) {
	loc := configLocation{Managed: configured}
	ok, err := hasSettings(configured)
	if err != nil {
		return loc, err
	}
	if ok {
		loc.Path = configured
		return loc, nil
	}
	if filepath.Clean(configured) == filepath.Join(packagedStateDir, "control.yaml") {
		if ok, err := hasSettings(legacyConfigPath); err != nil {
			return loc, err
		} else if ok {
			loc.Path = legacyConfigPath
		}
	}
	return loc, nil
}

// hasSettings reports whether path holds any setting. A missing file, an
// empty one and one of only comments -- what the package installs -- hold
// none. A file that does not parse counts as settings, so loading it reports
// the mistake rather than setup quietly starting over.
func hasSettings(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read config %s: %w", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return true, nil
	}
	return len(m) > 0, nil
}

// commandConfig is the file a subcommand reads.
func commandConfig(configured string) (string, error) {
	loc, err := locateConfig(configured)
	if err != nil {
		return "", err
	}
	if loc.Path == "" {
		return "", errNotSetUp
	}
	return loc.Path, nil
}

const configHeader = `# Argus control plane.
#
# Written by argus-control: by the setup page on first run, and by the
# console's Settings page since. Change settings there; an edit made here is
# kept, but the next save from the console rewrites this file without its
# comments. Secrets are not in this file: each is in its own file under
# secrets/ beside it, readable by the argus user alone.
`

// writeConfig replaces the configuration file in one step, so a crash
// mid-write leaves the previous file rather than half of a new one.
func writeConfig(path string, cfg config) error {
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, append([]byte(configHeader+"\n"), body...), 0o600)
}

// readRawConfig reads the file without resolving ${...} references, so a
// save writes them back as references.
func readRawConfig(path string) (config, error) {
	var cfg config
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = yaml.Unmarshal(data, &cfg)
	return cfg, err
}

// writeSecret puts one secret in its own owner-only file and returns the
// reference the configuration holds instead.
func writeSecret(dir, name, value string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := writeAtomic(path, []byte(value+"\n"), 0o600); err != nil {
		return "", err
	}
	return "${file:" + path + "}", nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// randomSecret is n random bytes, base64 without padding.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writable reports whether this process can create files in dir.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".argus-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// detectConsoleDir finds a built console: where the package installs it,
// then beside the binary and beside the configuration, where the macOS and
// Windows archives unpack it.
func detectConsoleDir(configPath string) string {
	candidates := []string{"/usr/share/argus/console"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "console"))
	}
	candidates = append(candidates,
		filepath.Join(filepath.Dir(configPath), "console"),
		filepath.Join(filepath.Dir(configPath), "..", "console"))
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "index.html")); err == nil {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}
