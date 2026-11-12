package control

import (
	"context"
	"errors"
	"github.com/gsoultan/argus/internal/control/servertls"
	"net/http"
	"strings"
)

/*
The control plane's configuration, from the console.

Everything argus-control reads from its configuration file can be read and
changed here by an administrator, so nobody has to open the file. The file is
still the store -- the database address cannot live in the database -- but the
console writes it, and the binary reloads itself to apply it.

Secrets are write-only. The console is told whether one is set, never what it
is, and a secret it does not send is kept. The file never holds one either:
each goes in its own owner-only file beside it, referenced as ${file:...}.
*/

// Settings is the configuration as the console edits it.
//
// A secret is a *string: absent keeps the current value, a value replaces it.
// The *Set booleans say whether one is configured.
type Settings struct {
	// ConfigFile is where the settings are written. Read-only.
	ConfigFile string `json:"configFile"`
	// ApplyError is why the last save did not take effect: the control plane
	// could not start on it and went back to the settings before. Read-only.
	ApplyError string `json:"applyError,omitempty"`

	ConsoleURL string `json:"consoleUrl"`
	FleetURL   string `json:"fleetUrl"`

	Listen         string   `json:"listen"`
	ConsoleListen  string   `json:"consoleListen"`
	QUICListen     string   `json:"quicListen"`
	BehindTLSProxy bool     `json:"behindTlsProxy"`
	AllowedOrigins []string `json:"allowedOrigins"`

	TrustedProxies   []string `json:"trustedProxies"`
	LoginPerMinute   int      `json:"loginPerMinute"`
	FailuresPerHour  int      `json:"failuresPerHour"`
	TicketsPerMinute int      `json:"ticketsPerMinute"`

	// Durations as Go writes them: "8h", "90s".
	SessionTTL      string `json:"sessionTtl"`
	TicketTTL       string `json:"ticketTtl"`
	AgentStaleAfter string `json:"agentStaleAfter"`
	LogLevel        string `json:"logLevel"`
	// AgentKeepRecordingsDays is how long servers keep their own copy of a
	// recording once it is stored here: 0 is the default (7), -1 for ever.
	AgentKeepRecordingsDays int `json:"agentKeepRecordingsDays"`
	// Archive is when stored recordings move to the archive. Nil from a
	// console older than the archive, which keeps what is configured.
	Archive *RecordingArchiveSettings `json:"archive,omitempty"`

	Database DatabaseSettings `json:"database"`
	Storage  StorageSettings  `json:"storage"`
	Email    EmailSettings    `json:"email"`
	Telegram TelegramSettings `json:"telegram"`
	FCM      FCMSettings      `json:"fcm"`
	TLS      TLSSettings      `json:"tls"`

	// SignInAlerts is nil from a console older than sign-in alerts, which
	// keeps what is configured rather than switching them off.
	SignInAlerts *SignInAlertSettings `json:"signInAlerts,omitempty"`
	// ServerAlerts and Security are nil from a console older than them, and
	// are then kept as configured, for the same reason.
	ServerAlerts *ServerAlertSettings `json:"serverAlerts,omitempty"`
	Security     *SecuritySettings    `json:"security,omitempty"`
	// Cloud is the identity argus-control has for each cloud. Nil from a
	// console older than it, which keeps what is configured.
	Cloud *CloudSettings `json:"cloud,omitempty"`
}

// CloudSettings is the control plane's own identities for each cloud: who
// it is before it assumes a role in a connected account. Every
// organization's accounts trust an identity only together with an external
// id derived for that organization and account.
type CloudSettings struct {
	AWS AWSCloudSettings `json:"aws"`
}

// AWSCloudSettings is AWS's identities. Identities nil (a console older
// than several identities) keeps what is configured.
type AWSCloudSettings struct {
	// Default is the identity a new connection starts with: its id, or
	// the name of one being added in the same save.
	Default    string                `json:"default"`
	Identities []AWSIdentitySettings `json:"identities"`
}

// MaxCloudIdentities bounds the identities per cloud, and with them the
// providers and session caches the control plane keeps.
const MaxCloudIdentities = 20

