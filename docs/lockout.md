# Locked out of a server: the admin runbook

Read this when nobody can SSH into a server that runs the Argus agent. Most of
the time the fix is one command run as root: `systemctl start argus-agent`.
The rest of this page is about how to get a root shell to run it.

## Why it happens

The agent puts itself in front of sshd with one line in sshd's config:

```
ForceCommand /usr/bin/argus-agent shim
```

Every SSH, SCP and SFTP login then goes through the agent, so the agent can
record it and decide whether to let it in. If the agent is **stopped**, it can
do neither, so the login is refused.

Two different things can refuse a login. Tell them apart by the message:

| What the person sees | What it means | Fix |
| --- | --- | --- |
| `argus: session recording is unavailable (...), refusing connection.` | The agent is stopped or crashed. | [Start the agent](#1-start-the-agent-again). |
| `argus: this server accepts sessions from the Argus console only ...` | The agent is running. The server's **Direct SSH logins** setting is *refuse*. | Open the server from the Argus console, or change the setting to *approve* or *allow* in the console. |
| `argus: this login is waiting for approval in the Argus console.` | The agent is running. Direct SSH is set to *approve*. | An admin approves it under **Access requests** in the console. |
| `argus: nobody approved this login in time, so it was refused.` | Same as above, but nobody approved it. | Approve it next time, or change the setting. |

Only the first row is a lockout. In the other rows the agent is working, and
the Argus console is your way in.

## Who can still get in while the agent is stopped (break-glass)

From the release with break-glass onwards, when the agent is stopped:

- **root** and members of **`sudo`**, **`wheel`** or **`admin`** can still SSH
  in. The session is **not recorded**. They see a warning on their terminal,
  and the shim writes a line tagged `argus-agent` to the system auth log
  (`/var/log/auth.log` or `/var/log/secure`, or `journalctl -t argus-agent`).
- **Everyone else** is still refused.

To choose the groups, add `--break-glass` to the ForceCommand line:

```
ForceCommand /usr/bin/argus-agent shim --break-glass=oncall,ops
```

`--break-glass=none` turns it off for everyone, root included. Turn it off only
if every server has a working out-of-band console (below), because then a
stopped agent locks out every account.

**Check before you need it:** make sure at least one admin account on each
server is in one of those groups and has a key or password that works.
Run `id <user>` to see a user's groups.

On an agent older than break-glass, nobody gets in while the agent is stopped.
Use the provider console.

## 1. Start the agent again

From any root shell on the server, by break-glass SSH or a provider console:

```
sudo systemctl start argus-agent
sudo systemctl status argus-agent        # should say "active (running)"
```

SSH and the Argus console work again as soon as it is running. Then find out
why it stopped, so it does not happen again:

```
sudo journalctl -u argus-agent -n 100 --no-pager
sudo journalctl -u argus-agent -b --no-pager | grep -iE "error|fatal|panic"
```

The unit restarts the agent on its own (`Restart=always`). If the agent keeps
stopping, it is crashing on start, and the log above says why. Common causes
are a full disk (`df -h /var/lib/argus`), a damaged config under
`/etc/argus/`, or a package upgrade that did not finish.

## 2. If the agent will not stay up: take Argus out of SSH

This puts SSH back to how it was before Argus. Logins are no longer recorded
until you undo it.

```
# find the line
sudo grep -rn "ForceCommand.*argus" /etc/ssh/sshd_config /etc/ssh/sshd_config.d/

# comment it out (or edit the file it names and put a # in front)
sudo sed -i 's/^\(\s*ForceCommand.*argus\)/# \1/' \
    /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf

# check the config, then reload sshd (ssh on Debian/Ubuntu, sshd elsewhere)
sudo sshd -t && (sudo systemctl reload ssh || sudo systemctl reload sshd)
```

Keep your current shell open until a new SSH login works from another
terminal.

To undo it once the agent is fixed, remove the `# ` in front of the line and
reload sshd again. `argus-agent scan` confirms that recording is back.

## 3. If nobody can SSH in: use the provider console

A hypervisor or cloud console is a keyboard and screen attached to the VM. It
does not go through SSH, so Argus cannot block it. Log in there with a local
username and **password** (keys do not work on a console), then do step 1.

| Where the server runs | Where the console is |
| --- | --- |
| **Nutanix** | Prism: **VMs** → select the VM → **Launch Console** |
| VMware vSphere / ESXi | vSphere Client: the VM → **Launch Web Console** |
| Proxmox | the VM → **Console** |
| AWS EC2 | the instance → **Connect** → **EC2 Serial Console** (must be enabled for the account) or **Session Manager** |
| Google Cloud | the VM → **Connect to serial console** |
| Azure | the VM → **Help** → **Serial console** |
| DigitalOcean | the Droplet → **Access** → **Launch Droplet Console** (Recovery Console if that fails) |
| Hetzner Cloud | the server → **Console** (top right) |
| Vultr / Linode | the server → **View Console** / **Launch LISH Console** |
| Physical machine | a keyboard and monitor, or its BMC (iDRAC, iLO, IPMI) |

**Check before you need it:** an out-of-band console only helps if some local
account has a password. If every account is key-only, set a strong password
on one admin account now (`sudo passwd <user>`), and keep it in your
password manager.

## 4. If the console has no password that works: rescue mode

1. **Reboot into single-user mode** from the console. At the GRUB menu,
   press `e`, add `init=/bin/bash` to the end of the line that starts with
   `linux`, and press Ctrl-X. You get a root shell with no password. Then:

   ```
   mount -o remount,rw /
   passwd root                       # or: passwd <your admin user>
   sed -i 's/^\(\s*ForceCommand.*argus\)/# \1/' /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf
   sync; exec /sbin/reboot -f
   ```

2. **Or mount the disk from another VM.** Power the server off, attach its
   disk to another VM (or boot the provider's rescue ISO), mount it at
   `/mnt`, and comment out the `ForceCommand` line in
   `/mnt/etc/ssh/sshd_config` and `/mnt/etc/ssh/sshd_config.d/*.conf`.
   Detach the disk and boot the server.

On Nutanix, attach a rescue ISO with **Update VM** → **Disks** → add a CD-ROM
from the image service, and boot from it. On a cloud provider, use its rescue
mode.

After either one, the server boots with SSH working, and you can do step 1 to
bring the agent back.

## Before a lockout: checklist for each server

- [ ] At least one admin account is in `sudo`, `wheel` or `admin` (or in the
      group you named in `--break-glass`) and has a working SSH key.
- [ ] That account, or root, has a password stored somewhere safe, for the
      provider console.
- [ ] You know where this server's provider console is, and you have logged
      in there once.
- [ ] `systemctl is-enabled argus-agent` says `enabled`.
