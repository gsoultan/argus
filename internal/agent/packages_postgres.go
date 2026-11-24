package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

/*
PostgreSQL around server updates, on Debian and Ubuntu (postgresql-common).

Updates from apt.postgresql.org bring each new major version's client the day
it ships: the postgresql-client metapackage follows the newest, so a server
whose cluster runs 17 finds psql 19 on its path after a routine update, and
a person reads "psql (19.0, server 17.12)" as a broken upgrade. It is not: a
newer psql talks to an older server. The check says so, and how to match
them if wanted.

Moving the cluster itself to the new major version is postgresql-common's
documented path: install the new server and its extensions, drop the empty
cluster the package created, pg_upgradecluster, analyze, and only once the
applications look fine drop the old cluster. Argus offers it as two runs a
person asks for separately (packages_postgres_apply.go): the upgrade, which
keeps the old cluster stopped beside the new one and a pg_dumpall of it, and
the removal of the old cluster. Until the second, going back is stopping the
new cluster and starting the old one.

Everything here is read; nothing changes until a run is asked for.
*/

// PostgresUpkeep is what the check found about PostgreSQL's major versions.
type PostgresUpkeep struct {
	// Clusters are what pg_lsclusters lists.
	Clusters []PgCluster `json:"clusters"`
	// NewerClient is a client newer than every cluster, which is harmless.
	NewerClient *PgNewerClient `json:"newer_client,omitempty"`
	// Upgrade is the cluster that can move to a newer major version.
	Upgrade *PgUpgrade `json:"upgrade,omitempty"`
	// OldCluster is the stopped cluster an upgrade left behind.
	OldCluster *PgOldCluster `json:"old_cluster,omitempty"`
}

// PgCluster is one line of pg_lsclusters.
type PgCluster struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Online  bool   `json:"online"`
	DataDir string `json:"data_dir,omitempty"`
}

// PgNewerClient is psql and friends from a newer major version than any
// cluster on the server.
type PgNewerClient struct {
	// Client is the newest postgresql-client-N installed; Server the newest
	// cluster's version.
	Client string `json:"client"`
	Server string `json:"server"`
	// ServerPsql is the psql of the server's own version.
	ServerPsql string `json:"server_psql,omitempty"`
}

// PgUpgrade is a cluster that can move to a newer major version, with what
// the run would do and what stands in the way.
type PgUpgrade struct {
	From    string `json:"from"`
	Cluster string `json:"cluster"`
	Port    int    `json:"port"`
	To      string `json:"to"`
	// Packages are what the run installs: the new server and the new
	// version of every extension package the old one has.
	Packages []string `json:"packages"`
	// DropEmpty is an empty cluster of the new version with the same name,
	// which the new server's package creates and the run drops first.
	DropEmpty bool `json:"drop_empty,omitempty"`
	// SizeBytes is the cluster's databases together; FreeBytes the space
	// left where its data lives.
	SizeBytes int64 `json:"size_bytes"`
	FreeBytes int64 `json:"free_bytes"`
	// Method is pg_upgradecluster's: dump for small clusters, upgrade
	// (pg_upgrade, copying the files) for big ones.
	Method string `json:"method"`
	// Blockers are why the run would not start now, each something to fix.
	Blockers []string `json:"blockers,omitempty"`
}

// Expect is how a run names this upgrade: "17/main:19".
func (u PgUpgrade) Expect() string { return u.From + "/" + u.Cluster + ":" + u.To }

// PgOldCluster is a stopped cluster with a newer one of the same name
// running: what an upgrade leaves until a person is sure.
type PgOldCluster struct {
	Version string `json:"version"`
	Cluster string `json:"cluster"`
	Port    int    `json:"port"`
	// Current is the version of the cluster that replaced it.
	Current string `json:"current"`
	// Packages are the old version's packages, removed with it when no
	// other cluster uses that version.
	Packages []string `json:"packages,omitempty"`
	// Backups are the dumps Argus took of it before the upgrade.
	Backups []string `json:"backups,omitempty"`
}

// Expect is how a run names this cluster: "17/main".
func (o PgOldCluster) Expect() string { return o.Version + "/" + o.Cluster }

