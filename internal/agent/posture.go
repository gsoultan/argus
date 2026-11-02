package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Posture is a point-in-time report of how reachable this host is without
// going through Argus.
//
// Recording a bypass is the fallback. Removing the ability to have one is the
// actual goal, and that starts with knowing where the ways in are.
type Posture struct {
	Hostname  string    `json:"hostname"`
	CheckedAt time.Time `json:"checked_at"`

	// UnmanagedKeys are authorized_keys entries Argus did not issue. Each one
	// is a standing credential that can reach this host directly.
	UnmanagedKeys []UnmanagedKey `json:"unmanaged_keys"`

	// ShimInstalled reports whether sshd is configured to run the recorder.
	// False means direct sessions are not being captured at all.
	ShimInstalled bool `json:"shim_installed"`
	// TrustedCAConfigured reports whether certificate auth is wired up, which
	// is what allows standing keys to be removed entirely.
	TrustedCAConfigured bool `json:"trusted_ca_configured"`
	// PasswordAuthEnabled is a bypass route that needs no key at all.
	PasswordAuthEnabled bool `json:"password_auth_enabled"`

	// LoginAccounts is every account with a login shell, with whether it has
	// a password and how many keys, and whether sshd would let it in.
	LoginAccounts []LoginAccount `json:"login_accounts"`
	// SSHDAuth is how sshd lets people sign in.
	SSHDAuth SSHDAuth `json:"sshd_auth"`

	// Drift lists sshd_config settings that differ from what Argus expects.
	// A host whose config quietly changed is the most likely way for recording
	// to stop without anyone noticing.
	Drift []string `json:"drift"`
}

// UnmanagedKey is one authorized_keys entry Argus cannot account for.
type UnmanagedKey struct {
	User        string `json:"user"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment"`
	KeyType     string `json:"key_type"`
}

// Rating summarises the posture the way the console displays it.
func (p Posture) Rating() string {
	switch {
	case !p.ShimInstalled:
		// Nothing is watching. A direct session here leaves no trace at all.
		return "open"
	case len(p.UnmanagedKeys) == 0 && p.TrustedCAConfigured && !p.PasswordAuthEnabled:
		// No standing credential exists, so there is nothing to bypass with.
		return "enforced"
	default:
		// A bypass is possible, but the agent will record it.
		return "monitored"
	}
}

// ScanConfig points the scanner at the files it inspects.
type ScanConfig struct {
	// SSHDConfig is the sshd configuration to read.
	SSHDConfig string
	// PasswdFile lists the accounts whose authorized_keys are checked.
	PasswdFile string
	// ShadowFile says which accounts have a password; empty where there is
	// none to read.
	ShadowFile string
	// ManagedFingerprints are keys Argus issued; anything else is unmanaged.
	ManagedFingerprints map[string]bool
	// ShimPath is the recorder binary that ForceCommand must point at.
	ShimPath string
}

// DefaultScanConfig returns production paths.
func DefaultScanConfig(shimPath string) ScanConfig {
	return ScanConfig{
		SSHDConfig:          sshdConfigPath,
		PasswdFile:          systemPasswd,
		ShadowFile:          systemShadow,
		ManagedFingerprints: map[string]bool{},
		ShimPath:            shimPath,
	}
}

// Scan inspects the host and reports how it can be reached.
func Scan(cfg ScanConfig) (Posture, error) {
	hostname, _ := os.Hostname()
	p := Posture{Hostname: hostname, CheckedAt: time.Now().UTC()}

	if err := scanSSHDConfig(&p, cfg); err != nil {
		// A missing or unreadable sshd_config is itself a finding: the agent
		// cannot confirm recording is configured, so assume it is not.
		p.Drift = append(p.Drift, fmt.Sprintf("cannot read %s: %v", cfg.SSHDConfig, err))
	}

	// Each authorized_keys file is parsed once and both readings -- the keys
	// Argus did not issue, and how many each account holds -- come from it.
	// They were two passes, and parsing a key is most of what a scan costs.
	memo := keyFiles{}
	keys, err := scanAuthorizedKeysWith(cfg, memo)
	if err != nil {
		return p, err
	}
	p.UnmanagedKeys = keys
	p.SSHDAuth = readSSHDAuth(cfg.SSHDConfig)
	p.LoginAccounts = scanLoginAccountsWith(cfg, p.SSHDAuth, memo)
	if p.ShimInstalled && !p.SSHDAuth.ExposeAuthInfo {
		p.Drift = append(p.Drift, "ExposeAuthInfo is off — a direct login is recorded, "+
			"but not whether it used a password or which key")
	}
	return p, nil
}

