package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gsoultan/gsmail"
	gsmtp "github.com/gsoultan/gsmail/smtp"
)

// Email delivery methods: an SMTP server, or a Panmail gateway's HTTP API.
const (
	ViaSMTP    = "smtp"
	ViaPanmail = "panmail"
)

// EmailConfig is the `alerts.email` block of the control plane's config.
type EmailConfig struct {
	// Via is how mail leaves: "smtp" (the default) through the server below,
	// or "panmail" through the gateway in Panmail.
	Via string `yaml:"via,omitempty"`

	// Host and Port of the SMTP server. Port 465 is implicit TLS; any other
	// port upgrades with STARTTLS when the server offers it.
	Host string `yaml:"host,omitempty"`
	Port int    `yaml:"port,omitempty"`
	// Username and Password are optional; with neither, no AUTH is sent.
	// Put the password in the environment or a file: "${file:/path}".
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`

	// Panmail is the gateway used when Via is "panmail".
	Panmail *PanmailConfig `yaml:"panmail,omitempty"`

	From string   `yaml:"from,omitempty"`
	To   []string `yaml:"to,omitempty"`
}

// PanmailConfig is a Panmail gateway: where it is, the API key Argus sends
// with, and which of the tenant's providers the mail goes out through.
type PanmailConfig struct {
	// URL is the gateway's origin, "https://mail.example.com".
	URL string `yaml:"url,omitempty"`
	// APIKey needs the email:send scope. Keep it out of the file:
	// "${file:/path}".
	APIKey string `yaml:"api_key,omitempty"`
	// ProviderID is the provider's id from Panmail's Email Providers page.
	ProviderID string `yaml:"provider_id,omitempty"`
}

// Email sends alerts by email, through gsmail: to an SMTP server, or to a
// Panmail gateway.
type Email struct {
	cfg    EmailConfig
	sender gsmail.Sender
}

// NewEmail checks the config and returns a sender.
func NewEmail(cfg EmailConfig) (*Email, error) {
	if cfg.Via == "" {
		cfg.Via = ViaSMTP
	}
	var missing []string
	switch cfg.Via {
	case ViaSMTP:
		if cfg.Port == 0 {
			cfg.Port = 587
		}
		if cfg.Host == "" {
			missing = append(missing, "host")
		}
	case ViaPanmail:
		p := cfg.Panmail
		if p == nil {
			p = &PanmailConfig{}
		}
		if p.URL == "" {
			missing = append(missing, "panmail.url")
		}
		if p.APIKey == "" {
			missing = append(missing, "panmail.api_key")
		}
		if p.ProviderID == "" {
			missing = append(missing, "panmail.provider_id")
		}
	default:
		return nil, fmt.Errorf("alerts.email: via is %q; use %q or %q", cfg.Via, ViaSMTP, ViaPanmail)
	}
	if cfg.From == "" {
		missing = append(missing, "from")
	}
	if len(cfg.To) == 0 {
		missing = append(missing, "to")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("alerts.email: %w: set %s", ErrNotConfigured, strings.Join(missing, ", "))
	}
	for _, a := range append([]string{cfg.From}, cfg.To...) {
		if !singleAddress(a) {
			return nil, fmt.Errorf("alerts.email: %q is not a single email address", a)
		}
	}

	e := &Email{cfg: cfg}
	switch cfg.Via {
	case ViaSMTP:
		if (cfg.Username == "") != (cfg.Password == "") {
			return nil, fmt.Errorf("alerts.email: %w: set both username and password, or neither", ErrNotConfigured)
		}
		s := gsmtp.NewSender(cfg.Host, cfg.Port, cfg.Username, cfg.Password, cfg.Port == 465)
		// A transient failure -- a 4xx, a dropped connection -- is tried
		// again a couple of times; a 5xx is not.
		s.SetRetryConfig(gsmail.RetryConfig{MaxRetries: 2})
		e.sender = s
	case ViaPanmail:
		s, err := newPanmailSender(*cfg.Panmail)
		if err != nil {
			return nil, fmt.Errorf("alerts.email: %w", err)
		}
		e.sender = s
	}
	return e, nil
}

// Name implements Sender.
func (e *Email) Name() string { return "email" }

// Via is how this sender delivers: ViaSMTP or ViaPanmail.
func (e *Email) Via() string { return e.cfg.Via }

// Send implements Sender: one plain-text message to the configured recipients.
func (e *Email) Send(ctx context.Context, subject, body string) error {
	return e.sender.Send(ctx, e.message(e.cfg.To, subject, body, ""))
}

// Recipients is the configured list of addresses every alert goes to.
func (e *Email) Recipients() []string { return append([]string(nil), e.cfg.To...) }

// SendTo sends one message to the given addresses, with an HTML part when html
// is not empty. The plain-text part always carries the whole message, so a
// client that shows only text -- or a lock screen -- loses nothing.
//
// The addresses come from accounts, not from the config file, so each is
// checked here as NewEmail checks the configured ones: a line break in an
// address would otherwise become a header of its own.
func (e *Email) SendTo(ctx context.Context, to []string, subject, text, html string) error {
	if len(to) == 0 {
		return nil
	}
	for _, a := range to {
		if !singleAddress(a) {
			return fmt.Errorf("%q is not a single email address", a)
		}
	}
	return e.sender.Send(ctx, e.message(to, subject, text, html))
}

func singleAddress(a string) bool {
	return !strings.ContainsAny(a, "\r\n<>,") && strings.Contains(a, "@")
}

// message is what gsmail sends: plain text, or plain text and HTML.
//
// The subject carries a hostname and an email address that came from a session
// report. gsmail strips line breaks from every header it writes, and the
// subject is cleaned here as well, so neither path -- SMTP or Panmail's API --
// lets a crafted hostname add recipients or rewrite the message.
func (e *Email) message(to []string, subject, text, html string) gsmail.Email {
	m := gsmail.Email{
		From:    e.cfg.From,
		To:      append([]string(nil), to...),
		Subject: strings.NewReplacer("\r", " ", "\n", " ").Replace(subject),
		Body:    []byte(text),
		Headers: map[string]string{"Auto-Submitted": "auto-generated"},
	}
	if html != "" {
		m.HTMLBody = []byte(html)
	}
	return m
}

// newKey is an idempotency key for one message: the same across every retry
// of that message, different for the next.
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "argus-" + time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(b[:])
}
