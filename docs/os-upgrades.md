# Upgrading a server's operating system to a new release

Argus moves a managed Linux server to the next major release of its
operating system from the server's **Updates** tab: Ubuntu 22.04 to 24.04,
Debian 12 to 13, Red Hat Enterprise Linux, AlmaLinux, Rocky Linux or Oracle
Linux 8 to 9. This page says what that does, what it needs, and how to
upgrade the one server Argus will not upgrade for you: the one running
`argus-control`.

## What the console offers, and when

The agent works out the server's release and the next one each time it looks
for updates (every six hours, or on **Check now**):

| Distribution | Next release from | The upgrade is done by |
|---|---|---|
| Ubuntu | `do-release-upgrade -c`, so it follows `/etc/update-manager/release-upgrades` (LTS to LTS by default) | `do-release-upgrade`, non-interactive |
| Debian | Argus's own table of stable releases: 10 → 11 → 12 → 13 | apt, as the Debian release notes describe |
| RHEL, AlmaLinux, Rocky, Oracle Linux 7–9 | the next major version | `leapp` (ELevate on the rebuilds) |

SUSE, Fedora, Windows feature updates and macOS are not upgraded from the
console. A server whose distribution is supported but whose tool is missing
says what to install: `ubuntu-release-upgrader-core` on Ubuntu,
`leapp-upgrade` (RHEL) or the ELevate packages (AlmaLinux, Rocky, Oracle
Linux).

The button stays off until the server is ready, and the card says why not:

