package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// Permissions a rule grants.
const (
	CanList   = "list"
	CanRead   = "read"
	CanWrite  = "write"
	CanRotate = "rotate"
	CanManage = "manage"
)

// Subject kinds a rule can name.
const (
	SubjectServer = "server"
	SubjectGroup  = "group"
	SubjectApp    = "app"
	SubjectPerson = "person"
	SubjectTeam   = "team"
	SubjectRole   = "role"
)

// Bundle is everything argus-control tells argus-secret about who may do
// what. It replaces the previous bundle whole; Serial only goes up, so an old
// bundle replayed cannot bring back a revoked grant.
type Bundle struct {
	Org      string    `json:"org"`
	Serial   int64     `json:"serial"`
	IssuedAt time.Time `json:"issuedAt"`
	// Folders are the top-level folders this server keeps. A name outside
	// them is not here, whatever a rule says.
	Folders []string         `json:"folders"`
	Servers []ServerIdentity `json:"servers"`
	Apps    []AppIdentity    `json:"apps"`
	Rules   []Rule           `json:"rules"`
}

// ServerIdentity is an enrolled agent: the SHA-256 (hex) of its credential,
// exactly as argus-control stores it, so the credential itself never travels.
type ServerIdentity struct {
	ID             string   `json:"id"`
	Hostname       string   `json:"hostname"`
	CredentialHash string   `json:"credentialHash"`
	Groups         []string `json:"groups,omitempty"`
}

// AppIdentity is an app token for a caller outside Argus.
type AppIdentity struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Rule grants Can on Path to Subjects. Path is a secret name or a folder; ""
// is every secret.
type Rule struct {
	Path     string    `json:"path"`
	Subjects []Subject `json:"subjects"`
	Can      []string  `json:"can"`
	// NeedsApproval applies to people: argus-control must say the read was
	// approved before argus-secret answers it.
	NeedsApproval bool `json:"needsApproval,omitempty"`
}

// Subject is who a rule is for.
type Subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Principal is a caller, with what it is a member of.
type Principal struct {
	Actor
	// Groups for a server, teams for a person.
	Groups []string
	Role   string
	// Approved is argus-control saying an approval this read needed was given.
	Approved bool
}

// Limits on a bundle, so a bad push cannot make argus-secret hold more than
// it can afford to search on every request.
const (
	maxBundleServers = 100_000
	maxBundleApps    = 10_000
	maxBundleRules   = 50_000
)

func (b Bundle) validate(org string) error {
	if b.Org != org {
		return fmt.Errorf("rules are for organization %q, this server keeps %q", b.Org, org)
	}
	if b.Serial <= 0 {
		return errors.New("rules need a serial above 0")
	}
	if len(b.Servers) > maxBundleServers || len(b.Apps) > maxBundleApps || len(b.Rules) > maxBundleRules {
		return errors.New("rules are larger than argus-secret accepts")
	}
	for _, f := range b.Folders {
		if err := validSegment(f); err != nil {
			return fmt.Errorf("folder %q: %w", f, err)
		}
	}
	for _, r := range b.Rules {
		if err := ValidPrefix(r.Path); err != nil {
			return fmt.Errorf("rule path: %w", err)
		}
		for _, c := range r.Can {
			switch c {
			case CanList, CanRead, CanWrite, CanRotate, CanManage:
			default:
				return fmt.Errorf("rule grants unknown permission %q", c)
			}
		}
	}
	for _, s := range b.Servers {
		if len(s.CredentialHash) != 64 {
			return fmt.Errorf("server %s: credential hash is not SHA-256 hex", s.ID)
		}
	}
	for _, a := range b.Apps {
		if len(a.TokenHash) != 64 {
			return fmt.Errorf("app %s: token hash is not SHA-256 hex", a.ID)
		}
	}
	return nil
}

// Rules holds the bundle in force and when argus-control was last heard from.
type Rules struct {
	mu        sync.RWMutex
	bundle    Bundle
	servers   map[string]*ServerIdentity
	apps      map[string]*AppIdentity
	contacted time.Time
	maxAge    time.Duration
	now       func() time.Time
}

// NewRules starts with no rules: everything is refused until argus-control
// pushes some.
func NewRules(maxAge time.Duration) *Rules {
	return &Rules{maxAge: maxAge, now: time.Now, servers: map[string]*ServerIdentity{}, apps: map[string]*AppIdentity{}}
}

