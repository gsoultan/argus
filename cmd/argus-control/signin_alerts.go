package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/notify"
)

// signInAlertsConfig is `alerts.sign_in`: who is told when someone signs in
// to the console, and when. Absent means the defaults: administrators and the
// person, for a new device or address only. Alerts go only through the email
// and Telegram channels configured beside it; with neither, nothing is sent.
type signInAlertsConfig struct {
	// Admins tells the organization's administrators. Default true.
	Admins *bool `yaml:"admins,omitempty"`
	// Person emails the person who signed in. Default true.
	Person *bool `yaml:"person,omitempty"`
	// When is "new_device" (the default) or "every".
	When string `yaml:"when,omitempty"`
}

func (c *signInAlertsConfig) policy() control.SignInAlertPolicy {
	p := control.DefaultSignInAlertPolicy
	if c == nil {
		return p
	}
	if c.Admins != nil {
		p.Admins = *c.Admins
	}
	if c.Person != nil {
		p.Person = *c.Person
	}
	p.Every = c.When == "every"
	return p
}

func signInConfigOf(cfg *alertsConfig) *signInAlertsConfig {
	if cfg == nil {
		return nil
	}
	return cfg.SignIn
}

// signInNotifier builds the sign-in alert channels: email and Telegram only,
// and only those configured. alertSenders has already refused an incomplete
// one, so an error here is not expected.
func signInNotifier(cfg *alertsConfig, consoleURL string, log *slog.Logger) (*notify.SignInNotifier, error) {
	if cfg == nil {
		return nil, nil
	}
	var (
		email    *notify.Email
		telegram *notify.Telegram
		err      error
	)
	if cfg.Email != nil {
		if email, err = notify.NewEmail(*cfg.Email); err != nil {
			return nil, err
		}
	}
	if cfg.Telegram != nil {
		if telegram, err = notify.NewTelegram(*cfg.Telegram, &http.Client{Timeout: 15 * time.Second}); err != nil {
			return nil, err
		}
	}
	return notify.NewSignInNotifier(email, telegram, consoleURL, log), nil
}
