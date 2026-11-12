package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/gsoultan/argus/internal/control/confexpand"
	"github.com/gsoultan/argus/internal/control/servertls"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/notify"
	"github.com/gsoultan/argus/internal/storage"
)

// settingsBackend is the console's Settings page's view of the
// configuration file; see internal/control/settings_routes.go.
type settingsBackend struct {
	loc configLocation
	// cfg is the configuration this process is running, references resolved.
	cfg config
	// reload asks the main loop to restart the control plane on the new file.
	reload func()

	mu sync.Mutex
}

// lastApplyError is why the previous save could not be applied, if it could
// not. The main loop sets it after restoring the configuration that worked.
var (
	lastApplyMu    sync.Mutex
	lastApplyError string
)

func setApplyError(msg string) {
	lastApplyMu.Lock()
	lastApplyError = msg
	lastApplyMu.Unlock()
}

func (b *settingsBackend) Settings() (control.Settings, error) {
	c := b.cfg
	s := control.Settings{
		ConfigFile:     b.loc.Managed,
		ConsoleURL:     c.ConsoleURL,
		Listen:         c.Listen,
		ConsoleListen:  c.ConsoleListen,
		BehindTLSProxy: c.BehindTLSProxy,
		AllowedOrigins: nonNil(c.AllowedOrigins),
		TrustedProxies: []string{},
		LogLevel:       c.LogLevel,

		AgentKeepRecordingsDays: c.AgentKeepRecordingsDays,
		Archive: &control.RecordingArchiveSettings{AfterDays: c.RecordingsArchiveAfterDays,
			StorageClass: c.RecordingsArchiveStorageClass},
	}
	lastApplyMu.Lock()
	s.ApplyError = lastApplyError
	lastApplyMu.Unlock()
	if c.Console != nil {
		s.FleetURL = c.Console.FleetURL
	}
	if c.Tunnel != nil {
		s.QUICListen = c.Tunnel.QUICListen
	}
	if rl := c.RateLimit; rl != nil {
		s.TrustedProxies = nonNil(rl.TrustedProxies)
		s.LoginPerMinute, s.FailuresPerHour, s.TicketsPerMinute =
			rl.LoginPerMinute, rl.FailuresPerHour, rl.TicketsPerMinute
	}
	s.SessionTTL = durationText(c.SessionTTL)
	s.TicketTTL = durationText(c.TicketTTL)
	s.AgentStaleAfter = durationText(c.AgentStaleAfter)

	if d, ok := control.ParseDatabaseURL(c.DatabaseURL); ok {
		s.Database = control.DatabaseSettings{Host: d.Host, Port: d.Port, Name: d.Name,
			User: d.User, SSLMode: d.SSLMode, PasswordSet: databasePassword(c.DatabaseURL) != ""}
	} else {
		s.Database = control.DatabaseSettings{Raw: true, PasswordSet: true}
	}
	if st := c.Storage; st != nil {
		s.Storage = control.StorageSettings{Enabled: true, Endpoint: st.Endpoint, Bucket: st.Bucket,
			Region: st.Region, UseSSL: st.UseSSL, AccessKey: st.AccessKey, SecretKeySet: st.SecretKey != ""}
	}
	if a := c.Alerts; a != nil {
		if e := a.Email; e != nil {
			s.Email = control.EmailSettings{Enabled: true, Via: e.Via, Host: e.Host, Port: e.Port, Username: e.Username,
				PasswordSet: e.Password != "", From: e.From, To: nonNil(e.To)}
			if p := e.Panmail; p != nil {
				s.Email.Panmail = control.PanmailSettings{URL: p.URL, ProviderID: p.ProviderID, APIKeySet: p.APIKey != ""}
			}
		}
		if t := a.Telegram; t != nil {
			s.Telegram = control.TelegramSettings{Enabled: true, BotTokenSet: t.BotToken != "",
				ChatIDs: nonNil(t.ChatIDs)}
		}
		if f := a.FCM; f != nil {
			s.FCM = control.FCMSettings{Enabled: true, CredentialsSet: f.CredentialsFile != "",
				ProjectID: f.ProjectID, Topic: f.Topic, Tokens: nonNil(f.Tokens)}
		}
	}
	pol := signInConfigOf(c.Alerts).policy()
	s.SignInAlerts = &control.SignInAlertSettings{Admins: pol.Admins, Person: pol.Person, When: "new_device",
		Location: string(signInConfigOf(c.Alerts).locationMode())}
	if pol.Every {
		s.SignInAlerts.When = "every"
	}
	s.ServerAlerts = &control.ServerAlertSettings{Connected: string(control.ServerAlertFirst)}
	if c.Alerts != nil && c.Alerts.ServerConnected != "" {
		s.ServerAlerts.Connected = c.Alerts.ServerConnected
	}
	s.Security = &control.SecuritySettings{RequireMFA: c.RequireMFA,
		TemporaryPasswordTTL: durationText(c.TemporaryPasswordTTL), AllowBrowserChange: c.AllowBrowserChange}
	if s.Security.RequireMFA == "" {
		s.Security.RequireMFA = string(control.MFAAll)
	}
	if rl := c.RateLimit; rl != nil {
		s.Security.AccountFailuresPerHour = rl.AccountFailuresPerHour
		s.Security.AccountCodeFailuresPerHour = rl.AccountCodeFailuresPerHour
	}
	if s.Email.Via == "" {
		s.Email.Via = notify.ViaSMTP
	}
	s.Email.To, s.Telegram.ChatIDs, s.FCM.Tokens = nonNil(s.Email.To), nonNil(s.Telegram.ChatIDs), nonNil(s.FCM.Tokens)

	s.Cloud = cloudSettingsOf(c.Cloud)

	pki := b.loc.pki()
	s.TLS.CanIssue = pki.HasCAKey()
	if ca, err := os.ReadFile(pki.CACert()); err == nil {
		s.TLS.CACert = string(ca)
	}
	if t := c.TLS; t != nil {
		s.TLS.Enabled = true
		s.TLS.MutualTLS = t.ClientCAFile != ""
		if sum, err := servertls.Summarize(t.CertFile); err == nil {
			s.TLS.Certificate = &sum
		}
		s.TLS.Managed = filepath.Clean(t.CertFile) == pki.ServerCert() && pki.IssuesServer()
	}
	return s, nil
}

