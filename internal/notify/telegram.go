package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// TelegramConfig is the `alerts.telegram` block of the control plane's config.
type TelegramConfig struct {
	// BotToken is the token @BotFather gives you. Keep it out of the file:
	// "${ARGUS_TELEGRAM_TOKEN}" or "${file:/path}".
	BotToken string `yaml:"bot_token,omitempty"`
	// ChatIDs are the chats to post to: a person's numeric id, or a group's
	// (negative) id. The bot must already be in a group to post there.
	ChatIDs []string `yaml:"chat_ids,omitempty"`
}

// Telegram sends alerts through a Telegram bot.
type Telegram struct {
	token   string
	chatIDs []string
	client  *http.Client
	// api is the Bot API's base URL; replaced in tests.
	api string
}

// NewTelegram checks the config and returns a sender.
func NewTelegram(cfg TelegramConfig, client *http.Client) (*Telegram, error) {
	var missing []string
	if cfg.BotToken == "" {
		missing = append(missing, "bot_token")
	}
	if len(cfg.ChatIDs) == 0 {
		missing = append(missing, "chat_ids")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("alerts.telegram: %w: set %s", ErrNotConfigured, strings.Join(missing, ", "))
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Telegram{
		token:   cfg.BotToken,
		chatIDs: cfg.ChatIDs,
		client:  client,
		api:     "https://api.telegram.org",
	}, nil
}

// WithAPI points the sender at another Bot API address: a test server.
func (t *Telegram) WithAPI(base string) *Telegram {
	c := *t
	c.api = strings.TrimRight(base, "/")
	return &c
}

// Name implements Sender.
func (t *Telegram) Name() string { return "telegram" }

// Send implements Sender. The subject is already the body's first line, so
// only the body is posted. Every chat is tried; the error names the ones that
// failed.
func (t *Telegram) Send(ctx context.Context, _, body string) error {
	// Telegram rejects a message over 4096 characters.
	if r := []rune(body); len(r) > 4000 {
		body = string(r[:4000]) + "…"
	}
	var errs []error
	for _, chat := range t.chatIDs {
		if err := t.post(ctx, chat, body); err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", chat, err))
		}
	}
	return errors.Join(errs...)
}

// SendHTML posts a message written in Telegram's HTML subset, with a button
// under it that opens link when link is not empty. Every chat is tried.
//
// Telegram refuses a button whose address it does not consider public -- a
// console on a private name or a bare IP address is common -- and refuses the
// whole message with it. A message that did not arrive because of its button
// is worse than one without, so the refusal is retried with the address as a
// line of text instead.
func (t *Telegram) SendHTML(ctx context.Context, text, linkLabel, link string) error {
	if r := []rune(text); len(r) > 3800 {
		text = string(r[:3800]) + "…"
	}
	var errs []error
	for _, chat := range t.chatIDs {
		err := t.postForm(ctx, chat, text, "HTML", linkLabel, link)
		if err != nil && link != "" && buttonRefused(err) {
			err = t.postForm(ctx, chat, text+"\n\n"+escapeHTML(linkLabel)+": "+escapeHTML(link), "HTML", "", "")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", chat, err))
		}
	}
	return errors.Join(errs...)
}

// ChatIDs are the chats this bot is set up to post to.
func (t *Telegram) ChatIDs() []string { return append([]string(nil), t.chatIDs...) }

// SendHTMLTo is SendHTML to some of the configured chats. A chat that is
// not one of them is never posted to: which chats exist is the operator's
// decision in Settings, and a feature or an item only narrows it. Empty
// chats means all of them.
func (t *Telegram) SendHTMLTo(ctx context.Context, chats []string, text, linkLabel, link string) error {
	only := t.Allowed(chats)
	if len(only) == 0 {
		return errors.New("none of the chosen chats is one Telegram is set up for")
	}
	c := *t
	c.chatIDs = only
	return c.SendHTML(ctx, text, linkLabel, link)
}

// Allowed is chats narrowed to the configured ones, in their order; empty
// chats is every configured chat.
func (t *Telegram) Allowed(chats []string) []string {
	if len(chats) == 0 {
		return t.ChatIDs()
	}
	want := map[string]bool{}
	for _, c := range chats {
		want[strings.TrimSpace(c)] = true
	}
	var out []string
	for _, c := range t.chatIDs {
		if want[c] {
			out = append(out, c)
		}
	}
	return out
}