- updates are waiting for the current release (install them first, with
  **Install it now** on the card; Ubuntu's upgrader refuses otherwise);
- an installed update waits for a reboot (reboot from the same tab);
- `/` or `/var` has less than 3 GB free;
- `argus-control` is installed on the server (see below).

The agent must be new enough to do release upgrades; an older one is asked to
update first. Only admins and owners can ask, the server name has to be typed
to confirm, the rule's **needs a second admin** applies, and every request and
outcome is in the audit log. A release upgrade is asked for one server at a
time, never for a batch.

## What a run does

The run is the same helper as any update: a transient systemd unit outside
the agent, so it goes on if the agent restarts or loses the control plane.

1. **Check.** It refreshes the package lists and works the release out again.
   If the server now offers a different release from the one approved, or is
   no longer ready, it stops without changing anything.
   On a server that boots with BIOS GRUB (`grub-pc`), it also makes sure
   GRUB knows which disk to install the boot loader on. If the disk on record
   is gone, as on a cloud image built elsewhere, nobody would answer the new
   release's question about it, and `grub-pc` would be left unconfigured part
   way through. The helper sets the disk `/boot` (or `/`) is on. If it cannot
   tell, for example with RAID across disks, it stops and asks for
   `sudo dpkg-reconfigure grub-pc` first.
2. **Snapshot.** It records which programs the agent uses on the server are
   present, and the package each came from: `sshd` and its `sftp-server`,
   and the PostgreSQL, MySQL/MariaDB and MongoDB clients.
3. **Upgrade.**
   - Ubuntu: `do-release-upgrade -f DistUpgradeViewNonInteractive`, with
     `RemoveObsoletes=no` in `/etc/update-manager/release-upgrades.d/argus.cfg`
     for the run, so nothing the server had is removed because the new release
     no longer ships it. Third-party apt sources are turned off by the upgrader,
     as always.
   - Debian: the Debian entries in `/etc/apt/sources.list`,
     `sources.list.d/*.list` and `*.sources` are pointed at the new codename
     (a copy of each file is kept under the agent's state directory); other
     entries naming the old codename are turned off and listed. If the new
     lists cannot be downloaded, the old sources are put back and nothing is
     upgraded. Then `apt-get upgrade --without-new-pkgs` and
     `apt-get full-upgrade`. No autoremove.
   - leapp: `leapp preupgrade`; any inhibitor stops the run, quoted with
     leapp's hint, and nothing is changed. Then `leapp upgrade`.

   Changed configuration files are kept (`--force-confold`, and
   `UCF_FORCE_CONFFOLD=1` for files ucf manages such as `sshd_config`), so the
   SSH settings that send logins through Argus survive.
4. **Reboot.** Always. With leapp the upgrade itself happens in this reboot.
5. **Confirm.** The agent that starts on the new boot reads `/etc/os-release`.
   The run is done only if the server is on the release that was approved;
   otherwise it fails and names the log to read
   (`/var/log/dist-upgrade/main.log`, `/var/log/apt/term.log` or
   `/var/log/leapp/leapp-upgrade.log`). It then finishes anything the package
   manager left, installs again any program from step 2 that the upgrade took
   away (from the same package, or the new release's equivalent such as
   `postgresql-client`), removes Argus's upgrade setting, and looks for the new
   release's updates. The run's message says what was put back, what is still
   missing, and which third-party sources to turn back on.

While it runs, the run's card on the **Updates** tab shows the step it is
on, the newest line the upgrader wrote, and its output as it comes. The agent
follows the upgrader's own logs for this (`/var/log/dist-upgrade/main.log` and
`apt-term.log`, or leapp's logs), since `do-release-upgrade` and leapp print
little themselves. Expect long stretches on one package; nothing is stuck
while the newest line still changes.

If the upgrade tool fails part way, the server is **not** rebooted. The
helper runs `dpkg --configure -a` and `apt-get -f install` to finish what can
be finished, and the run's message says what to do next. Do not reboot until
`sudo dpkg --configure -a` and `sudo apt-get -f install` finish cleanly and
`sudo apt full-upgrade` has completed the release.

If apt later refuses sources because their mirror "no longer has a Release
file", the **Updates** tab says which case it is. Either the sources still name
the old release after an upgrade (point them at the new codename), or the
mirror stopped serving the server's own release (switch to `archive.ubuntu.com`
or `deb.debian.org`).

A run cut off part way (power, a person stopping the unit) is marked
interrupted, says the server may be between releases, and the package
manager is repaired as for any interrupted update. A run that goes quiet for
eight hours is marked interrupted by the control plane; a late report from
the agent still lands as the real outcome.

### Why the agent survives it

`argus-agent` is one static binary from a package no distribution ships.
`do-release-upgrade` never removes such a package and leapp leaves it in
place, and its unit asks only for systemd. Its own configuration and state
(`/etc/argus`, `/var/lib/argus`) are not package-managed files the upgrade
touches. The PostgreSQL server packages are on Ubuntu's never-remove list, so
a cluster on the server keeps running on its old major version; moving the
cluster to the new version (`pg_upgradecluster`) is a separate, deliberate step.

Take a backup or snapshot of the server first. A release upgrade cannot be
stopped or undone from the console.

## The argus-control server, by hand

Argus will not upgrade the operating system of a server with `argus-control`
installed: the console would go down part way through and nobody could
follow the run. Upgrade it in a maintenance window:

1. **Back up, and prove it.** `sudo systemctl start argus-backup.service`, then
   `argus-control backup verify` against a scratch database (see
   [Backups](install.md#4-backups)). Copy the backup off the host. Snapshot the
   machine if you can.
2. **Note the PostgreSQL version** the database runs on
   (`sudo -u postgres psql -c 'select version()'`) if it is on this host, and
   the client version (`pg_dump --version`).
3. **Stop the control plane** so nothing writes during the upgrade:
   `sudo systemctl stop argus-control argus-backup.timer`. No session opens
   while it is stopped; agents report again once it is back.
4. **Upgrade** the way the distribution documents it, from a console that
   does not depend on Argus (the cloud provider's serial console, or a direct
   SSH login you have tested): `sudo do-release-upgrade` on Ubuntu, the Debian
   release notes' procedure, or `leapp preupgrade` / `leapp upgrade` on RHEL
   and its rebuilds. Keep the existing version of changed configuration files
   when asked.
5. **Reboot**, then check: `systemctl status argus-control` is active,
   `curl -sk https://localhost:8080/console-config.js` answers, and the console
   signs in and lists the fleet with agents reporting.
6. **PostgreSQL.** If the database is on this host, its old cluster keeps
   running on its old version. Upgrade it when convenient with
   `pg_upgradecluster` (Debian, Ubuntu) or `pg_upgrade`, after another backup.
   Backups need a `pg_dump` of the database's major version or newer: install
   the new client (`postgresql-client`) and run `argus-backup.service` once to
   confirm.
7. **Start the timer again**: `sudo systemctl start argus-backup.timer`.

If the control plane's server also runs an agent, the agent is covered by the
same steps: check `systemctl status argus-agent` after the reboot.
