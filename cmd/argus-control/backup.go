package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/recorder"
	"github.com/gsoultan/argus/internal/storage"
)

/*
Backups for a packaged install.

scripts/backup.sh is a developer's tool: it defaults to dev/control.yaml, the
dev MinIO password over plain http, and a container named argus-postgres. None
of that exists on a server, and it was not in any package. This is the same
sequence as a subcommand of the binary that is already installed, reading the
config that is already there:

	argus-control -config /etc/argus/control.yaml backup run -out /var/backups/argus
	argus-control -config /etc/argus/control.yaml backup verify -scratch-db URL <dir>

Layout under -out:

	recordings/          every object in the bucket, added to and never pruned
	20261010T020000Z/    one directory per run: argus.dump, manifest.json

Recordings are immutable once sealed, so they are mirrored rather than copied
whole every night; database dumps are small and taken in full. Pruning removes
old dump directories only. A recording leaves the backup when someone deletes
it by hand, not because a retention count said so.

The order is load-bearing, as in the script: chain head, then the dump, then
the recordings. A session sealed in between is a recording with no row -- a
harmless orphan. The reverse would be a row whose recording was never copied.
*/

const backupManifest = "manifest.json"

type manifest struct {
	CreatedAt    time.Time `json:"createdAt"`
	ChainHead    string    `json:"chainHead"`
	ChainEvents  int64     `json:"chainEvents"`
	Database     string    `json:"database"`
	DatabaseSHA  string    `json:"databaseSha256"`
	Recordings   int       `json:"recordings"`
	ArgusVersion string    `json:"argusVersion"`
}

func runBackup(configPath string, args []string) error {
	if len(args) > 0 && args[0] == "verify" {
		return runBackupVerify(configPath, args[1:])
	}
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("backup run", flag.ExitOnError)
	out := fs.String("out", "/var/backups/argus", "backup root; one directory per run beneath it")
	keep := fs.Int("keep", 14, "database dumps to keep; recordings are never pruned")
	_ = fs.Parse(args)

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	if _, err := exec.LookPath("pg_dump"); err != nil {
		return errors.New("pg_dump is not installed: install the PostgreSQL client " +
			"(postgresql-client on Debian/Ubuntu, postgresql on RHEL/Alpine) at the " +
			"server's major version or newer")
	}

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 1. The chain head, before the dump, so the dump is guaranteed to hold it.
	v, err := store.VerifyAuditChain(ctx)
	if err != nil {
		return fmt.Errorf("verify audit chain: %w", err)
	}
	if !v.OK {
		return fmt.Errorf("the audit chain is broken at sequence %d (%s); backing up a "+
			"broken chain would only preserve the break. Treat this as an incident", v.BrokenAt, v.Detail)
	}

	dir := filepath.Join(*out, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	// Mkdir, not MkdirAll: two runs in the same second must not share a
	// directory, or the second silently replaces the first's dump.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("backup directory: %w", err)
	}
	fmt.Printf("backing up to %s\n  chain    %d events, head %s\n", dir, v.Checked, short(v.Head))

	// 2. The database.
	dump := filepath.Join(dir, "argus.dump")
	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner",
		"--no-privileges", "--file="+dump, "--dbname="+cfg.DatabaseURL)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump: %w", err)
	}
	sum, size, err := fileSHA256(dump)
	if err != nil {
		return err
	}
	fmt.Printf("  database %s\n", humanBytes(size))

	// 3. Recordings. Configured storage that cannot be read fails the backup:
	// one that silently omits the evidence is worse than none, because it is
	// trusted.
	recordings := 0
	if cfg.Storage != nil && cfg.Storage.Endpoint != "" {
		sc, err := storage.New(*cfg.Storage, nil)
		if err != nil {
			return fmt.Errorf("object storage: %w", err)
		}
		added, total, err := mirrorRecordings(ctx, sc, filepath.Join(*out, "recordings"))
		if err != nil {
			return err
		}
		recordings = total
		fmt.Printf("  recordings %d (%d new)\n", total, added)
	} else {
		fmt.Println("  recordings: no object storage configured; recordings live on the agents only")
	}

	m := manifest{
		CreatedAt: time.Now().UTC(), ChainHead: v.Head, ChainEvents: v.Checked,
		Database: "argus.dump", DatabaseSHA: sum, Recordings: recordings, ArgusVersion: version,
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, backupManifest), b, 0o600); err != nil {
		return err
	}

	pruned, err := pruneBackups(*out, *keep)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if pruned > 0 {
		fmt.Printf("  pruned %d older dump(s), keeping %d\n", pruned, *keep)
	}
	fmt.Printf("done. Prove it restores: argus-control -config %s backup verify -scratch-db <url> %s\n",
		configPath, dir)
	return nil
}

