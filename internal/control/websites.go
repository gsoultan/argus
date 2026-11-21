package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Website checks: is a site up, and how much of the time.

An administrator adds an address. Every minute (or five, or ten) one
control server fetches it (tunnel.SiteProbe). The rule this file exists for
is that a site is never called down when it is not, so a failure has to
survive four questions before it counts:

 1. Is it more than a blip? A failed fetch is tried twice more, 20 seconds
    apart, in the same round. Any success makes the round "up".
 2. Is it Argus's own network? A failure Argus's network could cause (DNS,
    connecting, no answer) is checked against canaries: well-known
    addresses and names any working network reaches. If those fail too,
    the round is "unknown": not down, and not counted in uptime either. A
    canary that has never answered here (a network with no internet) is
    no canary, and is ignored.
 3. Does anywhere else agree? The servers chosen for the check (up to
    three, through their agents, tunnel/sitecheck.go) fetch it too. If any
    of them gets the page, the round is "partial": up, with a note that
    Argus's location could not reach it.
 4. Is it still failing a round later? Down needs two failed rounds in a
    row. The outage then starts at the first of them, and the first
    round's "pending" becomes "down" so uptime counts it.

A slow page is not a failure: only no answer within the check's timeout
(30 seconds unless set) is. Going down and coming back each raise an alert
in the bell and on the alert channels, once.

Uptime is seconds up over seconds up or down, from the rounds, so time no
control server was running and "unknown" rounds count neither way.

Rounds are kept websiteRoundsKeep, enough for the 30-day figure; outages
are kept with the check. Deleting a check deletes both, and is audited, as
adding and changing one are.

Across replicas, a replica takes due checks with a lease (FOR UPDATE SKIP
LOCKED), so each round runs once wherever it runs.
*/

const (
	websiteRetries    = 2
	websiteDownAfter  = 2
	websiteLease      = 5 * time.Minute
	websiteTickEvery  = 10 * time.Second
	websiteParallel   = 32
	websiteMaxChecks  = 200
	websiteRoundsKeep = 35 * 24 * time.Hour
	// websiteCanaryFresh is how long a canary answer is reused.
	websiteCanaryFresh = 20 * time.Second
	// websiteCanaryRefresh is how often the canaries are looked at anyway.
	websiteCanaryRefresh = 5 * time.Minute
	// websiteCanaryMemory is how recently a canary must have answered here
	// for its silence to mean Argus's network is down.
	websiteCanaryMemory = 24 * time.Hour
)

// websiteRetryGap is the wait between tries in one round; tests shorten it.
var websiteRetryGap = 20 * time.Second

// Round outcomes, as check_rounds stores them.
const (
	RoundUp      = "up"
	RoundPartial = "partial"
	RoundPending = "pending"
	RoundDown    = "down"
	RoundUnknown = "unknown"
)

// WebsiteCheck is one address Argus checks.
type WebsiteCheck struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	URL            string   `json:"url"`
	ExpectStatus   int      `json:"expectStatus"`
	Keyword        string   `json:"keyword"`
	EverySeconds   int      `json:"everySeconds"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	ConfirmFrom    []string `json:"confirmFrom"`
	Paused         bool     `json:"paused"`
	// Telegram is default (follow the Websites setting), off, or chats:
	// its own bot (TelegramBot) and some of that bot's chats.
	Telegram      string   `json:"telegram"`
	TelegramBot   string   `json:"telegramBot"`
	TelegramChats []string `json:"telegramChats"`
	// What the check sends: Method (GET unless set), Body, and headers. Only
	// the headers' names leave the console; their values are sealed.
	Method        string   `json:"method"`
	Body          string   `json:"body"`
	HeaderNames   []string `json:"headerNames"`
	headersSealed []byte
	CreatedBy     string    `json:"createdBy"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`

	State         string     `json:"state"`
	StateSince    *time.Time `json:"stateSince,omitempty"`
	FailingRounds int        `json:"failingRounds"`
	LastAt        *time.Time `json:"lastAt,omitempty"`
	LastMS        *int       `json:"lastMs,omitempty"`
	LastStatus    *int       `json:"lastStatus,omitempty"`
	LastDetail    string     `json:"lastDetail"`
	// LastOutcome is the last round's: up, partial (only a chosen server
	// reached it), pending, down or unknown.
	LastOutcome  string     `json:"lastOutcome"`
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`
	// GroupID is the Monitoring group it is in, or empty (checks_access.go).
	GroupID string `json:"groupId"`

	// Filled in for the console, not stored on the row.
	Uptime      WebsiteUptime  `json:"uptime"`
	OpenOutage  *WebsiteOutage `json:"openOutage,omitempty"`
	ConfirmHost []string       `json:"confirmHosts"`
	// CanOperate is whether the person asking may check it now and pause it.
	CanOperate bool `json:"canOperate"`
	// Limited is a check shown to someone outside the fleet roles, with what
	// it sends, where it alerts and which servers confirm it left out.
	Limited bool `json:"limited,omitempty"`
}

// WebsiteUptime is the share of time up, 0–100, over three windows; nil
// when nothing was counted in the window.
type WebsiteUptime struct {
	Day   *float64 `json:"day"`
	Week  *float64 `json:"week"`
	Month *float64 `json:"month"`
}

