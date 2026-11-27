package vault

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/secretwire"
	"go.etcd.io/bbolt"
)

// Buckets. The store is one bbolt file, so a value and the log entry that
// records its read commit in one transaction or not at all.
var (
	bucketMeta    = []byte("meta")
	bucketSecrets = []byte("secrets")
	bucketValues  = []byte("values")
	bucketLog     = []byte("log")
)

// Keys in bucketMeta.
var (
	metaCheck    = []byte("root.check")
	metaOrg      = []byte("org")
	metaOrgKey   = []byte("org.key")
	metaSealKey  = []byte("seal.key")
	metaLogSeq   = []byte("log.seq")
	metaLogHead  = []byte("log.head")
	metaLogBase  = []byte("log.base")
	metaLogAcked = []byte("log.acked")
)

// rootCheck is wrapped under the root key when the store is made. A root key
// that cannot open it is the wrong key, said plainly at startup rather than as
// every value failing to open later.
var rootCheck = []byte("argus-secret root key check v1")

var (
	// ErrNotFound is a secret or version that does not exist, or that the
	// caller may not know exists: a refusal by rule reads the same.
	ErrNotFound = errors.New("no such secret")
	// ErrDeleted is a secret scheduled for deletion. It can be restored.
	ErrDeleted = errors.New("this secret is scheduled for deletion; restore it first")
	// ErrNeedsApproval is a read a rule allows only once it is approved.
	ErrNeedsApproval = errors.New("reading this secret needs an approval first")
	// ErrWrongRootKey is a root key that did not make this store.
	ErrWrongRootKey = errors.New("the root key does not open this store; is root_key_file the one it was made with?")
)

// InvalidError is input the caller can correct.
type InvalidError struct{ Msg string }

func (e InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return InvalidError{fmt.Sprintf(format, a...)} }

// Secret is a secret's metadata. It never holds a value.
type Secret struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Versions    []Version         `json:"versions"`
	// Labels maps current, previous and pending to a version number.
	Labels    map[string]int `json:"labels"`
	CreatedAt time.Time      `json:"createdAt"`
	CreatedBy string         `json:"createdBy"`
	UpdatedAt time.Time      `json:"updatedAt"`
	// DeletedAt and PurgeAt are set while a deletion is scheduled.
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	DeletedBy string     `json:"deletedBy,omitempty"`
	PurgeAt   *time.Time `json:"purgeAt,omitempty"`
}

