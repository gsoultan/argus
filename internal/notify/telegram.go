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
	BotToken string `yaml:"bot_token"`
	// ChatIDs are the chats to post to: a person's numeric id, or a group's
	// (negative) id. The bot must already be in a group to post there.
	ChatIDs []string `yaml:"chat_ids"`
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

func (t *Telegram) post(ctx context.Context, chat, text string) error {
	form := url.Values{
		"chat_id":                  {chat},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
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