// SaveSettings applies in to the file on disk and asks for a reload.
//
// Nothing is written until the whole result has been checked, and secrets are
// written under new names so a save that is refused never changes a secret
// the running configuration refers to.
func (b *settingsBackend) SaveSettings(ctx context.Context, in control.Settings, actor string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	before, err := readRawConfig(b.loc.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", b.loc.Path, err)
	}
	next, err := cloneConfig(before)
	if err != nil {
		return nil, err
	}
	st := &secretStage{dir: b.loc.secretsDir()}
	ok := false
	defer func() {
		if !ok {
			st.discard()
		}
	}()

	if err := b.apply(ctx, &next, in, st); err != nil {
		return nil, err
	}
	resolved, err := resolveConfig(next, filepath.Dir(b.loc.Managed))
	if err != nil {
		return nil, control.BadInput(err)
	}
	if err := validateConfig(resolved); err != nil {
		return nil, control.BadInput(err)
	}
	changed, err := changedKeys(before, next)
	if err != nil {
		return nil, err
	}
	if len(changed) == 0 && b.loc.Path == b.loc.Managed {
		return nil, nil
	}
	// The file that worked, kept so a configuration that will not start can
	// be put back; see the main loop.
	if b.loc.Path != "" {
		if data, err := os.ReadFile(b.loc.Path); err == nil {
			if err := writeAtomic(b.loc.Managed+".previous", data, 0o600); err != nil {
				return nil, err
			}
		}
	}
	if err := writeConfig(b.loc.Managed, next); err != nil {
		return nil, fmt.Errorf("write %s: %w", b.loc.Managed, err)
	}
	ok = true
	st.prune()
	setApplyError("")
	if len(changed) == 0 {
		// Moved from the legacy file without changing anything.
		changed = []string{"config_file"}
	}
	b.reload()
	return changed, nil
}

