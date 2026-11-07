package tunnel

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

/*
Install scripts: what the control plane and the agent agree on.

A script someone wrote in the console runs as root on a server, so the agent
runs only a script a reviewer approved, exactly as they read it. On review
the control plane signs a ScriptApproval naming the SHA-256 of each part of
that version with the key agents already pin for session grants
(GrantKeyFromSecret). The run carries the token; the agent's helper checks
the signature against the pinned key and the text it was sent against the
hashes before anything runs. A changed database row or a relay in the middle
cannot slip a different script in.

The approval names content, not a server or a time: it is what the reviewer
said yes to, and running it on a server is a separate decision that travels
on the agent's authenticated heartbeat like any package run.
*/

// Script platforms, parts and limits.
const (
	ScriptLinux   = "linux"
	ScriptWindows = "windows"

	ScriptInstall   = "install"
	ScriptUninstall = "uninstall"
	ScriptCheck     = "check"

	// MaxScriptBytes bounds each part of a script.
	MaxScriptBytes = 256 << 10
	// MaxScriptParams bounds the parameters a script takes.
	MaxScriptParams = 20
	// MaxScriptTimeout bounds how long a run may take.
	MaxScriptTimeout = 2 * time.Hour
)

// ScriptApproval is what a reviewer approved, signed.
type ScriptApproval struct {
	// VersionID is the version's id; Script its script's.
	VersionID string `json:"v"`
	Script    string `json:"s"`
	Name      string `json:"name"`
	Version   int    `json:"n"`
	Platform  string `json:"p"`
	// InstallSHA, UninstallSHA and CheckSHA are the hex SHA-256 of each
	// part; "" for a part the version does not have.
	InstallSHA   string `json:"i"`
	UninstallSHA string `json:"u,omitempty"`
	CheckSHA     string `json:"c,omitempty"`
	// RunAs is the local account it runs as; "" is root (SYSTEM).
	RunAs      string `json:"r,omitempty"`
	TimeoutSec int    `json:"t"`
	// Params are the names of the parameters it takes; a run sets no other.
	Params     []string  `json:"a,omitempty"`
	ReviewedBy string    `json:"by"`
	ReviewedAt time.Time `json:"at"`
}

// SHAFor is the hash a part must have, and whether the version has it.
func (a ScriptApproval) SHAFor(part string) (string, bool) {
	switch part {
	case ScriptInstall:
		return a.InstallSHA, a.InstallSHA != ""
	case ScriptUninstall:
		return a.UninstallSHA, a.UninstallSHA != ""
	case ScriptCheck:
		return a.CheckSHA, a.CheckSHA != ""
	}
	return "", false
}

// ScriptHash is a part's SHA-256 in hex.
func ScriptHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// SignScript encodes a as payload.signature, the way grants are.
func SignScript(key ed25519.PrivateKey, a ScriptApproval) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("this control plane cannot sign scripts: set signing_secret")
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(key, []byte("argus script v1."+body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyScript checks token against key and returns what it approves. The
// signature covers a context string, so a session grant can never pass for
// a script approval nor the other way round.
func VerifyScript(key ed25519.PublicKey, token string) (ScriptApproval, error) {
	var a ScriptApproval
	if len(key) != ed25519.PublicKeySize {
		return a, errors.New("this agent does not know the control plane's key, so it cannot check the script")
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return a, errors.New("the script's approval is malformed")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(key, []byte("argus script v1."+body), raw) {
		return a, errors.New("the script's approval is not signed by the control plane")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(payload, &a) != nil || a.VersionID == "" || a.InstallSHA == "" {
		return a, errors.New("the script's approval is malformed")
	}
	return a, nil
}

var (
	reScriptParam = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
	reRunAs       = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
)

// ValidScriptParam says whether name can be a parameter: it becomes
// ARGUS_PARAM_<name> in the script's environment.
func ValidScriptParam(name string) bool { return reScriptParam.MatchString(name) }

// ValidRunAs says whether name can be the account a script runs as; ""
// is root.
func ValidRunAs(name string) bool { return name == "" || reRunAs.MatchString(name) }

// ScriptEnv is the environment variable a parameter is passed in.
func ScriptEnv(name string) string { return "ARGUS_PARAM_" + name }

// ValidateScriptValue refuses a parameter value the environment cannot
// carry intact.
func ValidateScriptValue(name, v string) error {
	if len(v) > 4096 {
		return fmt.Errorf("%s is longer than 4096 characters", name)
	}
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("%s cannot contain a line break", name)
	}
	return nil
}

// ScriptReport is what changed on a server while a script ran: Argus cannot
// know in advance what a script will do, so it says afterwards.
type ScriptReport struct {
	Exit int `json:"exit"`
	// Packages added, changed and removed, from the package database.
	Packages []SoftwareChange `json:"packages,omitempty"`
	// Services added and removed (systemd units).
	ServicesAdded   []string `json:"services_added,omitempty"`
	ServicesRemoved []string `json:"services_removed,omitempty"`
	// Ports something newly listens on, and ports no longer listened on.
	PortsOpened []int `json:"ports_opened,omitempty"`
	PortsClosed []int `json:"ports_closed,omitempty"`
	// Folders added and removed in /opt and /usr/local.
	FoldersAdded   []string `json:"folders_added,omitempty"`
	FoldersRemoved []string `json:"folders_removed,omitempty"`
	// Check is the check script's answer, when the version has one:
	// installed (exit 0) with the version it printed, or not.
	Checked      bool   `json:"checked,omitempty"`
	Installed    bool   `json:"installed,omitempty"`
	CheckVersion string `json:"check_version,omitempty"`
	CheckError   string `json:"check_error,omitempty"`
}