// Paths and limits, replaced in tests.
var (
	pgBackupDir = "/var/backups/postgresql"
	pgFree      = freeBytes
	// pgDumpMethodBelow is the size under which pg_upgradecluster dumps and
	// restores; above it, pg_upgrade copies the files, which is far faster.
	pgDumpMethodBelow int64 = 5 << 30
	// pgSpareBytes is kept free on top of the copy and the backup.
	pgSpareBytes int64 = 1 << 30
)

var (
	rePgMajor     = regexp.MustCompile(`^[0-9]{1,3}$`)
	rePgClusterNm = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	rePgServerPkg = regexp.MustCompile(`^postgresql-([0-9]{1,3})$`)
	rePgClientPkg = regexp.MustCompile(`^postgresql-client-([0-9]{1,3})$`)
	rePgCandidate = regexp.MustCompile(`(?m)^\s*Candidate:\s*(\S+)`)
)

// postgres works out what to say about PostgreSQL. nil when the server has
// no postgresql-common, so nothing of this applies.
func (c PackageChecker) postgres(ctx context.Context, installed map[string]string) *PostgresUpkeep {
	if _, err := c.Look("pg_lsclusters"); err != nil {
		return nil
	}
	out, code, err := output(ctx, c.Run, nil, "pg_lsclusters", "--no-header")
	if err != nil || code != 0 {
		return nil
	}
	pu := &PostgresUpkeep{Clusters: parsePgLsclusters(out)}
	if len(pu.Clusters) == 0 && !anyPgServer(installed) {
		return nil
	}
	pu.NewerClient = newerClient(pu.Clusters, installed)
	pu.OldCluster = oldCluster(pu.Clusters, installed)
	if pu.OldCluster != nil {
		pu.OldCluster.Backups = pgBackupsOf(pu.OldCluster.Version, pu.OldCluster.Cluster)
	}
	if to := c.newestPgServer(ctx); to != "" {
		pu.Upgrade = c.pgUpgrade(ctx, pu.Clusters, installed, to)
	}
	return pu
}

// parsePgLsclusters reads `pg_lsclusters --no-header`:
//
//	17  main    5432 online postgres /var/lib/postgresql/17/main /var/log/postgresql/postgresql-17-main.log
//	19  main    5433 down   postgres /var/lib/postgresql/19/main /var/log/postgresql/postgresql-19-main.log
//
// A standby says "online,recovery".
func parsePgLsclusters(b []byte) []PgCluster {
	var out []PgCluster
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !rePgMajor.MatchString(strings.Split(f[0], ".")[0]) {
			continue
		}
		port, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		cl := PgCluster{Version: f[0], Name: f[1], Port: port, Online: strings.HasPrefix(f[3], "online")}
		if len(f) >= 6 {
			cl.DataDir = f[5]
		}
		out = append(out, cl)
	}
	return out
}

func anyPgServer(installed map[string]string) bool {
	for n := range installed {
		if rePgServerPkg.MatchString(n) {
			return true
		}
	}
	return false
}

// newerClient is the client from a major version no cluster runs, newer
// than all of them. nil when the clients and the clusters agree.
func newerClient(clusters []PgCluster, installed map[string]string) *PgNewerClient {
	client := ""
	for n := range installed {
		if m := rePgClientPkg.FindStringSubmatch(n); m != nil && versionNewer(m[1], client) {
			client = m[1]
		}
	}
	server := ""
	for _, cl := range clusters {
		if cl.Version == client {
			return nil
		}
		if versionNewer(cl.Version, server) {
			server = cl.Version
		}
	}
	if client == "" || server == "" || !versionNewer(client, server) {
		return nil
	}
	return &PgNewerClient{Client: client, Server: server,
		ServerPsql: "/usr/lib/postgresql/" + server + "/bin/psql"}
}

// oldCluster is a stopped cluster with a newer one of the same name online.
func oldCluster(clusters []PgCluster, installed map[string]string) *PgOldCluster {
	for _, old := range clusters {
		if old.Online {
			continue
		}
		for _, cur := range clusters {
			if cur.Name != old.Name || !cur.Online || !versionNewer(cur.Version, old.Version) {
				continue
			}
			o := &PgOldCluster{Version: old.Version, Cluster: old.Name, Port: old.Port, Current: cur.Version}
			others := false
			for _, cl := range clusters {
				others = others || (cl.Version == old.Version && cl.Name != old.Name)
			}
			if !others {
				o.Packages = pgVersionPackages(installed, old.Version)
			}
			return o
		}
	}
	return nil
}