// Version is one value a secret has had.
type Version struct {
	N         int       `json:"n"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
	// Generated is a value argus-secret made itself, so nobody ever typed it.
	Generated bool `json:"generated,omitempty"`
	// Fields are a key-value version's field names, never their values.
	Fields []string `json:"fields,omitempty"`
	Bytes  int      `json:"bytes"`
}

// Current is the version the current label points at, or 0.
func (s Secret) Current() int { return s.Labels[LabelCurrent] }

func (s Secret) version(n int) (Version, bool) {
	for _, v := range s.Versions {
		if v.N == n {
			return v, true
		}
	}
	return Version{}, false
}

type sealedValue struct {
	DEK   []byte `json:"dek"`
	Value []byte `json:"value"`
}

// Store is argus-secret's database: secrets, their encrypted values, the keys
// that open them (wrapped under the root key) and the read log.
type Store struct {
	db     *bbolt.DB
	org    string
	orgKey []byte
	seal   *ecdh.PrivateKey
	now    func() time.Time
}

// Open opens or creates the store in dir. rootKey wraps everything; org names
// the organization this server belongs to, fixed when the store is made.
func Open(dir string, rootKey []byte, org string) (*Store, error) {
	if len(rootKey) != KeySize {
		return nil, fmt.Errorf("the root key must be %d bytes", KeySize)
	}
	if org == "" {
		return nil, errors.New("organization is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(filepath.Join(dir, "secrets.db"), 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s := &Store{db: db, org: org, now: time.Now}
	if err := db.Update(func(tx *bbolt.Tx) error { return s.init(tx, rootKey) }); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(tx *bbolt.Tx, root []byte) error {
	for _, b := range [][]byte{bucketMeta, bucketSecrets, bucketValues, bucketLog} {
		if _, err := tx.CreateBucketIfNotExists(b); err != nil {
			return err
		}
	}
	meta := tx.Bucket(bucketMeta)
	if check := meta.Get(metaCheck); check != nil {
		plain, err := unwrap(root, check, []byte("root-check"))
		if err != nil || !bytes.Equal(plain, rootCheck) {
			return ErrWrongRootKey
		}
		if got := string(meta.Get(metaOrg)); got != s.org {
			return fmt.Errorf("this store belongs to organization %q, not %q", got, s.org)
		}
		orgKey, err := unwrap(root, meta.Get(metaOrgKey), []byte("org-key\x00"+s.org))
		if err != nil {
			return fmt.Errorf("organization key: %w", err)
		}
		sealRaw, err := unwrap(root, meta.Get(metaSealKey), []byte("seal-key"))
		if err != nil {
			return fmt.Errorf("seal key: %w", err)
		}
		seal, err := ecdh.X25519().NewPrivateKey(sealRaw)
		if err != nil {
			return err
		}
		s.orgKey, s.seal = orgKey, seal
		return nil
	}
	// A new store.
	orgKey, err := randomKey()
	if err != nil {
		return err
	}
	seal, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	check, err := wrap(root, rootCheck, []byte("root-check"))
	if err != nil {
		return err
	}
	wrappedOrg, err := wrap(root, orgKey, []byte("org-key\x00"+s.org))
	if err != nil {
		return err
	}
	wrappedSeal, err := wrap(root, seal.Bytes(), []byte("seal-key"))
	if err != nil {
		return err
	}
	for k, v := range map[string][]byte{
		string(metaCheck): check, string(metaOrg): []byte(s.org), string(metaOrgKey): wrappedOrg,
		string(metaSealKey): wrappedSeal, string(metaLogHead): []byte(genesis),
	} {
		if err := meta.Put([]byte(k), v); err != nil {
			return err
		}
	}
	s.orgKey, s.seal = orgKey, seal
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Organization is the organization this store belongs to.
func (s *Store) Organization() string { return s.org }

// SealPublicKey is the X25519 key a new value is sealed to before it is sent
// here, so argus-control relays it without being able to read it.
func (s *Store) SealPublicKey() []byte { return s.seal.PublicKey().Bytes() }

// OpenWrite opens a value sealed to SealPublicKey for name.
func (s *Store) OpenWrite(name string, sealed []byte) ([]byte, error) {
	v, err := secretwire.OpenSealed(s.seal, sealed, secretwire.WriteAAD(name))
	if err != nil {
		return nil, invalid("the new value was not sealed to this server's key for %s", name)
	}
	return v, nil
}

// PutInput is a new version of a secret, or a new secret.
type PutInput struct {
	Name        string
	Type        string
	Description string
	Owner       string
	Tags        map[string]string
	Value       []byte
	Generated   bool
	Actor       Actor
	Reason      string
	Route       string
}

// Put stores a new version and makes it current; the version it replaces
// becomes previous. A secret that does not exist yet is created.
func (s *Store) Put(in PutInput) (Secret, error) {
	if err := ValidName(in.Name); err != nil {
		return Secret{}, InvalidError{err.Error()}
	}
	if err := checkMeta(in.Description, in.Tags); err != nil {
		return Secret{}, err
	}
	var out Secret
	err := s.db.Update(func(tx *bbolt.Tx) error {
		now := s.now().UTC()
		sec, found, err := getSecret(tx, in.Name)
		if err != nil {
			return err
		}
		if found && sec.DeletedAt != nil {
			return ErrDeleted
		}
		if !found {
			if err := ValidType(in.Type); err != nil {
				return InvalidError{err.Error()}
			}
			sec = Secret{Name: in.Name, Type: in.Type, Description: in.Description, Owner: in.Owner,
				Tags: in.Tags, Labels: map[string]int{}, CreatedAt: now, CreatedBy: in.Actor.ID}
		} else if in.Type != "" && in.Type != sec.Type {
			return invalid("%s is a %s secret; its type cannot change", in.Name, sec.Type)
		}
		if err := checkValue(sec.Type, in.Value); err != nil {
			return InvalidError{err.Error()}
		}
		n := 1
		if len(sec.Versions) > 0 {
			n = sec.Versions[len(sec.Versions)-1].N + 1
		}
		if err := s.putValue(tx, in.Name, n, in.Value); err != nil {
			return err
		}
		sec.Versions = append(sec.Versions, Version{N: n, CreatedAt: now, CreatedBy: in.Actor.ID,
			Generated: in.Generated, Fields: fieldNames(sec.Type, in.Value), Bytes: len(in.Value)})
		if cur, ok := sec.Labels[LabelCurrent]; ok {
			sec.Labels[LabelPrevious] = cur
		}
		sec.Labels[LabelCurrent] = n
		delete(sec.Labels, LabelPending)
		sec.UpdatedAt = now
		if err := s.dropOldVersions(tx, &sec); err != nil {
			return err
		}
		if err := putSecret(tx, sec); err != nil {
			return err
		}
		action := "write"
		if !found {
			action = "create"
		}
		if in.Generated {
			action += ".generated"
		}
		out = sec
		return s.appendLog(tx, Entry{Actor: in.Actor, Action: action, Secret: in.Name, Version: n,
			Reason: in.Reason, Route: in.Route, Outcome: OutcomeOK})
	})
	return out, err
}

// MetaInput changes a secret's description, owner or tags. A nil field is
// left as it is.
type MetaInput struct {
	Description *string
	Owner       *string
	Tags        map[string]string
	SetTags     bool
}

// UpdateMeta changes what is said about a secret, not its value.
func (s *Store) UpdateMeta(name string, in MetaInput, actor Actor) (Secret, error) {
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	if err := checkMeta(desc, in.Tags); err != nil {
		return Secret{}, err
	}
	var out Secret
	err := s.db.Update(func(tx *bbolt.Tx) error {
		sec, err := liveSecret(tx, name)
		if err != nil {
			return err
		}
		if in.Description != nil {
			sec.Description = *in.Description
		}
		if in.Owner != nil {
			sec.Owner = *in.Owner
		}
		if in.SetTags {
			sec.Tags = in.Tags
		}
		sec.UpdatedAt = s.now().UTC()
		if err := putSecret(tx, sec); err != nil {
			return err
		}
		out = sec
		return s.appendLog(tx, Entry{Actor: actor, Action: "describe", Secret: name, Outcome: OutcomeOK})
	})
	return out, err
}

func checkMeta(desc string, tags map[string]string) error {
	if len(desc) > maxDescription {
		return invalid("a description is at most %d characters", maxDescription)
	}
	if len(tags) > maxTags {
		return invalid("at most %d tags", maxTags)
	}
	for k, v := range tags {
		if k == "" || len(k) > maxTagLen || len(v) > maxTagLen {
			return invalid("a tag's name is 1 to %d characters and its value at most %d", maxTagLen, maxTagLen)
		}
	}
	return nil
}

// Get returns a secret's metadata, including one scheduled for deletion.
func (s *Store) Get(name string) (Secret, error) {
	var out Secret
	err := s.db.View(func(tx *bbolt.Tx) error {
		sec, found, err := getSecret(tx, name)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		out = sec
		return nil
	})
	return out, err
}

// List returns the secrets under prefix ("" for all), sorted by name. Secrets
// scheduled for deletion are included only when deleted is true.
func (s *Store) List(prefix string, deleted bool) ([]Secret, error) {
	var out []Secret
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketSecrets).Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, v = c.Next() {
			var sec Secret
			if err := json.Unmarshal(v, &sec); err != nil {
				return err
			}
			if sec.DeletedAt != nil && !deleted {
				continue
			}
			if !underPrefix(sec.Name, prefix) {
				continue
			}
			out = append(out, sec)
		}
		return nil
	})
	return out, err
}

// underPrefix is whether name is prefix itself or inside the folder prefix:
// "prod/db" is under "prod" and "prod/", but "production/db" is not under "prod".
func underPrefix(name, prefix string) bool {
	if prefix == "" || name == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(name, prefix)
	}
	return strings.HasPrefix(name, prefix+"/")
}

// ReadInput says which value to read and who is reading it.
type ReadInput struct {
	Name string
	// Version, when not 0, is read instead of Label.
	Version int
	// Label defaults to current.
	Label  string
	Field  string
	Actor  Actor
	Reason string
	Route  string
	Remote string
}

// Read returns a value and its version number. The read is in the log before
// this returns: a value that left argus-secret was recorded first.
func (s *Store) Read(in ReadInput) ([]byte, int, error) {
	var value []byte
	var ver int
	err := s.db.Update(func(tx *bbolt.Tx) error {
		sec, err := liveSecret(tx, in.Name)
		if err != nil {
			return err
		}
		ver = in.Version
		if ver == 0 {
			label := in.Label
			if label == "" {
				label = LabelCurrent
			}
			n, ok := sec.Labels[label]
			if !ok {
				return invalid("%s has no %s version", in.Name, label)
			}
			ver = n
		}
		if _, ok := sec.version(ver); !ok {
			return invalid("%s has no version %d", in.Name, ver)
		}
		plain, err := s.getValue(tx, in.Name, ver)
		if err != nil {
			return err
		}
		v, err := field(sec.Type, plain, in.Field)
		if err != nil {
			return InvalidError{err.Error()}
		}
		value = v
		return s.appendLog(tx, Entry{Actor: in.Actor, Action: "read", Secret: in.Name, Version: ver,
			Field: in.Field, Reason: in.Reason, Route: in.Route, Remote: in.Remote, Outcome: OutcomeOK})
	})
	if err != nil {
		return nil, 0, err
	}
	return value, ver, nil
}

// Refused records a request turned away by a rule, from a caller who proved
// who they are. Callers who did not are not logged here: anyone on the network
// could fill the log with them.
func (s *Store) Refused(e Entry) error {
	e.Outcome = OutcomeRefused
	return s.db.Update(func(tx *bbolt.Tx) error { return s.appendLog(tx, e) })
}

// Note records an event that changes no secret, such as new rules arriving.
func (s *Store) Note(e Entry) error {
	if e.Outcome == "" {
		e.Outcome = OutcomeOK
	}
	return s.db.Update(func(tx *bbolt.Tx) error { return s.appendLog(tx, e) })
}

// SetLabel points label at version: rolling back is pointing current at an
// older version.
func (s *Store) SetLabel(name, label string, version int, actor Actor, reason string) (Secret, error) {
	switch label {
	case LabelCurrent, LabelPrevious, LabelPending:
	default:
		return Secret{}, invalid("label is one of current, previous, pending")
	}
	var out Secret
	err := s.db.Update(func(tx *bbolt.Tx) error {
		sec, err := liveSecret(tx, name)
		if err != nil {
			return err
		}
		if _, ok := sec.version(version); !ok {
			return invalid("%s has no version %d", name, version)
		}
		if label == LabelCurrent {
			if cur, ok := sec.Labels[LabelCurrent]; ok && cur != version {
				sec.Labels[LabelPrevious] = cur
			}
		}
		sec.Labels[label] = version
		sec.UpdatedAt = s.now().UTC()
		if err := putSecret(tx, sec); err != nil {
			return err
		}
		out = sec
		return s.appendLog(tx, Entry{Actor: actor, Action: "label." + label, Secret: name, Version: version,
			Reason: reason, Outcome: OutcomeOK})
	})
	return out, err
}

// Recovery window bounds for a deletion.
const (
	MinRecoveryDays     = 7
	MaxRecoveryDays     = 30
	DefaultRecoveryDays = 30
)

// Delete schedules a secret for deletion. Until it is purged it can be
// restored, and no one can read it.
func (s *Store) Delete(name string, recoveryDays int, actor Actor, reason string) (Secret, error) {
	if recoveryDays == 0 {
		recoveryDays = DefaultRecoveryDays
	}
	if recoveryDays < MinRecoveryDays || recoveryDays > MaxRecoveryDays {
		return Secret{}, invalid("the recovery window is %d to %d days", MinRecoveryDays, MaxRecoveryDays)
	}
	var out Secret
	err := s.db.Update(func(tx *bbolt.Tx) error {
		sec, err := liveSecret(tx, name)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		purge := now.Add(time.Duration(recoveryDays) * 24 * time.Hour)
		sec.DeletedAt, sec.DeletedBy, sec.PurgeAt, sec.UpdatedAt = &now, actor.ID, &purge, now
		if err := putSecret(tx, sec); err != nil {
			return err
		}
		out = sec
		return s.appendLog(tx, Entry{Actor: actor, Action: "delete", Secret: name, Reason: reason,
			Detail: fmt.Sprintf("purged after %s", purge.Format(time.RFC3339)), Outcome: OutcomeOK})
	})
	return out, err
}

// Restore cancels a scheduled deletion.
func (s *Store) Restore(name string, actor Actor) (Secret, error) {
	var out Secret
	err := s.db.Update(func(tx *bbolt.Tx) error {
		sec, found, err := getSecret(tx, name)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if sec.DeletedAt == nil {
			return invalid("%s is not scheduled for deletion", name)
		}
		sec.DeletedAt, sec.DeletedBy, sec.PurgeAt, sec.UpdatedAt = nil, "", nil, s.now().UTC()
		if err := putSecret(tx, sec); err != nil {
			return err
		}
		out = sec
		return s.appendLog(tx, Entry{Actor: actor, Action: "restore", Secret: name, Outcome: OutcomeOK})
	})
	return out, err
}

// PurgeDue removes every secret whose recovery window has ended, values and
// all, and returns their names.
func (s *Store) PurgeDue() ([]string, error) {
	var purged []string
	err := s.db.Update(func(tx *bbolt.Tx) error {
		now := s.now()
		var due []Secret
		err := tx.Bucket(bucketSecrets).ForEach(func(_, v []byte) error {
			var sec Secret
			if err := json.Unmarshal(v, &sec); err != nil {
				return err
			}
			if sec.PurgeAt != nil && !now.Before(*sec.PurgeAt) {
				due = append(due, sec)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, sec := range due {
			for _, v := range sec.Versions {
				if err := tx.Bucket(bucketValues).Delete(valueKey(sec.Name, v.N)); err != nil {
					return err
				}
			}
			if err := tx.Bucket(bucketSecrets).Delete([]byte(sec.Name)); err != nil {
				return err
			}
			purged = append(purged, sec.Name)
			if err := s.appendLog(tx, Entry{Actor: Actor{Kind: ActorSystem, ID: "argus-secret"}, Action: "purge",
				Secret: sec.Name, Outcome: OutcomeOK}); err != nil {
				return err
			}
		}
		return nil
	})
	return purged, err
}

// dropOldVersions keeps at most maxVersions, dropping the oldest that no label
// points at.
func (s *Store) dropOldVersions(tx *bbolt.Tx, sec *Secret) error {
	labelled := map[int]bool{}
	for _, n := range sec.Labels {
		labelled[n] = true
	}
	for len(sec.Versions) > maxVersions {
		i := slices.IndexFunc(sec.Versions, func(v Version) bool { return !labelled[v.N] })
		if i < 0 {
			return nil
		}
		if err := tx.Bucket(bucketValues).Delete(valueKey(sec.Name, sec.Versions[i].N)); err != nil {
			return err
		}
		sec.Versions = slices.Delete(sec.Versions, i, i+1)
	}
	return nil
}

func (s *Store) putValue(tx *bbolt.Tx, name string, n int, plain []byte) error {
	dek, err := randomKey()
	if err != nil {
		return err
	}
	ct, err := wrap(dek, plain, valueAAD(name, n))
	if err != nil {
		return err
	}
	wrapped, err := wrap(s.orgKey, dek, dekAAD(s.org, name, n))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(sealedValue{DEK: wrapped, Value: ct})
	if err != nil {
		return err
	}
	return tx.Bucket(bucketValues).Put(valueKey(name, n), raw)
}

func (s *Store) getValue(tx *bbolt.Tx, name string, n int) ([]byte, error) {
	raw := tx.Bucket(bucketValues).Get(valueKey(name, n))
	if raw == nil {
		return nil, fmt.Errorf("%s version %d has no stored value", name, n)
	}
	var sv sealedValue
	if err := json.Unmarshal(raw, &sv); err != nil {
		return nil, err
	}
	dek, err := unwrap(s.orgKey, sv.DEK, dekAAD(s.org, name, n))
	if err != nil {
		return nil, fmt.Errorf("%s version %d: data key %w", name, n, err)
	}
	plain, err := unwrap(dek, sv.Value, valueAAD(name, n))
	if err != nil {
		return nil, fmt.Errorf("%s version %d: value %w", name, n, err)
	}
	return plain, nil
}

func valueAAD(name string, n int) []byte { return fmt.Appendf(nil, "value\x00%s\x00%d", name, n) }

func dekAAD(org, name string, n int) []byte {
	return fmt.Appendf(nil, "dek\x00%s\x00%s\x00%d", org, name, n)
}

func valueKey(name string, n int) []byte {
	k := make([]byte, 0, len(name)+1+8)
	k = append(k, name...)
	k = append(k, 0)
	return binary.BigEndian.AppendUint64(k, uint64(n))
}

func getSecret(tx *bbolt.Tx, name string) (Secret, bool, error) {
	raw := tx.Bucket(bucketSecrets).Get([]byte(name))
	if raw == nil {
		return Secret{}, false, nil
	}
	var sec Secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return Secret{}, false, err
	}
	if sec.Labels == nil {
		sec.Labels = map[string]int{}
	}
	return sec, true, nil
}

func liveSecret(tx *bbolt.Tx, name string) (Secret, error) {
	sec, found, err := getSecret(tx, name)
	if err != nil {
		return Secret{}, err
	}
	if !found {
		return Secret{}, ErrNotFound
	}
	if sec.DeletedAt != nil {
		return Secret{}, ErrDeleted
	}
	return sec, nil
}

func putSecret(tx *bbolt.Tx, sec Secret) error {
	raw, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketSecrets).Put([]byte(sec.Name), raw)
}

func sortedStrings(s []string) []string {
	slices.Sort(s)
	return s
}
