package control

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/argus/internal/auth"
)

/*
Installing in the browser.

setup.go makes the first account on a control plane that already has a
configuration. This is the step before it: a control plane started with no
configuration at all, which until now refused to start and pointed at a YAML
file and an env file somebody had to fill in by hand.

Now it starts anyway, in a mode that serves the console's /setup page and
nothing else. Every other page redirects there; every API route answers that
setup is not finished. The page asks for the database, the addresses people
and servers reach Argus at, a certificate if the operator has one, and the
first administrator. The control plane writes its configuration, secrets and
certificates itself, then starts properly, in the same process.

The page is guarded exactly as setup.go's is, by a one-time token written to a
file on the host that only the service's own user can read. Without it, the
first person to find a fresh install's address would choose its database and
its administrator.
*/

// DatabaseInput is where the control plane's PostgreSQL is, as the setup page
// asks for it: in parts, so a password with an @ in it needs no escaping.
type DatabaseInput struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
	// SSLMode is libpq's: disable, require, verify-ca or verify-full.
	SSLMode string `json:"sslMode"`
}

// URL renders the input as a connection string.
func (d DatabaseInput) URL() (string, error) {
	host := strings.TrimSpace(d.Host)
	if host == "" {
		return "", errors.New("enter the database server's host name or address")
	}
	if strings.ContainsAny(host, "/?#@ ") {
		return "", fmt.Errorf("%q is not a host name or address", host)
	}
	port := d.Port
	if port == 0 {
		port = 5432
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%d is not a port", port)
	}
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return "", errors.New("enter the database's name")
	}
	user := strings.TrimSpace(d.User)
	if user == "" {
		return "", errors.New("enter the database user")
	}
	mode := d.SSLMode
	switch mode {
	case "":
		mode = "require"
	case "disable", "require", "verify-ca", "verify-full", "prefer":
	default:
		return "", fmt.Errorf("unknown TLS mode %q", mode)
	}
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + name,
		RawQuery: url.Values{"sslmode": {mode}}.Encode(),
	}
	if d.Password != "" {
		u.User = url.UserPassword(user, d.Password)
	} else {
		u.User = url.User(user)
	}
	return u.String(), nil
}

// ParseDatabaseURL splits a connection string back into the parts the
// console shows. The password is left out; the caller knows whether one is set.
func ParseDatabaseURL(raw string) (DatabaseInput, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return DatabaseInput{}, false
	}
	d := DatabaseInput{Host: u.Hostname(), Name: strings.TrimPrefix(u.Path, "/"),
		User: u.User.Username(), SSLMode: u.Query().Get("sslmode")}
	if p := u.Port(); p != "" {
		d.Port, _ = strconv.Atoi(p)
	}
	return d, true
}

// DatabaseCheck is what the setup page shows after testing a connection.
type DatabaseCheck struct {
	// Version is the server's, so a too-old one is caught before install.
	Version string `json:"version"`
	// AccountsExist means this database already belongs to a deployment, so
	// install keeps its accounts rather than making a new administrator.
	AccountsExist bool `json:"accountsExist"`
}

// CheckDatabase connects without migrating anything.
func CheckDatabase(ctx context.Context, dsn string) (DatabaseCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return DatabaseCheck{}, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var out DatabaseCheck
	var num int
	if err := conn.QueryRow(ctx,
		`SELECT current_setting('server_version'), current_setting('server_version_num')::int`).
		Scan(&out.Version, &num); err != nil {
		return DatabaseCheck{}, err
	}
	if num < 130000 {
		return out, fmt.Errorf("PostgreSQL %s is too old: Argus needs 13 or newer", out.Version)
	}
	var hasUsers bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.users') IS NOT NULL`).Scan(&hasUsers); err != nil {
		return DatabaseCheck{}, err
	}
	if hasUsers {
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE password_hash IS NOT NULL)`).
			Scan(&out.AccountsExist); err != nil {
			return DatabaseCheck{}, err
		}
	}
	return out, nil
}

// InstallRequest is the setup page's whole form.
type InstallRequest struct {
	Token    string        `json:"token"`
	Database DatabaseInput `json:"database"`
	// ConsoleURL is where people open the console, such as
	// https://argus.example.com:8080.
	ConsoleURL string `json:"consoleUrl"`
	// GatewayURL is argus-gateway's web listener, if there is a gateway.
	GatewayURL string `json:"gatewayUrl"`
	// FleetURL is where servers reach the control plane, when not ConsoleURL.
	FleetURL string `json:"fleetUrl"`
	// Port is the TCP port to listen on, and UDP for QUIC.
	Port int  `json:"port"`
	QUIC bool `json:"quic"`
	// CertPEM and KeyPEM replace the certificate the control plane made for
	// itself. Both or neither.
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`
	Admin   struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	} `json:"admin"`
}

// InstallResult is what the page shows once the configuration is written.
type InstallResult struct {
	ConsoleURL string `json:"consoleUrl"`
	ConfigFile string `json:"configFile"`
	// Email is the administrator made, empty when the database already had
	// accounts and they were kept.
	Email string `json:"email,omitempty"`
}

