package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Inventory resolves target names to assets.
type Inventory struct {
	assets map[string]Asset
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

	inv := &Inventory{assets: make(map[string]Asset, len(assets)*2)}
	for _, a := range assets {
		inv.assets[a.Hostname] = a
		// Also index the short name, so `ssh ops:pay-01@gw` works as well as
		// the fully qualified form.
		if short, _, found := strings.Cut(a.Hostname, "."); found {
			if _, clash := inv.assets[short]; !clash {
				inv.assets[short] = a
			}
		}
	}
	return inv, nil
}

// Resolve looks up a target by hostname or short name.
func (i *Inventory) Resolve(target string) (Asset, error) {
	a, ok := i.assets[target]
	if !ok {
		// Deliberately does not list known hosts: an unauthenticated-ish error
		// path should not enumerate the fleet for someone probing it.
		return Asset{}, fmt.Errorf("unknown target %q", target)
	}
	return a, nil
}

// Unique returns each asset once, since the map indexes both the full and
// short hostname.
func (i *Inventory) Unique() []Asset {
	seen := map[string]bool{}
	var out []Asset
	for _, a := range i.assets {
		if seen[a.Hostname] {
			continue
		}
		seen[a.Hostname] = true
		out = append(out, a)
	}
	return out
}

// Count reports how many distinct assets are loaded.
func (i *Inventory) Count() int {
	seen := map[string]struct{}{}
	for _, a := range i.assets {
		seen[a.Hostname] = struct{}{}
	}
	return len(seen)
}
