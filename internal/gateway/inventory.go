package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Asset is a host Argus can broker a session to.
//
// This mirrors the shape the control plane will serve; for the spike it is
// loaded from a file so the gateway can run standalone.
type Asset struct {
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	// Principals the gateway is allowed to open on this host. A principal not
	// in this list is refused even if the credential would work — the
	// inventory, not the target's /etc/passwd, is the authority.
	Principals []string `json:"principals"`
	// KeyPath is the vaulted private key the gateway injects. The user never
	// sees it; that is the whole point of injection over brokering.
	//
	// Ignored when CredentialMode is "ca-certificate" — there is no standing
	// credential in that mode, which is the entire advantage of it.
	KeyPath string `json:"key_path"`

	// CredentialRef names the credential in this gateway's vault directory, as
	// the control plane holds it: a name, never a path. It is resolved into
	// KeyPath or CredentialDir when an inventory is synced, so everything below
	// this line works the same whether the asset came from a file or the
	// console. An entry that tries to leave the vault is dropped, loudly.
	CredentialRef string `json:"credential_ref,omitempty"`

	// CredentialMode is "injected-key" (default) or "ca-certificate".
	//
	// Certificate mode requires TrustedUserCAKeys on the host and leaves
	// nothing behind: no authorized_keys entry, no secret to rotate, nothing
	// that outlives the session.
	CredentialMode string `json:"credential_mode"`

	// Domain is the Windows domain for an RDP asset. Empty means the account is
	// local to the host, which changes the NTLM hash — so an empty domain and
	// the host's own name are not interchangeable.
	Domain string `json:"domain,omitempty"`

	// CredentialDir holds one file per principal, named for it, containing the
	// password Argus delegates.
	//
	// A directory rather than a field in this file: the inventory is read by
	// anything that can read the gateway's config, and a password in it would
	// be a standing secret sitting in the one place everybody looks. Read at
	// connect time rather than at load, so rotating a credential takes effect
	// without restarting the gateway.
	CredentialDir string `json:"credential_dir,omitempty"`

	// Protocol is "ssh" (default) or "rdp".
	//
	// One inventory for both, rather than a second file. An asset is a machine
	// someone has privileged access to; which protocol reaches it is a property
	// of the machine, not a reason to track it twice — and a host that appears
	// in one inventory but not the other is exactly the coverage gap discovery
	// exists to surface.
	Protocol string `json:"protocol,omitempty"`
}

// Protocol names.
const (
	ProtocolSSH = "ssh"
	ProtocolRDP = "rdp"
)

// Proto returns the asset's protocol, defaulting to SSH.
func (a Asset) Proto() string {
	if a.Protocol == "" {
		return ProtocolSSH
	}
	return a.Protocol
}

// UsesCertificate reports whether this asset is on certificate auth.
func (a Asset) UsesCertificate() bool { return a.CredentialMode == "ca-certificate" }

// Addr returns the dial address.
func (a Asset) Addr() string {
	port := a.Port
	if port == 0 {
		// The default follows the protocol. Defaulting everything to 22 would
		// send an RDP session to a host's SSH port, where it fails with a
		// protocol error that says nothing about the real mistake.
		port = 22
		if a.Proto() == ProtocolRDP {
			port = 3389
		}
	}
	return fmt.Sprintf("%s:%d", a.Address, port)
}

// AllowsPrincipal reports whether principal may be assumed on this host.
func (a Asset) AllowsPrincipal(principal string) bool {
	for _, p := range a.Principals {
		if p == principal {
			return true
		}
	}
	return false
}

// Inventory resolves target names to assets, and people to what they may open.
//
// Swappable under a lock rather than replaced wholesale, so the control plane
// can update it while sessions are being opened: every caller holds a *Inventory
// for the life of the process and always reads the current contents. An
// interface field here would be worse, not better — a nil *Inventory inside a
// non-nil interface is the trap that made four storage guards go dead at once.
type Inventory struct {
	mu     sync.RWMutex
	assets map[string]Asset
	// assignments is hostname -> lowercased email -> principals.
	assignments map[string]map[string][]string
	// unrestricted are the accounts assignment does not gate: admins and
	// owners, as the control plane reports them. The console exempts them for
	// the same reason, and a gateway that did not would refuse on ssh(1) what
	// the browser allows.
	unrestricted map[string]bool
	// controlled means the control plane is the authority on this inventory.
	// Set when the gateway is configured to sync, not when a sync first
	// succeeds: otherwise an outage at start-up would silently downgrade the
	// gateway to "no assignments known, so nobody is restricted".
	controlled bool
	source     string
}

