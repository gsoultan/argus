// Command argus-control is the Argus control plane.
//
// It is the system of record: assets, sessions, agent liveness and the
// hash-chained audit log. The console reads from it; gateways and agents report
// into it.
//
// Deliberately not on the critical path for a session. If this is down, the
// gateway still brokers and records, and the agent still captures bypasses —
// they spool locally and reconcile when it returns. A PAM tool whose outage
// stops people working gets removed.
//
//	argus-control --config control.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/storage"
)

var version = "dev"

type config struct {
	Listen         string            `yaml:"listen"`
	DatabaseURL    string            `yaml:"database_url"`
	AllowedOrigins []string          `yaml:"allowed_origins"`
	UserTokens     map[string]string `yaml:"user_tokens"`
	ReporterToken  string            `yaml:"reporter_token"`
	// AgentStaleAfter is how long an agent may be silent before the fleet is
	// told. Short on purpose: silence is how a killed agent looks.
	AgentStaleAfter time.Duration    `yaml:"agent_stale_after"`
	Storage         *storage.Config  `yaml:"storage"`
	OIDC            *auth.OIDCConfig `yaml:"oidc"`
	// SigningSecret signs session cookies and terminal tickets. Shared with the
	// gateway so it can verify tickets without calling back.
	SigningSecret string        `yaml:"signing_secret"`
	ConsoleURL    string        `yaml:"console_url"`
	SecureCookies bool          `yaml:"secure_cookies"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
	TicketTTL     time.Duration `yaml:"ticket_ttl"`
	LogLevel      string        `yaml:"log_level"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "argus-control: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "control.yaml", "path to config file")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("argus-control %s\n", version)
		return nil
	}

	cfg, err := loadConfig(configPath)
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

	var signer *auth.Signer
	if cfg.SigningSecret != "" {
		signer, err = auth.NewSigner(cfg.SigningSecret)
		if err != nil {
			return err
		}
	}

	var provider *auth.OIDC
	if cfg.OIDC != nil {
		if signer == nil {
			return fmt.Errorf("signing_secret is required when oidc is configured")
		}
		provider, err = auth.NewOIDC(ctx, *cfg.OIDC)
		if err != nil {
			return err
		}
		if provider != nil {
			log.Info("OIDC enabled", "issuer", cfg.OIDC.Issuer,
				"default_role", cfg.OIDC.DefaultRole)
		}
	}
	if provider == nil {
		log.Warn("OIDC is not configured — falling back to static console tokens",
			"detail", "acceptable for development only; every action is still "+
				"attributed, but anyone holding the token is that person")
	}

	api := control.NewAPI(store, log)
	api.SetStorage(storageClient)
	api.SetAuth(provider, signer, cfg.ConsoleURL, cfg.SecureCookies)
	api.SessionTTL = cfg.SessionTTL
	api.TicketTTL = cfg.TicketTTL
	api.UserTokens = cfg.UserTokens
	api.ReporterToken = cfg.ReporterToken
	api.AllowedOrigins = cfg.AllowedOrigins

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

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("argus-control listening", "version", version, "addr", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func loadConfig(path string) (config, error) {
	cfg := config{
		Listen:          "127.0.0.1:8080",
		DatabaseURL:     "postgres://argus:argus@localhost:5433/argus?sslmode=disable",
		AgentStaleAfter: 2 * time.Minute,
		LogLevel:        "info",
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.ReporterToken == "" {
		// Without this any host that can reach the API could write fabricated
		// sessions into the audit record.
		return cfg, fmt.Errorf("config %s: reporter_token is required", path)
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
