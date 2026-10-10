package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/reporter"
)

/*
Keeping the inventory current with the control plane.

An administrator adds a host in the console and assigns people to it; this is
how that reaches the gateway that has to broker it. Same shape as SyncPolicy,
and for the same reason: the control plane is where the decision is made, the
gateway is where it is applied, and the two must not be able to disagree for
longer than one interval.

Two rules the rest of this file exists to serve:

  - A control plane that cannot be reached never changes what this gateway
    permits. A failed fetch keeps the inventory already in force; it does not
    empty it, and emptying it would read as "every asset was deleted".

  - A credential is named, never pathed. The control plane sends a name and this
    file resolves it inside the gateway's own vault directory. An administrator
    with the console cannot make the gateway read a file outside it, which is
    the difference between "add an asset" and "read anything the daemon can".
*/

// InventorySyncInterval is how often the gateway re-reads the inventory.
//
// A minute, matching policy. An asset added in the console is reachable within
// one interval, which the console says out loud rather than implying it is
// instant.
const InventorySyncInterval = time.Minute

// InventoryFetcher is the part of the reporter client this needs.
//
// An interface so the sync can be tested without a control plane, and narrow
// enough that a test double is three lines.
type InventoryFetcher interface {
	Inventory(ctx context.Context) (*reporter.Inventory, error)
	Enabled() bool
}

// SyncInventory keeps an inventory current with the control plane.
//
// Blocks until ctx is done, so callers run it in a goroutine.
func SyncInventory(
	ctx context.Context, inv *Inventory, rep InventoryFetcher,
	vaultDir, cachePath string, log *slog.Logger,
) {
	if inv == nil || rep == nil || !rep.Enabled() {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	fetch := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		got, err := rep.Inventory(reqCtx)
		if err != nil {
			log.Warn("inventory fetch failed — keeping the inventory in force",
				"error", err, "source", inv.Source(), "assets", inv.Count())
			return
		}
		applyInventory(inv, *got, vaultDir, "control plane", log)
		if cachePath != "" {
			if err := writeInventoryCache(cachePath, *got); err != nil {
				// Not fatal: the running gateway is correct either way. It
				// matters at the *next* start-up, which is exactly when nobody
				// is watching, so it is a warning now rather than a surprise
				// then.
				log.Warn("could not cache the inventory for the next start-up",
					"path", cachePath, "error", err)
			}
		}
	}

	fetch()
	t := time.NewTicker(InventorySyncInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fetch()
		case <-ctx.Done():
			return
		}
	}
}

// applyInventory turns what the control plane sent into what the gateway uses.
//
// Assets whose credential cannot be resolved safely are dropped rather than
// loaded without one: an asset with no credential fails at dial time with an
// error about the target, which is the wrong place to learn that a vault entry
// is missing.
func applyInventory(
	inv *Inventory, in reporter.Inventory, vaultDir, source string, log *slog.Logger,
) {
	assets := make([]Asset, 0, len(in.Assets))
	assignments := map[string]map[string][]string{}

	for _, a := range in.Assets {
		asset := Asset{
			Hostname:       a.Hostname,
			Address:        a.Address,
			Port:           a.Port,
			Principals:     a.Principals,
			CredentialMode: a.CredentialMode,
			CredentialRef:  a.CredentialRef,
			Domain:         a.Domain,
			Protocol:       a.Protocol,
		}

		if asset.CredentialRef != "" {
			path, err := vaultPath(vaultDir, asset.CredentialRef)
			if err != nil {
				log.Error("refusing an asset whose credential is not in the vault",
					"hostname", a.Hostname, "credential", a.CredentialRef, "error", err)
				continue
			}
			// One name, resolved by protocol: SSH injects a key file, Remote
			// Desktop reads a directory holding one password per principal.
			if asset.Proto() == ProtocolRDP {
				asset.CredentialDir = path
			} else {
				asset.KeyPath = path
			}
		}

		assets = append(assets, asset)
		if len(a.Assignments) == 0 {
			continue
		}
		byEmail := make(map[string][]string, len(a.Assignments))
		for _, g := range a.Assignments {
			byEmail[strings.ToLower(g.Email)] = g.Principals
		}
		assignments[a.Hostname] = byEmail
	}

	before := inv.Count()
	inv.Replace(assets, assignments, in.Unrestricted, source)

	// Logged at warn when the fleet changes size, because an inventory that
	// shrinks is the event someone reconstructing "why can nobody reach
	// anything" needs to find, and it happens on the gateway rather than where
	// the administrator pressed the button.
	if before != len(assets) {
		log.Warn("inventory changed", "source", source,
			"assets", len(assets), "previously", before)
	} else {
		log.Info("inventory synced", "source", source, "assets", len(assets))
	}
}

// vaultPath resolves a credential name inside the vault directory.
//
// The name is checked rather than cleaned. Cleaning a hostile path produces
// something valid and wrong; refusing it produces a log line naming the asset.
func vaultPath(vaultDir, ref string) (string, error) {
	if vaultDir == "" {
		return "", fmt.Errorf("no vault directory is configured, so %q cannot be resolved; "+
			"set `vault_dir` in the gateway config", ref)
	}
	if ref == "" || ref == "." || ref == ".." ||
		strings.ContainsAny(ref, `/\`) || strings.Contains(ref, "..") ||
		strings.ContainsRune(ref, os.PathSeparator) {
		return "", fmt.Errorf("%q is not a credential name", ref)
	}
	return filepath.Join(vaultDir, ref), nil
}

/* ── Surviving a restart ─────────────────────────────────────────────────── */

// writeInventoryCache stores the last inventory the control plane served.
//
// Written 0600 next to the gateway's other state. It holds hostnames, account
// names and the addresses of managed hosts — no credential material — but it is
// still a map of the fleet, so it is not world-readable.
func writeInventoryCache(path string, inv reporter.Inventory) error {
	data, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// Renamed into place so a gateway that dies mid-write reads the previous
	// inventory at the next start rather than half of this one.
	return os.Rename(tmp, path)
}

// LoadInventoryCache restores the last synced inventory at start-up.
//
// This is what makes a restart during a control-plane outage safe. Without it a
// gateway that came back while the control plane was down would know of no
// assignments at all, and every session would have to be refused — an outage
// would become a lockout rather than a delay.
//
// A missing file is not an error: the first start after an upgrade has none.
func LoadInventoryCache(
	inv *Inventory, path, vaultDir string, log *slog.Logger,
) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && log != nil {
			log.Warn("could not read the cached inventory", "path", path, "error", err)
		}
		return false
	}
	var cached reporter.Inventory
	if err := json.Unmarshal(data, &cached); err != nil {
		if log != nil {
			log.Warn("the cached inventory is unreadable and was ignored",
				"path", path, "error", err)
		}
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	applyInventory(inv, cached, vaultDir, "cache", log)
	log.Warn("started from a cached inventory",
		"path", path, "generatedAt", cached.GeneratedAt, "assets", inv.Count(),
		"detail", "this is what the control plane last served; it is replaced by the first successful sync")
	return true
}