func (b *settingsBackend) apply(ctx context.Context, c *config, in control.Settings, st *secretStage) error {
	c.ConsoleURL = trimURL(in.ConsoleURL)
	if c.Console == nil {
		c.Console = &consoleConfig{}
	}
	c.Console.FleetURL = trimURL(in.FleetURL)
	if c.Console.Dir == "" {
		c.Console.Dir = detectConsoleDir(b.loc.Managed)
	}
	c.Listen = strings.TrimSpace(in.Listen)
	c.ConsoleListen = strings.TrimSpace(in.ConsoleListen)
	if q := strings.TrimSpace(in.QUICListen); q != "" {
		c.Tunnel = &tunnelConfig{QUICListen: q}
	} else {
		c.Tunnel = nil
	}
	c.BehindTLSProxy = in.BehindTLSProxy
	c.AllowedOrigins = cleanList(in.AllowedOrigins)
	prevRL := c.RateLimit
	c.RateLimit = &rateLimitConfig{
		TrustedProxies: cleanList(in.TrustedProxies),
		LoginPerMinute: in.LoginPerMinute, FailuresPerHour: in.FailuresPerHour,
		TicketsPerMinute: in.TicketsPerMinute,
	}
	// The per-account limits are edited under Security; a console that does
	// not send that section keeps them, rather than a save from the limits
	// card quietly dropping them from the file.
	if prevRL != nil {
		c.RateLimit.AccountFailuresPerHour = prevRL.AccountFailuresPerHour
		c.RateLimit.AccountCodeFailuresPerHour = prevRL.AccountCodeFailuresPerHour
	}
	if sec := in.Security; sec != nil {
		if !control.ValidMFAPolicy(sec.RequireMFA) {
			return control.BadInput(fmt.Errorf("unknown two-step policy %q", sec.RequireMFA))
		}
		// The default is written as nothing, so the file follows it.
		c.RequireMFA = sec.RequireMFA
		if c.RequireMFA == string(control.MFAAll) {
			c.RequireMFA = ""
		}
		ttl, err := parseDuration("temporary password lifetime", sec.TemporaryPasswordTTL)
		if err != nil {
			return control.BadInput(err)
		}
		if ttl != 0 && (ttl < time.Hour || ttl > 30*24*time.Hour) {
			return control.BadInput(fmt.Errorf("a temporary password must work for between 1h and 720h"))
		}
		c.TemporaryPasswordTTL = ttl
		c.AllowBrowserChange = sec.AllowBrowserChange
		if sec.AccountFailuresPerHour < 0 || sec.AccountCodeFailuresPerHour < 0 {
			return control.BadInput(fmt.Errorf("limits cannot be negative"))
		}
		c.RateLimit.AccountFailuresPerHour = sec.AccountFailuresPerHour
		c.RateLimit.AccountCodeFailuresPerHour = sec.AccountCodeFailuresPerHour
	}
	var err error
	for _, d := range []struct {
		name string
		text string
		dst  *time.Duration
	}{
		{"session lifetime", in.SessionTTL, &c.SessionTTL},
		{"ticket lifetime", in.TicketTTL, &c.TicketTTL},
		{"agent silence threshold", in.AgentStaleAfter, &c.AgentStaleAfter},
	} {
		if *d.dst, err = parseDuration(d.name, d.text); err != nil {
			return control.BadInput(err)
		}
	}
	if in.AgentKeepRecordingsDays < -1 || in.AgentKeepRecordingsDays > 3650 {
		return control.BadInput(fmt.Errorf("servers keep recordings for 1 to 3650 days, " +
			"0 for the default or -1 for ever"))
	}
	c.AgentKeepRecordingsDays = in.AgentKeepRecordingsDays
	if ar := in.Archive; ar != nil {
		if ar.AfterDays < -1 || ar.AfterDays > 3650 {
			return control.BadInput(fmt.Errorf("recordings are archived after 1 to 3650 days, " +
				"0 for the default or -1 for never"))
		}
		if !control.ValidArchiveStorageClass(ar.StorageClass) {
			return control.BadInput(fmt.Errorf("storage class %q cannot be replayed from at once; use one of %s",
				ar.StorageClass, strings.Join(control.ArchiveStorageClasses[1:], ", ")))
		}
		c.RecordingsArchiveAfterDays, c.RecordingsArchiveStorageClass = ar.AfterDays, ar.StorageClass
	}
	switch in.LogLevel {
	case "", "debug", "info", "warn", "error":
		c.LogLevel = in.LogLevel
	default:
		return control.BadInput(fmt.Errorf("unknown log level %q", in.LogLevel))
	}

	if err := b.applyDatabase(ctx, c, in.Database, st); err != nil {
		return err
	}
	if err := applyStorage(c, in.Storage, st); err != nil {
		return err
	}
	if err := applyAlerts(c, in, st); err != nil {
		return err
	}
	if err := applyCloud(c, in.Cloud, st); err != nil {
		return err
	}
	return b.applyTLS(c, in)
}