func scanSSHDConfig(p *Posture, cfg ScanConfig) error {
	f, err := os.Open(cfg.SSHDConfig)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)

		switch strings.ToLower(key) {
		case "forcecommand":
			// An empty ShimPath (Windows, where there is no recorder to
			// point at) is contained in every string, so it matches nothing.
			if (cfg.ShimPath != "" && strings.Contains(value, cfg.ShimPath)) ||
				strings.Contains(value, "argus") {
				p.ShimInstalled = true
			} else {
				// Someone replaced the recorder with something else. That is
				// not drift, that is an attempt to stop being recorded.
				p.Drift = append(p.Drift,
					fmt.Sprintf("ForceCommand points at %q, not the Argus recorder", value))
			}
		case "trustedusercakeys":
			p.TrustedCAConfigured = true
		case "passwordauthentication":
			if strings.EqualFold(value, "yes") {
				p.PasswordAuthEnabled = true
				p.Drift = append(p.Drift,
					"PasswordAuthentication is enabled — a bypass needs no key at all")
			}
		case "permitrootlogin":
			if strings.EqualFold(value, "yes") {
				p.Drift = append(p.Drift,
					"PermitRootLogin yes — root can connect directly, unattributable to a person")
			}
		}
	}
	return sc.Err()
}

// keyFiles holds each authorized_keys file one scan has parsed, every key in
// it whether Argus issued it or not, so a second reading of the same file in
// the same scan costs a map lookup.
type keyFiles map[string]keyFile

type keyFile struct {
	keys []UnmanagedKey
	err  error
}

func (m keyFiles) parse(path string) ([]UnmanagedKey, error) {
	if m == nil {
		return parseAuthorizedKeysFile(path, "", nil)
	}
	if f, ok := m[path]; ok {
		return f.keys, f.err
	}
	keys, err := parseAuthorizedKeysFile(path, "", nil)
	m[path] = keyFile{keys, err}
	return keys, err
}

// scanAuthorizedKeys walks every real account's authorized_keys.
func scanAuthorizedKeys(cfg ScanConfig) ([]UnmanagedKey, error) {
	return scanAuthorizedKeysWith(cfg, nil)
}

func scanAuthorizedKeysWith(cfg ScanConfig, memo keyFiles) ([]UnmanagedKey, error) {
	accounts, err := readAccounts(cfg.PasswdFile)
	if err != nil {
		return nil, err
	}

	var found []UnmanagedKey
	for _, acct := range accounts {
		for _, name := range []string{"authorized_keys", "authorized_keys2"} {
			path := filepath.Join(acct.home, ".ssh", name)
			keys, err := memo.parse(path)
			if err != nil {
				continue // absent or unreadable is normal for most accounts
			}
			for _, k := range keys {
				if cfg.ManagedFingerprints[k.Fingerprint] {
					continue
				}
				k.User = acct.name
				found = append(found, k)
			}
		}
	}
	// Windows' OpenSSH reads administrators' keys from one shared file, not
	// their homes.
	if adminKeysFile != "" {
		if keys, err := parseAuthorizedKeysFile(adminKeysFile, "Administrators",
			cfg.ManagedFingerprints); err == nil {
			found = append(found, keys...)
		}
	}
	return found, nil
}

func parseAuthorizedKeysFile(path, username string, managed map[string]bool) ([]UnmanagedKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var out []UnmanagedKey
	lineNo := 0
	for len(data) > 0 {
		lineNo++
		pub, comment, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		data = rest

		fp := ssh.FingerprintSHA256(pub)
		if managed[fp] {
			continue
		}
		out = append(out, UnmanagedKey{
			User:        username,
			File:        path,
			Line:        lineNo,
			Fingerprint: fp,
			Comment:     comment,
			KeyType:     pub.Type(),
		})
	}
	return out, nil
}

type account struct {
	name string
	home string
}

// readAccounts returns login-capable accounts; see openPasswd.
func readAccounts(passwdFile string) ([]account, error) {
	f, err := openPasswd(passwdFile)
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	defer f.Close()

	var out []account
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := passwdFields(sc.Text())
		if len(fields) < 7 {
			continue
		}
		shell := fields[6]
		// Accounts with a nologin shell cannot open a session, so their keys
		// are not a bypass route worth reporting.
		if strings.HasSuffix(shell, "nologin") || strings.HasSuffix(shell, "/false") {
			continue
		}
		if fields[5] == "" {
			continue
		}
		out = append(out, account{name: fields[0], home: fields[5]})
	}
	return out, sc.Err()
}
