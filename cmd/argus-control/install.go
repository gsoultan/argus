package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

// errStopped is a signal arriving while setup was waiting.
var errStopped = errors.New("stopped before setup finished")

// runInstall serves the setup page until someone finishes it, then returns so
// the caller starts the control plane properly. See internal/control/install.go.
func runInstall(ctx context.Context, loc configLocation, listen string, log *slog.Logger) error {
	if err := writable(loc.dataDir()); err != nil {
		return fmt.Errorf("there is no configuration at %s, and setup cannot write one in %s: %w\n"+
			"Run argus-control as a user that can write there, or pass --config with a path it can",
			loc.Managed, loc.dataDir(), err)
	}

	// A certificate before anything is served: the page carries the database
	// password and the administrator's.
	pki := loc.pki()
	if made, err := pki.EnsureCA(); err != nil {
		return fmt.Errorf("make the control plane's certificate authority: %w", err)
	} else if made {
		log.Info("made a certificate authority for this control plane", "dir", pki.Dir)
	}
	if _, err := os.Stat(pki.ServerCert()); err != nil {
		if err := pki.IssueServer(localHosts()); err != nil {
			return fmt.Errorf("issue the setup page's certificate: %w", err)
		}
	}

	token, err := control.NewSetupToken()
	if err != nil {
		return err
	}
	tokenFile := filepath.Join(loc.dataDir(), "setup-token")
	if err := writeSetupToken(tokenFile, token); err != nil {
		return fmt.Errorf("write the setup token to %s: %w", tokenFile, err)
	}

	var console http.Handler
	consoleDir := detectConsoleDir(loc.Managed)
	if consoleDir != "" {
		if console, err = control.NewConsoleHandler(control.ConsoleConfig{Dir: consoleDir, Version: version}); err != nil {
			return err
		}
	} else {
		log.Warn("no built console found, so the setup page cannot be shown",
			"looked_in", "/usr/share/argus/console, and console/ beside the binary and the configuration",
			"detail", "POST /auth/setup/install still works")
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen for setup on %s: %w (pass --setup-listen to use another address)", listen, err)
	}
	port := 8080
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}

	inst := &installer{loc: loc, consoleDir: consoleDir, log: log, done: make(chan struct{})}
	tlsCfg, err := tlsconfig.Server(tlsconfig.ServerOptions{
		CertFile: pki.ServerCert(), KeyFile: pki.ServerKey(), Log: log,
	})
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{
		Handler: control.NewInstallHandler(control.InstallOptions{
			Token: token, TokenFile: tokenFile, Port: port, Console: console, Installer: inst, Log: log,
		}),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()

	host, _ := os.Hostname()
	log.Info("argus-control is not set up yet: finish setup in the browser",
		"open", fmt.Sprintf("https://%s:%d/setup", host, port),
		"token_file", tokenFile,
		"will_write", loc.Managed,
		"detail", "the page asks for the token in that file. Its certificate is this "+
			"control plane's own, so the browser will warn about it once")

	shutdown := func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}
	select {
	case <-ctx.Done():
		shutdown()
		_ = os.Remove(tokenFile)
		return errStopped
	case <-inst.done:
		// Shutdown waits for the install request to finish writing its answer.
		shutdown()
		_ = os.Remove(tokenFile)
		log.Info("setup finished; starting the control plane", "config", loc.Managed)
		return nil
	case err := <-served:
		return err
	}
}

// installer writes what the setup page decided.
type installer struct {
	loc        configLocation
	consoleDir string
	log        *slog.Logger
	// mu makes two submissions one install.
	mu   sync.Mutex
	done chan struct{}
	over bool
}

func (i *installer) CheckDatabase(ctx context.Context, d control.DatabaseInput) (control.DatabaseCheck, error) {
	dsn, err := d.URL()
	if err != nil {
		return control.DatabaseCheck{}, err
	}
	return control.CheckDatabase(ctx, dsn)
}

