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
