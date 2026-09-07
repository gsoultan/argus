// Command argus-gateway is the Argus SSH gateway.
//
// It accepts SSH connections, resolves the target from the username, verifies
// the target's host key against a pin, dials out with a vaulted credential the
// user never sees, and records the session as asciicast v2 with a
// tamper-evident hash chain.
//
//	argus-gateway --config argus.yaml
//
// Users connect with a stock client — no wrapper, no agent on the target:
//
//	ssh ops:pay-01@gateway -p 2222
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/secrets"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/gateway"
	"github.com/gsoultan/argus/internal/hostkey"
	"github.com/gsoultan/argus/internal/ratelimit"
	"github.com/gsoultan/argus/internal/reporter"
	"github.com/gsoultan/argus/internal/sshca"
	"github.com/gsoultan/argus/internal/storage"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

var version = "dev"

type config struct {
	Listen         string `yaml:"listen"`
	HostKey        string `yaml:"host_key"`
	AuthorizedKeys string `yaml:"authorized_keys"`
	Inventory      string `yaml:"inventory"`
	HostKeyStore   string `yaml:"host_key_store"`
	RecordingDir   string `yaml:"recording_dir"`
	// TrustOnFirstUse pins an unknown host's key on first contact. Convenient
	// for development; production fleets should pin out of band and leave this
	// off so the very first connection is protected too.
	TrustOnFirstUse bool   `yaml:"trust_on_first_use"`
	LogLevel        string `yaml:"log_level"`

	// Control points at argus-control. Optional — without it the gateway still
	// brokers and records, it is simply not visible in the console.
	Control *controlConfig `yaml:"control"`

	// RateLimit bounds connection attempts per client address.
	RateLimit *gatewayRateLimit `yaml:"rate_limit"`

	// CA enables certificate auth for assets configured for it.
	CA *caConfig `yaml:"ca"`

	// Storage moves sealed recordings to object storage.
	Storage *storage.Config `yaml:"storage"`

	// Web enables the browser terminal. Optional: the gateway is fully usable
	// with ssh(1) alone, and a deployment that does not want a web-reachable
	// shell simply omits this block.
	Web *webConfig `yaml:"web"`

	// RDP enables the Remote Desktop listener. Absent means off: a port that
	// speaks a privileged protocol should be opened deliberately, not acquired
	// by upgrading.
	RDP *rdpConfig `yaml:"rdp"`
}

type rdpConfig struct {
	Listen string `yaml:"listen"`
	// TLS is the certificate Argus presents to RDP clients.
	//
	// Required rather than optional. Argus terminates TLS in order to record
	// the session at all, so it must have an identity of its own; without one
	// there is nothing for a client to verify, which is the situation this
	// product exists to remove.
	TLS *gatewayTLS `yaml:"tls"`
	// DialTimeout bounds the connection to a target. Zero uses the default.
	DialTimeout time.Duration `yaml:"dial_timeout"`
}

type gatewayRateLimit struct {
	// ConnectionsPerMinute per client address. Generous enough that an
	// operator with a script never notices, tight enough that a scanner does.
	ConnectionsPerMinute int `yaml:"connections_per_minute"`
	Burst                int `yaml:"burst"`
}

type caConfig struct {
	// KeyPath is the CA private key. Anyone who can read it can mint
	// credentials for every host that trusts this CA, so it belongs on an
	// encrypted volume at minimum, and behind a KMS or PKCS#11 signer in
	// production.
	KeyPath string `yaml:"key_path"`
	// Validity is how long a minted certificate lasts. It only has to survive
	// the handshake, so minutes is right; an established session is unaffected
	// when its certificate expires.
	Validity time.Duration `yaml:"validity"`
	// SourceAddress pins certificates to the gateway's egress address.
	SourceAddress string `yaml:"source_address"`
}

type controlConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
	Spool string `yaml:"spool"`
	// CAFile verifies the control plane's certificate. Empty uses system roots,
	// which is right for a public CA and wrong for an internal one.
	CAFile string `yaml:"ca_file"`
	// CertFile and KeyFile present a client certificate, so the control plane
	// can attribute a report to this gateway rather than to whoever holds the
	// shared token.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type webConfig struct {
	Listen         string   `yaml:"listen"`
	AllowedOrigins []string `yaml:"allowed_origins"`
	// SigningSecret must match the control plane's, so tickets it mints verify
	// here without a callback on every connection.
	SigningSecret string            `yaml:"signing_secret"`
	Tokens        map[string]string `yaml:"tokens"`
	// TLS serves the browser terminal over HTTPS, which makes the WebSocket
	// wss://. Without it the whole session stream is in the clear.
	TLS *gatewayTLS `yaml:"tls"`
}

type gatewayTLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "argus-gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "argus.yaml", "path to config file")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("argus-gateway %s\n", version)
		return nil
	}

	// `argus-gateway ca ...` manages the certificate authority.
	if flag.NArg() > 0 && flag.Arg(0) == "ca" {
		return runCA(flag.Args()[1:], configPath)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))

	inv, err := gateway.LoadInventory(cfg.Inventory)
	if err != nil {
		return err
	}

	keys, err := hostkey.Open(cfg.HostKeyStore, cfg.TrustOnFirstUse)
	if err != nil {
		return err
	}

	if cfg.TrustOnFirstUse {
		log.Warn("trust-on-first-use is enabled",
			"detail", "unknown host keys are pinned automatically; the first connection to a host is unprotected")
	}

	var rep *reporter.Client
	if cfg.Control != nil && cfg.Control.URL != "" {
		spool := cfg.Control.Spool
		if spool == "" {
			spool = "data/report-spool.jsonl"
		}
		var clientTLS *tls.Config
		if cfg.Control.CAFile != "" || cfg.Control.CertFile != "" {
			clientTLS, err = tlsconfig.Client(tlsconfig.ClientOptions{
				CAFile:   cfg.Control.CAFile,
				CertFile: cfg.Control.CertFile,
				KeyFile:  cfg.Control.KeyFile,
			})
			if err != nil {
				return err
			}
		}
		rep = reporter.NewWithTLS(cfg.Control.URL, cfg.Control.Token, spool, clientTLS, log)
		log.Info("reporting to control plane",
			"url", cfg.Control.URL,
			"verifies_certificate", clientTLS != nil || strings.HasPrefix(cfg.Control.URL, "https://"),
			"client_certificate", cfg.Control.CertFile != "")
		if strings.HasPrefix(cfg.Control.URL, "http://") {
			log.Warn("control plane URL is plaintext http://",
				"detail", "session records and audit events cross this link in the "+
					"clear and can be forged or suppressed in transit")
		}
	}

	// Share replay protection and host-key pins through the control plane.
	//
	// Both are per-instance sets otherwise, and the failure mode is silent: a
	// second gateway accepts replayed tickets and starts with no pins at all,
	// so under trust-on-first-use it accepts a host the first would refuse.
	if rep != nil {
		keys.UseRemote(gateway.NewControlPlanePins(rep))
		log.Info("host key pins shared via the control plane")
	} else {
		log.Warn("host key pins are LOCAL to this gateway",
			"detail", "a second gateway would start with no pins and could silently "+
				"accept a host this one refuses; configure `control` before running "+
				"more than one instance")
	}

	var store *storage.Client
	if cfg.Storage != nil {
		store, err = storage.New(*cfg.Storage, log)
		if err != nil {
			return err
		}
		if store != nil {
			ensureCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := store.EnsureBucket(ensureCtx); err != nil {
				cancel()
				return fmt.Errorf("object storage: %w", err)
			}
			cancel()
			log.Info("recordings will be uploaded", "bucket", cfg.Storage.Bucket)
		}
	}

	var ca *sshca.CA
	if cfg.CA != nil && cfg.CA.KeyPath != "" {
		ca, err = sshca.Load(cfg.CA.KeyPath, cfg.CA.Validity)
		if err != nil {
			return err
		}
		ca.SourceAddress = cfg.CA.SourceAddress
		log.Info("certificate authority loaded",
			"fingerprint", ca.Fingerprint(),
			"validity", ca.Validity.String())
	}

	rlRate, rlBurst := 30, 10
	if cfg.RateLimit != nil {
		if cfg.RateLimit.ConnectionsPerMinute > 0 {
			rlRate = cfg.RateLimit.ConnectionsPerMinute
		}
		if cfg.RateLimit.Burst > 0 {
			rlBurst = cfg.RateLimit.Burst
		}
	}
	authLimiter := ratelimit.New(ratelimit.Limit{
		Rate: rlRate, Window: time.Minute, Burst: rlBurst,
	}, ratelimit.DefaultMaxKeys)
	limiterStop := make(chan struct{})
	defer close(limiterStop)
	authLimiter.StartSweeper(5*time.Minute, limiterStop)
	log.Info("connection rate limit active",
		"per_minute", rlRate, "burst", rlBurst)

	// Starts closed. Every session opened before the first successful fetch
	// runs under the safe configuration, so a control plane that is slow or
	// absent at boot can never be the reason a channel was permitted.
	policy := gateway.NewPolicyHolder()

	srv, err := gateway.NewServer(gateway.Config{
		Listen:             cfg.Listen,
		HostKeyPath:        cfg.HostKey,
		AuthorizedKeysPath: cfg.AuthorizedKeys,
		RecordingDir:       cfg.RecordingDir,
		Inventory:          inv,
		HostKeys:           keys,
		Log:                log,
		CA:                 ca,
		AuthLimiter:        authLimiter,
		Reporter:           rep,
		Storage:            store,
		Policy:             policy,
	})
	if err != nil {
		return err
	}

	ctx, cancelWeb := context.WithCancel(context.Background())
	defer cancelWeb()

	// Retry anything that could not be delivered while the control plane was
	// unavailable, so an outage costs visibility but never a record.
	if rep != nil {
		rep.Drain(ctx)
		rep.StartDrainLoop(ctx, 30*time.Second)
		// Policy is owned by the control plane; without one this gateway keeps
		// the closed defaults, which is what it enforced before policy existed.
		go gateway.SyncPolicy(ctx, policy, rep, log)
	}

	if cfg.Web != nil {
		var ticketSigner *auth.Signer
		if cfg.Web.SigningSecret != "" {
			ticketSigner, err = auth.NewSigner(cfg.Web.SigningSecret)
			if err != nil {
				return err
			}
			defer ticketSigner.Close()
		} else {
			log.Warn("no web.signing_secret — browser terminal falls back to static tokens",
				"detail", "tickets are single-use and short-lived; static tokens are neither")
		}
		if ticketSigner != nil {
			if rep != nil {
				ticketSigner.SetRedeemer(gateway.NewControlPlaneRedeemer(rep))
				log.Info("terminal ticket replay protection shared via the control plane")
			} else {
				log.Warn("terminal ticket replay protection is LOCAL to this gateway",
					"detail", "a ticket burned here stays valid on any other gateway; "+
						"single-use degrades to single-use-per-instance")
			}
		}
		var webTLS *tls.Config
		if cfg.Web.TLS != nil {
			webTLS, err = tlsconfig.Server(tlsconfig.ServerOptions{
				CertFile: cfg.Web.TLS.CertFile,
				KeyFile:  cfg.Web.TLS.KeyFile,
				Log:      log,
			})
			if err != nil {
				return err
			}
		}
		go func() {
			err := srv.ServeWeb(ctx, gateway.WebConfig{
				Listen:         cfg.Web.Listen,
				Signer:         ticketSigner,
				Tokens:         cfg.Web.Tokens,
				AllowedOrigins: cfg.Web.AllowedOrigins,
				TLSConfig:      webTLS,
			})
			if err != nil {
				log.Error("browser terminal failed", "error", err)
			}
		}()
	}

	var rdpSrv *gateway.RDPServer
	if cfg.RDP != nil && cfg.RDP.Listen != "" {
		if cfg.RDP.TLS == nil {
			return fmt.Errorf("rdp.tls is required: Argus terminates TLS to record " +
				"the session, so it must present a certificate of its own")
		}
		rdpTLS, err := tlsconfig.Server(tlsconfig.ServerOptions{
			CertFile: cfg.RDP.TLS.CertFile,
			KeyFile:  cfg.RDP.TLS.KeyFile,
			Log:      log,
		})
		if err != nil {
			return fmt.Errorf("rdp tls: %w", err)
		}

		rdpSrv, err = gateway.NewRDPServer(srv, gateway.RDPConfig{
			Listen:       cfg.RDP.Listen,
			TLS:          rdpTLS,
			RecordingDir: cfg.RecordingDir,
			DialTimeout:  cfg.RDP.DialTimeout,
		})
		if err != nil {
			return fmt.Errorf("rdp: %w", err)
		}
		go func() {
			// A failure here is fatal to RDP but not to SSH: brokered Linux
			// access should not stop because a Windows listener could not bind.
			if err := rdpSrv.Listen(); err != nil {
				log.Error("rdp gateway failed", "error", err)
			}
		}()
	}

	// Drain in-flight sessions on signal rather than cutting them mid-command.
	//
	// Handled below rather than in a goroutine. Closing the listener is the
	// first thing a shutdown does, so Listen returns immediately -- and when
	// main returned on that, the process exited through the middle of its own
	// drain. Every session still open lost its recording to the exit, which is
	// precisely what the drain exists to prevent.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Publish the inventory so the console's credential-mode counts reflect
	// what the gateway actually does, rather than a field nobody sets.
	if rep != nil {
		go func() {
			for _, a := range inv.Unique() {
				mode := a.CredentialMode
				if mode == "" {
					mode = "injected-key"
				}
				rep.Asset(ctx, map[string]any{
					"hostname":       a.Hostname,
					"address":        a.Address,
					"port":           a.Port,
					"principals":     a.Principals,
					"credentialMode": mode,
					"hostKeyState":   "unpinned",
					"health":         "reachable",
				})
			}
		}()
	}

	log.Info("argus-gateway starting",
		"version", version,
		"listen", cfg.Listen,
		"recordings", cfg.RecordingDir)

	// Listen in the background so a signal and a listener failure can be told
	// apart. Waiting on Listen alone cannot distinguish "we are shutting down"
	// from "we could not bind".
	listenErr := make(chan error, 1)
	go func() { listenErr <- srv.Listen() }()

	select {
	case err := <-listenErr:
		// Failed on its own; no shutdown is in flight.
		return err
	case <-stop:
		log.Info("shutting down, waiting for sessions to finish")
		cancelWeb()
		if rdpSrv != nil {
			_ = rdpSrv.Close()
		}
		// Blocking, and on this goroutine. Close drains, then terminates
		// whatever is left so its recording is sealed; returning before it
		// finishes is how the recordings were being lost.
		_ = srv.Close()
		<-listenErr
		log.Info("shutdown complete")
		return nil
	}
}

