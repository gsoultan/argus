# Argus is down: the runbook

Read this when the Argus console does not load, servers show as disconnected,
or the outside check-in service tells you Argus stopped calling it. Print it
and keep a copy away from the control plane: you will need it at the moment
Argus cannot show it to you.

(A single server nobody can SSH into is [lockout.md](lockout.md). An
administrator who cannot sign in is [admin-recovery.md](admin-recovery.md).
Argus itself possibly taken over is the last section of this page.)

## What still works, what stops

| | While argus-control is down |
|---|---|
| **The servers themselves** | Run as normal. Nothing Argus does is in the path of your applications. |
| **Agents** | Keep running. They keep recording, refuse what they refused before, and keep the direct SSH setting they last heard, even across a restart. |
| **Session records and recordings** | Kept on each server and sent when Argus is back. Finished recordings stay until Argus has them. The queue of reports is capped at 64 MB per server; when it fills, routine status reports go first and session reports are kept. |
| **Package and upgrade runs already started** | Finish on their own and report when Argus is back. |
| **The console, terminals, desktops, database console, file transfer** | Stop. All of them go through argus-control. |
| **Approvals** | Stop. A server set to *ask for approval* refuses direct logins, because nobody can be asked. |
| **Scheduled database backups, cloud start/stop schedules, scale groups** | Pause. Due work runs when Argus is back; a cloud schedule more than 2 hours late is skipped and admins are told. |
| **Live metrics** | Have a gap. |
| **Alerts from Argus** | Stop, which is why the outside check-in exists (below). |

When argus-control comes back, it records the outage once: a
`control.outage` entry in the audit log, a notice in every organization's
bell with how long it lasted, and a message on the alert channels.

## Before it happens

Settings › Control plane › **If Argus goes down** shows how ready you are and
asks you to confirm what Argus cannot see. In short:

- [ ] **An outside check-in is set.** Settings › Alerts › *When Argus itself
      stops*. Create a check in Healthchecks.io, Uptime Kuma (push monitor),
      Better Stack (heartbeat) or Cronitor, set it to expect a call every
      minute with a few minutes' grace, and paste its address. Each control
      server calls it while it is up and its database answers. Send a test.
      Or point an uptime monitor at `https://<argus>:8080/healthz`, which
      answers `{"status":"ok"}`.
- [ ] **A second control server**, if the console must survive one server.
      Two argus-control servers on the same PostgreSQL, behind a load
      balancer, share everything; either can serve alone.
- [ ] **Backups run, and are copied off the control server.**
      [install.md, step 4](install.md#4-backups). A backup on the server it
      protects covers mistakes, not the loss of the server.
- [ ] **The signing key is kept somewhere else.** `signing_secret` in
      `/var/lib/argus-control/secrets/`. Without it, the encrypted database
      backups Argus takes of your Postgres servers cannot be opened.
- [ ] **You can reach each server without Argus.** Servers set to refuse
      direct logins, or to ask for approval, refuse plain SSH while Argus is
      down. The way in is the provider's console with a local password; see
      [lockout.md](lockout.md#3-if-nobody-can-ssh-in-use-the-provider-console).
      Make sure one local admin account on each server has a password stored
      in your password manager. Or turn on *While Argus cannot be reached* in
      Settings › Logins, so named admin groups can SSH in, recorded, after a
      wait. Weigh it: anyone who can cut a server off from Argus and holds a
      key or password for one of those groups gets in after that wait.

## When it happens

### 1. Is it argus-control, its database, or the network?

Plain SSH to the control server is never gated by Argus, so it is always the
way in to repair Argus.

```
ssh admin@argus-ctl-01
sudo systemctl status argus-control
sudo journalctl -u argus-control -n 100 --no-pager
curl -fsS --cacert /var/lib/argus-control/tls/ca.crt https://localhost:8080/healthz
```

| What you see | What it means | What to do |
|---|---|---|
| `inactive` or `failed` | The process stopped | `sudo systemctl start argus-control`, then read the log for why |
| Log says it cannot reach the database | PostgreSQL is down or unreachable | Fix PostgreSQL first; argus-control reconnects on its own |
| `healthz` answers but browsers cannot reach it | Network, DNS, load balancer or tunnel | Check from a browser on the control server's network |
| Disk full | `df -h /var/lib/argus-control /var/backups/argus` | Free space; old backups beyond the keep count are safe to remove |

### 2. If you need a server while Argus is down

- A server set to **allow** direct logins: SSH in as usual. The agent records
  the session on the server and sends it to Argus later.
- A server set to **refuse** or **ask for approval**: plain SSH is refused
  while Argus is down, unless **Settings › Logins › While Argus cannot be
  reached** is on. Then members of the groups it names get in once the server
  has not heard from Argus for the wait it sets (5 minutes, 15 minutes or an
  hour), still recorded; they are told so on their terminal, and the session
  is audited and put in the bell when Argus is back. Everyone else is still
  refused, and told which groups are let in. A server's Access tab can turn
  it off for that server.
- Otherwise use the provider's console
  ([lockout.md](lockout.md#3-if-nobody-can-ssh-in-use-the-provider-console)).
  Do not stop the agent to get in: stopping it lets root and the
  `sudo`/`wheel`/`admin` groups in **unrecorded**.

### 3. If the control server is lost

Build a new one and restore:

1. Install the same version of `argus-control`, and PostgreSQL client tools of
   the database's major version or newer.
2. Restore the database from the newest backup you copied off the old server
   (`pg_restore` of the dump in `/var/backups/argus/<timestamp>/`) into an
   empty database, and copy the recordings directory back.
3. Put back `/var/lib/argus-control/control.yaml` and its `secrets/` directory,
   or at least the same `signing_secret`. Agents pinned the key derived from
   it; with a different one they act on nothing argus-control asks until you
   delete `grant-key.pub` on each and restart them.
4. Point the old address (DNS name) at the new server, so agents find it.
5. `sudo argus-control audit verify` checks the audit chain end to end.

What is lost: anything written after the backup you restored, except what the
agents still hold. They send their spooled reports and recordings as they
reconnect, so sessions from the outage arrive on their own.

`sudo argus-control backup verify -scratch-db … /var/backups/argus/<timestamp>`
tests a backup against a scratch database before you need it.

## If Argus may have been taken over

Treat a taken-over control server as root on every server it manages: it
holds the key the agents trust.

1. **Stop it.** `sudo systemctl stop argus-control` on every control server,
   or cut them off the network. Agents keep recording and keep refusing what
   they refused.
2. **Keep the evidence.** Each server keeps its own copy of recent recordings
   (7 days by default) under the agent's state directory; `argus-verify`
   checks them without Argus. Copy the audit log and recordings off the
   control server before rebuilding it.
3. **Rebuild on a clean server** from a backup taken before the compromise,
   with a **new** `signing_secret`, then re-pin each agent: delete
   `/var/lib/argus/grant-key.pub` and restart `argus-agent`. Until it has
   pinned the new key, an agent acts on no order from argus-control.
4. **Rotate** every console password and second factor
   ([admin-recovery.md](admin-recovery.md)), cloud credentials stored in
   Argus, and SSH keys of personal accounts Argus created.

If only an administrator's **account** was taken over, not the server, follow
[admin-recovery.md](admin-recovery.md#i-think-the-account-was-taken-over):
`argus-control users sign-out`, then `set-password` and `reset-mfa`.
