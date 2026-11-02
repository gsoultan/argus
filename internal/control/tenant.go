package control

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Organizations: several of them sharing one control plane.
//
// Each organization's tables live in a Postgres schema of their own, and the
// store reaches them through a pool whose connections are opened with
// search_path set to that schema and nothing else. Isolation is decided by
// which connection a query runs on, not by a WHERE clause somebody has to
// remember: the SQL in this package does not know organizations exist.
//
// The schema an install already has is the default organization. Upgrading
// moves no row, and an install that never creates a second organization
// behaves exactly as it did. See plans/multi-tenant.md.

// DefaultTenantID names the organization an install had before there were
// several. Its tables are the ones the database already held.
const DefaultTenantID = "default"

// tenantPoolConns bounds each non-default organization's pool. Opened lazily,
// so an organization nobody is using holds no connection at all.
const tenantPoolConns = 4

// Tenant is one organization.
type Tenant struct {
	ID         string     `json:"id"`
	Slug       string     `json:"slug"`
	Name       string     `json:"name"`
	Schema     string     `json:"-"`
	CreatedAt  time.Time  `json:"createdAt"`
	CreatedBy  string     `json:"createdBy"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	DisabledBy *string    `json:"disabledBy,omitempty"`
}

// IsDefault reports whether t is the organization the install started as.
func (t *Tenant) IsDefault() bool { return t == nil || t.ID == DefaultTenantID }

// Disabled reports whether the organization has been switched off.
func (t *Tenant) Disabled() bool { return t != nil && t.DisabledAt != nil }

var (
	// ErrUnknownTenant means no organization has that id or slug.
	ErrUnknownTenant = errors.New("no such organization")
	// ErrTenantDisabled means the organization exists and is switched off.
	ErrTenantDisabled = errors.New("this organization is disabled")
	// ErrSlugTaken means another organization already answers to the slug.
	ErrSlugTaken = errors.New("another organization already uses that short name")
	// errNoTenant is what a query gets when nothing said whose it was.
	errNoTenant = errors.New("refused: the request named no organization")
)

type tenantCtxKey struct{}

// WithTenant scopes ctx to one organization. Everything the store does with
// ctx afterwards reads and writes that organization's tables only.
//
// Only code that has resolved the organization from something it trusts --
// a verified cookie, an agent credential, the registry -- may call this.
func WithTenant(ctx context.Context, t *Tenant) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, t)
}

// PlatformContext scopes ctx to the default organization, for the
// command-line tools and start-up checks that act on the install as a whole.
func PlatformContext(ctx context.Context) context.Context {
	return WithTenant(ctx, defaultTenant())
}

// TenantOf returns the organization ctx is scoped to, or nil.
func TenantOf(ctx context.Context) *Tenant {
	t, _ := ctx.Value(tenantCtxKey{}).(*Tenant)
	return t
}

// tenantIDOf is TenantOf's id, the default when none is set.
func tenantIDOf(ctx context.Context) string {
	if t := TenantOf(ctx); t != nil {
		return t.ID
	}
	return DefaultTenantID
}

func defaultTenant() *Tenant {
	return &Tenant{ID: DefaultTenantID, Slug: DefaultTenantID, Name: "Default organization"}
}

// tenantDB is one organization's connection to its tables, and the state the
// store keeps per organization: the audit chain is written one batch at a
// time per chain, and two organizations' events must never share a batch.
type tenantDB struct {
	id   string
	pool *pgxpool.Pool

	auditMu      sync.Mutex
	auditQueue   []*auditAppend
	auditWriting bool
}

// db returns the pool for ctx's organization.
func (s *Store) db(ctx context.Context) *pgxpool.Pool { return s.tdb(ctx).pool }

// tdb resolves ctx's organization to its connection.
//
// A context that names no organization gets the default one only when the
// store is not strict -- the case for tests that predate organizations and for
// nothing in production. A strict store hands it a closed pool, so the query
// fails rather than reading the default organization's tables on behalf of a
// request that may have been somebody else's.
func (s *Store) tdb(ctx context.Context) *tenantDB {
	t := TenantOf(ctx)
	if t == nil {
		if s.Strict {
			slog.Default().Error("store: a query named no organization and was refused",
				"detail", "every request and background loop must scope its context with WithTenant")
			return s.refused()
		}
		return s.def
	}
	if t.ID == DefaultTenantID {
		return s.def
	}
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if d, ok := s.tenants[t.ID]; ok {
		return d
	}
	d, err := s.openTenantDB(t)
	if err != nil {
		slog.Default().Error("store: cannot open an organization's pool",
			"tenant", t.ID, "error", err)
		return s.refusedLocked()
	}
	s.tenants[t.ID] = d
	return d
}

func (s *Store) refused() *tenantDB {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	return s.refusedLocked()
}

// refusedLocked returns a connection that answers every query with an error.
func (s *Store) refusedLocked() *tenantDB {
	if s.dead != nil {
		return s.dead
	}
	cfg := s.pool.Config()
	cfg.MinConns = 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err == nil {
		p.Close()
	}
	s.dead = &tenantDB{id: "", pool: p}
	return s.dead
}

// openTenantDB builds a pool whose every connection can see one schema.
//
// search_path holds the organization's schema alone. With public after it, a
// table missing from the organization's schema would resolve to the default
// organization's, and the query would succeed with someone else's rows.
func (s *Store) openTenantDB(t *Tenant) (*tenantDB, error) {
	if !validSchemaName(t.Schema) {
		return nil, fmt.Errorf("organization %s has no usable schema", t.ID)
	}
	cfg := s.pool.Config()
	cfg.MaxConns = tenantPoolConns
	cfg.MinConns = 0
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{t.Schema}.Sanitize()
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &tenantDB{id: t.ID, pool: p}, nil
}

var (
	tenantIDPattern   = regexp.MustCompile(`^[a-z0-9]{12}$`)
	tenantSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)
	schemaPattern     = regexp.MustCompile(`^t_[a-z0-9]{12}$`)
)

func validSchemaName(s string) bool { return schemaPattern.MatchString(s) }

// validTenantID is the shape of an id Argus minted. Anything else arriving
// in a credential prefix or a cookie is not looked up at all.
func validTenantID(id string) bool {
	return id == DefaultTenantID || tenantIDPattern.MatchString(id)
}

// ValidTenantSlug reports whether s can name an organization.
func ValidTenantSlug(s string) bool { return tenantSlugPattern.MatchString(s) }

// newTenantID mints an id: twelve characters of [a-z0-9]. Never taken from
// input, because it becomes part of a schema name.
func newTenantID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

/* ── The registry ────────────────────────────────────────────────────────── */

// platformDDL is the organization registry. Idempotent, run on every start
// before any organization's own migrations.
//
// The default organization's row is the whole of the upgrade for an existing
// install: its schema is whichever one the database URL already used.
const platformDDL = `
CREATE SCHEMA IF NOT EXISTS argus_platform;
CREATE TABLE IF NOT EXISTS argus_platform.tenants (
	id          TEXT PRIMARY KEY CHECK (id = 'default' OR id ~ '^[a-z0-9]{12}$'),
	slug        TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$'),
	name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
	schema_name TEXT NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	created_by  TEXT NOT NULL DEFAULT 'system',
	disabled_at TIMESTAMPTZ,
	disabled_by TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS tenants_slug_key ON argus_platform.tenants (slug);
CREATE UNIQUE INDEX IF NOT EXISTS tenants_schema_key ON argus_platform.tenants (schema_name);
INSERT INTO argus_platform.tenants (id, slug, name, schema_name)
VALUES ('default', 'default', 'Default organization', current_schema())
ON CONFLICT (id) DO NOTHING;
`

func (s *Store) migratePlatform(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, platformDDL); err != nil {
		return fmt.Errorf("organization registry: %w", err)
	}
	return nil
}

// migrateTenants brings every other organization's schema up to date, the
// same migrations in the same order as the default organization's.
func (s *Store) migrateTenants(ctx context.Context) error {
	list, err := s.Tenants(ctx)
	if err != nil {
		return err
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for i := range list {
		t := &list[i]
		if t.IsDefault() {
			continue
		}
		d := s.tdb(WithTenant(ctx, t))
		if err := ensureMigrationsTable(ctx, d.pool); err != nil {
			return fmt.Errorf("organization %s: %w", t.Slug, err)
		}
		if err := applyMigrationsOn(ctx, d.pool, names); err != nil {
			return fmt.Errorf("organization %s: %w", t.Slug, err)
		}
	}
	return nil
}

const tenantColumns = `id, slug, name, schema_name, created_at, created_by, disabled_at, disabled_by`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Schema, &t.CreatedAt, &t.CreatedBy,
		&t.DisabledAt, &t.DisabledBy)
	return t, err
}

// Tenants lists every organization, the default first.
func (s *Store) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tenantColumns+`
		FROM argus_platform.tenants ORDER BY (id <> 'default'), lower(name), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TenantByID looks an organization up by the id Argus minted.
func (s *Store) TenantByID(ctx context.Context, id string) (*Tenant, error) {
	if !validTenantID(id) {
		return nil, ErrUnknownTenant
	}
	t, err := scanTenant(s.pool.QueryRow(ctx, `SELECT `+tenantColumns+`
		FROM argus_platform.tenants WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnknownTenant
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TenantBySlug looks an organization up by the short name people type.
func (s *Store) TenantBySlug(ctx context.Context, slug string) (*Tenant, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		slug = DefaultTenantID
	}
	if !ValidTenantSlug(slug) {
		return nil, ErrUnknownTenant
	}
	t, err := scanTenant(s.pool.QueryRow(ctx, `SELECT `+tenantColumns+`
		FROM argus_platform.tenants WHERE slug = $1`, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnknownTenant
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MultiTenant reports whether more than the default organization exists.
// Until one does, the console asks nobody which organization they are in.
func (s *Store) MultiTenant(ctx context.Context) (bool, error) {
	var many bool
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) > 1 FROM argus_platform.tenants`).Scan(&many)
	return many, err
}

// NewTenant is what creating an organization takes.
type NewTenant struct {
	Slug          string
	Name          string
	OwnerEmail    string
	OwnerName     string
	OwnerPassword string
}

// CreateTenant registers an organization, builds its schema with every
// migration, and gives it its first owner.
//
// The registry row, the schema and its tables are one transaction: an
// organization exists whole or not at all. The owner and the first audit
// event are written through the organization's own pool afterwards, so they
// land in its tables and start its chain.
func (s *Store) CreateTenant(ctx context.Context, in NewTenant, by string) (*Tenant, error) {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Name = strings.TrimSpace(in.Name)
	if !ValidTenantSlug(in.Slug) {
		return nil, errors.New("the short name must be 2 to 40 lowercase letters, digits or hyphens")
	}
	if in.Name == "" || len([]rune(in.Name)) > 120 {
		return nil, errors.New("the name must be between 1 and 120 characters")
	}
	id, err := newTenantID()
	if err != nil {
		return nil, err
	}
	t := &Tenant{ID: id, Slug: in.Slug, Name: in.Name, Schema: "t_" + id, CreatedBy: by}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `
		INSERT INTO argus_platform.tenants (id, slug, name, schema_name, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING created_at`, t.ID, t.Slug, t.Name, t.Schema, by).Scan(&t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSlugTaken
	}
	if err != nil {
		return nil, err
	}
	schema := pgx.Identifier{t.Schema}.Sanitize()
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// SET LOCAL ends with the transaction, so the default pool's connection
	// goes back with its own search_path.
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, migrationsTableDDL); err != nil {
		return nil, err
	}
	for _, name := range names {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	tctx := WithTenant(ctx, t)
	if _, err := s.CreateAccount(tctx, in.OwnerEmail, in.OwnerName, "owner", in.OwnerPassword); err != nil {
		return t, fmt.Errorf("the organization was created but its owner was not: %w", err)
	}
	if _, err := s.AppendAudit(tctx, AuditEvent{
		Action: "tenant.created", Severity: "notice", ActorEmail: by, Target: t.Slug,
		Detail: "Organization " + t.Name + " (" + t.Slug + ") created by the platform, with " +
			normaliseEmail(in.OwnerEmail) + " as its first owner.",
	}); err != nil {
		return t, err
	}
	return t, nil
}

// SetTenantDisabled switches an organization off or back on. Never the
// default organization: it holds the platform's own administrators, and
// disabling it would lock everyone out of the switch that turns it back on.
func (s *Store) SetTenantDisabled(ctx context.Context, id string, disabled bool, by string) (*Tenant, error) {
	if id == DefaultTenantID {
		return nil, errors.New("the default organization cannot be disabled")
	}
	var q string
	if disabled {
		q = `UPDATE argus_platform.tenants SET disabled_at = COALESCE(disabled_at, now()),
		        disabled_by = $2 WHERE id = $1 RETURNING ` + tenantColumns
	} else {
		q = `UPDATE argus_platform.tenants SET disabled_at = NULL, disabled_by = NULL
		      WHERE id = $1 AND $2::text IS NOT NULL RETURNING ` + tenantColumns
	}
	t, err := scanTenant(s.pool.QueryRow(ctx, q, id, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnknownTenant
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TenantCounts is what the organizations list says about each one, without
// reading anything else out of it.
type TenantCounts struct {
	Servers int `json:"servers"`
	People  int `json:"people"`
}

// CountsFor reads two numbers from an organization's own tables.
func (s *Store) CountsFor(ctx context.Context, t *Tenant) (TenantCounts, error) {
	var c TenantCounts
	err := s.db(WithTenant(ctx, t)).QueryRow(ctx, `
		SELECT (SELECT count(*) FROM assets WHERE archived_at IS NULL),
		       (SELECT count(*) FROM users WHERE NOT disabled)`).Scan(&c.Servers, &c.People)
	return c, err
}

// ForEachTenant runs fn once per enabled organization, with ctx scoped to it.
// For the sweeps that used to run once per install.
func (s *Store) ForEachTenant(ctx context.Context, fn func(ctx context.Context, t *Tenant)) error {
	list, err := s.Tenants(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		t := &list[i]
		if t.Disabled() {
			continue
		}
		if t.IsDefault() {
			t = defaultTenantFrom(t)
		}
		fn(WithTenant(ctx, t), t)
	}
	return nil
}

// defaultTenantFrom keeps the registry's name for the default organization
// while making sure its id is the constant every check compares against.
func defaultTenantFrom(t *Tenant) *Tenant {
	c := *t
	c.ID = DefaultTenantID
	return &c
}

// closeTenants releases every organization's pool.
func (s *Store) closeTenants() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	for id, d := range s.tenants {
		d.pool.Close()
		delete(s.tenants, id)
	}
}
