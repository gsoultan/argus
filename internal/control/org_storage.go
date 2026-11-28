package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/caduceus"

	"github.com/gsoultan/argus/internal/storage"
)

/*
An organization's own recording storage.

The control plane's configuration file says where recordings go, and for an
organization other than the default one that used to be the whole answer:
its recordings went into the control plane's bucket under tenants/<id>/,
archived and kept on servers by the control plane's settings, and the only
way to change any of it was the platform's Settings -- which every
organization's admins could reach, so one organization could move another's
evidence.

Now each organization other than the default one has its own:

  - where its recordings are copied: the control plane's storage (the
    default), or its own S3, R2 or Azure destination, with its own key,
    sealed in its own schema;
  - when they are archived, and into which class or tier of its own
    destination;
  - how long its servers keep their own copies.

The default organization's are the configuration file's, as before, because
they are the platform's.

Nothing is stranded by a change. The destination an organization moves away
from stays as a retired row; reads fall back to it (storage.Layered) and a
background pass copies everything out of it (storage.CopyObjects), deleting
the row once nothing is left behind. Moving from the control plane's storage
to the organization's own copies out of the control plane's in the same way,
and moving back copies the organization's own into the control plane's.
*/

// orgStorageRow is one row of recording_storages.
type orgStorageRow struct {
	ID           string
	State        string
	Provider     string
	Endpoint     string
	Bucket       string
	Region       string
	UseSSL       bool
	AccessKey    string
	Sealed       []byte
	ArchiveClass string
	UpdatedAt    time.Time
}

// sameDestination reports whether a and b name the same place: a new key
// for the same bucket is an update, not a move.
func (r orgStorageRow) sameDestination(o orgStorageRow) bool {
	return r.Provider == o.Provider && strings.EqualFold(r.Endpoint, o.Endpoint) &&
		r.Bucket == o.Bucket && r.AccessKey == o.AccessKey && r.Region == o.Region
}

func (r orgStorageRow) name() string {
	if r.Provider == storage.ProviderAzure {
		return "Azure " + r.AccessKey + "/" + r.Bucket
	}
	if r.Endpoint != "" {
		return r.Endpoint + "/" + r.Bucket
	}
	return r.Bucket
}

// RecordingPolicy is an organization's own ages: 0 is the control plane's,
// -1 is for ever (keep) or never (archive).
type RecordingPolicy struct {
	KeepDays    int
	ArchiveDays int
}

const orgStorageCols = `id::text, state, provider, endpoint, bucket, region, use_ssl, access_key,
	secret_sealed, archive_class, updated_at`

func scanOrgStorage(row rowScanner) (orgStorageRow, error) {
	var r orgStorageRow
	err := row.Scan(&r.ID, &r.State, &r.Provider, &r.Endpoint, &r.Bucket, &r.Region, &r.UseSSL,
		&r.AccessKey, &r.Sealed, &r.ArchiveClass, &r.UpdatedAt)
	return r, err
}