// Where an identity comes from.
const (
	// IdentityDefault is whatever the host gives it: an EC2 instance
	// profile, a container role, AWS_ environment variables.
	IdentityDefault = "default"
	// IdentityKey is an access key entered here.
	IdentityKey = "key"
	// IdentityFile is a profile or credentials file named in the config.
	IdentityFile = "file"
)

// AWSIdentitySettings is one AWS identity. The secret access key is
// write-only.
type AWSIdentitySettings struct {
	// ID is empty for one being added; the control plane gives it one.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Organizations may use it, by id; empty is every organization.
	Organizations      []string `json:"organizations"`
	Source             string   `json:"source"`
	AccessKeyID        string   `json:"accessKeyId"`
	SecretAccessKeySet bool     `json:"secretAccessKeySet"`
	SecretAccessKey    *string  `json:"secretAccessKey,omitempty"`
	Profile            string   `json:"profile"`
	CredentialsFile    string   `json:"credentialsFile"`
	HomeRegion         string   `json:"homeRegion"`
}

// CloudIdentityCheck is who a cloud says an identity is.
type CloudIdentityCheck struct {
	Provider string `json:"provider"`
	// ARN is the caller's own ARN, as STS answered it.
	ARN string `json:"arn"`
	// Principal is what an account's trust policy names for it.
	Principal string `json:"principal"`
	// Source says where the credentials came from, in words.
	Source string `json:"source"`
}

// RecordingArchiveSettings is when a stored recording is recompressed and
// moved to the archive, and where.
type RecordingArchiveSettings struct {
	// AfterDays is the age in days: 0 is the default (30), -1 never.
	AfterDays int `json:"afterDays"`
	// StorageClass is the bucket storage class, empty for the default; one
	// of ArchiveStorageClasses.
	StorageClass string `json:"storageClass"`
}

// ServerAlertSettings is when administrators are told a server connected.
type ServerAlertSettings struct {
	// Connected is "first", "every" or "off".
	Connected string `json:"connected"`
}

// SecuritySettings is how sign-in is protected.
type SecuritySettings struct {
	// RequireMFA is "all", "privileged" or "off".
	RequireMFA string `json:"requireMfa"`
	// TemporaryPasswordTTL is how long a temporary password works unused, as
	// Go writes durations: "72h".
	TemporaryPasswordTTL string `json:"temporaryPasswordTtl"`
	// AllowBrowserChange keeps a cookie working in another browser than the
	// one that signed in. False refuses and revokes it.
	AllowBrowserChange bool `json:"allowBrowserChange"`
	// Wrong passwords and codes for one account an hour, from every address
	// together. Zero is the default (10 and 5).
	AccountFailuresPerHour     int `json:"accountFailuresPerHour"`
	AccountCodeFailuresPerHour int `json:"accountCodeFailuresPerHour"`
}

// SignInAlertSettings is who is told when someone signs in to the console.
// They go out through the email and Telegram channels above, and only those
// that are set up.
type SignInAlertSettings struct {
	Admins bool `json:"admins"`
	Person bool `json:"person"`
	// When is "new_device" or "every".
	When string `json:"when"`
	// Location is how an alert names where a sign-in came from: "proxy",
	// "lookup" or "off". Empty from an older console, which keeps it.
	Location string `json:"location,omitempty"`
}

// DatabaseSettings is the database in parts, with the password write-only.
type DatabaseSettings struct {
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	Name        string  `json:"name"`
	User        string  `json:"user"`
	SSLMode     string  `json:"sslMode"`
	PasswordSet bool    `json:"passwordSet"`
	Password    *string `json:"password,omitempty"`
	// Raw is set instead of the parts when the configured value is not a URL
	// the parts can describe, such as a key=value string. Read-only.
	Raw bool `json:"raw,omitempty"`
}

// StorageSettings is the S3-compatible bucket recordings are copied to.
type StorageSettings struct {
	Enabled      bool    `json:"enabled"`
	Endpoint     string  `json:"endpoint"`
	Bucket       string  `json:"bucket"`
	Region       string  `json:"region"`
	UseSSL       bool    `json:"useSsl"`
	AccessKey    string  `json:"accessKey"`
	SecretKeySet bool    `json:"secretKeySet"`
	SecretKey    *string `json:"secretKey,omitempty"`
}

