# Locked out of the console: getting an administrator back in

Read this when nobody who administers Argus can sign in to the console: the
last admin lost their phone and their recovery codes, forgot their password,
was demoted or removed by mistake, or you suspect their account was taken over.
(For a server nobody can SSH into, see [lockout.md](lockout.md) instead.)

## The rule

**Recovery happens on the control plane host, never in the console.**

The console cannot have a "reset the administrator" button, because the person
who would press it is the one who is locked out, and anything that lets a
stranger press it is a way in that nobody was given. So every recovery command
runs as `argus-control` on the host, by someone who already has a root shell
there and can read `/etc/argus/argus-control.yaml`. That is the same privilege
boundary the first account was created across, and it already exists: whoever
has that shell can read the database anyway. The commands give them a way that
is checked, refuses the dangerous cases, and leaves an audit entry under their
host user name.

Every command below:

- signs the account out of every browser the moment it runs,
- leaves a `warning` entry in the audit log, attributed to `<you>@host`,
- changes exactly one thing and prints what it did.

```
sudo argus-control users list                       # who exists, with role, MFA and state
sudo argus-control users reset-mfa <email>          # lost authenticator and recovery codes
sudo argus-control users set-password <email>       # forgotten password (prompts, no echo)
sudo argus-control users set-role <email> owner     # nobody with admin or owner can sign in
sudo argus-control users enable <email>             # removed by mistake
sudo argus-control users sign-out <email>           # the cookie may have been stolen
sudo argus-control users add <email> --role owner   # make a brand new administrator
```

`argus-control` reads the config from `/etc/argus/argus-control.yaml`; pass
`-config <path>` if it lives elsewhere.

## Pick the situation

### "I lost my phone" (authenticator gone, recovery codes gone too)

If any recovery code is left, use it: on the sign-in form, after the password,
choose **I cannot use my authenticator app** and enter one. Then open the
account menu, **Replace my authenticator**, and set up a new one. No host
access needed.

If another admin or owner can sign in, they do it from the console: **Teams →
People → Security → Reset lost authenticator** on your row. Identify the person
first by a channel you trust, not by the email asking.

Otherwise, on the host:

```
sudo argus-control users reset-mfa dewi.p@example.com
```

Their next sign-in takes the password only, and the console then shows nothing
but the enrolment screen until a new authenticator is confirmed. Tell them to
expect that.

### "I forgot my password"

```
sudo argus-control users set-password dewi.p@example.com
```

Type the new one twice (nothing is echoed). Their second factor is unchanged,
so they still need their authenticator. If that is lost as well, run
`reset-mfa` too.

Hand the password over by a channel the account holder already uses, and ask
them to change it from the account menu afterwards.

### "Nobody with admin or owner can sign in"

Everyone who could administer the deployment was demoted, removed, or has lost
access. Pick the account you trust most and put it back in charge:

```
sudo argus-control users list
sudo argus-control users enable dewi.p@example.com      # if it shows "disabled"
sudo argus-control users set-role dewi.p@example.com owner
```

Then recover its credentials with the commands above if it needs them. If no
suitable account exists at all, make one:

```
sudo argus-control users add oncall@example.com --role owner --name "On-call"
```

`users add` refuses an address that already has an account, so it cannot be
used to overwrite someone's password quietly. Use `set-password` for that, by
name.

### "I think the account was taken over"

In this order:

```
sudo argus-control users sign-out dewi.p@example.com        # every browser, now
sudo argus-control users set-password dewi.p@example.com    # a password the attacker does not have
sudo argus-control users reset-mfa dewi.p@example.com       # only if the attacker could have enrolled their own
```

Then read the audit log (**Activity**, or `argus-control audit verify` for the
chain) for `auth.login`, `auth.recovery_code_used`, `auth.mfa_enrolled`,
`auth.account_throttled` and anything under `user.` or `account.` in the
window you care about, and look at which sessions were opened under that
account in the same period. If the attacker used a stolen cookie rather than
the password, `sign-out` alone ends it: a cookie that was signed out stops
working on the next request, on every replica.

## What the console does on its own

These exist so most of the above never needs a host shell:

- **An admin resets someone else's second factor or signs them out**: Teams →
  People → Security. An admin cannot do it to an owner, and nobody can do it
  to themselves from a session (that would let a stolen session strip its own
  second factor).
- **Someone signs themselves out everywhere**: account menu → **Sign out
  everywhere**. For "I left myself signed in on a machine I no longer trust".
- **Replacing an authenticator** asks for the password and a code from the
  current one (or a recovery code). The old one stays in force until the new
  one is confirmed.
- **Being guessed at does not lock you out**: after too many wrong passwords
  from strangers, the account refuses sign-ins from browsers it has never seen
  for up to an hour, but a browser that has signed in to it before is let
  through to the password check. An alert goes to the configured channels and
  the audit log says `auth.account_throttled`.

## The last-admin rule, and why it is not a backdoor

The console and the CLI both refuse to disable or demote the last active admin
or owner. That rule is what keeps recovery rare. It does not help once that
last admin cannot sign in, which is what the commands above are for.

None of these commands can be reached over the network. There is no token, no
URL and no email that triggers them. If your control plane host is compromised,
the attacker already has the database and the signing secret; these commands
add nothing they do not have. If it is not, nobody but your host administrators
can run them, and the audit log says who did.

## Before you need it

- [ ] At least two accounts hold the admin or owner role, each with a second
      factor and recovery codes stored in a password manager.
- [ ] Someone on call has root on the control plane host and knows this page.
- [ ] `alerts` is configured, so `auth.account_throttled`, recovery-code
      sign-ins and second-factor resets reach a phone.
- [ ] The database is backed up (`argus-control backup run`): the recovery
      commands change rows, and a backup is what makes any mistake reversible.