// RecordingStorages are the organization's destinations, the active one
// first.
func (s *Store) RecordingStorages(ctx context.Context) ([]orgStorageRow, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+orgStorageCols+` FROM recording_storages
		ORDER BY state = 'active' DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orgStorageRow
	for rows.Next() {
		r, err := scanOrgStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRecordingStorage makes next the organization's active destination, or
// none when next is nil. An active destination it replaces is retired, not
// deleted, unless next is the same place with a new key or archive class.
func (s *Store) SetRecordingStorage(ctx context.Context, next *orgStorageRow, by string) error {
	return caduceus.BeginFunc(ctx, s.db(ctx), func(tx *caduceus.Tx) error {
		cur, err := scanOrgStorage(tx.QueryRow(ctx, `SELECT `+orgStorageCols+`
			FROM recording_storages WHERE state = 'active' FOR UPDATE`))
		has := err == nil
		if err != nil && !errors.Is(err, caduceus.ErrNoRows) {
			return err
		}
		if has && next != nil && cur.sameDestination(*next) {
			_, err := tx.Exec(ctx, `UPDATE recording_storages SET secret_sealed = $2, archive_class = $3,
				use_ssl = $4, updated_at = now() WHERE id = $1::uuid`,
				cur.ID, next.Sealed, next.ArchiveClass, next.UseSSL)
			return err
		}
		if has {
			if _, err := tx.Exec(ctx, `UPDATE recording_storages SET state = 'retired', updated_at = now()
				WHERE id = $1::uuid`, cur.ID); err != nil {
				return err
			}
		}
		if next == nil {
			return nil
		}
		// Moving back to a destination still being copied out of makes it
		// active again rather than a second row for one place.
		_, err = tx.Exec(ctx, `DELETE FROM recording_storages WHERE state = 'retired'
			AND provider = $1 AND lower(endpoint) = lower($2) AND bucket = $3 AND access_key = $4 AND region = $5`,
			next.Provider, next.Endpoint, next.Bucket, next.AccessKey, next.Region)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO recording_storages (state, provider, endpoint, bucket, region,
			use_ssl, access_key, secret_sealed, archive_class, created_by)
			VALUES ('active', $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			next.Provider, next.Endpoint, next.Bucket, next.Region, next.UseSSL, next.AccessKey,
			next.Sealed, next.ArchiveClass, strings.ToLower(by))
		return err
	})
}

// dropRetiredStorage deletes a retired destination that has nothing left
// to copy. An active one is never deleted here.
func (s *Store) dropRetiredStorage(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM recording_storages WHERE id = $1::uuid AND state = 'retired'`, id)
	return err
}

// RecordingPolicyOf is the organization's own ages; zeros when it has none.
func (s *Store) RecordingPolicyOf(ctx context.Context) (RecordingPolicy, error) {
	var p RecordingPolicy
	err := s.db(ctx).QueryRow(ctx, `SELECT keep_days, archive_days FROM recording_policies`).
		Scan(&p.KeepDays, &p.ArchiveDays)
	if errors.Is(err, caduceus.ErrNoRows) {
		return RecordingPolicy{}, nil
	}
	return p, err
}