// EmailSettings is the email alert channel: through an SMTP server, or
// through a Panmail gateway.
type EmailSettings struct {
	Enabled bool `json:"enabled"`
	// Via is "smtp" or "panmail". Empty, from a console older than Panmail,
	// is "smtp".
	Via         string   `json:"via"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Username    string   `json:"username"`
	PasswordSet bool     `json:"passwordSet"`
	Password    *string  `json:"password,omitempty"`
	From        string   `json:"from"`
	To          []string `json:"to"`

	Panmail PanmailSettings `json:"panmail"`
}

// PanmailSettings is the Panmail gateway email goes through, with the API key
// write-only.
type PanmailSettings struct {
	URL        string  `json:"url"`
	ProviderID string  `json:"providerId"`
	APIKeySet  bool    `json:"apiKeySet"`
	APIKey     *string `json:"apiKey,omitempty"`
}

// TelegramSettings is the Telegram alert channel.
type TelegramSettings struct {
	Enabled     bool     `json:"enabled"`
	BotTokenSet bool     `json:"botTokenSet"`
	BotToken    *string  `json:"botToken,omitempty"`
	ChatIDs     []string `json:"chatIds"`
}

// FCMSettings is the push-notification alert channel. Credentials is the
// service account key's JSON, uploaded rather than placed on the host.
type FCMSettings struct {
	Enabled        bool     `json:"enabled"`
	CredentialsSet bool     `json:"credentialsSet"`
	Credentials    *string  `json:"credentials,omitempty"`
	ProjectID      string   `json:"projectId"`
	Topic          string   `json:"topic"`
	Tokens         []string `json:"tokens"`
}

// TLSSettings describes the certificate served, and takes a replacement.
type TLSSettings struct {
	Enabled bool `json:"enabled"`
	// Certificate is the one being served. Read-only.
	Certificate *servertls.CertSummary `json:"certificate,omitempty"`
	// Managed means the control plane issued it and renews it itself.
	Managed bool `json:"managed"`
	// CanIssue means it holds a CA key, so it can make gateway certificates.
	CanIssue bool `json:"canIssue"`
	// CACert is the control plane's own CA, to trust in browsers and on
	// gateways. Public. Read-only.
	CACert string `json:"caCert,omitempty"`
	// MutualTLS means gateways must present a client certificate. Read-only.
	MutualTLS bool `json:"mutualTls"`
	// CertPEM and KeyPEM upload a certificate in place of the current one.
	CertPEM *string `json:"certPem,omitempty"`
	KeyPEM  *string `json:"keyPem,omitempty"`
	// Reissue asks for a fresh certificate from the control plane's own CA,
	// replacing an uploaded one.
	Reissue bool `json:"reissue,omitempty"`
}

// SettingsBackend reads and writes the configuration file. The binary
// implements it, because it alone knows the file's shape and where it lives.
type SettingsBackend interface {
	Settings() (Settings, error)
	// SaveSettings writes in and returns which settings changed. When any
	// did, the control plane reloads itself shortly after.
	SaveSettings(ctx context.Context, in Settings, actor string) ([]string, error)
	TestDatabase(ctx context.Context, d DatabaseSettings) (DatabaseCheck, error)
	// TestEmail sends one message with e, unsaved, to its recipients. A
	// secret e leaves out is the one configured.
	TestEmail(ctx context.Context, e EmailSettings) error
	// TestCloud asks the provider who in would be, without saving it. A
	// secret in leaves out is the one configured.
	TestCloud(ctx context.Context, provider string, in AWSIdentitySettings) (CloudIdentityCheck, error)
}

func (a *API) admin(w http.ResponseWriter, r *http.Request, what string) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if sess.Role != "admin" && sess.Role != "owner" {
		writeErr(w, http.StatusForbidden, what+" requires the admin or owner role")
		return "", false
	}
	if a.Settings == nil {
		writeErr(w, http.StatusNotImplemented, "this control plane was started without a settings store")
		return "", false
	}
	return sess.Email, true
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.admin(w, r, "reading the control plane's settings"); !ok {
		return
	}
	s, err := a.Settings.Settings()
	if err != nil {
		a.fail(w, "settings", err)
		return
	}
	if !platformScope(r.Context()) {
		// Every organization's accounts trust these identities; only the
		// platform sees or changes them. Others see the ones they may use
		// under Cloud, without anything about where they come from.
		s.Cloud = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s)
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.admin(w, r, "changing the control plane's settings")
	if !ok {
		return
	}
	var in Settings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !platformScope(r.Context()) {
		in.Cloud = nil // kept as configured
	}
	var before *CloudSettings
	if in.Cloud != nil && in.Cloud.AWS.Identities != nil {
		if cur, err := a.Settings.Settings(); err == nil {
			before = cur.Cloud
		}
		if msg, err := a.cloudIdentitiesInUse(r.Context(), before, in.Cloud); err != nil {
			a.fail(w, "cloud identity use", err)
			return
		} else if msg != "" {
			writeErr(w, http.StatusConflict, msg)
			return
		}
	}
	changed, err := a.Settings.SaveSettings(r.Context(), in, actor)
	if err != nil {
		var bad *InputError
		if errors.As(err, &bad) {
			writeErr(w, http.StatusBadRequest, bad.Error())
			return
		}
		a.fail(w, "save settings", err)
		return
	}
	if len(changed) > 0 && before != nil {
		a.auditCloudIdentities(r, actor, before, in.Cloud)
	}
	if len(changed) > 0 {
		a.log.Info("settings changed from the console", "by", actor, "changed", changed)
		if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
			Action: "settings.changed", Severity: "notice", ActorEmail: actor,
			Target: "argus-control",
			Detail: "Changed the control plane's settings from the console: " +
				strings.Join(changed, ", ") + ". The control plane reloaded to apply them.",
		}); aerr != nil {
			a.log.Error("audit append failed", "error", aerr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed, "restarting": len(changed) > 0})
}

func (a *API) postTestDatabase(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.admin(w, r, "testing a database"); !ok {
		return
	}
	var in DatabaseSettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	check, err := a.Settings.TestDatabase(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not use that database: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// postTestEmail sends a test message with the email settings on the form, so
// an administrator learns that mail arrives before relying on it for alerts.
func (a *API) postTestEmail(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.admin(w, r, "sending a test email")
	if !ok {
		return
	}
	var in EmailSettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.Settings.TestEmail(r.Context(), in); err != nil {
		a.log.Info("test email failed", "by", actor, "via", in.Via, "error", err)
		writeErr(w, http.StatusBadRequest, "the test email was not sent: "+err.Error())
		return
	}
	a.log.Info("test email sent", "by", actor, "via", in.Via, "to", len(in.To))
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "to": in.To})
}

// postTestCloud asks a cloud who the identity on the form is, so an
// administrator sees the ARN accounts will trust before saving it.
func (a *API) postTestCloud(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.admin(w, r, "testing a cloud identity")
	if !ok {
		return
	}
	if !platformScope(r.Context()) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	provider := r.PathValue("provider")
	if provider != "aws" {
		writeErr(w, http.StatusBadRequest, "only AWS has an identity to test so far")
		return
	}
	var in AWSIdentitySettings
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	check, err := a.Settings.TestCloud(r.Context(), provider, in)
	if err != nil {
		a.log.Info("cloud identity test failed", "by", actor, "provider", provider, "identity", in.ID, "error", err)
		writeErr(w, http.StatusBadRequest, "AWS did not accept that identity: "+err.Error())
		return
	}
	a.log.Info("cloud identity tested", "by", actor, "provider", provider, "identity", in.ID, "arn", check.ARN)
	writeJSON(w, http.StatusOK, check)
}

// platformScope says the request is in the default organization, where the
// platform-wide settings that every organization depends on are edited.
func platformScope(ctx context.Context) bool { return tenantIDOf(ctx) == DefaultTenantID }
