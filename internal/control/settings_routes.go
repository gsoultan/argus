package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tlsconfig"
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
	GatewayURL string `json:"gatewayUrl"`
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

	Database DatabaseSettings `json:"database"`
	Storage  StorageSettings  `json:"storage"`
	Email    EmailSettings    `json:"email"`
	Telegram TelegramSettings `json:"telegram"`
	FCM      FCMSettings      `json:"fcm"`
	TLS      TLSSettings      `json:"tls"`
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

// EmailSettings is the SMTP alert channel.
type EmailSettings struct {
	Enabled     bool     `json:"enabled"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Username    string   `json:"username"`
	PasswordSet bool     `json:"passwordSet"`
	Password    *string  `json:"password,omitempty"`
	From        string   `json:"from"`
	To          []string `json:"to"`
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
	Certificate *tlsconfig.CertSummary `json:"certificate,omitempty"`
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

// GatewayCredentials is everything a new gateway needs to report to this
// control plane, for the administrator to put on it.
type GatewayCredentials struct {
	Name string `json:"name"`
	// Env is the contents of /etc/argus/gateway.env.
	Env        string `json:"env"`
	ControlURL string `json:"controlUrl"`
	CACert     string `json:"caCert"`
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
	// ServerCert and ServerKey are for the gateway's own web listener.
	ServerCert string `json:"serverCert,omitempty"`
	ServerKey  string `json:"serverKey,omitempty"`
	// Config is the part of gateway.yaml that points it here.
	Config    string    `json:"config"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SettingsBackend reads and writes the configuration file. The binary
// implements it, because it alone knows the file's shape and where it lives.
type SettingsBackend interface {
	Settings() (Settings, error)
	// SaveSettings writes in and returns which settings changed. When any
	// did, the control plane reloads itself shortly after.
	SaveSettings(ctx context.Context, in Settings, actor string) ([]string, error)
	TestDatabase(ctx context.Context, d DatabaseSettings) (DatabaseCheck, error)
	GatewayCredentials(name, host string) (GatewayCredentials, error)
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

func (a *API) postGatewayCredentials(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.admin(w, r, "issuing gateway credentials")
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
		// Host is the name browsers reach the gateway's web listener at.
		Host string `json:"host"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	creds, err := a.Settings.GatewayCredentials(in.Name, in.Host)
	if err != nil {
		var bad *InputError
		if errors.As(err, &bad) {
			writeErr(w, http.StatusBadRequest, bad.Error())
			return
		}
		a.fail(w, "gateway credentials", err)
		return
	}
	// The shared secrets went to a browser. That is what installing a gateway
	// takes, and exactly what an investigation would want to find.
	if _, aerr := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "gateway.credentials_issued", Severity: "warn", ActorEmail: actor,
		Target: creds.Name,
		Detail: "Downloaded the credentials for gateway " + creds.Name +
			": the reporter token, the signing secret and a client certificate valid until " +
			creds.ExpiresAt.UTC().Format(time.RFC3339) + ".",
	}); aerr != nil {
		a.log.Error("audit append failed", "error", aerr)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, creds)
}
