package tunnel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

/*
The software service: what is installed on a Linux server, what its own
repositories offer, and what installing or removing something would change.

	argus-control ──"software" stream──▶ agent ──▶ argus-agent software-query (root, own unit)

One stream per question, like dbdiscover. After the header and the answer the
agent sends one 'z' SoftwareResult (JSON) and closes. Nothing is installed or
removed here: a preview is the package manager's own simulation (apt-get -s,
dnf --assumeno, zypper --dry-run), and the install or removal it describes
rides the heartbeat as a package run, which simulates again and stops if the
answer changed. An old agent refuses the service as unknown, which the console
turns into "update the agent".
*/

// SoftwareService is the tunnel service name.
const SoftwareService = "software"

// FrameSoftwareResult carries the one SoftwareResult.
const FrameSoftwareResult byte = 'z'

// Software questions.
const (
	SoftwareInstalled = "installed"
	SoftwareSearch    = "search"
	SoftwarePreview   = "preview"
)

// Software actions a preview or a run can be for.
const (
	SoftwareInstall = "install"
	SoftwareRemove  = "remove"
)

// Bounds on one answer.
const (
	MaxSoftwareInstalled = 5000
	MaxSoftwareFound     = 50
	MaxSoftwareItems     = 50
	MaxSoftwareVersions  = 20
)

// SoftwareItem is one package a person picked, with the version they picked
// ("" is the newest the repositories have).
type SoftwareItem struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Label is what the console shows for it, on Windows where Name is an
	// id; never passed to anything that runs.
	Label string `json:"label,omitempty"`
}

// ManagerWindows is the "package manager" a Windows server answers with:
// the Uninstall keys of its registry.
const ManagerWindows = "windows"

// maxLabel bounds a label.
const maxLabel = 200

// GrantSoftware is the question a software grant may ask; nil on every
// other grant.
type GrantSoftware struct {
	Op string `json:"op"`
	// Query is what to search for.
	Query string `json:"query,omitempty"`
	// All lists libraries and dependencies too, not only what someone asked
	// for.
	All bool `json:"all,omitempty"`
	// Action, Items and Purge are what to preview.
	Action string         `json:"action,omitempty"`
	Items  []SoftwareItem `json:"items,omitempty"`
	Purge  bool           `json:"purge,omitempty"`
}

// rePkg is a package name as apt, dnf and zypper spell them, with an
// optional architecture; the agent's own check is the same.
var rePkg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,199}$`)

// rePkgVersion is a version as the three spell them: epoch, upstream,
// release, with no space and nothing a shell or an option parser would read.
var rePkgVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,127}$`)

