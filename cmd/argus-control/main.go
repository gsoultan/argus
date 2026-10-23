// Command argus-control is the Argus control plane.
//
// It is the system of record: assets, sessions, agent liveness and the
// hash-chained audit log. The console reads from it and opens sessions through
// it; agents report into it and hold the tunnels it opens sessions over.
//
// Sessions open through it, so an outage stops new sessions from the console.
// It does not lose evidence: agents record on the host, spool what they could
// not report and reconcile when it returns.
//
//	argus-control --config control.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/secrets"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/notify"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/tunnel"
)

var version = "dev"

type config struct {
	Listen string `yaml:"listen,omitempty"`
	// ConsoleListen optionally moves the human surface to its own address.
	//
	// Empty, which is the default, means one listener serves everything --
	// unchanged from before this existed. Set it and the two surfaces split:
	// `listen` carries only what agents post to, and this carries
	// only what a person signs in to. That lets the console be bound somewhere
	// narrow without taking the fleet's reporting with it, which is the reason
	// `listen` cannot simply be moved to loopback.
	ConsoleListen  string            `yaml:"console_listen,omitempty"`
	DatabaseURL    string            `yaml:"database_url,omitempty"`
	AllowedOrigins []string          `yaml:"allowed_origins,omitempty"`
	UserTokens     map[string]string `yaml:"user_tokens,omitempty"`
	ReporterToken  string            `yaml:"reporter_token,omitempty"`
	// AgentStaleAfter is how long an agent may be silent before the fleet is
	// told. Short on purpose: silence is how a killed agent looks.
	AgentStaleAfter time.Duration   `yaml:"agent_stale_after,omitempty"`
	Storage         *storage.Config `yaml:"storage,omitempty"`
	// SigningSecret signs session cookies and terminal tickets.
	SigningSecret string `yaml:"signing_secret,omitempty"`
	ConsoleURL    string `yaml:"console_url,omitempty"`
	// TLS serves the API over HTTPS. Strongly recommended: session cookies,
	// recordings and the audit log all cross this listener.
	TLS *tlsConfig `yaml:"tls,omitempty"`
	// BehindTLSProxy marks that something in front terminates TLS. Cookies must
	// still be Secure in that case, even though this process speaks plain HTTP.
	BehindTLSProxy bool  `yaml:"behind_tls_proxy,omitempty"`
	SecureCookies  *bool `yaml:"secure_cookies,omitempty"`
	// WebSetup offers the console's /setup page while no account exists.
	// Default on; false leaves `argus-control users add` as the only way to
	// make the first one.
	WebSetup *bool `yaml:"web_setup,omitempty"`
	// SetupTokenFile is where the one-time setup token is written. Empty
	// means /var/lib/argus-control/setup-token when that directory exists, which the
	// packaged unit can write, and beside the config file otherwise.
	SetupTokenFile string `yaml:"setup_token_file,omitempty"`
	// RateLimit bounds the authentication surface. Omit for defaults, which
	// are already appropriate — a person never reaches them, a script does.
	RateLimit  *rateLimitConfig `yaml:"rate_limit,omitempty"`
	SessionTTL time.Duration    `yaml:"session_ttl,omitempty"`
	TicketTTL  time.Duration    `yaml:"ticket_ttl,omitempty"`
	LogLevel   string           `yaml:"log_level,omitempty"`
	// Alerts tells administrators, by email, Telegram or a push notification
	// through Firebase Cloud Messaging, every time somebody opens a session.
	// Omit for none.
	Alerts *alertsConfig `yaml:"alerts,omitempty"`
	// Console serves the web console from this process. Omit to serve the API
	// alone, as every deployment did before the console shipped in a package.
	Console *consoleConfig `yaml:"console,omitempty"`
	// Tunnel is where agents hold their outbound connections. The HTTPS
	// fallback is always served on `listen`; QUIC needs its own UDP address.
	Tunnel *tunnelConfig `yaml:"tunnel,omitempty"`
}