func (b *settingsBackend) applyDatabase(ctx context.Context, c *config, in control.DatabaseSettings, st *secretStage) error {
	if in.Raw {
		return nil
	}
	password := databasePassword(b.cfg.DatabaseURL)
	if in.Password != nil {
		password = *in.Password
	}
	dsn, err := control.DatabaseInput{Host: in.Host, Port: in.Port, Name: in.Name, User: in.User,
		Password: password, SSLMode: in.SSLMode}.URL()
	if err != nil {
		return control.BadInput(err)
	}
	if sameConnection(dsn, b.cfg.DatabaseURL) {
		return nil
	}
	if _, err := control.CheckDatabase(ctx, dsn); err != nil {
		return control.BadInput(fmt.Errorf("could not use that database: %w", err))
	}
	c.DatabaseURL, err = st.write("database-url", dsn)
	return err
}

func applyStorage(c *config, in control.StorageSettings, st *secretStage) error {
	if !in.Enabled {
		c.Storage = nil
		return nil
	}
	prev := c.Storage
	c.Storage = &storage.Config{Endpoint: strings.TrimSpace(in.Endpoint), Bucket: strings.TrimSpace(in.Bucket),
		Region: strings.TrimSpace(in.Region), UseSSL: in.UseSSL, AccessKey: strings.TrimSpace(in.AccessKey)}
	switch {
	case in.SecretKey != nil:
		ref, err := st.write("storage-secret-key", *in.SecretKey)
		if err != nil {
			return err
		}
		c.Storage.SecretKey = ref
	case prev != nil:
		c.Storage.SecretKey = prev.SecretKey
	}
	return nil
}

func applyAlerts(c *config, in control.Settings, st *secretStage) error {
	prev := c.Alerts
	if prev == nil {
		prev = &alertsConfig{}
	}
	next := &alertsConfig{}
	if e := in.Email; e.Enabled {
		email, err := emailConfigFrom(e, prev.Email, st)
		if err != nil {
			return err
		}
		next.Email = email
	}
	if t := in.Telegram; t.Enabled {
		next.Telegram = &notify.TelegramConfig{ChatIDs: cleanList(t.ChatIDs)}
		switch {
		case t.BotToken != nil:
			ref, err := st.write("telegram-bot-token", strings.TrimSpace(*t.BotToken))
			if err != nil {
				return err
			}
			next.Telegram.BotToken = ref
		case prev.Telegram != nil:
			next.Telegram.BotToken = prev.Telegram.BotToken
		}
	}
	if f := in.FCM; f.Enabled {
		next.FCM = &notify.FCMConfig{ProjectID: strings.TrimSpace(f.ProjectID),
			Topic: strings.TrimSpace(f.Topic), Tokens: cleanList(f.Tokens)}
		switch {
		case f.Credentials != nil:
			// A file, not a reference: notify reads the key from a path.
			path, err := st.writeFile("fcm-service-account.json", *f.Credentials)
			if err != nil {
				return err
			}
			next.FCM.CredentialsFile = path
		case prev.FCM != nil:
			next.FCM.CredentialsFile = prev.FCM.CredentialsFile
		}
	}
	next.SignIn = prev.SignIn
	if in.SignInAlerts != nil {
		sa := *in.SignInAlerts
		if sa.Location == "" && prev.SignIn != nil {
			sa.Location = prev.SignIn.Location
		}
		if !control.ValidLocationMode(sa.Location) {
			return control.BadInput(fmt.Errorf("unknown location source %q", sa.Location))
		}
		next.SignIn = signInFromSettings(sa)
	}
	next.ServerConnected = prev.ServerConnected
	if in.ServerAlerts != nil {
		if !control.ValidServerAlertPolicy(in.ServerAlerts.Connected) {
			return control.BadInput(fmt.Errorf("unknown server alert setting %q", in.ServerAlerts.Connected))
		}
		next.ServerConnected = in.ServerAlerts.Connected
		if next.ServerConnected == string(control.ServerAlertFirst) {
			next.ServerConnected = ""
		}
	}
	if next.Email == nil && next.Telegram == nil && next.FCM == nil && next.SignIn == nil &&
		next.ServerConnected == "" {
		c.Alerts = nil
	} else {
		c.Alerts = next
	}
	return nil
}