// reSoftwareQuery is a search: words, no options.
var reSoftwareQuery = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._ -]{0,63}$`)

// ValidPackageName reports whether s is a package name the agent will pass
// to a package manager.
func ValidPackageName(s string) bool { return rePkg.MatchString(s) }

// ValidPackageVersion reports whether s is a version the agent will pass to
// a package manager.
func ValidPackageVersion(s string) bool { return rePkgVersion.MatchString(s) }

// ValidSoftwareQuery reports whether s is a search the agent will run.
func ValidSoftwareQuery(s string) bool { return reSoftwareQuery.MatchString(strings.TrimSpace(s)) }

// Validate refuses a question that could carry anything but names.
func (g *GrantSoftware) Validate() error {
	switch g.Op {
	case SoftwareInstalled:
		return nil
	case SoftwareSearch:
		if !ValidSoftwareQuery(g.Query) {
			return errors.New("search for a package name: letters, digits and . _ + - only")
		}
		return nil
	case SoftwarePreview:
		return ValidateSoftwareChange(g.Action, g.Items, g.Purge)
	}
	return errors.New("unknown question " + g.Op)
}

// ValidateSoftwareChange checks an install or a removal before anything
// runs it: names and versions only, never an option.
func ValidateSoftwareChange(action string, items []SoftwareItem, purge bool) error {
	switch action {
	case SoftwareInstall:
		if purge {
			return errors.New("purge is for removing")
		}
	case SoftwareRemove:
	default:
		return errors.New("say whether to install or remove")
	}
	if len(items) == 0 {
		return errors.New("pick at least one package")
	}
	if len(items) > MaxSoftwareItems {
		return fmt.Errorf("pick at most %d packages at a time", MaxSoftwareItems)
	}
	seen := map[string]bool{}
	for _, it := range items {
		if !rePkg.MatchString(it.Name) {
			return fmt.Errorf("%q is not a package name", it.Name)
		}
		if seen[it.Name] {
			return fmt.Errorf("%s is named twice", it.Name)
		}
		seen[it.Name] = true
		if len(it.Label) > maxLabel || strings.ContainsAny(it.Label, "\x00\r\n") {
			return fmt.Errorf("the label of %s cannot be shown", it.Name)
		}
		if it.Version != "" {
			if action == SoftwareRemove {
				return errors.New("a removal takes no version")
			}
			if !rePkgVersion.MatchString(it.Version) {
				return fmt.Errorf("%q is not a package version", it.Version)
			}
		}
	}
	return nil
}

// InstalledPackage is one package on the server.
type InstalledPackage struct {
	Name string `json:"name"`
	// Label is the program's name on Windows, where Name is an id.
	Label   string `json:"label,omitempty"`
	Version string `json:"version"`
	Arch    string `json:"arch,omitempty"`
	// Origin is the repository the installed version came from, when the
	// package manager can say.
	Origin  string `json:"origin,omitempty"`
	Summary string `json:"summary,omitempty"`
	SizeKB  int64  `json:"size_kb,omitempty"`
	// InstalledAt is a Unix time, 0 when not known.
	InstalledAt int64 `json:"installed_at,omitempty"`
	// Manual is someone asking for it, rather than it being pulled in.
	Manual bool `json:"manual"`
	// Protected is why the console must not offer to remove it; "" when it
	// may.
	Protected string `json:"protected,omitempty"`
	// KeepsData is a package whose purge can delete data (a database
	// server), so purge is never offered for it.
	KeepsData bool `json:"keeps_data,omitempty"`
}

// FoundPackage is one search result.
type FoundPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Origin  string `json:"origin,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Installed is the version on the server, "" when it is not installed.
	Installed string `json:"installed,omitempty"`
	// Versions are what the repositories offer, newest first.
	Versions []string `json:"versions,omitempty"`
}

// Software change kinds.
const (
	ChangeAdd       = "add"
	ChangeUpgrade   = "upgrade"
	ChangeDowngrade = "downgrade"
	ChangeRemove    = "remove"
)

// SoftwareChange is one package a simulation says would change.
type SoftwareChange struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	// Asked is one of the packages the person picked; the rest come with
	// them.
	Asked bool `json:"asked,omitempty"`
}

// SoftwarePreviewResult is what installing or removing would change.
type SoftwarePreviewResult struct {
	Action  string           `json:"action"`
	Items   []SoftwareItem   `json:"items"`
	Purge   bool             `json:"purge,omitempty"`
	Changes []SoftwareChange `json:"changes"`
	// DownloadKB and DiskKB are what the package manager said, 0 when it
	// did not; DiskKB is negative for space freed.
	DownloadKB int64 `json:"download_kb,omitempty"`
	DiskKB     int64 `json:"disk_kb,omitempty"`
	// Blocked is why this cannot be done; "" when it can.
	Blocked string `json:"blocked,omitempty"`
	// Warnings are things a person should know before saying yes.
	Warnings []string `json:"warnings,omitempty"`
	// Services are the systemd services the packages bring (install) or
	// that stop (remove).
	Services []string `json:"services,omitempty"`
	// PurgeAllowed is false when a package removed keeps data.
	PurgeAllowed bool `json:"purge_allowed"`
	// Hash names Changes, so a run can tell whether the server still
	// answers the same.
	Hash string `json:"hash"`
}

// SoftwareResult is the agent's answer.
type SoftwareResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Manager string `json:"manager,omitempty"`
	// Installed answers "installed"; Total is how many there are in all,
	// Manual how many someone asked for.
	Installed []InstalledPackage `json:"installed,omitempty"`
	Total     int                `json:"total,omitempty"`
	Manual    int                `json:"manual,omitempty"`
	// Found answers "search".
	Found []FoundPackage `json:"found,omitempty"`
	// Preview answers "preview".
	Preview   *SoftwarePreviewResult `json:"preview,omitempty"`
	Truncated bool                   `json:"truncated,omitempty"`
}
