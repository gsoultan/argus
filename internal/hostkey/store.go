// Package hostkey pins and verifies the identity of target hosts.
//
// This is the single most important control in the gateway. Argus terminates
// the client's SSH connection and opens its own to the target, which means it
// is a deliberate man-in-the-middle. The client trusts the gateway's host key,
// so the client can no longer detect a MITM between the gateway and the target
// — only the gateway can. If it does not verify the target's key, nobody does,
// and the bastion becomes the easiest interception point in the network.
//
// Homegrown bastions get this wrong more often than any other detail, usually
// by passing ssh.InsecureIgnoreHostKey() to get things working and never
// coming back to it.
package hostkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ErrMismatch is returned when a host presents a key that differs from its pin.
// Callers must treat this as fatal for the connection: it is either a rebuilt
// host or an active interception, and the gateway cannot tell which.
var ErrMismatch = errors.New("host key does not match the pinned fingerprint")

// ErrUnpinned is returned in strict mode when a host has no pin yet.
var ErrUnpinned = errors.New("host key is not pinned")

// Pin records the accepted identity of one target.
type Pin struct {
	Host string `json:"host"`
	// Fingerprint is the OpenSSH SHA256 form, e.g. "SHA256:abc…". Stored rather
	// than the raw key so it can be compared by eye against what an operator
	// sees from console access.
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"key_type"`
	PinnedAt    time.Time `json:"pinned_at"`
	// PinnedBy records who vouched for it — "tofu" when accepted automatically
	// on first contact, otherwise an operator identity.
	PinnedBy string `json:"pinned_by"`
}

// Remote is shared pin storage, so every gateway verifies a target against the
// same recorded identity.
//
// Without it each gateway keeps its own file: a second gateway starts with no
// pins, and under trust-on-first-use it silently accepts a host the first would
// refuse. The strongest control in the product degrades to nothing on the
// second node, with no error to notice.
type Remote interface {
	// Pin returns the recorded pin, or nil when the host has never been pinned.
	Pin(ctx context.Context, host string) (*Pin, error)
	// Record stores a first-contact pin. It must refuse rather than overwrite
	// when a different fingerprint is already recorded.
	Record(ctx context.Context, p Pin) error
}

// Store is a persistent set of host key pins.
//
// The zero value is not usable; call Open.
type Store struct {
	mu   sync.RWMutex
	path string
	pins map[string]Pin

	// TOFU accepts an unpinned host's key on first contact and records it.
	// This is a real, if modest, security property: it protects every
	// subsequent connection, and matches what OpenSSH itself does by default.
	// It does NOT protect the first one, so production fleets should pin out
	// of band and run with TOFU off.
	TOFU bool

	// remote, when set, is authoritative. The local file becomes a cache that
	// is never consulted for a trust decision.
	remote Remote
}

// UseRemote switches verification to shared storage.
func (s *Store) UseRemote(r Remote) { s.remote = r }

// Shared reports whether pins span gateway instances.
func (s *Store) Shared() bool { return s.remote != nil }

// Open loads pins from path, creating an empty store if the file is absent.
func Open(path string, tofu bool) (*Store, error) {
	s := &Store{path: path, pins: map[string]Pin{}, TOFU: tofu}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read host key store: %w", err)
	}

	var pins []Pin
	if err := json.Unmarshal(data, &pins); err != nil {
		// Refuse to start rather than silently continue with no pins — an
		// unreadable store must not degrade into trusting everything.
		return nil, fmt.Errorf("parse host key store %s: %w", path, err)
	}
	for _, p := range pins {
		s.pins[p.Host] = p
	}
	return s, nil
}

// Lookup returns the pin for host, if any.
func (s *Store) Lookup(host string) (Pin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pins[host]
	return p, ok
}

// All returns every pin, for the control plane's asset inventory.
func (s *Store) All() []Pin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Pin, 0, len(s.pins))
	for _, p := range s.pins {
		out = append(out, p)
	}
	return out
}

// Callback returns an ssh.HostKeyCallback bound to host.
//
// The returned callback is what makes the gateway's outbound connection safe.
// Never substitute ssh.InsecureIgnoreHostKey here.
func (s *Store) Callback(host string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		return s.Verify(host, key)
	}
}

// Verify checks a presented key against the pin for host.
func (s *Store) Verify(host string, key ssh.PublicKey) error {
	presented := ssh.FingerprintSHA256(key)

	if s.remote != nil {
		return s.verifyRemote(host, key, presented)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	pin, ok := s.pins[host]
	if !ok {
		if !s.TOFU {
			return fmt.Errorf("%w: %s presented %s", ErrUnpinned, host, presented)
		}
		pin = Pin{
			Host:        host,
			Fingerprint: presented,
			KeyType:     key.Type(),
			PinnedAt:    time.Now().UTC(),
			PinnedBy:    "tofu",
		}
		s.pins[host] = pin
		if err := s.saveLocked(); err != nil {
			return fmt.Errorf("persist pin for %s: %w", host, err)
		}
		return nil
	}

	if pin.Fingerprint != presented {
		return fmt.Errorf("%w: %s pinned %s but presented %s",
			ErrMismatch, host, pin.Fingerprint, presented)
	}
	return nil
}

// verifyRemote checks against shared storage.
//
// A failure to reach it is fatal for the connection. Falling back to the local
// file would mean a network blip downgrades the check to whatever this gateway
// happens to remember, which is how a mismatch goes unnoticed.
func (s *Store) verifyRemote(host string, key ssh.PublicKey, presented string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pin, err := s.remote.Pin(ctx, host)
	if err != nil {
		return fmt.Errorf("cannot check host key for %s: %w", host, err)
	}

	if pin == nil {
		if !s.TOFU {
			return fmt.Errorf("%w: %s presented %s", ErrUnpinned, host, presented)
		}
		rerr := s.remote.Record(ctx, Pin{
			Host:        host,
			Fingerprint: presented,
			KeyType:     key.Type(),
			PinnedAt:    time.Now().UTC(),
			PinnedBy:    "tofu",
		})
		if rerr != nil {
			// A refusal here means another gateway recorded a different key
			// first, which is a mismatch by any other name.
			return fmt.Errorf("%w: %s presented %s but the pin could not be recorded: %v",
				ErrMismatch, host, presented, rerr)
		}
		return nil
	}

	if pin.Fingerprint != presented {
		return fmt.Errorf("%w: %s pinned %s but presented %s",
			ErrMismatch, host, pin.Fingerprint, presented)
	}
	return nil
}

// Repin replaces the pin for host after an operator has verified the new key
// out of band. Deliberately explicit — nothing repins automatically, because
// automatic repinning would defeat the entire control.
func (s *Store) Repin(host string, key ssh.PublicKey, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pins[host] = Pin{
		Host:        host,
		Fingerprint: ssh.FingerprintSHA256(key),
		KeyType:     key.Type(),
		PinnedAt:    time.Now().UTC(),
		PinnedBy:    by,
	}
	return s.saveLocked()
}

// saveLocked persists the store. Caller must hold the write lock.
func (s *Store) saveLocked() error {
	pins := make([]Pin, 0, len(s.pins))
	for _, p := range s.pins {
		pins = append(pins, p)
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write cannot leave a truncated store
	// that would fail to parse and take the gateway down on restart.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