// SetRecordingPolicy writes the organization's own ages.
func (s *Store) SetRecordingPolicy(ctx context.Context, p RecordingPolicy, by string) error {
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO recording_policies (keep_days, archive_days, updated_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (singleton) DO UPDATE SET keep_days = $1, archive_days = $2, updated_by = $3, updated_at = now()`,
		p.KeepDays, p.ArchiveDays, strings.ToLower(by))
	return err
}

/* ── Sealing keys ─────────────────────────────────────────────────────── */

// storageKey is the AES key an organization's storage secrets are sealed
// with: derived from signing_secret through the organization's grant key,
// so one organization's sealed secret opens in no other.
func (a *API) storageKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, errors.New("this control plane cannot keep storage keys safe: set signing_secret in its configuration")
	}
	sum := sha256.Sum256(append([]byte("argus recording storage v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealStorageSecret(ctx context.Context, secret string) ([]byte, error) {
	key, err := a.storageKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(secret), []byte("storage-secret")), nil
}

func (a *API) openStorageSecret(ctx context.Context, sealed []byte) (string, error) {
	key, err := a.storageKey(ctx)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("the storage key cannot be read")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("storage-secret"))
	if err != nil {
		return "", errors.New("the storage key cannot be read; was signing_secret changed? Enter the key again")
	}
	return string(plain), nil
}

func (r orgStorageRow) config(secret string) storage.Config {
	p := r.Provider
	if p == storage.ProviderS3 {
		p = ""
	}
	return storage.Config{Provider: p, Endpoint: r.Endpoint, Bucket: r.Bucket, Region: r.Region,
		UseSSL: r.UseSSL, AccessKey: r.AccessKey, SecretKey: secret}
}

/* ── Which store an organization's recordings use ─────────────────────── */

// orgStoreTTL is how long a resolved destination is used before the
// database is asked again: what another replica saved is followed within
// it, and a replay does not cost a query.
const orgStoreTTL = 30 * time.Second

type orgDest struct {
	row    orgStorageRow
	bucket storage.Bucket
}

type orgStoreEntry struct {
	loaded  time.Time
	active  *orgDest
	retired []orgDest
	policy  RecordingPolicy
	err     error
}

// orgStores caches each organization's destinations. Keyed by organization
// id, which only the platform's owners create, so it is bounded by the
// organizations there are.
type orgStores struct {
	mu sync.Mutex
	m  map[string]*orgStoreEntry
}

func (o *orgStores) forget(tid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e := o.m[tid]; e != nil {
		releaseEntry(e)
	}
	delete(o.m, tid)
}

func releaseEntry(e *orgStoreEntry) {
	if e.active != nil {
		storage.Release(e.active.bucket)
	}
	for _, d := range e.retired {
		storage.Release(d.bucket)
	}
}

// orgStore is ctx's organization's own storage, or nil for the default
// organization, which uses the configuration file's.
func (a *API) orgStore(ctx context.Context) *orgStoreEntry {
	tid := tenantIDOf(ctx)
	if tid == "" || tid == DefaultTenantID {
		return nil
	}
	a.orgStorage.mu.Lock()
	defer a.orgStorage.mu.Unlock()
	if e := a.orgStorage.m[tid]; e != nil && time.Since(e.loaded) < orgStoreTTL {
		return e
	}
	e := a.loadOrgStore(ctx)
	if a.orgStorage.m == nil {
		a.orgStorage.m = map[string]*orgStoreEntry{}
	}
	if old := a.orgStorage.m[tid]; old != nil {
		releaseEntry(old)
	}
	a.orgStorage.m[tid] = e
	return e
}

func (a *API) loadOrgStore(ctx context.Context) *orgStoreEntry {
	e := &orgStoreEntry{loaded: time.Now()}
	rows, err := a.store.RecordingStorages(ctx)
	if err != nil {
		e.err = err
		a.log.Error("reading an organization's recording storage failed; using the control plane's",
			"error", err)
		return e
	}
	if e.policy, err = a.store.RecordingPolicyOf(ctx); err != nil {
		a.log.Error("reading an organization's recording policy failed", "error", err)
	}
	for _, r := range rows {
		secret, err := a.openStorageSecret(ctx, r.Sealed)
		if err == nil {
			var b storage.Bucket
			if b, err = storage.Open(r.config(secret), a.log); err == nil && b != nil {
				d := orgDest{row: r, bucket: b}
				if r.State == "active" {
					e.active = &d
				} else {
					e.retired = append(e.retired, d)
				}
				continue
			}
		}
		e.err = err
		a.log.Error("an organization's recording storage cannot be used", "bucket", r.name(), "error", err)
	}
	return e
}

// recordingsFor is the recording store as ctx's organization may see it:
// its own prefix for writes, and a refusal for any key outside it. For an
// organization with its own destination, that destination, in front of
// everywhere its recordings were kept before.
//
// The result is a nil interface when there is no store at all, never a nil
// *storage.Scoped inside one.
func (a *API) recordingsFor(ctx context.Context) storage.Store {
	if s := a.scopedRecordings(ctx); s != nil {
		return s
	}
	return nil
}

// scopedRecordings is recordingsFor as the *storage.Scoped the archive,
// removals and backups need; nil when there is no store at all.
func (a *API) scopedRecordings(ctx context.Context) *storage.Scoped {
	tid := tenantIDOf(ctx)
	e := a.orgStore(ctx)
	if e == nil || (e.active == nil && len(e.retired) == 0) {
		if a.recordings == nil {
			return nil
		}
		return storage.ForTenant(a.recordings, tid)
	}
	var primary storage.Store
	var fallbacks []storage.Store
	if e.active != nil {
		primary = e.active.bucket
	} else if a.recordings != nil {
		primary = a.recordings
	}
	for _, d := range e.retired {
		if primary == nil {
			primary = d.bucket
			continue
		}
		fallbacks = append(fallbacks, d.bucket)
	}
	if e.active != nil && a.recordings != nil {
		fallbacks = append(fallbacks, a.recordings)
	}
	return storage.ForTenant(&storage.Layered{Primary: primary, Fallbacks: fallbacks}, tid)
}

// presignStore is the bucket a direct link for ctx's organization is made
// from: its own when it has one.
func (a *API) presignStore(ctx context.Context) storage.Bucket {
	if e := a.orgStore(ctx); e != nil && e.active != nil {
		return e.active.bucket
	}
	return a.storage
}

// archiveAfterFor is when ctx's organization's recordings are archived.
func (a *API) archiveAfterFor(ctx context.Context) time.Duration {
	if e := a.orgStore(ctx); e != nil {
		switch d := e.policy.ArchiveDays; {
		case d < 0:
			return 0
		case d > 0:
			return time.Duration(d) * 24 * time.Hour
		}
	}
	return a.ArchiveAfter
}

// archiveClassFor is the class or tier ctx's organization archives into:
// its own destination's, or the control plane's.
func (a *API) archiveClassFor(ctx context.Context) string {
	if e := a.orgStore(ctx); e != nil && e.active != nil {
		return e.active.row.ArchiveClass
	}
	return a.ArchiveStorageClass
}

// keepRecordingsFor is how long ctx's organization's servers keep their own
// copy: negative for ever, zero the agent's default.
func (a *API) keepRecordingsFor(ctx context.Context) time.Duration {
	if e := a.orgStore(ctx); e != nil {
		switch d := e.policy.KeepDays; {
		case d < 0:
			return -1
		case d > 0:
			return time.Duration(d) * 24 * time.Hour
		}
	}
	return a.AgentKeepRecordings
}

/* ── Copying out of where recordings were ─────────────────────────────── */

// orgCopy is how far each organization's copy has got, for the console.
type orgCopy struct {
	mu   sync.Mutex
	m    map[string]*RecordingsCopy
	kick chan struct{}
	// done is the active destination (row id and update time) whose copy
	// out of the control plane's storage finished, per organization, so a
	// finished copy is not listed again every pass.
	done map[string]string
}

func (o *orgCopy) get(tid string) *RecordingsCopy {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c := o.m[tid]; c != nil {
		cp := *c
		return &cp
	}
	return nil
}

// kickOrgCopy starts a pass now rather than at the next interval.
func (a *API) kickOrgCopy() {
	a.orgCopies.mu.Lock()
	k := a.orgCopies.kick
	a.orgCopies.mu.Unlock()
	if k == nil {
		return
	}
	select {
	case k <- struct{}{}:
	default:
	}
}

// RunOrgRecordingCopies copies, for every organization with storage of its
// own, what is still where its recordings used to be into where they go
// now: at start, after each change, and every interval until ctx ends.
func (a *API) RunOrgRecordingCopies(ctx context.Context, interval time.Duration) {
	kick := make(chan struct{}, 1)
	a.orgCopies.mu.Lock()
	a.orgCopies.kick = kick
	a.orgCopies.mu.Unlock()
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		_ = a.store.ForEachTenant(ctx, func(tctx context.Context, tn *Tenant) {
			if !tn.IsDefault() && ctx.Err() == nil {
				a.copyOrgRecordings(tctx)
			}
		})
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(interval)
	}
}

// copyOrgRecordings runs one pass for ctx's organization.
func (a *API) copyOrgRecordings(ctx context.Context) {
	tid := tenantIDOf(ctx)
	a.orgStorage.forget(tid) // read what is saved now
	e := a.orgStore(ctx)
	if e == nil || (e.active == nil && len(e.retired) == 0) {
		return
	}
	var dst *storage.Scoped
	var dstName string
	if e.active != nil {
		dst, dstName = storage.ForTenant(e.active.bucket, tid), e.active.row.name()
	} else if a.recordings != nil {
		dst, dstName = storage.ForTenant(a.recordings, tid), "the control plane's storage"
	}
	if dst == nil {
		return
	}
	type source struct {
		name string
		st   *storage.Scoped
		row  *orgStorageRow
	}
	var sources []source
	for i := range e.retired {
		d := e.retired[i]
		sources = append(sources, source{name: d.row.name(), st: storage.ForTenant(d.bucket, tid), row: &d.row})
	}
	doneKey := ""
	if e.active != nil && a.recordings != nil {
		doneKey = e.active.row.ID + "@" + e.active.row.UpdatedAt.String()
		a.orgCopies.mu.Lock()
		finished := a.orgCopies.done[tid] == doneKey
		a.orgCopies.mu.Unlock()
		if !finished {
			sources = append(sources, source{name: "the control plane's storage", st: storage.ForTenant(a.recordings, tid)})
		}
	}

	st := &RecordingsCopy{From: "", Done: true}
	var names []string
	for _, s := range sources {
		names = append(names, s.name)
		res, err := storage.CopyObjects(ctx, s.st, dst, chainHeadOf, a.log)
		st.Copied += res.Copied
		st.Already += res.Already
		st.Failed += res.Failed
		if err != nil {
			st.Done = false
			st.LastError = err.Error()
			continue
		}
		if res.Copied > 0 {
			if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
				Action: "recordings.copied", Severity: "info", ActorEmail: "argus-control", Target: dstName,
				Detail: fmt.Sprintf("Copied %d recordings and backups from %s to %s, where this organization's "+
					"recordings now go. The copies in %s were left in place.", res.Copied, s.name, dstName, s.name),
			}); aerr != nil {
				a.log.Error("audit append failed", "error", aerr)
			}
		}
		switch {
		case s.row != nil:
			// Everything is across: the old destination and its sealed key
			// are no longer needed.
			if err := a.store.dropRetiredStorage(ctx, s.row.ID); err != nil {
				a.log.Error("could not forget a drained recording destination", "error", err)
			}
		case doneKey != "":
			a.orgCopies.mu.Lock()
			if a.orgCopies.done == nil {
				a.orgCopies.done = map[string]string{}
			}
			a.orgCopies.done[tid] = doneKey
			a.orgCopies.mu.Unlock()
		}
	}
	st.From = strings.Join(names, ", ")
	now := time.Now().UTC()
	st.LastPass = &now
	a.orgCopies.mu.Lock()
	if a.orgCopies.m == nil {
		a.orgCopies.m = map[string]*RecordingsCopy{}
	}
	if len(sources) > 0 {
		a.orgCopies.m[tid] = st
	}
	a.orgCopies.mu.Unlock()
	a.orgStorage.forget(tid)
}

/* ── HTTP ─────────────────────────────────────────────────────────────── */

// OrgStorageSettings is an organization's recording storage, as Settings ›
// Storage shows and saves it outside the default organization.
type OrgStorageSettings struct {
	// Storage is the organization's own destination; Enabled false is the
	// control plane's storage.
	Storage StorageSettings `json:"storage"`
	// Archive: AfterDays 0 is the control plane's age, -1 never.
	// StorageClass applies to the organization's own destination only.
	Archive RecordingArchiveSettings `json:"archive"`
	// AgentKeepRecordingsDays: 0 is the control plane's, -1 for ever.
	AgentKeepRecordingsDays int `json:"agentKeepRecordingsDays"`

	// What the control plane does when this organization leaves a setting
	// to it. Read-only, and naming nothing of the platform's.
	Platform OrgStoragePlatform `json:"platform"`
	// Copy is the copy out of where recordings were, while there is one.
	Copy *RecordingsCopy `json:"copy,omitempty"`
	// Moving is how many earlier destinations are still being copied out of.
	Moving int `json:"moving"`
	// Error says why the saved destination cannot be used now.
	Error string `json:"error,omitempty"`
}

// OrgStoragePlatform is what an organization gets from the control plane.
type OrgStoragePlatform struct {
	// Where is "bucket", "directory" or "".
	Where       string `json:"where"`
	KeepDays    int    `json:"keepDays"`
	ArchiveDays int    `json:"archiveDays"`
}

func (a *API) orgStorageAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, "only an admin or owner can see or change where recordings are kept")
		return "", false
	}
	if platformScope(r.Context()) {
		writeErr(w, http.StatusConflict, "the default organization's recordings are kept where the control "+
			"plane's settings say: Settings › Storage")
		return "", false
	}
	return sess.Email, true
}

func (a *API) orgStorageView(ctx context.Context) (OrgStorageSettings, error) {
	rows, err := a.store.RecordingStorages(ctx)
	if err != nil {
		return OrgStorageSettings{}, err
	}
	pol, err := a.store.RecordingPolicyOf(ctx)
	if err != nil {
		return OrgStorageSettings{}, err
	}
	v := OrgStorageSettings{
		AgentKeepRecordingsDays: pol.KeepDays,
		Archive:                 RecordingArchiveSettings{AfterDays: pol.ArchiveDays},
		Storage:                 StorageSettings{Provider: storage.ProviderS3, UseSSL: true},
		Copy:                    a.orgCopies.get(tenantIDOf(ctx)),
	}
	for _, r := range rows {
		if r.State != "active" {
			v.Moving++
			continue
		}
		v.Storage = StorageSettings{Enabled: true, Provider: r.Provider, Endpoint: r.Endpoint, Bucket: r.Bucket,
			Region: r.Region, UseSSL: r.UseSSL, AccessKey: r.AccessKey, SecretKeySet: len(r.Sealed) > 0}
		v.Archive.StorageClass = r.ArchiveClass
	}
	if e := a.orgStore(ctx); e != nil && e.err != nil {
		v.Error = e.err.Error()
	}
	switch a.recordings.(type) {
	case nil:
	case *storage.Dir:
		v.Platform.Where = "directory"
	default:
		v.Platform.Where = "bucket"
	}
	v.Platform.KeepDays = int(a.AgentKeepRecordings / (24 * time.Hour))
	if a.AgentKeepRecordings < 0 {
		v.Platform.KeepDays = -1
	}
	v.Platform.ArchiveDays = int(a.ArchiveAfter / (24 * time.Hour))
	if a.ArchiveAfter <= 0 {
		v.Platform.ArchiveDays = -1
	}
	return v, nil
}

func (a *API) getOrgStorage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.orgStorageAdmin(w, r); !ok {
		return
	}
	v, err := a.orgStorageView(r.Context())
	if err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

// orgDestinationFrom is the destination in, checked. A secret in leaves out
// is cur's, and only for the same place: a key is never carried to another
// bucket.
func (a *API) orgDestinationFrom(ctx context.Context, in StorageSettings, cur *orgStorageRow) (orgStorageRow, string, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = storage.ProviderS3
	}
	if provider != storage.ProviderS3 && provider != storage.ProviderAzure {
		return orgStorageRow{}, "", BadInput(fmt.Errorf("unknown object storage provider %q", in.Provider))
	}
	r := orgStorageRow{Provider: provider, Endpoint: strings.TrimSpace(in.Endpoint),
		Bucket: strings.TrimSpace(in.Bucket), Region: strings.TrimSpace(in.Region), UseSSL: in.UseSSL,
		AccessKey: strings.TrimSpace(in.AccessKey)}
	if r.Bucket == "" {
		if provider == storage.ProviderAzure {
			return r, "", BadInput(errors.New("Azure Blob Storage needs a container"))
		}
		return r, "", BadInput(errors.New("object storage needs a bucket"))
	}
	if r.AccessKey == "" {
		if provider == storage.ProviderAzure {
			return r, "", BadInput(errors.New("Azure Blob Storage needs the storage account's name"))
		}
		return r, "", BadInput(errors.New("object storage needs an access key"))
	}
	if provider == storage.ProviderS3 && r.Endpoint == "" {
		return r, "", BadInput(errors.New("object storage needs an endpoint"))
	}
	secret := ""
	switch {
	case in.SecretKey != nil && strings.TrimSpace(*in.SecretKey) != "":
		secret = strings.TrimSpace(*in.SecretKey)
	case cur != nil && cur.sameDestination(r):
		var err error
		if secret, err = a.openStorageSecret(ctx, cur.Sealed); err != nil {
			return r, "", BadInput(err)
		}
	default:
		if provider == storage.ProviderAzure {
			return r, "", BadInput(errors.New("Azure Blob Storage needs one of the account's access keys"))
		}
		return r, "", BadInput(errors.New("object storage needs a secret key"))
	}
	if _, err := storage.Open(r.config(secret), nil); err != nil {
		return r, "", BadInput(err)
	}
	return r, secret, nil
}

// checkOrgArchiveClass refuses a class the organization's destination does
// not have, or one a recording cannot be replayed from at once. Without a
// destination of its own an organization cannot choose one: the class is
// the control plane's.
func checkOrgArchiveClass(provider, class string, own bool) error {
	if class == "" {
		return nil
	}
	if !own {
		return errors.New("an archive storage class is chosen with a destination of the organization's own")
	}
	if provider == storage.ProviderAzure {
		if !slices.Contains(storage.AzureTiers, class) {
			return fmt.Errorf("%q is not an Azure access tier a recording can be replayed from; use one of %s",
				class, strings.Join(storage.AzureTiers, ", "))
		}
		return nil
	}
	if !ValidArchiveStorageClass(class) {
		return fmt.Errorf("storage class %q cannot be replayed from at once; use one of %s",
			class, strings.Join(ArchiveStorageClasses[1:], ", "))
	}
	return nil
}

func (a *API) putOrgStorage(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.orgStorageAdmin(w, r)
	if !ok {
		return
	}
	var in OrgStorageSettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	bad := func(err error) { writeErr(w, http.StatusBadRequest, err.Error()) }
	if in.AgentKeepRecordingsDays < -1 || in.AgentKeepRecordingsDays > 3650 {
		bad(errors.New("servers keep recordings for 1 to 3650 days, 0 for the control plane's setting or -1 for ever"))
		return
	}
	if in.Archive.AfterDays < -1 || in.Archive.AfterDays > 3650 {
		bad(errors.New("recordings are archived after 1 to 3650 days, 0 for the control plane's setting or -1 for never"))
		return
	}
	provider := in.Storage.Provider
	if provider == "" {
		provider = storage.ProviderS3
	}
	if err := checkOrgArchiveClass(provider, in.Archive.StorageClass, in.Storage.Enabled); err != nil {
		bad(err)
		return
	}
	rows, err := a.store.RecordingStorages(ctx)
	if err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	var cur *orgStorageRow
	if len(rows) > 0 && rows[0].State == "active" {
		cur = &rows[0]
	}

	var next *orgStorageRow
	detail := "Recordings now go to the control plane's storage."
	if in.Storage.Enabled {
		row, secret, err := a.orgDestinationFrom(ctx, in.Storage, cur)
		if err != nil {
			var bi *InputError
			if errors.As(err, &bi) {
				bad(err)
				return
			}
			a.fail(w, "recording storage", err)
			return
		}
		// Proven before it is saved: a destination that cannot be written
		// to would lose every recording made until someone noticed.
		b, _ := storage.Open(row.config(secret), a.log)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = storage.Check(cctx, b)
		cancel()
		storage.Release(b)
		if err != nil {
			bad(fmt.Errorf("not saved: %w", err))
			return
		}
		if row.Sealed, err = a.sealStorageSecret(ctx, secret); err != nil {
			a.fail(w, "recording storage", err)
			return
		}
		row.ArchiveClass = in.Archive.StorageClass
		next = &row
		detail = "Recordings now go to " + row.name() + "."
	}
	moved := (cur == nil) != (next == nil) || (cur != nil && next != nil && !cur.sameDestination(*next))
	if err := a.store.SetRecordingStorage(ctx, next, actor); err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	if err := a.store.SetRecordingPolicy(ctx, RecordingPolicy{KeepDays: in.AgentKeepRecordingsDays,
		ArchiveDays: in.Archive.AfterDays}, actor); err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	a.orgStorage.forget(tenantIDOf(ctx))
	if moved {
		detail += " Recordings kept where they went before are copied across, and replay from there until they are."
		a.orgCopies.mu.Lock()
		delete(a.orgCopies.done, tenantIDOf(ctx))
		a.orgCopies.mu.Unlock()
		a.kickOrgCopy()
	}
	if _, aerr := a.store.AppendAudit(ctx, AuditEvent{
		Action: "recording_storage.changed", Severity: "notice", ActorEmail: actor, Target: "recordings",
		Detail: fmt.Sprintf("Changed where this organization's recordings are kept. %s Servers keep their copy: %s. "+
			"Archive after: %s.", detail, daysText(in.AgentKeepRecordingsDays, "for ever"),
			daysText(in.Archive.AfterDays, "never")),
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	v, err := a.orgStorageView(ctx)
	if err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func daysText(d int, never string) string {
	switch {
	case d < 0:
		return never
	case d == 0:
		return "the control plane's setting"
	case d == 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", d)
}

// postOrgStorageTest writes to and reads from the destination on the form,
// without saving it.
func (a *API) postOrgStorageTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.orgStorageAdmin(w, r); !ok {
		return
	}
	var in StorageSettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := a.store.RecordingStorages(r.Context())
	if err != nil {
		a.fail(w, "recording storage", err)
		return
	}
	var cur *orgStorageRow
	if len(rows) > 0 && rows[0].State == "active" {
		cur = &rows[0]
	}
	row, secret, err := a.orgDestinationFrom(r.Context(), in, cur)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := storage.Open(row.config(secret), a.log)
	defer storage.Release(b)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := storage.Check(ctx, b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