// WebsiteOutage is one confirmed outage.
type WebsiteOutage struct {
	ID        int64      `json:"id"`
	CheckID   string     `json:"checkId"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Cause     string     `json:"cause"`
	Confirmed string     `json:"confirmed"`
}

// WebsiteRound is one round's outcome.
type WebsiteRound struct {
	At      time.Time `json:"at"`
	Secs    int       `json:"secs"`
	Outcome string    `json:"outcome"`
	MS      *int      `json:"ms,omitempty"`
	Status  *int      `json:"status,omitempty"`
	Detail  string    `json:"detail"`
}

// WebsiteInput is what an administrator sets.
type WebsiteInput struct {
	Name           string   `json:"name"`
	URL            string   `json:"url"`
	ExpectStatus   int      `json:"expectStatus"`
	Keyword        string   `json:"keyword"`
	EverySeconds   int      `json:"everySeconds"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	ConfirmFrom    []string `json:"confirmFrom"`
	Paused         bool     `json:"paused"`
	Telegram       string   `json:"telegram"`
	TelegramBot    string   `json:"telegramBot"`
	TelegramChats  []string `json:"telegramChats"`
	Method         string   `json:"method"`
	Body           string   `json:"body"`
	// Headers replaces the saved headers; nil keeps them as they are.
	Headers []WebsiteHeaderInput `json:"headers"`

	// Filled by the API (prepareWebsiteRequest): the headers in the clear,
	// to validate and test with, and as they are stored.
	headers       []tunnel.SiteHeader
	headerNames   []string
	headersSealed []byte
}

// WebsiteHeaderInput is one header in the form. Keep sends the value saved
// for that name, which the console never sees again.
type WebsiteHeaderInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Keep  bool   `json:"keep"`
}

// ErrInvalidWebsite is an input refused, with why.
type ErrInvalidWebsite struct{ Reason string }

func (e ErrInvalidWebsite) Error() string { return e.Reason }

// normalise fills defaults and refuses what cannot be checked.
func (in *WebsiteInput) normalise() error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.Keyword = strings.TrimSpace(in.Keyword)
	if in.URL != "" && !strings.Contains(in.URL, "://") {
		in.URL = "https://" + in.URL
	}
	if in.Name == "" {
		if u, err := url.Parse(in.URL); err == nil {
			in.Name = u.Hostname()
		}
	}
	if in.EverySeconds == 0 {
		in.EverySeconds = 60
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = tunnel.SiteTimeoutDefault
	}
	switch {
	case len(in.Name) > 100:
		return ErrInvalidWebsite{"the name is longer than 100 characters"}
	case in.EverySeconds != 60 && in.EverySeconds != 300 && in.EverySeconds != 600:
		return ErrInvalidWebsite{"check every 1, 5 or 10 minutes"}
	case in.TimeoutSeconds < 5 || in.TimeoutSeconds > tunnel.SiteTimeoutMax:
		return ErrInvalidWebsite{fmt.Sprintf("the timeout must be between 5 and %d seconds", tunnel.SiteTimeoutMax)}
	case len(in.ConfirmFrom) > 3:
		return ErrInvalidWebsite{"choose at most three servers to confirm from"}
	}
	seen := map[string]bool{}
	from := []string{}
	for _, id := range in.ConfirmFrom {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			from = append(from, id)
		}
	}
	in.ConfirmFrom = from
	switch in.Telegram {
	case "":
		in.Telegram = TelegramDefault
	case TelegramDefault, TelegramOff, TelegramChats:
	default:
		return ErrInvalidWebsite{"Telegram is default, off or chats"}
	}
	in.TelegramChats = cleanChats(in.TelegramChats)
	in.TelegramBot = strings.TrimSpace(in.TelegramBot)
	if in.Telegram != TelegramChats {
		in.TelegramChats = []string{}
		in.TelegramBot = BotDefault
	} else if in.TelegramBot != BotDefault && in.TelegramBot != BotPlatform && !validUUID(in.TelegramBot) {
		return ErrInvalidWebsite{"choose one of the Telegram bots"}
	}
	if len(in.TelegramChats) > 20 {
		return ErrInvalidWebsite{"choose at most 20 Telegram chats"}
	}
	in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
	if in.Method == "" {
		in.Method = http.MethodGet
	}
	if strings.TrimSpace(in.Body) == "" {
		in.Body = ""
	}
	if in.headerNames == nil {
		in.headerNames = []string{}
	}
	g := in.grant()
	if err := g.Validate(); err != nil {
		return ErrInvalidWebsite{err.Error()}
	}
	return nil
}

// A check's Telegram choice.
const (
	TelegramDefault = "default"
	TelegramOff     = "off"
	TelegramChats   = "chats"
)

