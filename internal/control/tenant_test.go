package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/storage"
)

/*
Organizations sharing one control plane.

Every test here is a refusal: something one organization holds that another
must not be able to read, write, or confirm exists. Each one builds a second
organization in the shared test database and checks the default organization
cannot see it, and the reverse.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Tenant
*/

// newTenant creates an organization for a test and removes it after.
func newTenant(t *testing.T, s *Store) (*Tenant, string, string) {
	t.Helper()
	ctx := PlatformContext(context.Background())
	slug := strings.ToLower(unique("org"))
	email, password := slug+"-owner@northwind.id", "correct horse battery staple"
	tn, err := s.CreateTenant(ctx, NewTenant{Slug: slug, Name: "Org " + slug,
		OwnerEmail: email, OwnerName: "Owner", OwnerPassword: password}, "platform@northwind.id")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		s.closeTenants()
		_, _ = s.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+tn.Schema+`" CASCADE`)
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM argus_platform.tenants WHERE id = $1`, tn.ID)
	})
	return tn, email, password
}

func TestTenantCreationBuildsItsOwnSchemaAndChain(t *testing.T) {
	s := testStore(t)
	tn, owner, _ := newTenant(t, s)
	ctx := context.Background()

	if !strings.HasPrefix(tn.Schema, "t_") || len(tn.ID) != 12 {
		t.Fatalf("tenant %+v has an unexpected id or schema", tn)
	}
	// Every migration the default organization has, the new one has too.
	names, _ := migrationNames()
	var applied int
	if err := s.db(WithTenant(ctx, tn)).QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(names) {
		t.Errorf("%d migrations recorded in the new organization, want %d", applied, len(names))
	}
	// The owner exists there, and only there.
	if _, err := s.Account(WithTenant(ctx, tn), owner); err != nil {
		t.Errorf("owner not found in the organization: %v", err)
	}
	if _, err := s.Account(PlatformContext(ctx), owner); err == nil {
		t.Error("the organization's owner is visible as an account of the default organization")
	}
	// Its chain starts with its creation, and verifies on its own.
	events, err := s.AuditEvents(WithTenant(ctx, tn), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "tenant.created" || events[0].PrevHash != GenesisHash {
		t.Errorf("new organization's chain = %+v, want one tenant.created from genesis", events)
	}
	v, err := s.VerifyAuditChain(WithTenant(ctx, tn))
	if err != nil || !v.OK {
		t.Errorf("new organization's chain does not verify: %v %+v", err, v)
	}
	// A second organization cannot take the same short name.
	if _, err := s.CreateTenant(PlatformContext(ctx), NewTenant{Slug: tn.Slug, Name: "again",
		OwnerEmail: "x@northwind.id", OwnerPassword: "correct horse battery staple"}, "p"); err != ErrSlugTaken {
		t.Errorf("duplicate slug: err = %v, want ErrSlugTaken", err)
	}
}

func TestTenantsDoNotSeeEachOthersServersSessionsOrPeople(t *testing.T) {
	s := testStore(t)
	tn, _, _ := newTenant(t, s)
	ctx := context.Background()
	in, out := WithTenant(ctx, tn), PlatformContext(ctx)

	host := unique("shared-name")
	// The same host name in both organizations: each is its own server.
	for _, c := range []context.Context{in, out} {
		if _, err := s.CreateAsset(c, AssetInput{Hostname: host, Address: host, Port: 22,
			Protocol: "ssh", Principals: []string{"ops"}}); err != nil {
			t.Fatalf("CreateAsset: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = s.pool.Exec(ctx, `DELETE FROM assets WHERE hostname = $1`, host) })

	if _, err := s.RecordSession(in, Session{ID: testUUID(), UserEmail: "someone@b.id",
		AssetHostname: host, Principal: "ops", Protocol: "ssh", Origin: "console", State: "active"}); err != nil {
		t.Fatal(err)
	}

	assetsIn, _ := s.Assets(in)
	assetsOut, _ := s.Assets(out)
	countHost := func(list []Asset) int {
		n := 0
		for _, a := range list {
			if a.Hostname == host {
				n++
			}
		}
		return n
	}
	if countHost(assetsIn) != 1 || countHost(assetsOut) != 1 {
		t.Errorf("host appears %d times inside and %d outside; want one each", countHost(assetsIn), countHost(assetsOut))
	}
	sessOut, _ := s.Sessions(out, SessionFilter{Limit: 1000})
	for _, x := range sessOut {
		if x.UserEmail == "someone@b.id" {
			t.Fatal("a session recorded in one organization is listed by the default one")
		}
	}
	sessIn, _ := s.Sessions(in, SessionFilter{Limit: 10})
	if len(sessIn) != 1 {
		t.Errorf("the organization sees %d sessions, want its one", len(sessIn))
	}
	// Its stats are its own.
	st, err := s.Stats(in)
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionsActive != 1 {
		t.Errorf("organization stats say %d active sessions, want 1", st.SessionsActive)
	}
}

func TestStrictStoreRefusesAQueryThatNamesNoTenant(t *testing.T) {
	s := testStore(t)
	s.Strict = true
	t.Cleanup(func() { s.Strict = false })
	if _, err := s.Assets(context.Background()); err == nil {
		t.Fatal("a query with no organization on its context was answered")
	}
	if _, err := s.Assets(PlatformContext(context.Background())); err != nil {
		t.Fatalf("the same query scoped to the platform failed: %v", err)
	}
}

func TestSignedValuesDoNotCrossTenants(t *testing.T) {
	root, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	a, b := root.ForTenant("aaaaaaaaaaaa"), root.ForTenant("bbbbbbbbbbbb")

	cookie, _ := a.IssueSession(auth.Session{Email: "x@a", Role: "owner"}, time.Hour)
	if _, err := b.VerifySession(cookie); err == nil {
		t.Error("a cookie from one organization verified in another")
	}
	if _, err := root.VerifySession(cookie); err == nil {
		t.Error("a cookie from an organization verified in the default one")
	}
	if sess, err := a.VerifySession(cookie); err != nil || sess.Tenant != "aaaaaaaaaaaa" {
		t.Errorf("the issuing organization did not verify its own cookie: %v %+v", err, sess)
	}
	if auth.PeekTenant(cookie) != "aaaaaaaaaaaa" {
		t.Errorf("PeekTenant = %q", auth.PeekTenant(cookie))
	}

	ticket, _ := a.IssueTicket("x@a", "db-01", "ops", time.Minute)
	if _, err := b.RedeemTicket(context.Background(), ticket, "db-01", "ops"); err == nil {
		t.Error("a terminal ticket from one organization opened a host in another")
	}
	if _, err := a.RedeemTicket(context.Background(), ticket, "db-01", "ops"); err != nil {
		t.Errorf("the issuing organization refused its own ticket: %v", err)
	}
	// Burnt once is burnt everywhere: the redemption set is shared.
	if _, err := a.RedeemTicket(context.Background(), ticket, "db-01", "ops"); err != auth.ErrUsed {
		t.Errorf("replay: err = %v, want ErrUsed", err)
	}

	ch, _ := a.IssueMFAChallenge("x@a", time.Minute)
	if _, err := b.RedeemMFAChallenge(ch); err == nil {
		t.Error("a second-factor challenge from one organization was accepted in another")
	}
	// A cookie minted before organizations existed has no org claim and
	// stays valid for the default one.
	old, _ := root.IssueSession(auth.Session{Email: "x@default"}, time.Hour)
	if _, err := root.VerifySession(old); err != nil {
		t.Errorf("a default-organization cookie failed: %v", err)
	}
}

// tenantHarness is the full router with sign-in, for two organizations.
type tenantHarness struct {
	api   *API
	store *Store
	srv   *httptest.Server
}

func newTenantHarness(t *testing.T) *tenantHarness {
	t.Helper()
	store := testStore(t)
	api := NewAPI(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signer, err := auth.NewSigner("a-development-secret-of-sufficient-length")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Close)
	api.SetAuth(signer, "", false)
	api.ReporterToken = testReporterToken
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &tenantHarness{api: api, store: store, srv: srv}
}

// signIn returns a client holding a session in org (blank for the default).
func (h *tenantHarness) signIn(t *testing.T, org, email, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	code, body := h.call(t, c, org, http.MethodPost, "/auth/password",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if code != http.StatusOK {
		t.Fatalf("sign in to %q as %s = %d: %s", org, email, code, body)
	}
	return c
}

func (h *tenantHarness) call(t *testing.T, c *http.Client, org, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if org != "" {
		req.Header.Set(OrgHeader, org)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestConsoleSessionsAreScopedToTheirTenant(t *testing.T) {
	h := newTenantHarness(t)
	tn, owner, pw := newTenant(t, h.store)
	defEmail, defPw := newAccount(t, h.store, "owner")

	// The organization's owner signs in by naming it; a wrong name is refused.
	if code, _ := h.call(t, &http.Client{}, "no-such-org", http.MethodPost, "/auth/password",
		`{"email":"`+owner+`","password":"`+pw+`"}`); code != http.StatusNotFound {
		t.Errorf("sign-in to an unknown organization = %d, want 404", code)
	}
	// Its owner's password does not work in the default organization.
	if code, _ := h.call(t, &http.Client{}, "", http.MethodPost, "/auth/password",
		`{"email":"`+owner+`","password":"`+pw+`"}`); code != http.StatusUnauthorized {
		t.Errorf("an organization's account signed in to the default one: %d", code)
	}
	inB := h.signIn(t, tn.Slug, owner, pw)
	inDef := h.signIn(t, "", defEmail, defPw)

	// Who am I says where I am.
	_, me := h.call(t, inB, "", http.MethodGet, "/auth/me", "")
	var id struct {
		Org           map[string]any `json:"org"`
		PlatformOwner bool           `json:"platformOwner"`
	}
	_ = json.Unmarshal([]byte(me), &id)
	if id.Org["slug"] != tn.Slug || id.PlatformOwner {
		t.Errorf("/auth/me in the organization = %s", me)
	}
	_, me = h.call(t, inDef, "", http.MethodGet, "/auth/me", "")
	_ = json.Unmarshal([]byte(me), &id)
	if id.Org["slug"] != "default" || !id.PlatformOwner {
		t.Errorf("/auth/me in the default organization = %s", me)
	}

	// A server added in the organization is not in the default's list, and
	// the reverse. The cookie decides; the header is ignored once signed in.
	host := unique("b-host")
	if code, body := h.call(t, inB, "", http.MethodPost, "/api/v1/assets",
		`{"hostname":"`+host+`","address":"`+host+`","port":22,"protocol":"ssh","principals":["ops"]}`); code/100 != 2 {
		t.Fatalf("add asset in organization = %d: %s", code, body)
	}
	_, listDef := h.call(t, inDef, tn.Slug, http.MethodGet, "/api/v1/assets", "")
	if strings.Contains(listDef, host) {
		t.Error("the default organization lists a server added in another organization")
	}
	_, listB := h.call(t, inB, "", http.MethodGet, "/api/v1/assets", "")
	if !strings.Contains(listB, host) {
		t.Error("the organization does not list its own server")
	}
	// Audit: the add landed in B's chain, not the platform's.
	_, auditDef := h.call(t, inDef, "", http.MethodGet, "/api/v1/audit?limit=50", "")
	if strings.Contains(auditDef, host) {
		t.Error("the platform's audit log carries another organization's event")
	}

	// The platform routes: 404 for an organization's owner, however senior.
	if code, _ := h.call(t, inB, "", http.MethodGet, "/api/v1/platform/organizations", ""); code != http.StatusNotFound {
		t.Errorf("an organization's owner reached the platform routes: %d", code)
	}
	code, list := h.call(t, inDef, "", http.MethodGet, "/api/v1/platform/organizations", "")
	if code != http.StatusOK || !strings.Contains(list, tn.Slug) {
		t.Errorf("platform owner listing organizations = %d: %s", code, list)
	}
	// The list carries counts, and nothing from inside: not the host's name.
	if strings.Contains(list, host) {
		t.Error("the organizations list names a server inside an organization")
	}

	// Disabled: the organization's cookie stops working at once, and sign-in
	// is refused; the default is untouched. Then it comes back.
	if code, body := h.call(t, inDef, "", http.MethodPost,
		"/api/v1/platform/organizations/"+tn.ID+"/disable", ""); code != http.StatusOK {
		t.Fatalf("disable = %d: %s", code, body)
	}
	if code, _ := h.call(t, inB, "", http.MethodGet, "/api/v1/assets", ""); code != http.StatusForbidden {
		t.Errorf("a disabled organization's session still works: %d", code)
	}
	if code, _ := h.call(t, &http.Client{}, tn.Slug, http.MethodPost, "/auth/password",
		`{"email":"`+owner+`","password":"`+pw+`"}`); code != http.StatusForbidden {
		t.Errorf("sign-in to a disabled organization = %d, want 403", code)
	}
	if code, _ := h.call(t, inDef, "", http.MethodGet, "/api/v1/assets", ""); code != http.StatusOK {
		t.Errorf("disabling one organization broke the default: %d", code)
	}
	if code, _ := h.call(t, inDef, "", http.MethodPost,
		"/api/v1/platform/organizations/"+tn.ID+"/enable", ""); code != http.StatusOK {
		t.Fatalf("enable = %d", code)
	}
	if code, _ := h.call(t, inB, "", http.MethodGet, "/api/v1/assets", ""); code != http.StatusOK {
		t.Errorf("a re-enabled organization's session does not work: %d", code)
	}
	// The default organization cannot be disabled.
	if code, _ := h.call(t, inDef, "", http.MethodPost,
		"/api/v1/platform/organizations/default/disable", ""); code != http.StatusBadRequest {
		t.Errorf("disabling the default organization = %d, want 400", code)
	}
}

func TestAgentCredentialsAreBoundToTheirTenant(t *testing.T) {
	h := newTenantHarness(t)
	tn, owner, pw := newTenant(t, h.store)
	inB := h.signIn(t, tn.Slug, owner, pw)
	ctx := context.Background()
	host := unique("agent")

	// Both organizations know a server by this name.
	for _, c := range []context.Context{WithTenant(ctx, tn), PlatformContext(ctx)} {
		if _, err := h.store.CreateAsset(c, AssetInput{Hostname: host, Address: host, Port: 22,
			Protocol: "ssh", Principals: []string{"ops"}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = h.store.pool.Exec(ctx, `DELETE FROM assets WHERE hostname = $1`, host) })

	// A token minted in the organization carries its prefix, and so does the
	// credential it buys.
	code, body := h.call(t, inB, "", http.MethodPost, "/api/v1/agents/enroll-tokens", `{"hostname":"`+host+`"}`)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, body)
	}
	var minted struct{ Token string }
	_ = json.Unmarshal([]byte(body), &minted)
	if !strings.HasPrefix(minted.Token, enrollTokenPrefix+tn.ID+".") {
		t.Fatalf("token %q does not carry the organization", minted.Token)
	}
	code, body = h.call(t, &http.Client{}, "", http.MethodPost, "/api/v1/agents/enroll",
		`{"token":"`+minted.Token+`","hostname":"`+host+`"}`)
	if code != http.StatusOK {
		t.Fatalf("enroll = %d: %s", code, body)
	}
	var enrolled struct{ Credential string }
	_ = json.Unmarshal([]byte(body), &enrolled)
	if !strings.HasPrefix(enrolled.Credential, agentCredentialPrefix+tn.ID+".") {
		t.Fatalf("credential %q does not carry the organization", enrolled.Credential)
	}

	// Its heartbeat lands on the organization's server, not the default's.
	hb := func(cred string) int {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/v1/report/heartbeat",
			strings.NewReader(`{"hostname":"`+host+`","version":"t-1","active_sessions":0,"exec_tracing":true}`))
		req.Header.Set("Authorization", "Bearer "+cred)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := hb(enrolled.Credential); code != http.StatusOK {
		t.Fatalf("heartbeat = %d", code)
	}
	tracingB, _, _ := h.store.ExecTracingFor(WithTenant(ctx, tn), host)
	tracingDef, _, _ := h.store.ExecTracingFor(PlatformContext(ctx), host)
	if !tracingB || tracingDef {
		t.Errorf("heartbeat landed: organization=%v default=%v; want it in the organization only", tracingB, tracingDef)
	}

	// The same secret with its prefix stripped is nobody's credential: the
	// hash covers the whole string.
	stripped := agentCredentialPrefix + strings.TrimPrefix(enrolled.Credential, agentCredentialPrefix+tn.ID+".")
	if code := hb(stripped); code != http.StatusUnauthorized {
		t.Errorf("a credential re-addressed to the default organization = %d, want 401", code)
	}
	// And one addressed to an organization that does not exist is refused
	// without a lookup.
	if code := hb(agentCredentialPrefix + "zzzzzzzzzzzz.whatever"); code != http.StatusUnauthorized {
		t.Errorf("a credential for an unknown organization = %d, want 401", code)
	}
	// A disabled organization's agents are refused.
	if _, err := h.store.SetTenantDisabled(PlatformContext(ctx), tn.ID, true, "p"); err != nil {
		t.Fatal(err)
	}
	if code := hb(enrolled.Credential); code != http.StatusForbidden {
		t.Errorf("a disabled organization's agent reported: %d", code)
	}
}

func TestRecordingsAreReadWithinTheirTenantOnly(t *testing.T) {
	dir, err := storage.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	write := func(sid string) string {
		p := t.TempDir() + "/" + sid + ".cast"
		if err := writeFile(p, "recording"); err != nil {
			t.Fatal(err)
		}
		return p
	}
	def := storage.ForTenant(dir, "default")
	b := storage.ForTenant(dir, "bbbbbbbbbbbb")
	started := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

	keyDef, err := def.Upload(ctx, write("s1"), "s1", "h", started)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := b.Upload(ctx, write("s2"), "s2", "h", started)
	if err != nil {
		t.Fatal(err)
	}
	if keyDef != "2026/11/02/s1.cast" {
		t.Errorf("default key = %q; an upgraded install must find its recordings where they were", keyDef)
	}
	if keyB != storage.TenantPrefix+"bbbbbbbbbbbb/2026/11/02/s2.cast" {
		t.Errorf("organization key = %q", keyB)
	}
	// Each reads its own and refuses the other's, whatever key a session
	// row might name.
	if _, err := b.Get(ctx, keyDef); err == nil {
		t.Error("an organization read the default organization's recording")
	}
	if _, err := def.Get(ctx, keyB); err == nil {
		t.Error("the default organization read another organization's recording")
	}
	if _, err := storage.ForTenant(dir, "cccccccccccc").Get(ctx, keyB); err == nil {
		t.Error("an organization read another organization's recording")
	}
	if rc, err := b.Get(ctx, keyB); err != nil {
		t.Errorf("an organization cannot read its own recording: %v", err)
	} else {
		rc.Close()
	}
	listB, _ := b.List(ctx)
	if len(listB) != 1 || listB[0].Key != keyB {
		t.Errorf("organization list = %+v", listB)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

// testUUID is a session id in the shape the column takes.
func testUUID() string {
	n := uint64(time.Now().UnixNano())
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", uint32(n), uint16(n>>32), uint16(n>>48)&0xfff,
		uint16(n>>8)&0xfff, n&0xffffffffffff)
}

func TestTenantSchemaMatchesTheDefaultOne(t *testing.T) {
	s := testStore(t)
	tn, _, _ := newTenant(t, s)
	ctx := context.Background()
	norm := func(schema, text string) string {
		return strings.ReplaceAll(text, " "+schema+".", " public.")
	}
	a := schemaIn(t, s, "public")
	b := norm(tn.Schema, schemaIn(t, s, tn.Schema))
	if a != b {
		t.Errorf("an organization's schema differs from the default organization's\n--- default ---\n%s\n--- %s ---\n%s", a, tn.Slug, b)
	}
	// The registry knows the default by the schema the install already used.
	var schema string
	if err := s.pool.QueryRow(ctx, `SELECT schema_name FROM argus_platform.tenants WHERE id = 'default'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != "public" {
		t.Errorf("default organization's schema recorded as %q", schema)
	}
}

// schemaIn is schemaOf for one schema.
func schemaIn(t *testing.T, s *Store, schema string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	rows, err := s.pool.Query(ctx, `
		SELECT table_name, column_name, data_type, is_nullable, coalesce(column_default,'')
		  FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name <> 'schema_migrations'
		 ORDER BY table_name, column_name`, schema)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tbl, col, typ, null, def string
		if err := rows.Scan(&tbl, &col, &typ, &null, &def); err != nil {
			t.Fatal(err)
		}
		// A default that names a sequence carries the schema; strip it.
		def = strings.ReplaceAll(def, schema+".", "")
		fmt.Fprintf(&b, "%s.%s %s null=%s default=%s\n", tbl, col, typ, null, def)
	}
	rows.Close()
	idx, err := s.pool.Query(ctx, `
		SELECT tablename, indexdef FROM pg_indexes
		 WHERE schemaname = $1 AND tablename <> 'schema_migrations'
		 ORDER BY tablename, indexdef`, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for idx.Next() {
		var tbl, def string
		if err := idx.Scan(&tbl, &def); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s: %s\n", tbl, def)
	}
	return b.String()
}