// pgVersionPackages are the server packages of one major version: the
// server and its extensions, not its client, which the newer one replaces
// anyway and other tools may still name.
func pgVersionPackages(installed map[string]string, ver string) []string {
	var out []string
	for n := range installed {
		if n == "postgresql-"+ver || (strings.HasPrefix(n, "postgresql-"+ver+"-") && !strings.HasSuffix(n, "-dbgsym")) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// pgBackupsOf are the dumps an upgrade run kept of a cluster.
func pgBackupsOf(ver, name string) []string {
	m, _ := filepath.Glob(filepath.Join(pgBackupDir, "argus-"+ver+"-"+name+"-*.sql"))
	sort.Strings(m)
	return m
}

// pgBackupPath is where a run keeps a cluster's dump.
func pgBackupPath(ver, name, stamp string) string {
	return filepath.Join(pgBackupDir, "argus-"+ver+"-"+name+"-"+stamp+".sql")
}

// newestPgServer is the newest postgresql-N apt can install, "" when apt
// cannot say.
func (c PackageChecker) newestPgServer(ctx context.Context) string {
	out, code, err := output(ctx, c.Run, aptEnv, "apt-cache", "pkgnames", "postgresql-")
	if err != nil || code != 0 {
		return ""
	}
	var vers []string
	for _, n := range strings.Fields(string(out)) {
		if m := rePgServerPkg.FindStringSubmatch(n); m != nil {
			vers = append(vers, m[1])
		}
	}
	sort.Slice(vers, func(i, j int) bool { return versionNewer(vers[i], vers[j]) })
	for i, v := range vers {
		if i == 3 {
			break
		}
		if c.aptCandidate(ctx, "postgresql-"+v) != "" {
			return v
		}
	}
	return ""
}

// aptCandidate is the version apt would install of name, "" for none.
func (c PackageChecker) aptCandidate(ctx context.Context, name string) string {
	out, code, err := output(ctx, c.Run, aptEnv, "apt-cache", "policy", name)
	if err != nil || code != 0 {
		return ""
	}
	m := rePgCandidate.FindSubmatch(out)
	if m == nil || string(m[1]) == "(none)" {
		return ""
	}
	return string(m[1])
}

// pgUpgrade is the offer to move the one running cluster older than to.
func (c PackageChecker) pgUpgrade(ctx context.Context, clusters []PgCluster, installed map[string]string,
	to string) *PgUpgrade {
	var older []PgCluster
	for _, cl := range clusters {
		if cl.Online && versionNewer(to, cl.Version) {
			older = append(older, cl)
		}
	}
	// One cluster is a run's to move. Several are a person's to order.
	if len(older) != 1 {
		return nil
	}
	src := older[0]
	if !rePgMajor.MatchString(src.Version) || !rePgClusterNm.MatchString(src.Name) {
		return nil
	}
	// A newer cluster of this name already running is an upgrade done.
	for _, cl := range clusters {
		if cl.Name == src.Name && cl.Online && versionNewer(cl.Version, src.Version) && cl.Version != to {
			return nil
		}
	}
	u := &PgUpgrade{From: src.Version, Cluster: src.Name, Port: src.Port, To: to, Method: "dump",
		Packages: []string{"postgresql-" + to}}

	for _, p := range pgVersionPackages(installed, src.Version) {
		if p == "postgresql-"+src.Version {
			continue
		}
		want := "postgresql-" + to + strings.TrimPrefix(p, "postgresql-"+src.Version)
		if _, have := installed[want]; have || c.aptCandidate(ctx, want) != "" {
			u.Packages = append(u.Packages, want)
			continue
		}
		u.Blockers = append(u.Blockers, fmt.Sprintf("%s is installed, and apt has no %s for PostgreSQL %s: "+
			"if no database uses it, remove it (`sudo apt remove %s`); otherwise wait until its maker ships it for %s",
			p, want, to, p, to))
	}

	psql := func(port int, db, sql string) (string, error) {
		b, code, err := output(ctx, c.Run, nil, "runuser", "-u", "postgres", "--", "psql", "-X", "-A", "-t", "-q",
			"-p", strconv.Itoa(port), "-d", db, "-c", sql)
		if err == nil && code != 0 {
			err = fmt.Errorf("psql exited %d: %s", code, tail(b, 200))
		}
		return strings.TrimSpace(string(b)), err
	}
	row, err := psql(src.Port, "postgres", "select (select sum(pg_database_size(oid)) from pg_database)::bigint, "+
		"pg_is_in_recovery(), (select count(*) from pg_stat_replication)")
	if err != nil {
		u.Blockers = append(u.Blockers, fmt.Sprintf("Argus could not ask PostgreSQL %s %s (port %d) how big it is: %v",
			src.Version, src.Name, src.Port, err))
	} else {
		f := strings.Split(row, "|")
		if len(f) == 3 {
			u.SizeBytes, _ = strconv.ParseInt(f[0], 10, 64)
			if f[1] == "t" {
				u.Blockers = append(u.Blockers, "this cluster is a standby of another server; upgrade the primary "+
					"and its standbys together, by hand")
			}
			if f[2] != "0" {
				u.Blockers = append(u.Blockers, "this cluster sends its changes to "+f[2]+" standby server(s); a major "+
					"upgrade breaks replication, so upgrade the primary and its standbys together, by hand")
			}
		}
	}
	if u.SizeBytes >= pgDumpMethodBelow {
		u.Method = "upgrade"
	}

	dataDir := src.DataDir
	if dataDir == "" {
		dataDir = "/var/lib/postgresql"
	}
	u.FreeBytes = pgFree(filepath.Dir(dataDir))
	// The new cluster's copy and the dump beside it, plus room to spare.
	if need := 2*u.SizeBytes + pgSpareBytes; u.FreeBytes > 0 && u.FreeBytes < need {
		u.Blockers = append(u.Blockers, fmt.Sprintf("the upgrade needs about %s free for the new cluster and a "+
			"backup of this one, and %s has %s; grow the disk or free space first", humanBytes(need),
			filepath.Dir(dataDir), humanBytes(u.FreeBytes)))
	}

	for _, cl := range clusters {
		if cl.Version != to || cl.Name != src.Name {
			continue
		}
		if !cl.Online {
			u.Blockers = append(u.Blockers, fmt.Sprintf("PostgreSQL %s already has a cluster named %s (port %d), and it "+
				"is not running, so Argus cannot see whether it is empty. If it is not needed, drop it with "+
				"`sudo pg_dropcluster %s %s`", to, cl.Name, cl.Port, to, cl.Name))
			break
		}
		n, err := psql(cl.Port, "postgres", "select (select count(*) from pg_database where datname not in "+
			"('postgres','template0','template1')) + (select count(*) from pg_class c join pg_namespace n on "+
			"n.oid = c.relnamespace where n.nspname not in ('pg_catalog','information_schema','pg_toast'))")
		switch {
		case err != nil:
			u.Blockers = append(u.Blockers, fmt.Sprintf("Argus could not see whether PostgreSQL %s %s (port %d) is "+
				"empty: %v", to, cl.Name, cl.Port, err))
		case n != "0":
			u.Blockers = append(u.Blockers, fmt.Sprintf("PostgreSQL %s already has a cluster named %s (port %d) with "+
				"data in it, and Argus will not drop it. Look at it, and if it is not needed drop it with "+
				"`sudo pg_dropcluster %s %s --stop`", to, cl.Name, cl.Port, to, cl.Name))
		default:
			u.DropEmpty = true
		}
	}

	if _, err := os.Stat(argusControlBinary); err == nil {
		u.Blockers = append(u.Blockers, "argus-control runs on this server, and its database may be this cluster: "+
			"the console would go down part way. Upgrade it by hand, as the operating system upgrade guide describes")
	}
	return u
}

// pgInstalled reads every installed package whose name starts with
// postgresql, and whether anything came from apt.postgresql.org.
func (c PackageChecker) pgInstalled(ctx context.Context) (map[string]string, bool) {
	out, code, err := output(ctx, c.Run, nil, "dpkg-query", "-W", "-f",
		"${Package}\t${db:Status-Abbrev}\t${Version}\n", "postgresql*", "libpq5")
	// dpkg-query exits 1 when a pattern matches nothing installed.
	if err != nil || (code != 0 && code != 1) {
		return nil, false
	}
	got, pgdg := map[string]string{}, false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || !strings.HasPrefix(strings.TrimSpace(f[1]), "ii") {
			continue
		}
		got[f[0]] = f[2]
		pgdg = pgdg || strings.Contains(f[2], "pgdg")
	}
	return got, pgdg
}