func (b *settingsBackend) applyTLS(c *config, in control.Settings) error {
	pki := b.loc.pki()
	switch {
	case in.TLS.CertPEM != nil || in.TLS.KeyPEM != nil:
		if in.TLS.CertPEM == nil || in.TLS.KeyPEM == nil {
			return control.BadInput(errors.New("upload the certificate and its private key together"))
		}
		if err := pki.WriteServerPair([]byte(*in.TLS.CertPEM), []byte(*in.TLS.KeyPEM)); err != nil {
			return control.BadInput(err)
		}
	case in.TLS.Reissue:
		if _, err := pki.EnsureCA(); err != nil {
			return err
		}
		hosts := append(urlHosts(c.ConsoleURL, c.Console.FleetURL), localHosts()...)
		if err := pki.IssueServer(hosts); err != nil {
			return err
		}
	default:
		// The addresses may have changed: a certificate the control plane
		// issued itself is reissued to cover them. One uploaded is left alone.
		if c.TLS != nil && filepath.Clean(c.TLS.CertFile) == pki.ServerCert() {
			if _, err := pki.RenewServerIfDue(urlHosts(c.ConsoleURL, c.Console.FleetURL)); err != nil {
				return err
			}
		}
		return nil
	}
	if c.TLS == nil {
		c.TLS = &tlsConfig{}
		if pki.HasCAKey() {
			c.TLS.ClientCAFile = pki.CACert()
		}
	}
	c.TLS.CertFile, c.TLS.KeyFile = pki.ServerCert(), pki.ServerKey()
	return nil
}