func buttonRefused(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "button") || strings.Contains(m, "url")
}

func (t *Telegram) post(ctx context.Context, chat, text string) error {
	return t.postForm(ctx, chat, text, "", "", "")
}

func (t *Telegram) postForm(ctx context.Context, chat, text, parseMode, linkLabel, link string) error {
	form := url.Values{
		"chat_id":                  {chat},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	if parseMode != "" {
		form.Set("parse_mode", parseMode)
	}
	if link != "" {
		markup, _ := json.Marshal(map[string]any{
			"inline_keyboard": [][]map[string]string{{{"text": linkLabel, "url": link}}},
		})
		form.Set("reply_markup", string(markup))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.api+"/bot"+t.token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		// The bot token is part of the URL, and net/http puts the URL in its
		// errors. Without this every failed delivery would write the token
		// into the log.
		return t.redact(err)
	}
	defer resp.Body.Close()

	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("telegram answered %s with something that is not JSON", resp.Status)
	}
	if !out.OK {
		return fmt.Errorf("telegram refused the message: %s", out.Description)
	}
	return nil
}

func (t *Telegram) redact(err error) error {
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "<bot-token>"))
}

// TelegramBotInfo is who a token belongs to.
type TelegramBotInfo struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// TelegramChat is a chat a bot can post to.
type TelegramChat struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Kind is private, group, supergroup or channel.
	Kind string `json:"kind,omitempty"`
}

// NewTelegramBot is a sender for an organization's own bot: its token and
// the chats it may post to, without the config file's checks.
func NewTelegramBot(token string, chatIDs []string, client *http.Client) *Telegram {
	if client == nil {
		client = http.DefaultClient
	}
	return &Telegram{token: token, chatIDs: chatIDs, client: client, api: "https://api.telegram.org"}
}

// ValidTelegramToken reports whether s looks like a token from @BotFather:
// digits, a colon, then the secret. It keeps anything else out of a URL.
func ValidTelegramToken(s string) bool {
	id, secret, ok := strings.Cut(s, ":")
	if !ok || id == "" || len(secret) < 20 || len(s) > 200 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range secret {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// Me asks Telegram who the token belongs to, which also proves it works.
func (t *Telegram) Me(ctx context.Context) (TelegramBotInfo, error) {
	var out struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	}
	if err := t.call(ctx, "getMe", nil, &out); err != nil {
		return TelegramBotInfo{}, err
	}
	return TelegramBotInfo{ID: out.ID, Username: out.Username, Name: out.FirstName}, nil
}

// RecentChats are the chats the bot has heard from lately: groups it was
// added to and people who wrote to it. Telegram keeps a day of updates, and
// only while no webhook is set.
func (t *Telegram) RecentChats(ctx context.Context) ([]TelegramChat, error) {
	type chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	type withChat struct {
		Chat *chat `json:"chat"`
	}
	var updates []struct {
		Message           *withChat `json:"message"`
		ChannelPost       *withChat `json:"channel_post"`
		MyChatMember      *withChat `json:"my_chat_member"`
		EditedMessage     *withChat `json:"edited_message"`
		EditedChannelPost *withChat `json:"edited_channel_post"`
	}
	if err := t.call(ctx, "getUpdates", url.Values{"limit": {"100"}, "timeout": {"0"}}, &updates); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []TelegramChat
	for _, u := range updates {
		for _, w := range []*withChat{u.MyChatMember, u.Message, u.ChannelPost, u.EditedMessage, u.EditedChannelPost} {
			if w == nil || w.Chat == nil || seen[w.Chat.ID] {
				continue
			}
			c := w.Chat
			seen[c.ID] = true
			title := c.Title
			if title == "" {
				title = strings.TrimSpace(c.FirstName + " " + c.LastName)
			}
			if title == "" && c.Username != "" {
				title = "@" + c.Username
			}
			out = append(out, TelegramChat{ID: fmt.Sprint(c.ID), Title: title, Kind: c.Type})
		}
	}
	return out, nil
}

// call is one Bot API method whose result is decoded into result.
func (t *Telegram) call(ctx context.Context, method string, form url.Values, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.api+"/bot"+t.token+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("telegram answered %s with something that is not JSON", resp.Status)
	}
	if !out.OK {
		return fmt.Errorf("telegram refused it: %s", out.Description)
	}
	return json.Unmarshal(out.Result, result)
}
