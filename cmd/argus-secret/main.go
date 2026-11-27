// Command argus-secret keeps an organization's secrets for Argus.
//
// It is installed and enrolled from argus-control (Settings › Secrets
// servers), which pushes the rules it decides by and collects its read log.
// See internal/vault for what it guarantees.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gsoultan/argus/internal/vault"
	"gopkg.in/yaml.v3"
)

var (
	version = "dev"
	commit  = ""
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		os.Exit(enroll(os.Args[2:]))
	}
	configPath := flag.String("config", "/etc/argus-secret/secret.yaml", "configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	check := flag.Bool("check", false, "check the configuration and the store, then exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("argus-secret %s %s\n", version, commit)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Error("configuration", "file", *configPath, "err", err)
		os.Exit(2)
	}
	if *check {
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			log.Error("configuration", "err", err)
			os.Exit(2)
		}
		fmt.Println("configuration is valid")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := vault.Run(ctx, cfg, version, log); err != nil {
		log.Error("argus-secret stopped", "err", err)
		os.Exit(1)
	}
}

// enroll exchanges the one-time token from Settings › Secrets servers for
// this server's credential, so it can dial argus-control instead of
// listening. Run it once, then restart the service.
func enroll(args []string) int {
	fs := flag.NewFlagSet("argus-secret enroll", flag.ExitOnError)
	configPath := fs.String("config", "/etc/argus-secret/secret.yaml", "configuration file")
	control := fs.String("control", "", "argus-control's https:// address")
	token := fs.String("token", "", "the argus-secret-enrol-… token (or set ARGUS_SECRET_ENROLL_TOKEN)")
	pin := fs.String("pin", "", "sha256:… pin of argus-control's certificate, when the system does not trust it")
	ca := fs.String("ca", "", "CA file that signed argus-control's certificate")
	_ = fs.Parse(args)
	if *token == "" {
		*token = os.Getenv("ARGUS_SECRET_ENROLL_TOKEN")
	}
	if *pin != "" && *ca != "" {
		fmt.Fprintln(os.Stderr, "give --pin or --ca, not both")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	cfg.Defaults()
	e, err := vault.Enroll(context.Background(), vault.EnrollOptions{DataDir: cfg.DataDir, ControlURL: *control,
		Token: *token, Pin: *pin, CAFile: *ca, Version: version, Organization: cfg.Organization})
	if err != nil {
		fmt.Fprintln(os.Stderr, "enroll:", err)
		return 1
	}
	fmt.Printf("enrolled into organization %s as server %s; restart argus-secret to connect to %s\n", e.Org, e.ServerID, e.ControlURL)
	return 0
}

// loadConfig reads the YAML configuration. Unknown keys are refused, so a
// mistyped setting such as behind_tls_proxi is an error rather than a default.
func loadConfig(path string) (vault.Config, error) {
	var cfg vault.Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, err
	}
	return cfg, nil
}