// emailConfigFrom is the email block for e. A secret e leaves out is kept
// from prev; one it sends is written to a file through st, or, with st nil,
// used as it is -- for a test send that saves nothing.
//
// Both the SMTP server and the Panmail gateway are kept whichever is chosen,
// so switching to try the other and back does not lose the first one's
// settings.
func emailConfigFrom(e control.EmailSettings, prev *notify.EmailConfig, st *secretStage) (*notify.EmailConfig, error) {
	if prev == nil {
		prev = &notify.EmailConfig{}
	}
	via := strings.TrimSpace(e.Via)
	if via == "" {
		via = notify.ViaSMTP
	}
	out := &notify.EmailConfig{Via: via, Host: strings.TrimSpace(e.Host), Port: e.Port,
		Username: strings.TrimSpace(e.Username), From: strings.TrimSpace(e.From), To: cleanList(e.To)}
	if via == notify.ViaSMTP {
		// The default stays out of the file, as it was before Panmail.
		out.Via = ""
	}
	keep := func(name string, in *string, old string) (string, error) {
		switch {
		case in == nil:
			return old, nil
		case *in == "":
			return "", nil
		case st == nil:
			return *in, nil
		}
		return st.write(name, *in)
	}
	var err error
	if out.Password, err = keep("smtp-password", e.Password, prev.Password); err != nil {
		return nil, err
	}
	p := e.Panmail
	if url, id := strings.TrimSpace(p.URL), strings.TrimSpace(p.ProviderID); url != "" || id != "" ||
		p.APIKey != nil || via == notify.ViaPanmail {
		out.Panmail = &notify.PanmailConfig{URL: url, ProviderID: id}
		var oldKey string
		if prev.Panmail != nil {
			oldKey = prev.Panmail.APIKey
		}
		var key *string
		if p.APIKey != nil {
			k := strings.TrimSpace(*p.APIKey)
			key = &k
		}
		if out.Panmail.APIKey, err = keep("panmail-api-key", key, oldKey); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TestEmail sends one message with e to its recipients, without saving it.
func (b *settingsBackend) TestEmail(ctx context.Context, e control.EmailSettings) error {
	var prev *notify.EmailConfig
	if b.cfg.Alerts != nil {
		prev = b.cfg.Alerts.Email
	}
	cfg, err := emailConfigFrom(e, prev, nil)
	if err != nil {
		return err
	}
	sender, err := notify.NewEmail(*cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	how := "your SMTP server"
	if sender.Via() == notify.ViaPanmail {
		how = "Panmail"
	}
	return sender.Send(ctx, "Argus: test email",
		"This is a test from Argus, sent through "+how+".\n\n"+
			"Session and sign-in alerts will arrive the same way once these settings are saved.\n")
}

func (b *settingsBackend) TestDatabase(ctx context.Context, d control.DatabaseSettings) (control.DatabaseCheck, error) {
	password := databasePassword(b.cfg.DatabaseURL)
	if d.Password != nil {
		password = *d.Password
	}
	dsn, err := control.DatabaseInput{Host: d.Host, Port: d.Port, Name: d.Name, User: d.User,
		Password: password, SSLMode: d.SSLMode}.URL()
	if err != nil {
		return control.DatabaseCheck{}, err
	}
	return control.CheckDatabase(ctx, dsn)
}

// validateConfig checks what would otherwise only fail once the control
// plane restarts on it.
func validateConfig(c config) error {
	if err := validateCloud(c.Cloud); err != nil {
		return err
	}
	if c.ReporterToken == "" {
		return errors.New("reporter_token is required")
	}
	if c.SigningSecret != "" {
		s, err := auth.NewSigner(c.SigningSecret)
		if err != nil {
			return err
		}
		s.Close()
	}
	if c.ConsoleURL != "" {
		if _, err := control.CheckURL(c.ConsoleURL, "the console address", false); err != nil {
			return err
		}
	}
	if c.Console != nil {
		if c.Console.FleetURL != "" {
			if _, err := control.CheckURL(c.Console.FleetURL, "the address servers use", true); err != nil {
				return err
			}
		}
		if c.Console.Dir != "" {
			if _, err := control.NewConsoleHandler(control.ConsoleConfig{Dir: c.Console.Dir,
				FleetURL: c.Console.FleetURL}); err != nil {
				return err
			}
		}
	}
	for name, addr := range map[string]string{"the listen address": c.Listen,
		"the console listen address": c.ConsoleListen} {
		if addr == "" && name == "the listen address" {
			return errors.New("the listen address is required, such as :8080")
		}
		if addr != "" {
			if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
				return fmt.Errorf("%s %q must be host:port or :port", name, addr)
			}
		}
	}
	if c.ConsoleListen != "" && c.ConsoleListen == c.Listen {
		return errors.New("the console listen address must differ from the listen address, or be empty")
	}
	if c.Tunnel != nil && c.Tunnel.QUICListen != "" {
		if c.TLS == nil {
			return errors.New("QUIC needs TLS: there is no certificate configured")
		}
		if _, _, err := net.SplitHostPort(c.Tunnel.QUICListen); err != nil {
			return fmt.Errorf("the QUIC address %q must be host:port or :port", c.Tunnel.QUICListen)
		}
	}
	if c.TLS != nil {
		if _, err := servertls.Server(servertls.ServerOptions{CertFile: c.TLS.CertFile,
			KeyFile: c.TLS.KeyFile, ClientCAFile: c.TLS.ClientCAFile}); err != nil {
			return err
		}
	}
	if c.RateLimit != nil {
		t, err := control.NewThrottles(control.ThrottleConfig{TrustedProxies: c.RateLimit.TrustedProxies})
		if err != nil {
			return fmt.Errorf("trusted proxies: %w", err)
		}
		t.Close()
		for _, n := range []int{c.RateLimit.LoginPerMinute, c.RateLimit.FailuresPerHour, c.RateLimit.TicketsPerMinute} {
			if n < 0 {
				return errors.New("a rate limit cannot be negative; 0 means the default")
			}
		}
	}
	if c.Storage != nil {
		if c.Storage.Endpoint == "" || c.Storage.Bucket == "" {
			return errors.New("object storage needs an endpoint and a bucket")
		}
		if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
			return errors.New("object storage needs an access key and a secret key")
		}
	}
	if _, err := alertSenders(c.Alerts); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	return nil
}

// resolveConfig is what loadConfig would make of c once written: references
// resolved and relative paths made absolute.
func resolveConfig(c config, base string) (config, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return config{}, err
	}
	body, err = confexpand.Expand(body)
	if err != nil {
		return config{}, err
	}
	var out config
	if err := yaml.Unmarshal(body, &out); err != nil {
		return config{}, err
	}
	absolutize(&out, base)
	return out, nil
}

