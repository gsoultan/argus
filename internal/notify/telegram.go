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
