package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/controlupdate"
)

// Updating the control plane from the console: see internal/controlupdate.

// defaultUpdateDir is the update directory beside the managed configuration,
// which the control plane and the helper agree on.
const defaultUpdateDir = "/var/lib/argus-control/update"

// updateDir is where this control plane and its helper meet: beside the
// configuration the console manages.
func updateDir(loc configLocation) string {
	return filepath.Join(loc.dataDir(), "update")
}

// runUpdateApply is `argus-control update-apply`, run as root by
// argus-control-update.service when the control plane asks for an update.
// Its own settings come from the unit's environment, never from the control
// plane's files, which the account it updates can write.
func runUpdateApply(args []string) error {
	fs := flag.NewFlagSet("update-apply", flag.ContinueOnError)
	dir := fs.String("dir", defaultUpdateDir, "the control plane's update directory")
	work := fs.String("work", "/var/lib/argus-control-update", "the helper's own directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("update-apply installs a package and must run as root, from argus-control-update.service")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := &controlupdate.Helper{
		Dir: *dir, Work: *work,
		ReleasesURL: os.Getenv("ARGUS_CONTROL_RELEASES_URL"),
		Log:         slog.New(slog.NewTextHandler(os.Stdout, nil)),
	}
	switch h.Apply(ctx) {
	case controlupdate.StateDone, "":
		return nil
	case controlupdate.StateRolledBack:
		return fmt.Errorf("the new version did not come up and was rolled back")
	default:
		return fmt.Errorf("the update failed; see %s", filepath.Join(*dir, controlupdate.StatusFile))
	}
}

// runMigrations is `argus-control migrations -json`: the migrations this
// version carries, which each release publishes so a control plane about to
// update can tell whether going back would be clean.
func runMigrations(args []string) error {
	fs := flag.NewFlagSet("migrations", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := control.MigrationManifest()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"version": version, "migrations": list})
	}
	for _, m := range list {
		if m.Breaking {
			fmt.Printf("%s\tbreaking: %s\n", m.Name, m.Why)
		} else {
			fmt.Println(m.Name)
		}
	}
	return nil
}
