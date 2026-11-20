package control

/*
An organization's own Telegram bots, and which bot each kind of alert uses.

Before this the only bot was the platform's, from the config file, and only
the default organization used it. Now an organization adds its own bots
(token from @BotFather, the chats each may post to) and chooses, per kind of
alert, a bot and some of its chats -- or none. A website can choose its own.

A bot only ever posts to the chats listed on it: an alert's choice narrows
them, never adds one. Tokens are sealed with a key derived from
signing_secret and never leave the control plane again; the console sees the
last three characters.

The platform bot stays: the default organization keeps using it while it has
no default bot of its own, so nothing changes until somebody adds one. Other
organizations cannot use it.
*/

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/notify"
)

// The kinds of alert that can go to Telegram.
const (
	TelegramRouteSignIn  = "signin"
	TelegramRouteServer  = "server"
	TelegramRouteWebsite = "website"
	TelegramRouteBackup  = "backup"
)

// TelegramRouteKinds are the kinds, in the order the console lists them.
var TelegramRouteKinds = []string{TelegramRouteSignIn, TelegramRouteServer, TelegramRouteWebsite, TelegramRouteBackup}

// What an alert's bot can be besides a bot's id.
const (
	BotDefault  = ""
	BotOff      = "off"
	BotPlatform = "platform"
)

const (
	maxTelegramBots  = 20
	maxTelegramChats = 20
)

// TelegramBot is one of the organization's bots. The token is never in it.
type TelegramBot struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Username    string                `json:"username"`
	TokenTail   string                `json:"tokenTail"`
	Chats       []notify.TelegramChat `json:"chats"`
	Default     bool                  `json:"default"`
	LastOKAt    *time.Time            `json:"lastOkAt,omitempty"`
	LastError   string                `json:"lastError,omitempty"`
	LastErrorAt *time.Time            `json:"lastErrorAt,omitempty"`
	CreatedBy   string                `json:"createdBy"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	// UsedBy is what sends through it, in words; filled by the routes.
	UsedBy []string `json:"usedBy"`
}

// TelegramRoute is which bot and chats one kind of alert uses.
type TelegramRoute struct {
	Kind string `json:"kind"`
	// Bot is BotDefault, BotOff, BotPlatform or a bot's id.
	Bot string `json:"bot"`
	// Chats narrows the bot's chats; empty is all of them.
	Chats     []string   `json:"chats"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// TelegramBotInput is a bot as the console sends it. An empty Token on a
// change keeps the one saved.
type TelegramBotInput struct {
	Name    string                `json:"name"`
	Token   string                `json:"token"`
	Chats   []notify.TelegramChat `json:"chats"`
	Default bool                  `json:"default"`
}

// ErrInvalidTelegram is a bot or a choice the console should fix.
type ErrInvalidTelegram struct{ msg string }

func (e ErrInvalidTelegram) Error() string { return e.msg }

// ErrNoTelegramBot is a bot that is not there.
var ErrNoTelegramBot = errors.New("no such Telegram bot")

/* ── Sealing tokens ─────────────────────────────────────────────────────── */

func (a *API) telegramKey(ctx context.Context) ([]byte, error) {
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return nil, ErrInvalidTelegram{"this control plane cannot keep bot tokens safe: set signing_secret in its configuration"}
	}
	sum := sha256.Sum256(append([]byte("argus telegram tokens v1\x00"), a.Tunnels.grantKeyFor(ctx).Seed()...))
	return sum[:], nil
}

func (a *API) sealTelegramToken(ctx context.Context, token string) ([]byte, error) {
	key, err := a.telegramKey(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(token), []byte("telegram-token")), nil
}

func (a *API) openTelegramToken(ctx context.Context, sealed []byte) (string, error) {
	key, err := a.telegramKey(ctx)
	if err != nil {
		return "", err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("the bot's token cannot be read")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("telegram-token"))
	if err != nil {
		return "", errors.New("the bot's token cannot be read; was signing_secret changed? Enter the token again")
	}
	return string(plain), nil
}

/* ── Store ──────────────────────────────────────────────────────────────── */