// LoadInventory reads assets from a JSON file.
//
// Relative key paths resolve against the inventory's directory so the gateway
// can be started from anywhere.
func LoadInventory(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inventory: %w", err)
	}
	var assets []Asset
	if err := json.Unmarshal(data, &assets); err != nil {
		return nil, fmt.Errorf("parse inventory %s: %w", path, err)
	}

	base := filepath.Dir(path)
	for i := range assets {
		if assets[i].KeyPath != "" && !filepath.IsAbs(assets[i].KeyPath) {
			assets[i].KeyPath = filepath.Join(base, assets[i].KeyPath)
		}
	}

	inv := &Inventory{source: "file"}
	inv.Replace(assets, nil, nil, "file")
	return inv, nil
}

// NewInventory returns an empty inventory for a gateway that will be told its
// contents by the control plane.
func NewInventory() *Inventory {
	inv := &Inventory{}
	inv.Replace(nil, nil, nil, "empty")
	return inv
}

// Replace swaps the whole inventory atomically.
//
// Assignments and unrestricted accounts arrive with the assets they belong to,
// because a set of assets and a set of assignments from different moments would
// be a third thing that was never true.
func (i *Inventory) Replace(
	assets []Asset, assignments map[string]map[string][]string,
	unrestricted []string, source string,
) {
	index := make(map[string]Asset, len(assets)*2)
	for _, a := range assets {
		index[a.Hostname] = a
		// Also index the short name, so `ssh ops:pay-01@gw` works as well as
		// the fully qualified form.
		if short, _, found := strings.Cut(a.Hostname, "."); found {
			if _, clash := index[short]; !clash {
				index[short] = a
			}
		}
	}
	exempt := make(map[string]bool, len(unrestricted))
	for _, e := range unrestricted {
		exempt[strings.ToLower(e)] = true
	}
	if assignments == nil {
		assignments = map[string]map[string][]string{}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.assets = index
	i.assignments = assignments
	i.unrestricted = exempt
	i.source = source
}

// UnderControlPlane marks this inventory as the control plane's to fill.
//
// From here on an unassigned session is refused rather than waved through, even
// before the first sync lands — see the comment on the field.
func (i *Inventory) UnderControlPlane() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.controlled = true
}

// EnforcesAssignment reports whether this gateway checks who a host was
// assigned to, as opposed to only which principals it permits.
func (i *Inventory) EnforcesAssignment() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.controlled
}

// Source says where the current contents came from, for the log line that tells
// an operator which inventory is in force.
func (i *Inventory) Source() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.source
}

// Unrestricted reports whether assignment does not gate this account.
func (i *Inventory) Unrestricted(email string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.unrestricted[strings.ToLower(email)]
}

// Resolve looks up a target by hostname or short name.
func (i *Inventory) Resolve(target string) (Asset, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()

	a, ok := i.assets[target]
	if !ok {
		// Deliberately does not list known hosts: an unauthenticated-ish error
		// path should not enumerate the fleet for someone probing it.
		return Asset{}, fmt.Errorf("unknown target %q", target)
	}
	return a, nil
}

// Unique returns each asset once, since the map indexes both the full and
// short hostname. Sorted, so a publish or a log line is stable between runs.
func (i *Inventory) Unique() []Asset {
	i.mu.RLock()
	defer i.mu.RUnlock()

	seen := map[string]bool{}
	var out []Asset
	for _, a := range i.assets {
		if seen[a.Hostname] {
			continue
		}
		seen[a.Hostname] = true
		out = append(out, a)
	}
	sort.Slice(out, func(x, y int) bool { return out[x].Hostname < out[y].Hostname })
	return out
}

// Count reports how many distinct assets are loaded.
func (i *Inventory) Count() int {
	i.mu.RLock()
	defer i.mu.RUnlock()

	seen := map[string]struct{}{}
	for _, a := range i.assets {
		seen[a.Hostname] = struct{}{}
	}
	return len(seen)
}

// Assigned reports whether this person was assigned this principal on this
// host, according to the last inventory that arrived.
//
// A false answer is not a refusal on its own: the caller asks the control plane,
// which also knows about approved access requests and is a minute fresher. This
// exists so the common case — an assigned person opening an assigned host —
// costs nothing.
func (i *Inventory) Assigned(hostname, email, principal string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	asset, ok := i.assets[hostname]
	if !ok {
		return false
	}
	// The assignment is keyed by the stored hostname, so a short-name target
	// has to be resolved to it first or `ssh ops:pay-01@gw` would never match
	// an assignment made against pay-01.example.com.
	for _, p := range i.assignments[asset.Hostname][strings.ToLower(email)] {
		if p == principal {
			// The asset's own list still bounds it: a principal removed from
			// the host must not survive in an assignment that still names it.
			return asset.AllowsPrincipal(principal)
		}
	}
	return false
}