// changedKeys names the top-level settings that differ, for the audit log.
func changedKeys(before, after config) ([]string, error) {
	a, err := asMap(before)
	if err != nil {
		return nil, err
	}
	b, err := asMap(after)
	if err != nil {
		return nil, err
	}
	var out []string
	for k := range b {
		if !reflect.DeepEqual(a[k], b[k]) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

func asMap(c config) (map[string]any, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	err = yaml.Unmarshal(body, &m)
	return m, err
}

func cloneConfig(c config) (config, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return config{}, err
	}
	var out config
	err = yaml.Unmarshal(body, &out)
	return out, err
}

// secretStage writes each secret under a fresh name, so the running
// configuration's files are untouched until the new one is in place.
type secretStage struct {
	dir     string
	written map[string]string // base name -> new path
	// dropped is files a saved configuration no longer refers to: the
	// secret of an identity removed or given a new one.
	dropped []string
}

// drop marks the file ref points at for deletion once the save has worked.
// Only a file in the secrets directory: a reference a person typed by hand
// to somewhere else is theirs.
func (s *secretStage) drop(ref string) {
	if s == nil || !strings.HasPrefix(ref, "${file:") || !strings.HasSuffix(ref, "}") {
		return
	}
	path := filepath.Clean(strings.TrimSuffix(strings.TrimPrefix(ref, "${file:"), "}"))
	if filepath.Dir(path) != filepath.Clean(s.dir) {
		return
	}
	s.dropped = append(s.dropped, path)
}

func (s *secretStage) writeFile(name, value string) (string, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, name+"."+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := writeAtomic(path, []byte(value), 0o600); err != nil {
		return "", err
	}
	if s.written == nil {
		s.written = map[string]string{}
	}
	s.written[name] = path
	return path, nil
}

func (s *secretStage) write(name, value string) (string, error) {
	path, err := s.writeFile(name, value+"\n")
	if err != nil {
		return "", err
	}
	return "${file:" + path + "}", nil
}

// discard removes what a refused save wrote.
func (s *secretStage) discard() {
	for _, p := range s.written {
		_ = os.Remove(p)
	}
}

// prune removes the versions a saved configuration no longer refers to.
func (s *secretStage) prune() {
	for _, p := range s.dropped {
		_ = os.Remove(p)
	}
	for name, keep := range s.written {
		matches, _ := filepath.Glob(filepath.Join(s.dir, name+"*"))
		for _, m := range matches {
			if m != keep && (filepath.Base(m) == name || strings.HasPrefix(filepath.Base(m), name+".")) {
				_ = os.Remove(m)
			}
		}
	}
}

func parseDuration(name, text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("the %s %q is not a duration, such as 8h, 15m or 90s", name, text)
	}
	return d, nil
}

func durationText(d time.Duration) string {
	if d == 0 {
		return ""
	}
	s := d.String()
	// "8h0m0s" reads better as "8h", and "1m0s" as "1m".
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func databasePassword(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}

// sameConnection compares two connection strings as the parts that matter.
func sameConnection(a, b string) bool {
	da, okA := control.ParseDatabaseURL(a)
	db, okB := control.ParseDatabaseURL(b)
	if !okA || !okB {
		return a == b
	}
	if da.Port == 0 {
		da.Port = 5432
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	return da == db && databasePassword(a) == databasePassword(b)
}

func trimURL(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// reloadAfter lets the response that asked for a reload reach the browser
// before the listener it is on closes.
func reloadAfter(ch chan<- struct{}) func() {
	return func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			select {
			case ch <- struct{}{}:
			default:
			}
		}()
	}
}

// signInFromSettings writes only what differs from the defaults, so a config
// file that never mentioned sign-in alerts keeps not mentioning them.
func signInFromSettings(in control.SignInAlertSettings) *signInAlertsConfig {
	def := control.DefaultSignInAlertPolicy
	out := &signInAlertsConfig{}
	if in.Admins != def.Admins {
		v := in.Admins
		out.Admins = &v
	}
	if in.Person != def.Person {
		v := in.Person
		out.Person = &v
	}
	if in.When == "every" {
		out.When = "every"
	}
	if in.Location != "" && in.Location != string(control.LocationProxy) {
		out.Location = in.Location
	}
	if out.Admins == nil && out.Person == nil && out.When == "" && out.Location == "" {
		return nil
	}
	return out
}