const telegramBotColumns = `id::text, name, username, token_tail, chats, is_default, last_ok_at, last_error,
	last_error_at, created_by, created_at, updated_at`

func scanTelegramBot(row pgx.Row) (TelegramBot, error) {
	var b TelegramBot
	var chats []byte
	err := row.Scan(&b.ID, &b.Name, &b.Username, &b.TokenTail, &chats, &b.Default, &b.LastOKAt, &b.LastError,
		&b.LastErrorAt, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return b, err
	}
	_ = json.Unmarshal(chats, &b.Chats)
	if b.Chats == nil {
		b.Chats = []notify.TelegramChat{}
	}
	b.UsedBy = []string{}
	return b, nil
}

// TelegramBots are the organization's bots, the default first.
func (s *Store) TelegramBots(ctx context.Context) ([]TelegramBot, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT `+telegramBotColumns+` FROM telegram_bots
		ORDER BY is_default DESC, lower(name), created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramBot{}
	for rows.Next() {
		b, err := scanTelegramBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TelegramBot is one bot and its sealed token.
func (s *Store) TelegramBot(ctx context.Context, id string) (TelegramBot, []byte, error) {
	if !validUUID(id) {
		return TelegramBot{}, nil, ErrNoTelegramBot
	}
	var sealed []byte
	var b TelegramBot
	var chats []byte
	err := s.db(ctx).QueryRow(ctx, `SELECT `+telegramBotColumns+`, token_sealed FROM telegram_bots WHERE id = $1`, id).
		Scan(&b.ID, &b.Name, &b.Username, &b.TokenTail, &chats, &b.Default, &b.LastOKAt, &b.LastError,
			&b.LastErrorAt, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, nil, ErrNoTelegramBot
	}
	if err != nil {
		return b, nil, err
	}
	_ = json.Unmarshal(chats, &b.Chats)
	if b.Chats == nil {
		b.Chats = []notify.TelegramChat{}
	}
	b.UsedBy = []string{}
	return b, sealed, nil
}

// DefaultTelegramBotID is the organization's default bot, or "".
func (s *Store) DefaultTelegramBotID(ctx context.Context) (string, error) {
	var id string
	err := s.db(ctx).QueryRow(ctx, `SELECT id::text FROM telegram_bots WHERE is_default`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// HasTelegramBots reports whether the organization has a bot of its own.
func (s *Store) HasTelegramBots(ctx context.Context) bool {
	var n int
	if err := s.db(ctx).QueryRow(ctx, `SELECT count(*) FROM telegram_bots`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// SaveTelegramBot adds a bot (id empty) or changes one. sealed nil keeps the
// saved token.
func (s *Store) SaveTelegramBot(ctx context.Context, id string, in TelegramBotInput, username, tail string, sealed []byte, by string) (TelegramBot, error) {
	chats, _ := json.Marshal(in.Chats)
	var newID string
	err := pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		if in.Default {
			if _, err := tx.Exec(ctx, `UPDATE telegram_bots SET is_default = false WHERE is_default AND id::text <> $1`, id); err != nil {
				return err
			}
		}
		if id == "" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM telegram_bots`).Scan(&n); err != nil {
				return err
			}
			if n >= maxTelegramBots {
				return ErrInvalidTelegram{fmt.Sprintf("an organization can have at most %d bots", maxTelegramBots)}
			}
			return tx.QueryRow(ctx, `INSERT INTO telegram_bots (name, username, token_sealed, token_tail, chats, is_default, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
				in.Name, username, sealed, tail, chats, in.Default, strings.ToLower(by)).Scan(&newID)
		}
		newID = id
		tag, err := tx.Exec(ctx, `UPDATE telegram_bots SET name = $2, chats = $3, is_default = $4,
			username = CASE WHEN $5::bytea IS NULL THEN username ELSE $6 END,
			token_tail = CASE WHEN $5::bytea IS NULL THEN token_tail ELSE $7 END,
			token_sealed = COALESCE($5::bytea, token_sealed),
			last_error = CASE WHEN $5::bytea IS NULL THEN last_error ELSE '' END,
			last_error_at = CASE WHEN $5::bytea IS NULL THEN last_error_at ELSE NULL END,
			updated_at = now()
			WHERE id = $1`, id, in.Name, chats, in.Default, sealed, username, tail)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNoTelegramBot
		}
		return nil
	})
	if err != nil {
		return TelegramBot{}, err
	}
	b, _, err := s.TelegramBot(ctx, newID)
	return b, err
}

// DeleteTelegramBot removes a bot. What used it moves to moveTo (BotDefault,
// BotOff or another bot's id), with its chats cleared, since they were the
// removed bot's.
func (s *Store) DeleteTelegramBot(ctx context.Context, id, moveTo string) error {
	if !validUUID(id) {
		return ErrNoTelegramBot
	}
	return pgx.BeginFunc(ctx, s.db(ctx), func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM telegram_bots WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNoTelegramBot
		}
		if _, err := tx.Exec(ctx, `UPDATE telegram_routes SET bot = $2, chats = '{}', updated_at = now() WHERE bot = $1`, id, moveTo); err != nil {
			return err
		}
		// A website on its own chats of the removed bot follows the Websites
		// setting, or goes quiet if that was asked.
		mode := TelegramDefault
		if moveTo == BotOff {
			mode = TelegramOff
		}
		if moveTo != BotDefault && moveTo != BotOff {
			_, err = tx.Exec(ctx, `UPDATE website_checks SET telegram_bot = $2, telegram = 'default', telegram_chats = '{}', updated_at = now()
				WHERE telegram_bot = $1`, id, moveTo)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE website_checks SET telegram_bot = '', telegram = $2, telegram_chats = '{}', updated_at = now()
			WHERE telegram_bot = $1`, id, mode)
		return err
	})
}

// NoteTelegramResult records how a bot's last message went.
func (s *Store) NoteTelegramResult(ctx context.Context, id string, sendErr error) {
	if !validUUID(id) {
		return
	}
	var err error
	if sendErr == nil {
		_, err = s.db(ctx).Exec(ctx, `UPDATE telegram_bots SET last_ok_at = now(), last_error = '', last_error_at = NULL WHERE id = $1`, id)
	} else {
		msg := sendErr.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		_, err = s.db(ctx).Exec(ctx, `UPDATE telegram_bots SET last_error = $2, last_error_at = now() WHERE id = $1`, id, msg)
	}
	_ = err
}

// TelegramRoutes is every kind's choice; a kind never chosen uses the
// default bot and all of its chats.
func (s *Store) TelegramRoutes(ctx context.Context) ([]TelegramRoute, error) {
	got := map[string]TelegramRoute{}
	rows, err := s.db(ctx).Query(ctx, `SELECT kind, bot, chats, updated_by, updated_at FROM telegram_routes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r TelegramRoute
		var at time.Time
		if err := rows.Scan(&r.Kind, &r.Bot, &r.Chats, &r.UpdatedBy, &at); err != nil {
			return nil, err
		}
		r.UpdatedAt = &at
		got[r.Kind] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TelegramRoute, 0, len(TelegramRouteKinds))
	for _, k := range TelegramRouteKinds {
		r, ok := got[k]
		if !ok {
			r = TelegramRoute{Kind: k}
		}
		if r.Chats == nil {
			r.Chats = []string{}
		}
		out = append(out, r)
	}
	return out, nil
}

// TelegramRoute is one kind's choice.
func (s *Store) TelegramRoute(ctx context.Context, kind string) (TelegramRoute, error) {
	all, err := s.TelegramRoutes(ctx)
	if err != nil {
		return TelegramRoute{Kind: kind, Chats: []string{}}, err
	}
	for _, r := range all {
		if r.Kind == kind {
			return r, nil
		}
	}
	return TelegramRoute{Kind: kind, Chats: []string{}}, nil
}

// SetTelegramRoute saves one kind's choice.
func (s *Store) SetTelegramRoute(ctx context.Context, r TelegramRoute, by string) error {
	if r.Chats == nil {
		r.Chats = []string{}
	}
	_, err := s.db(ctx).Exec(ctx, `INSERT INTO telegram_routes (kind, bot, chats, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (kind) DO UPDATE SET bot = EXCLUDED.bot, chats = EXCLUDED.chats,
		  updated_by = EXCLUDED.updated_by, updated_at = now()`, r.Kind, r.Bot, r.Chats, strings.ToLower(by))
	return err
}

/* ── Where an alert goes ────────────────────────────────────────────────── */

// telegramTarget is a bot ready to send, and the chats to send to.
type telegramTarget struct {
	bot *notify.Telegram
	// botID is the organization's bot; empty for the platform's.
	botID string
	name  string
	chats []string
}

func (a *API) telegramHTTP() *http.Client {
	if a.telegramClient != nil {
		return a.telegramClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// platformTelegramUsable reports whether this organization may use the
// config file's bot.
func (a *API) platformTelegramUsable(ctx context.Context) bool {
	return a.SignInAlerts.PlatformTelegram() != nil && tenantIDOf(ctx) == DefaultTenantID
}

// telegramBotSender opens one of the organization's bots.
func (a *API) telegramBotSender(ctx context.Context, id string) (*notify.Telegram, TelegramBot, error) {
	b, sealed, err := a.store.TelegramBot(ctx, id)
	if err != nil {
		return nil, b, err
	}
	token, err := a.openTelegramToken(ctx, sealed)
	if err != nil {
		return nil, b, err
	}
	ids := make([]string, 0, len(b.Chats))
	for _, c := range b.Chats {
		ids = append(ids, c.ID)
	}
	t := notify.NewTelegramBot(token, ids, a.telegramHTTP())
	if a.telegramAPI != "" {
		t = t.WithAPI(a.telegramAPI)
	}
	return t, b, nil
}

// telegramFor resolves a choice of bot and chats to something that can
// send, or says why nothing will. noPlatform leaves the config file's bot
// out, for alerts it already gets another way.
func (a *API) telegramFor(ctx context.Context, bot string, chats []string, noPlatform bool) (telegramTarget, string) {
	switch bot {
	case BotOff:
		return telegramTarget{}, "Telegram is off for it"
	case BotDefault:
		id, err := a.store.DefaultTelegramBotID(ctx)
		if err != nil {
			return telegramTarget{}, "the Telegram bots could not be read"
		}
		if id != "" {
			return a.telegramFor(ctx, id, chats, noPlatform)
		}
		if noPlatform || !a.platformTelegramUsable(ctx) {
			return telegramTarget{}, "there is no default Telegram bot: add one under Settings, Telegram bots"
		}
		return a.telegramFor(ctx, BotPlatform, chats, noPlatform)
	case BotPlatform:
		if noPlatform {
			return telegramTarget{}, "the platform bot already gets these through the platform's own alert channels"
		}
		if !a.platformTelegramUsable(ctx) {
			return telegramTarget{}, "the platform bot is not available to this organization"
		}
		t := a.SignInAlerts.PlatformTelegram()
		only := t.Allowed(chats)
		if len(only) == 0 {
			return telegramTarget{}, "none of the chats chosen for it is one the platform bot posts to"
		}
		return telegramTarget{bot: t, name: "Platform bot", chats: only}, ""
	}
	t, b, err := a.telegramBotSender(ctx, bot)
	if errors.Is(err, ErrNoTelegramBot) {
		return telegramTarget{}, "the Telegram bot chosen for it was removed"
	}
	if err != nil {
		return telegramTarget{}, err.Error()
	}
	only := t.Allowed(chats)
	if len(only) == 0 {
		if len(b.Chats) == 0 {
			return telegramTarget{}, b.Name + " has no chats yet"
		}
		return telegramTarget{}, "none of the chats chosen for it is one " + b.Name + " posts to"
	}
	return telegramTarget{bot: t, botID: b.ID, name: b.Name, chats: only}, ""
}

// telegramForKind is where one kind of alert goes.
func (a *API) telegramForKind(ctx context.Context, kind string) (telegramTarget, string) {
	r, err := a.store.TelegramRoute(ctx, kind)
	if err != nil {
		return telegramTarget{}, "the Telegram choices could not be read"
	}
	return a.telegramFor(ctx, r.Bot, r.Chats, kind == TelegramRouteBackup)
}

// sendTelegram posts to a target, now, and records how it went on the bot.
func (a *API) sendTelegram(ctx context.Context, t telegramTarget, text, linkLabel, link string) error {
	err := t.bot.SendHTMLTo(ctx, t.chats, text, linkLabel, link)
	if t.botID != "" {
		a.store.NoteTelegramResult(context.WithoutCancel(ctx), t.botID, err)
	}
	return err
}

// queueTelegram sends in the background, so an alert never waits on
// Telegram. Too many at once are dropped and logged rather than piling up.
func (a *API) queueTelegram(ctx context.Context, t telegramTarget, text, linkLabel, link, what string) {
	if t.bot == nil {
		return
	}
	select {
	case a.telegramSlots <- struct{}{}:
	default:
		a.log.Warn("telegram alert dropped; too many are being sent", "what", what)
		return
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		defer func() { <-a.telegramSlots }()
		sctx, cancel := context.WithTimeout(bg, 30*time.Second)
		defer cancel()
		if err := a.sendTelegram(sctx, t, text, linkLabel, link); err != nil {
			a.log.Warn("telegram alert not delivered", "what", what, "bot", t.name, "error", err)
		}
	}()
}

// routedTelegram is the Telegram half of an administrators' alert of kind,
// for the sign-in notifier: it sends m where the organization chose.
func (a *API) routedTelegram(ctx context.Context, kind, what string) func(notify.Rendered) {
	return func(m notify.Rendered) {
		t, why := a.telegramForKind(ctx, kind)
		if t.bot == nil {
			a.log.Debug("alert not sent to Telegram", "what", what, "why", why)
			return
		}
		a.queueTelegram(ctx, t, m.Telegram, m.LinkLabel, m.Link, what)
	}
}

// telegramText is a title and a detail as a Telegram message.
func telegramText(icon, title, detail string) string {
	return icon + " <b>" + notify.TelegramHTML(title) + "</b>\n\n" + notify.TelegramHTML(detail)
}

// consoleLinkTo is an address in the console, or "" when it is not known.
func (a *API) consoleLinkTo(path string) string {
	base := a.consoleURL
	if base == "" {
		base = a.SignInAlerts.ConsoleURL()
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	return base + path
}

// cleanTelegramChats trims, drops empties and repeats, and keeps the title
// short.
func cleanTelegramChats(in []notify.TelegramChat) ([]notify.TelegramChat, error) {
	out := []notify.TelegramChat{}
	seen := map[string]bool{}
	for _, c := range in {
		c.ID = strings.TrimSpace(c.ID)
		c.Title = strings.TrimSpace(c.Title)
		if c.ID == "" || seen[c.ID] {
			continue
		}
		if !validChatID(c.ID) {
			return nil, ErrInvalidTelegram{fmt.Sprintf("%q is not a Telegram chat ID: use a number such as -1001234567890, or @channelname", c.ID)}
		}
		if len([]rune(c.Title)) > 100 {
			c.Title = string([]rune(c.Title)[:100])
		}
		if c.Title == "" {
			c.Title = c.ID
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	if len(out) > maxTelegramChats {
		return nil, ErrInvalidTelegram{fmt.Sprintf("a bot can have at most %d chats", maxTelegramChats)}
	}
	return out, nil
}

// validChatID is a numeric chat ID or a public @channel.
func validChatID(s string) bool {
	if strings.HasPrefix(s, "@") {
		name := s[1:]
		if len(name) < 4 || len(name) > 64 {
			return false
		}
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
				return false
			}
		}
		return true
	}
	t := strings.TrimPrefix(s, "-")
	if t == "" || len(t) > 20 {
		return false
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// tokenTail is the last three characters of a token, to tell bots apart.
func tokenTail(token string) string {
	if len(token) < 3 {
		return ""
	}
	return token[len(token)-3:]
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// WebsitesWithOwnTelegram are the websites that chose their own bot.
func (s *Store) WebsitesWithOwnTelegram(ctx context.Context) ([]WebsiteCheck, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT id::text, name, telegram_bot FROM website_checks WHERE telegram = 'chats' ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebsiteCheck
	for rows.Next() {
		var c WebsiteCheck
		if err := rows.Scan(&c.ID, &c.Name, &c.TelegramBot); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