// cleanChats trims, drops empties and repeats.
func cleanChats(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func (in WebsiteInput) grant() tunnel.GrantSite {
	return tunnel.GrantSite{URL: in.URL, Status: in.ExpectStatus, Keyword: in.Keyword, TimeoutSec: in.TimeoutSeconds,
		Method: sentMethod(in.Method), Body: in.Body, Headers: in.headers}
}

// sentMethod leaves GET out of a grant, so a plain check's grant is what it
// always was.
func sentMethod(m string) string {
	if m == http.MethodGet {
		return ""
	}
	return m
}

/* ── What a check sends ─────────────────────────────────────────────────── */

func (a *API) websiteKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidWebsite{"this control plane cannot keep header values safe: set signing_secret in its configuration, or send no headers"}
	}
	sum := sha256.Sum256(append([]byte("argus website headers v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealWebsiteHeaders(ctx context.Context, hs []tunnel.SiteHeader) ([]byte, error) {
	if len(hs) == 0 {
		return nil, nil
	}
	key, err := a.websiteKey(ctx)
	if err != nil {
		return nil, err
	}
	plain, _ := json.Marshal(hs)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("website-headers")), nil
}

// errWebsiteHeaders is headers that were saved but cannot be read now.
var errWebsiteHeaders = errors.New("the check's request headers cannot be read; was signing_secret changed? Enter their values again")

func (a *API) openWebsiteHeaders(ctx context.Context, sealed []byte) ([]tunnel.SiteHeader, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	key, err := a.websiteKey(ctx)
	if err != nil {
		return nil, errWebsiteHeaders
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return nil, errWebsiteHeaders
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("website-headers"))
	if err != nil {
		return nil, errWebsiteHeaders
	}
	var hs []tunnel.SiteHeader
	if err := json.Unmarshal(plain, &hs); err != nil {
		return nil, errWebsiteHeaders
	}
	return hs, nil
}

// websiteGrant is what a round fetches, its headers opened.
func (a *API) websiteGrant(ctx context.Context, c WebsiteCheck) (tunnel.GrantSite, error) {
	hs, err := a.openWebsiteHeaders(ctx, c.headersSealed)
	g := tunnel.GrantSite{URL: c.URL, Status: c.ExpectStatus, Keyword: c.Keyword, TimeoutSec: c.TimeoutSeconds,
		Method: sentMethod(c.Method), Body: c.Body, Headers: hs}
	return g, err
}

// prepareWebsiteRequest works out the headers a saved check will send: the
// form's, with a kept one taking the value saved before under its name. nil
// Headers keeps them all. before is nil for a new check.
func (a *API) prepareWebsiteRequest(ctx context.Context, in *WebsiteInput, before *WebsiteCheck) error {
	var saved []tunnel.SiteHeader
	if before != nil && len(before.headersSealed) > 0 && (in.Headers == nil || slices.ContainsFunc(in.Headers, func(h WebsiteHeaderInput) bool { return h.Keep })) {
		var err error
		if saved, err = a.openWebsiteHeaders(ctx, before.headersSealed); err != nil {
			return ErrInvalidWebsite{err.Error()}
		}
	}
	var hs []tunnel.SiteHeader
	if in.Headers == nil {
		hs = saved
	} else {
		left := map[string][]string{}
		for _, h := range saved {
			k := http.CanonicalHeaderKey(h.Name)
			left[k] = append(left[k], h.Value)
		}
		for _, h := range in.Headers {
			name := strings.TrimSpace(h.Name)
			if name == "" && h.Value == "" && !h.Keep {
				continue
			}
			if err := tunnel.ValidSiteHeaderName(name); err != nil {
				return ErrInvalidWebsite{err.Error()}
			}
			v := h.Value
			if h.Keep {
				k := http.CanonicalHeaderKey(name)
				if len(left[k]) == 0 {
					return ErrInvalidWebsite{"enter a value for " + name}
				}
				v, left[k] = left[k][0], left[k][1:]
			} else if strings.TrimSpace(v) == "" {
				return ErrInvalidWebsite{"enter a value for " + name}
			}
			hs = append(hs, tunnel.SiteHeader{Name: name, Value: v})
		}
	}
	in.headers = hs
	in.headerNames = []string{}
	for _, h := range hs {
		in.headerNames = append(in.headerNames, h.Name)
	}
	in.headersSealed = nil
	if before != nil && in.Headers == nil {
		in.headersSealed = before.headersSealed
		return nil
	}
	sealed, err := a.sealWebsiteHeaders(ctx, hs)
	if err != nil {
		return err
	}
	in.headersSealed = sealed
	return nil
}

/* ── Store ───────────────────────────────────────────────────────────────── */

// A website check is a row of checks (checks.go) of kind website, its own
// settings in the settings column; websiteCols reads them back out in the
// order scanWebsite takes them.
const websiteCols = `id::text, name, settings->>'url', COALESCE((settings->>'expectStatus')::int, 0),
	COALESCE(settings->>'keyword', ''), every_seconds, timeout_seconds,
	confirm_from::text[], paused, telegram, telegram_bot, telegram_chats, COALESCE(settings->>'method', 'GET'),
	COALESCE(settings->>'body', ''), ARRAY(SELECT jsonb_array_elements_text(COALESCE(settings->'headerNames', '[]'::jsonb))),
	secrets_sealed, created_by, created_at, updated_at, state, state_since, failing_rounds,
	last_at, last_ms, last_status, last_detail, last_outcome, expires_at, COALESCE(group_id::text, '')`

// websiteSettings is the settings column for a website check, built from
// the numbered parameters $url..$headerNames of an INSERT or UPDATE.
func websiteSettings(url, status, keyword, method, body, names int) string {
	return fmt.Sprintf(`jsonb_build_object('url', $%d::text, 'expectStatus', $%d::int, 'keyword', $%d::text,
		'method', $%d::text, 'body', $%d::text, 'headerNames', to_jsonb($%d::text[]))`, url, status, keyword, method, body, names)
}

func scanWebsite(row pgx.Row) (WebsiteCheck, error) {
	var c WebsiteCheck
	err := row.Scan(&c.ID, &c.Name, &c.URL, &c.ExpectStatus, &c.Keyword, &c.EverySeconds, &c.TimeoutSeconds,
		&c.ConfirmFrom, &c.Paused, &c.Telegram, &c.TelegramBot, &c.TelegramChats, &c.Method, &c.Body, &c.HeaderNames, &c.headersSealed, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.State, &c.StateSince,
		&c.FailingRounds, &c.LastAt, &c.LastMS, &c.LastStatus, &c.LastDetail, &c.LastOutcome, &c.CertNotAfter, &c.GroupID)
	if c.ConfirmFrom == nil {
		c.ConfirmFrom = []string{}
	}
	if c.TelegramChats == nil {
		c.TelegramChats = []string{}
	}
	if c.HeaderNames == nil {
		c.HeaderNames = []string{}
	}
	c.ConfirmHost = []string{}
	return c, err
}

// ErrNoWebsite is a check that does not exist in this organization.
var ErrNoWebsite = errors.New("no such website check")

// WebsiteChecks lists every check, by name.
func (s *Store) WebsiteChecks(ctx context.Context) ([]WebsiteCheck, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+websiteCols+` FROM checks WHERE kind = 'website' ORDER BY lower(name), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebsiteCheck{}
	for rows.Next() {
		c, err := scanWebsite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WebsiteCheck is one check.
func (s *Store) WebsiteCheck(ctx context.Context, id string) (WebsiteCheck, error) {
	c, err := scanWebsite(s.db(ctx).QueryRow(ctx, `SELECT `+websiteCols+` FROM checks
		WHERE id::text = $1 AND kind = 'website'`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNoWebsite
	}
	return c, err
}

// confirmServersExist refuses a server id that is not an asset here.
func (s *Store) confirmServersExist(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := s.AssetByID(ctx, id); err != nil {
			return ErrInvalidWebsite{"a server chosen to confirm from does not exist"}
		}
	}
	return nil
}

// CreateWebsiteCheck adds a check, due at once.
func (s *Store) CreateWebsiteCheck(ctx context.Context, in WebsiteInput, by string) (WebsiteCheck, error) {
	if err := in.normalise(); err != nil {
		return WebsiteCheck{}, err
	}
	if err := s.confirmServersExist(ctx, in.ConfirmFrom); err != nil {
		return WebsiteCheck{}, err
	}
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM checks`).Scan(&n); err != nil {
		return WebsiteCheck{}, err
	}
	if n >= websiteMaxChecks {
		return WebsiteCheck{}, ErrInvalidWebsite{fmt.Sprintf("an organization can check at most %d websites", websiteMaxChecks)}
	}
	return scanWebsite(s.db(ctx).QueryRow(ctx, `INSERT INTO checks
		(kind, name, settings, every_seconds, timeout_seconds, confirm_from, paused, created_by,
		 telegram, telegram_chats, telegram_bot, secrets_sealed)
		VALUES ('website', $1, `+websiteSettings(2, 3, 4, 13, 14, 15)+`, $5, $6, $7::uuid[], $8, $9, $10, $11, $12, $16)
		RETURNING `+websiteCols,
		in.Name, in.URL, in.ExpectStatus, in.Keyword, in.EverySeconds, in.TimeoutSeconds, in.ConfirmFrom, in.Paused,
		strings.ToLower(by), in.Telegram, in.TelegramChats, in.TelegramBot, in.Method, in.Body, in.headerNames, in.headersSealed))
}