// Replace installs a newer bundle.
func (r *Rules) Replace(b Bundle, org string) error {
	if err := b.validate(org); err != nil {
		return InvalidError{err.Error()}
	}
	servers := make(map[string]*ServerIdentity, len(b.Servers))
	for i := range b.Servers {
		servers[strings.ToLower(b.Servers[i].CredentialHash)] = &b.Servers[i]
	}
	apps := make(map[string]*AppIdentity, len(b.Apps))
	for i := range b.Apps {
		apps[strings.ToLower(b.Apps[i].TokenHash)] = &b.Apps[i]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b.Serial <= r.bundle.Serial {
		return InvalidError{fmt.Sprintf("rules serial %d is not newer than %d", b.Serial, r.bundle.Serial)}
	}
	r.bundle, r.servers, r.apps = b, servers, apps
	r.contacted = r.now()
	return nil
}

// Contacted records that argus-control was heard from just now.
func (r *Rules) Contacted() {
	r.mu.Lock()
	r.contacted = r.now()
	r.mu.Unlock()
}

// ErrRulesStale refuses a read when argus-control has not been heard from for
// longer than the configured age: a revocation may have been missed.
var ErrRulesStale = errors.New("argus-secret has not heard from argus-control recently enough to trust its rules")

// Status is the rules' serial and age.
func (r *Rules) Status() (serial int64, contacted time.Time, fresh bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bundle.Serial, r.contacted, r.freshLocked()
}

func (r *Rules) freshLocked() bool {
	return r.bundle.Serial > 0 && r.now().Sub(r.contacted) <= r.maxAge
}

// hashSecret is the SHA-256 (hex) argus-control stores for a credential or
// token. The input is 256 random bits, so a plain digest is enough.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Credential prefixes.
const (
	AgentCredentialPrefix = "argus-agent-"
	AppTokenPrefix        = "argus-app-"
)

// Authenticate turns a presented agent credential or app token into a
// principal. ok is false for anything argus-control has not told us about,
// or an app token past its expiry.
func (r *Rules) Authenticate(secret string) (Principal, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := hashSecret(secret)
	switch {
	case strings.HasPrefix(secret, AgentCredentialPrefix):
		s, ok := r.servers[h]
		if !ok {
			return Principal{}, false
		}
		return Principal{Actor: Actor{Kind: ActorServer, ID: s.ID, Name: s.Hostname}, Groups: s.Groups}, true
	case strings.HasPrefix(secret, AppTokenPrefix):
		a, ok := r.apps[h]
		if !ok || !r.now().Before(a.ExpiresAt) {
			return Principal{}, false
		}
		return Principal{Actor: Actor{Kind: ActorApp, ID: a.ID, Name: a.Name}}, true
	}
	return Principal{}, false
}

// Decision is the answer to "may p do action on name".
type Decision struct {
	Allowed bool
	// NeedsApproval is set when the only rule that allows it asks for an
	// approval the principal has not got.
	NeedsApproval bool
}

// Allow decides. Stale rules refuse everything.
func (r *Rules) Allow(p Principal, action, name string) (Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.freshLocked() {
		return Decision{}, ErrRulesStale
	}
	if !r.keepsLocked(name) {
		return Decision{}, nil
	}
	var d Decision
	for _, rule := range r.bundle.Rules {
		if !underPrefix(name, rule.Path) || !grants(rule.Can, action) || !matches(rule.Subjects, p) {
			continue
		}
		if rule.NeedsApproval && p.Kind == ActorPerson && !p.Approved {
			d.NeedsApproval = true
			continue
		}
		return Decision{Allowed: true}, nil
	}
	return d, nil
}

// Keeps is whether name is in a folder this server keeps.
func (r *Rules) Keeps(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keepsLocked(name)
}

func (r *Rules) keepsLocked(name string) bool {
	return slices.Contains(r.bundle.Folders, Folder(name))
}

// grants is whether can includes action. Read, write, rotate and manage each
// include list: someone who may read a secret may see that it exists.
func grants(can []string, action string) bool {
	for _, c := range can {
		if c == action || (action == CanList && c != "") {
			return true
		}
	}
	return false
}

func matches(subjects []Subject, p Principal) bool {
	for _, s := range subjects {
		switch {
		case s.Kind == SubjectServer && p.Kind == ActorServer && s.ID == p.ID,
			s.Kind == SubjectApp && p.Kind == ActorApp && s.ID == p.ID,
			s.Kind == SubjectPerson && p.Kind == ActorPerson && strings.EqualFold(s.ID, p.ID),
			s.Kind == SubjectRole && p.Kind == ActorPerson && s.ID == p.Role:
			return true
		case (s.Kind == SubjectGroup && p.Kind == ActorServer) || (s.Kind == SubjectTeam && p.Kind == ActorPerson):
			if slices.Contains(p.Groups, s.ID) {
				return true
			}
		}
	}
	return false
}

var metaRules = []byte("rules")

// saveRules keeps the bundle across a restart. When argus-control was last
// heard from is not kept: after a restart the rules are trusted only once
// argus-control is heard from again, or until maxAge from when they arrived.
func (s *Store) saveRules(b Bundle, received time.Time) error {
	raw, err := json.Marshal(struct {
		Bundle   Bundle    `json:"bundle"`
		Received time.Time `json:"received"`
	}{b, received})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketMeta).Put(metaRules, raw) })
}

// LoadRules restores the last bundle into r.
func (s *Store) LoadRules(r *Rules) error {
	var saved struct {
		Bundle   Bundle    `json:"bundle"`
		Received time.Time `json:"received"`
	}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(metaRules)
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &saved)
	})
	if err != nil || saved.Bundle.Serial == 0 {
		return err
	}
	if err := r.Replace(saved.Bundle, s.org); err != nil {
		return err
	}
	r.mu.Lock()
	r.contacted = saved.Received
	r.mu.Unlock()
	return nil
}

// ApplyRules installs a bundle pushed by argus-control, keeps it, and logs it.
func (s *Store) ApplyRules(r *Rules, b Bundle) error {
	if err := r.Replace(b, s.org); err != nil {
		return err
	}
	if err := s.saveRules(b, s.now()); err != nil {
		return err
	}
	return s.Note(Entry{Actor: Actor{Kind: ActorControl, ID: "argus-control"}, Action: "rules",
		Detail: fmt.Sprintf("serial %d: %d rules, %d servers, %d apps", b.Serial, len(b.Rules), len(b.Servers), len(b.Apps))})
}