func (i *installer) Install(ctx context.Context, in control.InstallRequest, client string) (control.InstallResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.over {
		return control.InstallResult{}, control.BadInput(errors.New("setup has already finished"))
	}

	dsn, err := in.Database.URL()
	if err != nil {
		return control.InstallResult{}, control.BadInput(err)
	}
	check, err := control.CheckDatabase(ctx, dsn)
	if err != nil {
		return control.InstallResult{}, control.BadInput(fmt.Errorf("could not use that database: %w", err))
	}
	if err := in.Validate(check.AccountsExist); err != nil {
		return control.InstallResult{}, control.BadInput(err)
	}
	consoleURL := strings.TrimRight(strings.TrimSpace(in.ConsoleURL), "/")
	gatewayURL := strings.TrimRight(strings.TrimSpace(in.GatewayURL), "/")
	fleetURL := strings.TrimRight(strings.TrimSpace(in.FleetURL), "/")

	pki := i.loc.pki()
	if in.CertPEM != "" {
		if err := pki.WriteServerPair([]byte(in.CertPEM), []byte(in.KeyPEM)); err != nil {
			return control.InstallResult{}, control.BadInput(err)
		}
	} else if _, err := pki.RenewServerIfDue(urlHosts(consoleURL, fleetURL)); err != nil {
		return control.InstallResult{}, fmt.Errorf("issue the certificate: %w", err)
	}

	// Migrations run here, so a database the control plane cannot own fails
	// now, on the page, rather than on the first start after it.
	store, err := control.Open(ctx, dsn)
	if err != nil {
		return control.InstallResult{}, control.BadInput(fmt.Errorf("could not set up that database: %w", err))
	}
	defer store.Close()

	email := ""
	if !check.AccountsExist {
		email = strings.ToLower(strings.TrimSpace(in.Admin.Email))
		switch _, err := store.CreateFirstAccount(ctx, email, strings.TrimSpace(in.Admin.DisplayName),
			"admin", in.Admin.Password); {
		case errors.Is(err, control.ErrSetupComplete):
			email = ""
		case err != nil:
			return control.InstallResult{}, err
		}
	}

	cfg, err := i.config(in, dsn, consoleURL, gatewayURL, fleetURL)
	if err != nil {
		return control.InstallResult{}, err
	}
	if err := writeConfig(i.loc.Managed, cfg); err != nil {
		return control.InstallResult{}, fmt.Errorf("write %s: %w", i.loc.Managed, err)
	}

	detail := "Installed from the console's setup page, from " + client +
		", using the one-time setup token from the host. Configuration written to " + i.loc.Managed + "."
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action: "control.installed", Severity: "notice", ActorEmail: firstNonEmpty(email, "setup"),
		Target: "argus-control", Detail: detail,
	}); err != nil {
		i.log.Error("audit append failed", "error", err)
	}
	if email != "" {
		if _, err := store.AppendAudit(ctx, control.AuditEvent{
			Action: "user.created", Severity: "notice", ActorEmail: email, Target: email,
			Detail: "First local account created through the console's setup page with role admin, from " +
				client + ". Setup is now closed.",
		}); err != nil {
			i.log.Error("audit append failed", "error", err)
		}
	}
	i.log.Info("configuration written by the setup page", "file", i.loc.Managed, "client", client,
		"admin", email)

	i.over = true
	close(i.done)
	return control.InstallResult{ConsoleURL: consoleURL, ConfigFile: i.loc.Managed, Email: email}, nil
}

// config is a fresh install's configuration. Everything the page did not ask
// is a default, and every one of them is on the Settings page afterwards.
func (i *installer) config(in control.InstallRequest, dsn, consoleURL, gatewayURL, fleetURL string) (config, error) {
	dir := i.loc.secretsDir()
	signing, err := randomSecret(48)
	if err != nil {
		return config{}, err
	}
	reporter, err := randomSecret(32)
	if err != nil {
		return config{}, err
	}
	refs := map[string]string{}
	for name, value := range map[string]string{
		"database-url": dsn, "signing-secret": signing, "reporter-token": reporter,
	} {
		if refs[name], err = writeSecret(dir, name, value); err != nil {
			return config{}, fmt.Errorf("write secret %s: %w", name, err)
		}
	}

	pki := i.loc.pki()
	listen := ":" + strconv.Itoa(in.Port)
	cfg := config{
		Listen:        listen,
		DatabaseURL:   refs["database-url"],
		ConsoleURL:    consoleURL,
		SigningSecret: refs["signing-secret"],
		ReporterToken: refs["reporter-token"],
		TLS: &tlsConfig{
			CertFile: pki.ServerCert(), KeyFile: pki.ServerKey(),
			// Gateways prove which host they are with a certificate from this
			// CA, which the Settings page issues them.
			ClientCAFile: pki.CACert(),
		},
		Console: &consoleConfig{Dir: i.consoleDir, GatewayURL: gatewayURL, FleetURL: fleetURL},
		RateLimit: &rateLimitConfig{
			LoginPerMinute: 20, FailuresPerHour: 20, TicketsPerMinute: 30,
		},
		SessionTTL:      8 * time.Hour,
		TicketTTL:       60 * time.Second,
		AgentStaleAfter: 90 * time.Second,
		LogLevel:        "info",
	}
	if in.QUIC {
		cfg.Tunnel = &tunnelConfig{QUICListen: listen}
	}
	return cfg, nil
}

// localHosts is every name this host is likely reached by before anyone has
// said which, for the setup page's certificate.
func localHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, n.IP.String())
			}
		}
	}
	return hosts
}

// urlHosts is the host part of each address, for the certificate.
func urlHosts(urls ...string) []string {
	var out []string
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		if u, err := control.CheckURL(raw, "address", false); err == nil {
			out = append(out, u.Hostname())
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
