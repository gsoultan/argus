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
	if len(os.Args) > 1 && os.Args[1] == "rekey" {
		os.Exit(rekey(os.Args[2:]))
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

// rekey replaces the organization key, gives every value a new data key and
// re-encrypts everything in the committing format; --rotate-root also moves
// it all under a new root key. Stop the service first: the store is locked
// while argus-secret runs.
func rekey(args []string) int {
	fs := flag.NewFlagSet("argus-secret rekey", flag.ExitOnError)
	configPath := fs.String("config", "/etc/argus-secret/secret.yaml", "configuration file")
	rotateRoot := fs.Bool("rotate-root", false, "also make a new root key (root_key_file only)")
	reason := fs.String("reason", "", "why, for the audit log")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	cfg.Defaults()
	if _, err := cfg.ApplyEnrollment(); err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		return 2
	}
	root, src, _, err := vault.LoadConfiguredRootKey(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "root key:", err)
		return 1
	}
	defer clear(root)
	if *rotateRoot && src != vault.RootKeyFromFile {
		fmt.Fprintf(os.Stderr, "the root key comes from %s: rotate it there, then run rekey without --rotate-root\n", src)
		return 2
	}
	store, err := vault.Open(cfg.DataDir, root, cfg.Organization)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err, "(is argus-secret still running? stop it first)")
		return 1
	}
	defer store.Close()
	var newRoot []byte
	next := cfg.RootKeyFile + ".new"
	if *rotateRoot {
		if newRoot, err = vault.NewRootKeyFile(next); err != nil {
			fmt.Fprintln(os.Stderr, "new root key:", err)
			return 1
		}
		defer clear(newRoot)
	}
	who := "argus-secret rekey"
	if u := os.Getenv("SUDO_USER"); u != "" {
		who += " by " + u
	}
	res, err := store.Rekey(root, newRoot, vault.Actor{Kind: vault.ActorSystem, ID: who}, *reason)
	if err != nil {
		if *rotateRoot {
			_ = os.Remove(next)
		}
		fmt.Fprintln(os.Stderr, "rekey:", err, "(nothing was changed)")
		return 1
	}
	if *rotateRoot {
		// The store now opens only with the new key: put it in place. If this
		// fails, the new key is at root.key.new; move it over root.key by hand.
		if err := os.Rename(next, cfg.RootKeyFile); err != nil {
			fmt.Fprintf(os.Stderr, "the store moved to the new root key, but it could not replace %s: %v\nmove %s over it before starting argus-secret\n", cfg.RootKeyFile, err, next)
			return 1
		}
	}
	fmt.Printf("re-encrypted %d values under a new organization key", res.Values)
	if res.RootRotate {
		fmt.Printf(" and a new root key in %s; back it up now and destroy copies of the old one", cfg.RootKeyFile)
	}
	fmt.Println(". Start argus-secret again.")
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