func loadConfig(path string) (config, error) {
	cfg := config{
		Listen:       "0.0.0.0:2222",
		HostKeyStore: "data/hostkeys.json",
		RecordingDir: "data/recordings",
		LogLevel:     "info",
	}

	data, err := os.ReadFile(path)
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

	// Resolve relative paths against the config file, so the gateway can be
	// started from any working directory.
	base := filepath.Dir(path)
	relative := []*string{
		&cfg.HostKey, &cfg.AuthorizedKeys, &cfg.Inventory,
		&cfg.HostKeyStore, &cfg.RecordingDir,
	}
	if cfg.Web != nil && cfg.Web.TLS != nil {
		relative = append(relative, &cfg.Web.TLS.CertFile, &cfg.Web.TLS.KeyFile)
	}
	if cfg.RDP != nil && cfg.RDP.TLS != nil {
		relative = append(relative, &cfg.RDP.TLS.CertFile, &cfg.RDP.TLS.KeyFile)
	}
	if cfg.Control != nil {
		relative = append(relative, &cfg.Control.CAFile, &cfg.Control.CertFile, &cfg.Control.KeyFile)
	}
	if cfg.CA != nil {
		relative = append(relative, &cfg.CA.KeyPath)
	}
	for _, p := range relative {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}

	for name, v := range map[string]string{
		"host_key":        cfg.HostKey,
		"authorized_keys": cfg.AuthorizedKeys,
		"inventory":       cfg.Inventory,
	} {
		if v == "" {
			return cfg, fmt.Errorf("config %s: %s is required", path, name)
		}
	}
	return cfg, nil
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

// runCA generates a CA and prints what to put on a host.
func runCA(args []string, configPath string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, `argus-gateway ca <command>

  init [path]   generate a CA key pair (default: keys/argus_ca)
  show [path]   print the public key and the sshd_config line for a host

`)
		return nil
	}

	path := "keys/argus_ca"
	if len(args) > 1 {
		path = args[1]
	} else if cfg, err := loadConfig(configPath); err == nil && cfg.CA != nil && cfg.CA.KeyPath != "" {
		path = cfg.CA.KeyPath
	}

	switch args[0] {
	case "init":
		if _, err := os.Stat(path); err == nil {
			// Overwriting a CA silently would orphan every host that trusts it,
			// locking Argus out of the fleet it is supposed to reach.
			return fmt.Errorf("%s already exists; refusing to overwrite a CA that hosts may already trust", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		ca, err := sshca.Generate(path)
		if err != nil {
			return err
		}
		fmt.Printf("Created CA at %s (fingerprint %s)\n\n", path, ca.Fingerprint())
		printHostSetup(ca)
		return nil

	case "show":
		ca, err := sshca.Load(path, 0)
		if err != nil {
			return err
		}
		fmt.Printf("CA %s (fingerprint %s)\n\n", path, ca.Fingerprint())
		printHostSetup(ca)
		return nil
	}
	return fmt.Errorf("unknown ca command %q", args[0])
}

func printHostSetup(ca *sshca.CA) {
	fmt.Printf(`On each host that should accept certificate auth:

  echo '%s' | sudo tee /etc/ssh/argus_ca.pub
  echo 'TrustedUserCAKeys /etc/ssh/argus_ca.pub' | sudo tee -a /etc/ssh/sshd_config
  sudo sshd -t && sudo systemctl reload sshd

Then remove that host's standing keys — the point of certificate auth is that
nothing in authorized_keys outlives a session:

  sudo truncate -s 0 /home/*/.ssh/authorized_keys

Set the asset to "credential_mode": "ca-certificate" in the inventory.
`, ca.PublicKey())
}