// Validate checks everything that does not need the database or the disk.
func (in *InstallRequest) Validate(accountsExist bool) error {
	if _, err := CheckURL(in.ConsoleURL, "the console address", true); err != nil {
		return err
	}
	if in.GatewayURL != "" {
		if _, err := CheckURL(in.GatewayURL, "the gateway address", false); err != nil {
			return err
		}
	}
	if in.FleetURL != "" {
		if _, err := CheckURL(in.FleetURL, "the address servers use", true); err != nil {
			return err
		}
	}
	if in.Port == 0 {
		in.Port = 8080
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%d is not a port", in.Port)
	}
	if (in.CertPEM == "") != (in.KeyPEM == "") {
		return errors.New("give the certificate and its private key together, or neither")
	}
	if accountsExist {
		return nil
	}
	email := normaliseEmail(in.Admin.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return errors.New("enter the administrator's email address, such as you@example.com")
	}
	if len([]rune(in.Admin.Password)) < auth.MinPasswordLength {
		return fmt.Errorf("the administrator's password must be at least %d characters", auth.MinPasswordLength)
	}
	return nil
}

// CheckURL accepts an absolute http(s) URL with a host and nothing after it.
func CheckURL(raw, what string, httpsOnly bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%s must be a full address, such as https://argus.example.com", what)
	}
	if httpsOnly && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must start with https://", what)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s must be just the scheme, host and port", what)
	}
	return u, nil
}

// Installer does what the setup page asks: the half that touches the disk
// lives with the binary, which knows where its files go.
type Installer interface {
	CheckDatabase(ctx context.Context, d DatabaseInput) (DatabaseCheck, error)
	Install(ctx context.Context, in InstallRequest, client string) (InstallResult, error)
}

// InstallOptions builds the handler for a control plane with no configuration.
type InstallOptions struct {
	Token     string
	TokenFile string
	// Port is the one this process is listening on, the page's default.
	Port      int
	Console   http.Handler
	Installer Installer
	Log       *slog.Logger
}

// NewInstallHandler serves /setup and the routes it calls, and sends every
// other page there.
func NewInstallHandler(o InstallOptions) http.Handler {
	h := &installHandler{o: o}
	if h.o.Log == nil {
		h.o.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "setup"})
	})
	// What the console's sign-in gate asks first. Answered the way a fresh
	// install with accounts-to-come answers, so the console goes to /setup.
	mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"authenticated": false, "passwordEnabled": true, "accountsExist": false,
			"setupAvailable": true, "installing": true,
		})
	})
	mux.HandleFunc("GET /auth/setup", h.status)
	mux.HandleFunc("POST /auth/setup/database", h.database)
	mux.HandleFunc("POST /auth/setup/install", h.install)
	mux.HandleFunc("/", h.other)
	api := &API{}
	return api.securityHeaders(mux)
}

type installHandler struct {
	o InstallOptions
}

// installStatus extends setupStatus with what the install form needs.
type installStatus struct {
	setupStatus
	// Mode "install" is a control plane with no configuration yet.
	Mode     string `json:"mode"`
	Port     int    `json:"port"`
	Hostname string `json:"hostname,omitempty"`
}

func (h *installHandler) status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	host := r.Host
	if hn, _, err := net.SplitHostPort(host); err == nil {
		host = hn
	}
	writeJSON(w, http.StatusOK, installStatus{
		setupStatus: setupStatus{Available: true, TokenFile: h.o.TokenFile},
		Mode:        "install", Port: h.o.Port, Hostname: host,
	})
}

// checkToken is first and charged like a wrong password, for the reason
// handleSetup gives: an answer about anything else would be an oracle.
func (h *installHandler) checkToken(w http.ResponseWriter, r *http.Request, token string) bool {
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(h.o.Token)) == 1 {
		return true
	}
	h.o.Log.Warn("setup refused: wrong setup token", "client", r.RemoteAddr)
	// A pause, so a wrong guess costs the guesser and not the server. The
	// token is 160 bits; this only keeps the log readable.
	time.Sleep(500 * time.Millisecond)
	writeErr(w, http.StatusUnauthorized,
		"that is not this control plane's setup token; read it from "+h.o.TokenFile+" on the host")
	return false
}

func (h *installHandler) database(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string        `json:"token"`
		Database DatabaseInput `json:"database"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.checkToken(w, r, in.Token) {
		return
	}
	check, err := h.o.Installer.CheckDatabase(r.Context(), in.Database)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not use that database: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, check)
}

func (h *installHandler) install(w http.ResponseWriter, r *http.Request) {
	var in InstallRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !h.checkToken(w, r, in.Token) {
		return
	}
	res, err := h.o.Installer.Install(r.Context(), in, r.RemoteAddr)
	if err != nil {
		var bad *InputError
		if errors.As(err, &bad) {
			writeErr(w, http.StatusBadRequest, bad.Error())
			return
		}
		h.o.Log.Error("install failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "install failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// other sends pages to /setup and refuses everything else until it is done.
func (h *installHandler) other(w http.ResponseWriter, r *http.Request) {
	p := path.Clean("/" + r.URL.Path)
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/auth/") ||
		(r.Method != http.MethodGet && r.Method != http.MethodHead) {
		writeErr(w, http.StatusServiceUnavailable,
			"argus-control is not set up yet: finish setup at /setup in the console")
		return
	}
	// Files the page itself loads, and the page.
	if p == "/setup" || path.Ext(p) != "" {
		if h.o.Console == nil {
			writeErr(w, http.StatusNotFound,
				"no console is installed beside this control plane; finish setup with the API, or install the console")
			return
		}
		h.o.Console.ServeHTTP(w, r)
		return
	}
	http.Redirect(w, r, "/setup", http.StatusFound)
}

// InputError is a mistake in what the setup page or Settings sent, worded for
// the person who made it.
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// BadInput wraps err as the caller's mistake.
func BadInput(err error) error {
	if err == nil {
		return nil
	}
	return &InputError{Err: err}
}
