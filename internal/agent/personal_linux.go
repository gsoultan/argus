//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const personalSupported = true

// newAccountShell is the shell a new personal account gets: bash where there is
// one, which is nearly everywhere, and sh where there is not.
func newAccountShell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

const authorizedKeysHeader = "# Managed by Argus. Rewritten every minute: a key added here by hand does not last.\n"

// writeAuthorizedKeys replaces ~/.ssh/authorized_keys of an account with
// exactly these keys.
//
// The agent is root and the home belongs to the account, so every step goes
// through a directory descriptor opened without following links: an owner who
// turned .ssh into a link to /etc must not get root to write there for them.
func writeAuthorizedKeys(e passwdEntry, keys []string) error {
	if e.home == "" || !filepath.IsAbs(e.home) {
		return fmt.Errorf("the account has no home directory")
	}
	home, err := unix.Open(e.home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open home %s: %w", e.home, err)
	}
	defer unix.Close(home)

	if err := unix.Mkdirat(home, ".ssh", 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("make .ssh: %w", err)
	}
	dir, err := unix.Openat(home, ".ssh", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open .ssh (is it a link?): %w", err)
	}
	defer unix.Close(dir)
	if err := unix.Fchown(dir, e.uid, e.gid); err != nil {
		return fmt.Errorf("chown .ssh: %w", err)
	}
	if err := unix.Fchmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod .ssh: %w", err)
	}

	var b strings.Builder
	b.WriteString(authorizedKeysHeader)
	for _, k := range keys {
		// One key per line, and nothing that would start another.
		if k = strings.TrimSpace(k); k != "" && !strings.ContainsAny(k, "\r\n") {
			b.WriteString(k)
			b.WriteByte('\n')
		}
	}

	tmp := fmt.Sprintf(".authorized_keys.argus-%d", os.Getpid())
	_ = unix.Unlinkat(dir, tmp, 0)
	fd, err := unix.Openat(dir, tmp,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, werr := f.WriteString(b.String())
	if werr == nil {
		werr = f.Chown(e.uid, e.gid)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		return fmt.Errorf("write: %w", werr)
	}
	// Renaming over a link replaces the link, not what it points at.
	if err := unix.Renameat(dir, tmp, dir, "authorized_keys"); err != nil {
		_ = unix.Unlinkat(dir, tmp, 0)
		return fmt.Errorf("replace: %w", err)
	}
	return nil
}

// writeSudoers gives the named accounts sudo with no password, in a drop-in
// file of its own, checked by visudo before it is put in place: a broken
// sudoers file would take sudo away from everyone on the server.
func writeSudoers(ctx context.Context, p *Provisioner, names []string) error {
	path := p.Sudoers
	if path == "" {
		return nil
	}
	if len(names) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sudo is not installed on this server")
	}
	var b strings.Builder
	b.WriteString("# Managed by Argus: personal accounts whose team was given sudo.\n")
	b.WriteString("# Rewritten every minute; edit the team's access in the console instead.\n")
	for _, n := range names {
		if rePersonalName.MatchString(n) {
			fmt.Fprintf(&b, "%s ALL=(ALL:ALL) NOPASSWD: ALL\n", n)
		}
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == b.String() {
		return nil
	}
	tmp := path + ".argus-new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o440); err != nil {
		return err
	}
	if out, err := p.Run(ctx, "visudo", "-cf", tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("visudo refused it: %s", firstLine(out, err))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