// mirrorRecordings copies every object not already present at its size.
func mirrorRecordings(ctx context.Context, sc *storage.Client, root string) (added, total int, err error) {
	objs, err := sc.List(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, o := range objs {
		dst, err := safeJoin(root, o.Key)
		if err != nil {
			return added, total, err
		}
		total++
		if info, err := os.Stat(dst); err == nil && info.Size() == o.Size {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return added, total, err
		}
		r, err := sc.Get(ctx, o.Key)
		if err != nil {
			return added, total, fmt.Errorf("fetch %s: %w", o.Key, err)
		}
		tmp := dst + ".partial"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err == nil {
			_, err = io.Copy(f, r)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		_ = r.Close()
		if err != nil {
			_ = os.Remove(tmp)
			return added, total, fmt.Errorf("copy %s: %w", o.Key, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return added, total, err
		}
		added++
	}
	return added, total, nil
}

// safeJoin keeps an object key inside root. Keys come from the bucket, which
// is not this process's to trust: "../../etc/cron.d/x" is a valid key.
func safeJoin(root, key string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(key))
	if p != root && !strings.HasPrefix(p, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("object key %q escapes the backup directory", key)
	}
	return p, nil
}

// pruneBackups removes all but the newest keep dump directories.
func pruneBackups(root string, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	var runs []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "recordings" {
			continue
		}
		if _, err := time.Parse("20060102T150405Z", e.Name()); err != nil {
			continue // not ours; never delete what we did not make
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), backupManifest)); err != nil {
			continue // an incomplete run is evidence of a failure, kept for whoever looks
		}
		runs = append(runs, e.Name())
	}
	sort.Strings(runs)
	removed := 0
	for len(runs) > keep {
		if err := os.RemoveAll(filepath.Join(root, runs[0])); err != nil {
			return removed, err
		}
		runs = runs[1:]
		removed++
	}
	return removed, nil
}

// runBackupVerify restores a backup into a scratch database and checks it.
//
// Restoring is the only proof a backup restores. It goes into a database the
// operator names for the purpose, never the live one, and checks the three
// things scripts/drill.sh does: the chain verifies, it contains the head the
// backup recorded, and every recording the database names is present and
// verifies to its stored head.
func runBackupVerify(configPath string, args []string) error {
	fs := flag.NewFlagSet("backup verify", flag.ExitOnError)
	scratch := fs.String("scratch-db", os.Getenv("ARGUS_SCRATCH_DATABASE_URL"),
		"an EMPTY database to restore into; it is overwritten")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: backup verify -scratch-db URL <backup-dir>")
	}
	dir := fs.Arg(0)
	if *scratch == "" {
		return errors.New("-scratch-db is required: a database this check may overwrite")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if sameDatabase(*scratch, cfg.DatabaseURL) {
		return errors.New("-scratch-db is the live database; refusing to restore over it")
	}
	if _, err := exec.LookPath("pg_restore"); err != nil {
		return errors.New("pg_restore is not installed: install the PostgreSQL client")
	}

	raw, err := os.ReadFile(filepath.Join(dir, backupManifest))
	if err != nil {
		return fmt.Errorf("%s: %w (an incomplete backup has no manifest)", dir, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	dump := filepath.Join(dir, m.Database)
	if sum, _, err := fileSHA256(dump); err != nil {
		return err
	} else if sum != m.DatabaseSHA {
		return fmt.Errorf("%s does not match the checksum taken when it was written", dump)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists", "--no-owner",
		"--no-privileges", "--exit-on-error", "--dbname="+*scratch, dump)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}
	fmt.Printf("restored %s into the scratch database\n", dump)

	// Opening it applies any migration newer than the backup, which is also
	// what an upgrade-then-restore would do.
	store, err := control.Open(ctx, *scratch)
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	defer store.Close()

	var problems []string
	v, err := store.VerifyAuditChain(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("verify restored chain: %w", err)
	case !v.OK:
		problems = append(problems, fmt.Sprintf("audit chain broken at sequence %d: %s", v.BrokenAt, v.Detail))
	default:
		fmt.Printf("  chain    intact, %d events\n", v.Checked)
	}
	if m.ChainHead != control.GenesisHash {
		if seq, err := store.AuditHashSeq(ctx, m.ChainHead); err != nil {
			return err
		} else if seq == 0 {
			problems = append(problems, "the restored log does not contain the head "+
				"recorded at backup time ("+short(m.ChainHead)+"); this is not the chain that was backed up")
		} else {
			fmt.Printf("  head     %s present at sequence %d\n", short(m.ChainHead), seq)
		}
	}

	refs, err := store.RecordingIndex(ctx)
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(filepath.Clean(dir)), "recordings")
	var missing, corrupt, unheaded, verified int
	for _, ref := range refs {
		p, err := safeJoin(root, ref.Key)
		if err != nil {
			corrupt++
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			missing++
			if missing <= 5 {
				fmt.Printf("  missing  %s (session %s)\n", ref.Key, ref.SessionID)
			}
			continue
		}
		if ref.ChainHead == "" {
			unheaded++
			_ = f.Close()
			continue
		}
		rv, err := recorder.Verify(f, ref.ChainHead)
		_ = f.Close()
		if err != nil || !rv.OK {
			corrupt++
			if corrupt <= 5 {
				fmt.Printf("  corrupt  %s (session %s)\n", ref.Key, ref.SessionID)
			}
			continue
		}
		verified++
	}
	fmt.Printf("  recordings %d named by the database: %d verified, %d with no stored head\n",
		len(refs), verified, unheaded)
	if missing > 0 {
		problems = append(problems, fmt.Sprintf("%d recording(s) the database names are not in the backup", missing))
	}
	if corrupt > 0 {
		problems = append(problems, fmt.Sprintf("%d recording(s) do not verify to their stored chain head", corrupt))
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Printf("FAIL  %s\n", p)
		}
		return errors.New("this backup is not usable as evidence")
	}
	fmt.Println("OK    this backup restores, and what it restores verifies")
	return nil
}

// sameDatabase compares host, port and database name, which is what decides
// whether a restore would land on the live data.
func sameDatabase(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		return "5432"
	}
	return strings.EqualFold(ua.Hostname(), ub.Hostname()) && port(ua) == port(ub) &&
		strings.Trim(ua.Path, "/") == strings.Trim(ub.Path, "/")
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func short(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