type tunnelConfig struct {
	// QUICListen is a UDP address for agents' QUIC connections, typically the
	// same port as `listen` (":8080"). Empty leaves QUIC off; agents then use
	// the HTTPS fallback, which works but is less smooth on a lossy network.
	// Needs `tls`: QUIC is always encrypted, with this process's certificate.
	QUICListen string `yaml:"quic_listen,omitempty"`
}

type consoleConfig struct {
	// Dir is where the package installs the built console.
	Dir string `yaml:"dir,omitempty"`
	// FleetURL is where servers reach the control plane, when it is not the
	// console's own address -- behind a Cloudflare Tunnel, for one. "Add a
	// server" puts it in the enrollment command.
	FleetURL string `yaml:"fleet_url,omitempty"`
}

type alertsConfig struct {
	Email    *notify.EmailConfig    `yaml:"email,omitempty"`
	Telegram *notify.TelegramConfig `yaml:"telegram,omitempty"`
	FCM      *notify.FCMConfig      `yaml:"fcm,omitempty"`
}

// alertSenders builds a sender per configured channel.
//
// A channel that is present but incomplete stops start-up rather than being
// skipped: an administrator who configured alerts believes they will hear
// about every session, and a typo that silently turned that off would leave
// them believing it.
func alertSenders(cfg *alertsConfig) ([]notify.Sender, error) {
	if cfg == nil {
		return nil, nil
	}
	var out []notify.Sender
	if cfg.Email != nil {
		e, err := notify.NewEmail(*cfg.Email)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if cfg.Telegram != nil {
		t, err := notify.NewTelegram(*cfg.Telegram, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if cfg.FCM != nil {
		f, err := notify.NewFCM(*cfg.FCM, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

type rateLimitConfig struct {
	// TrustedProxies are networks whose X-Forwarded-For is believed. Leave
	// empty when reached directly: trusting a header the client sets lets an
	// attacker both evade their own limit and throttle somebody else.
	TrustedProxies   []string `yaml:"trusted_proxies,omitempty"`
	LoginPerMinute   int      `yaml:"login_per_minute,omitempty"`
	FailuresPerHour  int      `yaml:"failures_per_hour,omitempty"`
	TicketsPerMinute int      `yaml:"tickets_per_minute,omitempty"`
}

type tlsConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	// ClientCAFile enables mutual TLS on the reporter endpoints, so an agent
	// proves which host it is rather than only that it holds a token
	// identical on every host.
	ClientCAFile string `yaml:"client_ca_file,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "argus-control: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath, setupListen string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "path to config file (default "+defaultConfigPath()+")")
	flag.StringVar(&setupListen, "setup-listen", ":8080",
		"where the setup page listens while there is no configuration yet")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("argus-control %s\n", version)
		return nil
	}
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	if flag.NArg() > 0 {
		path, err := commandConfig(configPath)
		if err != nil {
			return err
		}
		return runCommand(path)
	}

	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	boot := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Round until stopped: set up if there is nothing to run on, run, and run
	// again on the new file whenever the Settings page saves one.
	reloaded := false
	for {
		loc, err := locateConfig(configPath)
		if err != nil {
			return err
		}
		if loc.Path == "" {
			if err := runInstall(sig, loc, setupListen, boot); err != nil {
				if errors.Is(err, errStopped) {
					return nil
				}
				return err
			}
			continue
		}
		err = serve(sig, loc)
		var startErr *startupError
		switch {
		case errors.Is(err, errReload):
			reloaded = true
			continue
		case reloaded && errors.As(err, &startErr) && restorePrevious(loc):
			// The console saved settings the control plane cannot start on.
			// Exiting would leave it down until someone edits the file by
			// hand, which is what the Settings page exists to avoid.
			boot.Error("the saved settings did not start; went back to the previous ones",
				"error", startErr.err, "config", loc.Managed)
			setApplyError(startErr.err.Error())
			reloaded = false
			continue
		default:
			return err
		}
	}
}

// errReload is serve returning so the loop starts it again on a new file.
var errReload = errors.New("reloading settings")

// startupError is a failure before the control plane was serving: the
// configuration, not something that happened while running.
type startupError struct{ err error }

func (e *startupError) Error() string { return e.err.Error() }
func (e *startupError) Unwrap() error { return e.err }

// restorePrevious puts back the file a save from the console replaced.
func restorePrevious(loc configLocation) bool {
	prev := loc.Managed + ".previous"
	if _, err := os.Stat(prev); err != nil {
		return false
	}
	return os.Rename(prev, loc.Managed) == nil
}

// runCommand runs a subcommand against the configuration at path.
func runCommand(configPath string) error {
	// `argus-control audit verify` recomputes the stored chain. Used by the
	// restore drill and by whatever schedules integrity checks.
	if flag.NArg() >= 2 && flag.Arg(0) == "audit" && flag.Arg(1) == "verify" {
		return runAuditVerify(configPath)
	}

	// `argus-control backup run|verify`: see backup.go.
	if flag.Arg(0) == "backup" {
		return runBackup(configPath, flag.Args()[1:])
	}

	// `argus-control users add <email>` creates a local account. A deployment
	// needs one before anyone can sign in, and seeding a default administrator
	// with a known password would be shipping a backdoor.
	if flag.NArg() >= 2 && flag.Arg(0) == "users" {
		switch flag.Arg(1) {
		case "add":
			return runUsersAdd(configPath, flag.Args()[2:])
		case "list":
			return runUsersList(configPath)
		// Revoking access is the other half of creating it, and until these
		// existed only the first half did: disabled_at was read on every
		// sign-in and written by nothing.
		case "disable":
			return runUsersSetDisabled(configPath, flag.Args()[2:], true)
		case "enable":
			return runUsersSetDisabled(configPath, flag.Args()[2:], false)
		default:
			return fmt.Errorf("unknown users command %q: add, list, disable, enable", flag.Arg(1))
		}
	}
	return fmt.Errorf("unknown command %q: audit verify, backup run|verify, users add|list|disable|enable",
		strings.Join(flag.Args(), " "))
}

// serve runs the control plane on the configuration at loc until a signal,
// or until the Settings page saves a new one (errReload).
func serve(sig context.Context, loc configLocation) (retErr error) {
	// Anything that fails before the listeners are up is the configuration's
	// fault, which the main loop can undo when a save from the console caused it.
	started := false
	defer func() {
		if retErr != nil && !started && !errors.Is(retErr, errReload) {
			retErr = &startupError{err: retErr}
		}
	}()
	cfg, err := loadConfig(loc.Path)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var storageClient *storage.Client

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	log.Info("database ready, migrations applied")

	if cfg.Storage != nil {
		sc, err := storage.New(*cfg.Storage, log)
		if err != nil {
			return err
		}
		if sc != nil {
			if err := sc.EnsureBucket(ctx); err != nil {
				return fmt.Errorf("object storage: %w", err)
			}
			log.Info("serving recordings from object storage", "bucket", cfg.Storage.Bucket)
			defer func() { _ = sc }()
			storageClient = sc
		}
	}
	if storageClient == nil {
		// Said once, loudly, at startup. Without object storage every recording
		// and the audit chain exist only in this database, on this host --
		// which means whoever compromises the host can delete the record of
		// having done so. The threat model in the console tells operators to
		// replicate the audit log somewhere the gateway cannot write; this is
		// the binary noticing that they have not.
		log.Warn("no object storage configured: recordings and the audit archive have no copy outside this database",
			"fix", "set storage: in the config to an S3-compatible bucket the control plane can write but not delete from")
	}

	var signer *auth.Signer
	if cfg.SigningSecret != "" {
		signer, err = auth.NewSigner(cfg.SigningSecret)
		if err != nil {
			return err
		}
		defer signer.Close()
		signer.SetRedeemer(store)
	}

	rl := control.ThrottleConfig{}
	if cfg.RateLimit != nil {
		rl = control.ThrottleConfig{
			TrustedProxies:   cfg.RateLimit.TrustedProxies,
			LoginPerMinute:   cfg.RateLimit.LoginPerMinute,
			FailuresPerHour:  cfg.RateLimit.FailuresPerHour,
			TicketsPerMinute: cfg.RateLimit.TicketsPerMinute,
		}
	}
	throttles, err := control.NewThrottles(rl)
	if err != nil {
		return fmt.Errorf("rate limit configuration: %w", err)
	}
	defer throttles.Close()
	if len(rl.TrustedProxies) == 0 {
		log.Info("rate limiting keyed on the peer address",
			"detail", "forwarding headers are ignored; set rate_limit.trusted_proxies "+
				"if a load balancer terminates connections in front of this")
	}

	senders, err := alertSenders(cfg.Alerts)
	if err != nil {
		return err
	}
	alerts := notify.NewDispatcher(senders, cfg.ConsoleURL, log)
	if alerts != nil {
		names := make([]string, len(senders))
		for i, s := range senders {
			names[i] = s.Name()
		}
		log.Info("session alerts on", "channels", names)
	}

	api := control.NewAPI(store, log)
	reload := make(chan struct{}, 1)
	api.Settings = &settingsBackend{loc: loc, cfg: cfg, reload: reloadAfter(reload)}
	renewManagedCert(ctx, loc, cfg, log)
	replica, _ := os.Hostname()
	api.Tunnels = control.NewTunnelHub(store, log, replica)
	defer api.Tunnels.Close()
	// Sessions through an agent's tunnel are authorised by a grant signed
	// with a key derived from the signing secret, so every replica holds the
	// same one and an existing configuration needs nothing new.
	if cfg.SigningSecret != "" {
		if api.Tunnels.GrantKey, err = tunnel.GrantKeyFromSecret(cfg.SigningSecret); err != nil {
			return err
		}
	}
	api.Alerts = alerts
	if cfg.Console != nil && cfg.Console.Dir != "" {
		h, err := control.NewConsoleHandler(control.ConsoleConfig{
			Dir: cfg.Console.Dir, Version: version,
			FleetURL: cfg.Console.FleetURL,
		})
		if err != nil {
			return err
		}
		// Assigned only when built: a nil *consoleHandler in the interface
		// field would be a non-nil handler that panics.
		api.Console = h
		log.Info("serving the console", "dir", cfg.Console.Dir)
	}
	api.SetThrottles(throttles)
	api.SetStorage(storageClient)
	// Cookies must be Secure whenever the browser reaches Argus over HTTPS,
	// which includes the case where a proxy terminates TLS and this process
	// only ever sees plain HTTP.
	secureCookies := cfg.TLS != nil || cfg.BehindTLSProxy
	if cfg.SecureCookies != nil {
		secureCookies = *cfg.SecureCookies
	}
	if !secureCookies {
		log.Warn("session cookies are NOT marked Secure",
			"detail", "they will be sent over plain HTTP and can be captured in "+
				"transit; acceptable for local development only")
	}
	api.SetAuth(signer, cfg.ConsoleURL, secureCookies)
	api.SessionTTL = cfg.SessionTTL
	api.TicketTTL = cfg.TicketTTL
	api.UserTokens = cfg.UserTokens

	// Static tokens are a bootstrap convenience and nothing more. The moment a
	// deployment has a real way in -- one local account -- honouring them would
	// mean a string in a config file walking past the password and second
	// factor that account was given.
	//
	// Refused, not ignored. A deployment that thinks its dev token still works
	// will use it, and finding out at the wrong moment is the whole problem.
	if len(cfg.UserTokens) > 0 {
		accounts, err := store.AnyAccountExists(ctx)
		if err != nil {
			return fmt.Errorf("cannot tell whether local accounts exist: %w", err)
		}
		if accounts {
			return errors.New("user_tokens is set and local accounts exist.\n" +
				"Remove user_tokens: a bearer token in a config file must not stand in " +
				"for a password and a second factor")
		}
		log.Warn("static console tokens are active",
			"detail", "no local accounts yet, so these are the only way in; create "+
				"an account with `argus-control users add` and remove user_tokens "+
				"before this is used for anything real")
	} else {
		api.StaticTokensDisabled = true
	}
	if signer != nil && (cfg.WebSetup == nil || *cfg.WebSetup) {
		offerSetup(ctx, store, api, setupTokenPath(cfg.SetupTokenFile, loc.Managed), cfg.ConsoleURL, log)
	}
	api.ReporterToken = cfg.ReporterToken
	api.AllowedOrigins = cfg.AllowedOrigins
	api.BehindTLSProxy = cfg.BehindTLSProxy

	// Sweep for agents that stopped reporting. This is what turns "someone
	// killed the agent" from an invisible event into a visible one.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				n, err := store.MarkStaleAgents(ctx, cfg.AgentStaleAfter)
				if err != nil {
					log.Error("stale agent sweep failed", "error", err)
					continue
				}
				// A session whose agent never came back. Closed at its
				// last report rather than at now(), and marked inferred, so
				// the record says what was observed and what was deduced.
				if closed, cerr := store.CloseAbandonedSessions(ctx, control.SessionAbandonedAfter); cerr != nil {
					log.Error("abandoned session sweep failed", "error", cerr)
				} else {
					for _, sess := range closed {
						log.Warn("closed a session nothing reported for a day",
							"session", sess.ID, "user", sess.UserEmail,
							"target", sess.Principal+"@"+sess.AssetHostname,
							"last_reported", sess.LastReportedAt)
						// One event per session, not one per sweep. An auditor
						// asking what became of a particular session has to
						// find the answer under that session.
						if _, aerr := store.AppendAudit(ctx, control.AuditEvent{
							Action:     "session.closed_unreported",
							Severity:   "warn",
							ActorEmail: "system",
							Target:     sess.ID,
							Detail: "Nothing reported " + sess.UserEmail + "'s session as " +
								sess.Principal + "@" + sess.AssetHostname + " for " +
								control.SessionAbandonedAfter.String() + ". Closed at its last " +
								"report, " + sess.LastReportedAt.Format(time.RFC3339) +
								"; the actual end was not observed.",
						}); aerr != nil {
							log.Error("could not record an inferred session close",
								"session", sess.ID, "error", aerr)
						}
					}
				}
				if _, serr := store.SweepTickets(ctx); serr != nil {
					log.Error("ticket sweep failed", "error", serr)
				}
				if expired, eerr := store.ExpireGrants(ctx); eerr != nil {
					log.Error("grant expiry sweep failed", "error", eerr)
				} else if expired > 0 {
					log.Info("access grants expired", "count", expired)
				}
				if n > 0 {
					log.Warn("agents went silent",
						"count", n,
						"threshold", cfg.AgentStaleAfter.String(),
						"detail", "a host whose agent stopped reporting can no longer record a bypass")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// One listener unless the console is given its own address.
	type listener struct {
		name string
		srv  *http.Server
	}
	var listeners []listener
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	}
	if cfg.ConsoleListen == "" {
		listeners = append(listeners, listener{"control", newServer(cfg.Listen, api.Handler())})
	} else {
		listeners = append(listeners,
			listener{"fleet", newServer(cfg.Listen, api.FleetHandler())},
			listener{"console", newServer(cfg.ConsoleListen, api.ConsoleHandler())})
	}

	if cfg.TLS != nil {
		for _, l := range listeners {
			// A listener that serves no browsers can demand a client
			// certificate at the handshake rather than per route. That is the
			// one security difference the split makes rather than merely
			// moving an address: an unauthorised caller is refused before it
			// reaches a handler at all. The combined and console listeners
			// cannot do this -- a browser has no certificate to offer -- so
			// they verify one when given and the reporter routes check for it
			// themselves, exactly as before.
			requireCert := l.name == "fleet" && cfg.TLS.ClientCAFile != ""
			l.srv.TLSConfig, err = tlsconfig.Server(tlsconfig.ServerOptions{
				CertFile:          cfg.TLS.CertFile,
				KeyFile:           cfg.TLS.KeyFile,
				ClientCAFile:      cfg.TLS.ClientCAFile,
				RequireClientCert: requireCert,
				Log:               log,
			})
			if err != nil {
				return err
			}
		}
		// Unchanged either way: the route-level check is what a leaked reporter
		// token still has to get past, and it does not become optional because
		// a listener happens to be strict.
		api.RequirePeerCert = cfg.TLS.ClientCAFile != ""

		// What an enrolling agent is handed to trust this process. The pin is
		// of the certificate this process serves, so it means nothing when a
		// proxy in front presents another -- there the agent uses the system
		// trust store, or a --control-ca it is given.
		if !cfg.BehindTLSProxy {
			certFile := cfg.TLS.CertFile
			api.Enroll.Pin = func() (string, error) { return tlsconfig.PinFile(certFile) }
		}
		// The internal CA, which in the packaged layout signs this process's
		// certificate as well as the fleet's. Public, so handing it over costs
		// nothing; the agent checks it verifies this server before keeping it.
		if cfg.TLS.ClientCAFile != "" {
			if api.Enroll.CAPEM, err = os.ReadFile(cfg.TLS.ClientCAFile); err != nil {
				return fmt.Errorf("read client_ca_file: %w", err)
			}
		}
	}

	if cfg.Tunnel != nil && cfg.Tunnel.QUICListen != "" {
		if cfg.TLS == nil {
			return errors.New("tunnel.quic_listen needs `tls`: QUIC carries this " +
				"process's certificate, and there is none configured")
		}
		qtls, err := tlsconfig.Server(tlsconfig.ServerOptions{
			CertFile: cfg.TLS.CertFile, KeyFile: cfg.TLS.KeyFile, Log: log,
		})
		if err != nil {
			return err
		}
		qtls.NextProtos = []string{tunnel.ALPN}
		ln, err := quic.ListenAddr(cfg.Tunnel.QUICListen, qtls, tunnel.ServerQUICConfig())
		if err != nil {
			return fmt.Errorf("tunnel.quic_listen: %w", err)
		}
		defer ln.Close()
		if udp, ok := ln.Addr().(*net.UDPAddr); ok {
			api.Tunnels.QUICPort = udp.Port
		}
		go api.ServeQUIC(ctx, ln)
		log.Info("agent tunnels: QUIC listening", "addr", ln.Addr())
	} else {
		log.Info("agent tunnels: QUIC is off; agents connect over HTTPS",
			"detail", "set tunnel.quic_listen for smoother sessions on lossy networks")
	}

	// Bound here rather than in ListenAndServe, so an address that is taken
	// is a startup failure the main loop can undo, not a crash a moment later.
	bound := make([]net.Listener, 0, len(listeners))
	defer func() {
		if !started {
			for _, ln := range bound {
				_ = ln.Close()
			}
		}
	}()
	for _, l := range listeners {
		ln, err := net.Listen("tcp", l.srv.Addr)
		if err != nil {
			return fmt.Errorf("listen on %s (%s): %w", l.srv.Addr, l.name, err)
		}
		bound = append(bound, ln)
	}

	// Shutdown runs where it can be waited for, not in a goroutine.
	//
	// Shutdown closes the listener first, so ListenAndServe returns
	// ErrServerClosed straight away -- and returning on that exited the process
	// while Shutdown was still draining. A gateway posting a session report, or
	// an agent posting an audit event, had its connection cut mid-request by a
	// shutdown that called itself graceful. Those are spooled and retried, so
	// nothing was lost permanently, but the restart was dropping work it had
	// promised to finish.
	served := make(chan error, len(listeners))
	if cfg.TLS == nil {
		log.Warn("argus-control is serving PLAINTEXT HTTP",
			"detail", "session cookies, recordings and the audit log cross this "+
				"listener in the clear; configure `tls` or terminate TLS in front of it")
	}
	started = true
	for i, l := range listeners {
		l, ln := l, bound[i]
		if cfg.TLS != nil {
			log.Info("argus-control listening (TLS)",
				"version", version, "surface", l.name, "addr", l.srv.Addr,
				"mtls", cfg.TLS.ClientCAFile != "")
		} else {
			log.Info("argus-control listening",
				"version", version, "surface", l.name, "addr", l.srv.Addr)
		}
		go func() {
			if cfg.TLS != nil {
				served <- l.srv.ServeTLS(ln, "", "")
				return
			}
			served <- l.srv.Serve(ln)
		}()
	}

	var stopping error
	select {
	case err := <-served:
		// Failed on its own; no shutdown is in flight.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-sig.Done():
		log.Info("shutting down, finishing requests in flight")
	case <-reload:
		log.Info("settings changed from the console; restarting on them", "config", loc.Managed)
		stopping = errReload
	}
	shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	// Every listener, and all of them before the sweeper stops: a request
	// in flight on one must not be cut off because the other drained first.
	for _, l := range listeners {
		if err := l.srv.Shutdown(shutdownCtx); err != nil {
			log.Error("some requests did not finish before the deadline",
				"surface", l.name, "error", err)
		}
	}
	// The sweeper stops only once nothing is being served, so a request
	// already in a handler is not cut off by its own background context.
	cancel()
	for range listeners {
		<-served
	}
	// After the listeners, so an alert for a report that arrived during
	// the drain is still sent.
	alerts.Close(shutdownCtx)
	if stopping == nil {
		log.Info("shutdown complete")
	}
	return stopping
}

// renewManagedCert keeps a certificate the control plane issued itself
// current: at start, and twice a day after, so nobody has to remember it.
func renewManagedCert(ctx context.Context, loc configLocation, cfg config, log *slog.Logger) {
	pki := loc.pki()
	if cfg.TLS == nil || filepath.Clean(cfg.TLS.CertFile) != pki.ServerCert() {
		return
	}
	hosts := urlHosts(cfg.ConsoleURL)
	if cfg.Console != nil {
		hosts = append(hosts, urlHosts(cfg.Console.FleetURL)...)
	}
	renew := func() {
		if done, err := pki.RenewServerIfDue(hosts); err != nil {
			log.Error("could not renew this control plane's certificate", "error", err)
		} else if done {
			log.Info("renewed this control plane's certificate", "file", pki.ServerCert())
		}
	}
	renew()
	go func() {
		t := time.NewTicker(12 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				renew()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func loadConfig(path string) (config, error) {
	cfg := config{
		Listen:          "127.0.0.1:8080",
		DatabaseURL:     "postgres://argus:argus@localhost:5433/argus?sslmode=disable",
		AgentStaleAfter: 2 * time.Minute,
		LogLevel:        "info",
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, errNotSetUp
	}
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	// Resolve ${VAR} and ${file:/path} references before parsing, so the file
	// on disk never has to contain a live credential.
	data, err = secrets.Expand(data)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	absolutize(&cfg, filepath.Dir(path))

	if cfg.ReporterToken == "" {
		// Without this any host that can reach the API could write fabricated
		// sessions into the audit record.
		return cfg, fmt.Errorf("config %s: reporter_token is required", path)
	}
	return cfg, nil
}

// absolutize resolves relative paths against the config file's directory so
// the binary can be started from any working directory.
func absolutize(cfg *config, base string) {
	if cfg.TLS != nil {
		for _, f := range []*string{&cfg.TLS.CertFile, &cfg.TLS.KeyFile, &cfg.TLS.ClientCAFile} {
			if *f != "" && !filepath.IsAbs(*f) {
				*f = filepath.Join(base, *f)
			}
		}
	}
	if cfg.Console != nil && cfg.Console.Dir != "" && !filepath.IsAbs(cfg.Console.Dir) {
		cfg.Console.Dir = filepath.Join(base, cfg.Console.Dir)
	}
}

// setupTokenPath decides where the one-time setup token goes.
func setupTokenPath(configured, configPath string) string {
	if configured != "" {
		return configured
	}
	// The packaged unit runs with ProtectSystem=strict and may write only
	// here, and /etc/argus is not the place for something the service writes.
	if info, err := os.Stat(packagedStateDir); err == nil && info.IsDir() {
		return filepath.Join(packagedStateDir, "setup-token")
	}
	return filepath.Join(filepath.Dir(configPath), "setup-token")
}

// offerSetup opens the console's /setup page when nobody has an account yet.
//
// The token goes in a file only this process's user can read, never in the
// log: logs are shipped off hosts, and whoever reads the token first owns the
// deployment. A failure here costs the browser path and nothing else -- the
// control plane still starts, and `users add` still works.
func offerSetup(ctx context.Context, store *control.Store, api *control.API,
	path, consoleURL string, log *slog.Logger) {
	complete, err := store.SetupComplete(ctx)
	if err != nil {
		log.Warn("could not tell whether setup is complete; the setup page is off", "error", err)
		return
	}
	if complete {
		// Remove a token a previous run left behind once `users add` made the
		// first account instead.
		_ = os.Remove(path)
		return
	}
	token, err := control.NewSetupToken()
	if err != nil {
		log.Warn("the setup page is off", "error", err)
		return
	}
	if err := writeSetupToken(path, token); err != nil {
		log.Warn("could not write the setup token; the setup page is off",
			"file", path, "error", err,
			"fix", "set setup_token_file to a path this user can write, or run `argus-control users add`")
		return
	}
	api.OfferSetup(token, path)
	where := "/setup on the console"
	if consoleURL != "" {
		where = strings.TrimRight(consoleURL, "/") + "/setup"
	}
	log.Info("no accounts yet: finish setup in the browser",
		"open", where, "token_file", path,
		"detail", "the page asks for the token in that file; it works once, and "+
			"`argus-control users add` works too")
}

// writeSetupToken writes the token readable by this user alone. The old file
// goes first: rewriting an existing file keeps its mode, which may be wider.
func writeSetupToken(path, token string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// runAuditVerify recomputes the audit chain and reports the result.
//
// Exits non-zero when the chain is broken, so it drops straight into a restore
// drill or a monitoring check without parsing output.
func runAuditVerify(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	v, err := store.VerifyAuditChain(ctx)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	if v.OK {
		fmt.Printf("INTACT   %d events\n", v.Checked)
		fmt.Printf("  head   %s\n", v.Head)
		fmt.Printf("  took   %s\n", v.Took.Round(time.Millisecond))
		return nil
	}

	fmt.Printf("BROKEN   at sequence %d, after %d intact events\n", v.BrokenAt, v.Checked)
	fmt.Printf("  %s\n\n", v.Detail)
	fmt.Println("  Everything from that sequence onward is unverifiable. Preserve the")
	fmt.Println("  current database, restore from a backup whose chain verifies, and")
	fmt.Println("  treat this as an incident.")
	os.Exit(1)
	return nil
}