// UpdateWebsiteCheck changes a check. A changed address starts over: its
// state and failing rounds are about the old one, so it is checked at once.
func (s *Store) UpdateWebsiteCheck(ctx context.Context, id string, in WebsiteInput) (before, after WebsiteCheck, err error) {
	if err := in.normalise(); err != nil {
		return before, after, err
	}
	if err := s.confirmServersExist(ctx, in.ConfirmFrom); err != nil {
		return before, after, err
	}
	if before, err = s.WebsiteCheck(ctx, id); err != nil {
		return before, after, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return before, after, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	after, err = scanWebsite(tx.QueryRow(ctx, `UPDATE checks SET name = $2, settings = `+websiteSettings(3, 4, 5, 13, 14, 15)+`,
		every_seconds = $6, timeout_seconds = $7, confirm_from = $8::uuid[], paused = $9,
		telegram = $10, telegram_chats = $11, telegram_bot = $12, secrets_sealed = $16, updated_at = now(),
		state = CASE WHEN settings->>'url' <> $3 THEN 'pending' ELSE state END,
		state_since = CASE WHEN settings->>'url' <> $3 THEN NULL ELSE state_since END,
		failing_rounds = CASE WHEN settings->>'url' <> $3 THEN 0 ELSE failing_rounds END,
		next_at = CASE WHEN settings->>'url' <> $3 OR (paused AND NOT $9) THEN now() ELSE next_at END
		WHERE id::text = $1 AND kind = 'website' RETURNING `+websiteCols,
		id, in.Name, in.URL, in.ExpectStatus, in.Keyword, in.EverySeconds, in.TimeoutSeconds, in.ConfirmFrom, in.Paused,
		in.Telegram, in.TelegramChats, in.TelegramBot, in.Method, in.Body, in.headerNames, in.headersSealed))
	if errors.Is(err, pgx.ErrNoRows) {
		return before, after, ErrNoWebsite
	}
	if err != nil {
		return before, after, err
	}
	if before.URL != after.URL {
		// An outage of the old address is over as far as this check goes.
		if _, err := tx.Exec(ctx, `UPDATE check_outages SET ended_at = now()
			WHERE check_id::text = $1 AND ended_at IS NULL`, id); err != nil {
			return before, after, err
		}
		if _, err := tx.Exec(ctx, `UPDATE check_rounds SET outcome = 'unknown'
			WHERE check_id::text = $1 AND outcome = 'pending'`, id); err != nil {
			return before, after, err
		}
	}
	return before, after, tx.Commit(ctx)
}

// DeleteWebsiteCheck removes a check and its history.
func (s *Store) DeleteWebsiteCheck(ctx context.Context, id string) (WebsiteCheck, error) {
	c, err := scanWebsite(s.db(ctx).QueryRow(ctx, `DELETE FROM checks WHERE id::text = $1 AND kind = 'website'
		RETURNING `+websiteCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNoWebsite
	}
	return c, err
}

// CheckWebsiteNow makes a check due at once.
func (s *Store) CheckWebsiteNow(ctx context.Context, id string) error {
	tag, err := s.db(ctx).Exec(ctx, `UPDATE checks SET next_at = now() WHERE id::text = $1 AND kind = 'website' AND NOT paused`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoWebsite
	}
	return err
}

// TakeDueWebsiteChecks leases up to limit due checks to this replica.
func (s *Store) TakeDueWebsiteChecks(ctx context.Context, limit int) ([]WebsiteCheck, error) {
	rows, err := s.db(ctx).Query(ctx, `UPDATE checks SET lease_until = now() + $2::interval
		WHERE id IN (SELECT id FROM checks
		              WHERE kind = 'website' AND NOT paused AND next_at <= now() AND (lease_until IS NULL OR lease_until < now())
		              ORDER BY next_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+websiteCols, limit, fmt.Sprintf("%d seconds", int(websiteLease.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebsiteCheck{}
	for rows.Next() {
		c, err := scanWebsite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReleaseWebsiteLease gives a check back without a round, to be taken
// again at once.
func (s *Store) ReleaseWebsiteLease(ctx context.Context, id string) error {
	_, err := s.db(ctx).Exec(ctx, `UPDATE checks SET lease_until = NULL WHERE id::text = $1`, id)
	return err
}

// websiteRoundResult is what a round found, before it is recorded.
type websiteRoundResult struct {
	At      time.Time
	Outcome string // up, partial, unknown, or "fail"
	MS      *int
	Status  *int
	Detail  string
	Cert    *time.Time
	// Confirmed names who saw it fail: "Argus" and servers.
	Confirmed string
}

// websiteTransition is a change worth telling anyone about.
type websiteTransition struct {
	WentDown  bool
	CameBack  bool
	Outage    WebsiteOutage
	Check     WebsiteCheck
	Recovered time.Time
}

// RecordWebsiteRound stores a round and moves the check's state, returning
// a transition when it went down or came back.
func (s *Store) RecordWebsiteRound(ctx context.Context, id string, r websiteRoundResult) (*websiteTransition, error) {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanWebsite(tx.QueryRow(ctx, `SELECT `+websiteCols+` FROM checks WHERE id::text = $1 AND kind = 'website' FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // deleted while it was being checked
	}
	if err != nil {
		return nil, err
	}
	state, since, failing := c.State, c.StateSince, c.FailingRounds
	outcome := r.Outcome
	var tr *websiteTransition
	at := r.At

	switch r.Outcome {
	case RoundUp, RoundPartial:
		if state == "down" {
			var o WebsiteOutage
			err := tx.QueryRow(ctx, `UPDATE check_outages SET ended_at = $2
				WHERE check_id::text = $1 AND ended_at IS NULL
				RETURNING id, check_id::text, started_at, ended_at, cause, confirmed`, id, at).
				Scan(&o.ID, &o.CheckID, &o.StartedAt, &o.EndedAt, &o.Cause, &o.Confirmed)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				tr = &websiteTransition{CameBack: true, Outage: o, Recovered: at}
			}
		}
		// Failed rounds that were never confirmed were blips.
		if _, err := tx.Exec(ctx, `UPDATE check_rounds SET outcome = 'up'
			WHERE check_id::text = $1 AND outcome = 'pending'`, id); err != nil {
			return nil, err
		}
		if state != "up" {
			state, since = "up", &at
		}
		failing = 0
	case RoundUnknown:
		// Argus could not tell. A site that was down stays down; one never
		// checked says so; one that was up is shown as not checkable now.
		if state != "down" {
			if state != "unknown" {
				since = &at
			}
			state = "unknown"
		}
	default: // a failure confirmed from everywhere asked
		failing++
		switch {
		case state == "down":
			outcome = RoundDown
		case failing >= websiteDownAfter:
			outcome = RoundDown
			first := at
			var pendingFirst *time.Time
			if err := tx.QueryRow(ctx, `SELECT min(at) FROM check_rounds
				WHERE check_id::text = $1 AND outcome = 'pending'`, id).Scan(&pendingFirst); err != nil {
				return nil, err
			}
			if pendingFirst != nil {
				first = *pendingFirst
			}
			if _, err := tx.Exec(ctx, `UPDATE check_rounds SET outcome = 'down'
				WHERE check_id::text = $1 AND outcome = 'pending'`, id); err != nil {
				return nil, err
			}
			o := WebsiteOutage{CheckID: id, StartedAt: first, Cause: clip(r.Detail, 500), Confirmed: clip(r.Confirmed, 300)}
			if err := tx.QueryRow(ctx, `INSERT INTO check_outages (check_id, started_at, cause, confirmed)
				VALUES ($1::uuid, $2, $3, $4) ON CONFLICT (check_id) WHERE ended_at IS NULL DO NOTHING
				RETURNING id`, id, o.StartedAt, o.Cause, o.Confirmed).Scan(&o.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			state, since = "down", &first
			tr = &websiteTransition{WentDown: true, Outage: o}
		default:
			outcome = RoundPending
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO check_rounds (check_id, at, secs, outcome, ms, status, detail)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		id, at, c.EverySeconds, outcome, r.MS, r.Status, clip(r.Detail, 500)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE checks SET state = $2, state_since = $3, failing_rounds = $4,
		last_at = $5::timestamptz, last_ms = $6, last_status = $7, last_detail = $8,
		expires_at = COALESCE($9, expires_at), last_outcome = $10,
		next_at = $5::timestamptz + make_interval(secs => every_seconds), lease_until = NULL
		WHERE id::text = $1`, id, state, since, failing, at, r.MS, r.Status, clip(r.Detail, 500), r.Cert, outcome); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if tr != nil {
		c.State, c.StateSince = state, since
		tr.Check = c
	}
	return tr, nil
}

// WebsiteRounds is a check's rounds since since, oldest first.
func (s *Store) WebsiteRounds(ctx context.Context, id string, since time.Time) ([]WebsiteRound, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT at, secs, outcome, ms, status, detail FROM check_rounds
		WHERE check_id::text = $1 AND at >= $2 ORDER BY at LIMIT 5000`, id, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebsiteRound{}
	for rows.Next() {
		var r WebsiteRound
		if err := rows.Scan(&r.At, &r.Secs, &r.Outcome, &r.MS, &r.Status, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WebsiteOutages is a check's outages, newest first; an empty id lists
// every check's open ones.
func (s *Store) WebsiteOutages(ctx context.Context, id string, limit int) ([]WebsiteOutage, error) {
	q := `SELECT id, check_id::text, started_at, ended_at, cause, confirmed FROM check_outages
		WHERE check_id::text = $1 ORDER BY started_at DESC LIMIT $2`
	args := []any{id, limit}
	if id == "" {
		q = `SELECT id, check_id::text, started_at, ended_at, cause, confirmed FROM check_outages
			WHERE ended_at IS NULL ORDER BY started_at DESC LIMIT $1`
		args = []any{limit}
	}
	rows, err := s.db(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebsiteOutage{}
	for rows.Next() {
		var o WebsiteOutage
		if err := rows.Scan(&o.ID, &o.CheckID, &o.StartedAt, &o.EndedAt, &o.Cause, &o.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// WebsiteUptimes is every check's uptime at now, by id.
func (s *Store) WebsiteUptimes(ctx context.Context, now time.Time) (map[string]WebsiteUptime, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT check_id::text,
		sum(secs) FILTER (WHERE outcome IN ('up','partial') AND at >= $1::timestamptz - interval '1 day'),
		sum(secs) FILTER (WHERE outcome IN ('up','partial','down') AND at >= $1::timestamptz - interval '1 day'),
		sum(secs) FILTER (WHERE outcome IN ('up','partial') AND at >= $1::timestamptz - interval '7 days'),
		sum(secs) FILTER (WHERE outcome IN ('up','partial','down') AND at >= $1::timestamptz - interval '7 days'),
		sum(secs) FILTER (WHERE outcome IN ('up','partial')),
		sum(secs) FILTER (WHERE outcome IN ('up','partial','down'))
		FROM check_rounds WHERE at >= $1::timestamptz - interval '30 days' GROUP BY check_id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WebsiteUptime{}
	for rows.Next() {
		var id string
		var v [6]*int64
		if err := rows.Scan(&id, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
			return nil, err
		}
		out[id] = WebsiteUptime{Day: share(v[0], v[1]), Week: share(v[2], v[3]), Month: share(v[4], v[5])}
	}
	return out, rows.Err()
}

func share(up, counted *int64) *float64 {
	if counted == nil || *counted == 0 {
		return nil
	}
	var u int64
	if up != nil {
		u = *up
	}
	f := float64(u) * 100 / float64(*counted)
	return &f
}

// pruneWebsiteRounds drops rounds older than websiteRoundsKeep.
func (s *Store) pruneWebsiteRounds(ctx context.Context, now time.Time) error {
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM check_rounds WHERE at < $1`, now.Add(-websiteRoundsKeep))
	return err
}

/* ── The loop ────────────────────────────────────────────────────────────── */

// websiteRunner is this replica's share of the checking.
type websiteRunner struct {
	sem chan struct{}
	wg  sync.WaitGroup
	// any is whether this replica has run a check, so a control plane
	// with none never touches the canaries.
	any atomic.Bool

	canary canaryState
}

// WebsiteCanaries are what any working network reaches; when every one
// fails, a failure is Argus's network, not the site.
type WebsiteCanaries struct {
	// Addrs are host:port to connect to, by address so DNS is not needed.
	Addrs []string
	// Names are looked up to tell whether DNS works.
	Names []string
}

// DefaultWebsiteCanaries are three public resolvers' anycast addresses and
// names, run by three different companies.
var DefaultWebsiteCanaries = WebsiteCanaries{
	Addrs: []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"},
	Names: []string{"one.one.one.one", "dns.google", "dns.quad9.net"},
}

type canaryState struct {
	mu        sync.Mutex
	at        time.Time
	netOK     bool
	dnsOK     bool
	netGood   time.Time // the last time a canary connection worked
	dnsGood   time.Time // the last time a canary name resolved
	lookupNow func(ctx context.Context) (netOK, dnsOK bool)
}

// RunWebsiteChecks runs every organization's due checks until ctx ends,
// then waits, bounded, for rounds under way.
func (a *API) RunWebsiteChecks(ctx context.Context) {
	if a.websites.sem == nil {
		a.websites.sem = make(chan struct{}, websiteParallel)
	}
	t := time.NewTicker(websiteTickEvery)
	defer t.Stop()
	var pruned, canaried time.Time
	for {
		select {
		case <-ctx.Done():
			done := make(chan struct{})
			go func() { a.websites.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			}
			return
		case <-t.C:
		}
		now := time.Now().UTC()
		prune := now.Sub(pruned) >= time.Hour
		if now.Sub(canaried) >= websiteCanaryRefresh && a.websites.any.Load() {
			canaried = now
			go a.refreshCanaries(ctx)
		}
		err := a.store.ForEachTenant(ctx, func(tctx context.Context, _ *Tenant) {
			a.startDueWebsiteChecks(tctx)
			if prune {
				if err := a.store.pruneWebsiteRounds(tctx, now); err != nil {
					a.log.Error("website rounds prune", "error", err)
				}
			}
		})
		if prune {
			pruned = now
		}
		if err != nil {
			a.log.Error("website checks", "error", err)
		}
	}
}

// startDueWebsiteChecks leases what is due in ctx's organization and runs
// each round on its own goroutine, as many as there is room for.
func (a *API) startDueWebsiteChecks(ctx context.Context) {
	free := cap(a.websites.sem) - len(a.websites.sem)
	if free <= 0 {
		return
	}
	list, err := a.store.TakeDueWebsiteChecks(ctx, free)
	if err != nil {
		a.log.Error("website checks: cannot take due checks", "error", err)
		return
	}
	if len(list) > 0 {
		a.websites.any.Store(true)
	}
	for _, c := range list {
		select {
		case a.websites.sem <- struct{}{}:
		default:
			_ = a.store.ReleaseWebsiteLease(ctx, c.ID)
			continue
		}
		a.websites.wg.Add(1)
		go func(c WebsiteCheck) {
			defer a.websites.wg.Done()
			defer func() { <-a.websites.sem }()
			a.runWebsiteRound(ctx, c)
		}(c)
	}
}

// runWebsiteRound checks c once and records what it found, unless the
// round was cut short by a shutdown, which found nothing about the site.
func (a *API) runWebsiteRound(ctx context.Context, c WebsiteCheck) {
	r := a.websiteRound(ctx, c)
	if ctx.Err() != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = a.store.ReleaseWebsiteLease(rctx, c.ID)
		return
	}
	tr, err := a.store.RecordWebsiteRound(ctx, c.ID, r)
	if err != nil {
		a.log.Error("website checks: cannot record a round", "check", c.Name, "error", err)
		return
	}
	if tr != nil {
		a.announceWebsite(ctx, *tr)
	}
}

// websiteRound asks the four questions for one round.
func (a *API) websiteRound(ctx context.Context, c WebsiteCheck) websiteRoundResult {
	r := websiteRoundResult{At: time.Now().UTC()}
	g, err := a.websiteGrant(ctx, c)
	if err != nil {
		// Not the site's failure: sending the request without its headers
		// would be checking something else.
		r.Outcome, r.Detail = RoundUnknown, "Argus could not check: "+err.Error()
		return r
	}
	var res tunnel.SiteResult
	for try := 0; ; try++ {
		res = a.probeSite(ctx, g)
		if res.OK || res.Fail == tunnel.SiteFailBlocked || try >= websiteRetries || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(websiteRetryGap):
		}
	}
	ms := int(res.MS)
	r.MS = &ms
	if res.Status != 0 {
		st := res.Status
		r.Status = &st
	}
	r.Cert = res.CertNotAfter
	if res.OK {
		r.Outcome, r.Detail = RoundUp, res.Detail
		return r
	}
	tries := fmt.Sprintf("%d tries", websiteRetries+1)
	if res.Fail == tunnel.SiteFailBlocked {
		tries = "1 try"
	}
	if tunnel.NetworkFailure(res.Fail) && !a.checkerNetworkOK(ctx, c.URL, res.Fail) {
		r.Outcome = RoundUnknown
		r.Detail = "Argus could not check: its own network is not reaching the internet right now (" + res.Detail + ")"
		return r
	}
	failedFrom := []string{"Argus"}
	notes := []string{}
	for _, op := range a.askConfirmServers(ctx, c, g) {
		switch {
		case op.err != "":
			notes = append(notes, op.host+" could not be asked: "+op.err)
		case op.res.OK:
			r.Outcome = RoundPartial
			r.Detail = fmt.Sprintf("Argus could not reach it (%s), but %s could: %s", res.Detail, op.host, op.res.Detail)
			return r
		default:
			failedFrom = append(failedFrom, op.host)
		}
	}
	r.Outcome = "fail"
	r.Confirmed = strings.Join(failedFrom, ", ")
	r.Detail = fmt.Sprintf("%s (%s from %s)", res.Detail, tries, r.Confirmed)
	if len(failedFrom) > 1 {
		r.Detail = fmt.Sprintf("%s (%s from Argus; also failed from %s)", res.Detail, tries, strings.Join(failedFrom[1:], ", "))
	}
	if len(notes) > 0 {
		r.Detail += ". " + strings.Join(notes, "; ")
	}
	return r
}

// probeSite fetches once from this control server.
func (a *API) probeSite(ctx context.Context, g tunnel.GrantSite) tunnel.SiteResult {
	if a.websiteProbe != nil {
		return a.websiteProbe(ctx, g)
	}
	return tunnel.SiteProbe{UserAgent: "Argus-Website-Check (argus-control " + a.Version + ")"}.Probe(ctx, g)
}

// refreshCanaries looks at the canaries now. The loop does it every few
// minutes whatever the checks found, so that when Argus's network does go,
// the canaries are known to have answered before it did: a canary first
// looked at during the failure would look like one that never answers.
func (a *API) refreshCanaries(ctx context.Context) {
	cs := &a.websites.canary
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.lookLocked(ctx, a)
}

func (cs *canaryState) lookLocked(ctx context.Context, a *API) {
	look := cs.lookupNow
	if look == nil {
		look = a.lookupCanaries
	}
	now := time.Now()
	cs.netOK, cs.dnsOK = look(ctx)
	cs.at = now
	if cs.netOK {
		cs.netGood = now
	}
	if cs.dnsOK {
		cs.dnsGood = now
	}
}

// checkerNetworkOK is whether this control server's own network works for
// a failure of kind fail. It says yes when it cannot tell: a canary that
// has never answered here proves nothing about now.
func (a *API) checkerNetworkOK(ctx context.Context, rawURL, fail string) bool {
	cs := &a.websites.canary
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := time.Now()
	if now.Sub(cs.at) > websiteCanaryFresh {
		cs.lookLocked(ctx, a)
	}
	if fail == tunnel.SiteFailDNS {
		return cs.dnsOK || cs.dnsGood.IsZero() || now.Sub(cs.dnsGood) > websiteCanaryMemory
	}
	// A site on a private address is not reached over the internet, so the
	// internet's canaries say nothing about it.
	if u, err := url.Parse(rawURL); err == nil {
		if ip := net.ParseIP(u.Hostname()); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
			return true
		}
	}
	return cs.netOK || cs.netGood.IsZero() || now.Sub(cs.netGood) > websiteCanaryMemory
}

// lookupCanaries connects to the canary addresses and looks up the
// canary names, all at once, five seconds at most.
func (a *API) lookupCanaries(ctx context.Context) (netOK, dnsOK bool) {
	cv := a.WebsiteCanaries
	if len(cv.Addrs) == 0 && len(cv.Names) == 0 {
		cv = DefaultWebsiteCanaries
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, addr := range cv.Addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var d net.Dialer
			if c, err := d.DialContext(cctx, "tcp", addr); err == nil {
				_ = c.Close()
				mu.Lock()
				netOK = true
				mu.Unlock()
			}
		}()
	}
	for _, name := range cv.Names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if addrs, err := net.DefaultResolver.LookupHost(cctx, name); err == nil && len(addrs) > 0 {
				mu.Lock()
				dnsOK = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return netOK, dnsOK
}

// serverOpinion is what one chosen server saw.
type serverOpinion struct {
	host string
	res  tunnel.SiteResult
	err  string
}

// askConfirmServers asks every chosen server at once.
func (a *API) askConfirmServers(ctx context.Context, c WebsiteCheck, g tunnel.GrantSite) []serverOpinion {
	if len(c.ConfirmFrom) == 0 {
		return nil
	}
	out := make([]serverOpinion, len(c.ConfirmFrom))
	var wg sync.WaitGroup
	for i, id := range c.ConfirmFrom {
		wg.Add(1)
		go func() {
			defer wg.Done()
			asset, err := a.store.AssetByID(ctx, id)
			if err != nil {
				out[i] = serverOpinion{host: "a removed server", err: "it is no longer in Argus"}
				return
			}
			if a.websiteAsk != nil {
				out[i] = a.websiteAsk(ctx, asset.Hostname, g)
				return
			}
			out[i] = a.siteFromServer(ctx, asset.Hostname, g)
		}()
	}
	wg.Wait()
	return out
}

// siteFromServer has the agent on hostname fetch the site.
func (a *API) siteFromServer(ctx context.Context, hostname string, g tunnel.GrantSite) serverOpinion {
	op := serverOpinion{host: hostname}
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		op.err = "this control plane cannot reach agents"
		return op
	}
	agentHost, err := a.agentOf(ctx, hostname)
	if err != nil {
		op.err = "its agent is not known"
		return op
	}
	gr := tunnel.NewGrant("argus-control", agentHost, "sitecheck", "")
	gr.Service = tunnel.SiteCheckService
	gr.Site = &g
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), gr)
	if err != nil {
		op.err = err.Error()
		return op
	}
	octx, cancel := context.WithTimeout(ctx, time.Duration(max(g.TimeoutSec, tunnel.SiteTimeoutDefault)+15)*time.Second)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, agentHost, tunnel.Header{Service: tunnel.SiteCheckService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		op.err = "its agent is not connected"
		return op
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		op.err = "its agent is too old to check websites; update it from its Settings tab"
		return op
	case tunnel.IsRefused(err):
		op.err = open.Error
		return op
	case err != nil:
		op.err = "could not reach its agent"
		return op
	}
	defer st.Close()
	go func() {
		<-octx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			op.err = "its agent went away before it answered"
			return op
		}
		if typ != tunnel.FrameSiteResult {
			continue
		}
		if err := json.Unmarshal(p, &op.res); err != nil {
			op.err = "its agent's answer could not be read"
		} else if g.HasRequest() && op.res.Method == "" {
			// It sent a plain GET, which is not this check.
			op.res = tunnel.SiteResult{}
			op.err = "its agent is too old to send this check's request; update it from its Settings tab"
		}
		return op
	}
}

/* ── Telling people ──────────────────────────────────────────────────────── */

func websiteSubject(id string) string { return "website:" + id }

// announceWebsite raises the alert for a check going down or coming back,
// in the bell and on the alert channels.
func (a *API) announceWebsite(ctx context.Context, tr websiteTransition) {
	c := tr.Check
	link := "/websites?check=" + c.ID
	name := c.Name
	if org := orgLabel(ctx); org != "" {
		name += " (" + org + ")"
	}
	var title, detail, sev string
	switch {
	case tr.WentDown:
		sev = "critical"
		title = name + " is down"
		detail = fmt.Sprintf("%s has not answered since %s UTC: %s. Confirmed over two rounds, with Argus's own network working.",
			c.URL, tr.Outage.StartedAt.UTC().Format("2 Jan 15:04"), tr.Outage.Cause)
	case tr.CameBack:
		sev = "info"
		mins := int(tr.Recovered.Sub(tr.Outage.StartedAt).Round(time.Minute) / time.Minute)
		title = fmt.Sprintf("%s is back up after %d minute%s", name, mins, plural(mins))
		detail = fmt.Sprintf("%s answered again at %s UTC. It was down from %s UTC.", c.URL,
			tr.Recovered.UTC().Format("15:04"), tr.Outage.StartedAt.UTC().Format("2 Jan 15:04"))
		// The "is down" alert is settled now.
		a.closeAlerts(ctx, websiteSubject(c.ID), "Back up at "+tr.Recovered.UTC().Format("15:04")+" UTC")
	default:
		return
	}
	al := ConsoleAlert{Kind: AlertWebsite, Severity: sev, Audience: AudienceAdmins, Title: title, Detail: detail, Link: link}
	if tr.WentDown {
		al.Subject = websiteSubject(c.ID)
	}
	a.raiseAlert(ctx, al)
	a.alertCheckWatchers(ctx, c.ID, al)
	icon := "🔴"
	if tr.CameBack {
		icon = "🟢"
	}
	a.telegramWebsite(ctx, c, icon, title, detail, link)
	a.log.Warn("website "+map[bool]string{true: "down", false: "back up"}[tr.WentDown], "check", c.Name)
}

/* ── Telegram, per feature and per site ──────────────────────────────────── */

// websiteTelegram is where c's alerts go on Telegram, or why nowhere: its
// own bot and chats, or else the Websites choice (telegram_bots.go).
func (a *API) websiteTelegram(ctx context.Context, c WebsiteCheck) (telegramTarget, string) {
	switch c.Telegram {
	case TelegramOff:
		return telegramTarget{}, "Telegram is off for this website"
	case TelegramChats:
		return a.telegramFor(ctx, c.TelegramBot, c.TelegramChats, false)
	}
	t, why := a.telegramForKind(ctx, TelegramRouteWebsite)
	if t.bot == nil && why == "Telegram is off for it" {
		why = "Telegram is off for all websites"
	}
	return t, why
}

// websiteTelegramText is one alert as a Telegram message.
func websiteTelegramText(icon, title, detail string) string { return telegramText(icon, title, detail) }

// telegramWebsite queues c's alert to its chats, if it has any.
func (a *API) telegramWebsite(ctx context.Context, c WebsiteCheck, icon, title, detail, link string) {
	t, why := a.websiteTelegram(ctx, c)
	if t.bot == nil {
		a.log.Debug("website alert not sent to Telegram", "check", c.Name, "why", why)
		return
	}
	a.queueTelegram(ctx, t, websiteTelegramText(icon, title, detail), "Open in Argus",
		a.consoleLinkTo(link), "website "+c.Name)
}
