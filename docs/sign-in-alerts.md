# Sign-in alerts

When someone signs in to the Argus console:

- **Administrators** of the organization they signed in to are emailed at
  their own address. For the default organization, the addresses under
  `alerts.email.to` and the chats under `alerts.telegram.chat_ids` are told
  too. Another organization's sign-ins never reach those.
- **The person who signed in** is emailed at the address on their account,
  never on Telegram. The email has a "This wasn't me" link. It works once,
  for 24 hours. Opening it shows the sign-in; confirming signs out every
  session on the account, writes an `auth.sign_in_reported` audit entry at
  `warning`, and sends administrators an alert.

Each alert says who, when (UTC), the client address, the browser and
operating system, the second step used (authenticator app, recovery code or
none), and the organization.

Nothing is sent through a channel that is not set up. With neither email
nor Telegram configured, nobody is told. Push notifications (FCM) carry
session alerts only.

## When

By default, only a sign-in from a browser or address the account has not used
in the last 90 days is announced, and so is any sign-in with a recovery code.
`when: every` announces every sign-in.

Only successful sign-ins are announced, so password guessing never sends mail.
Each account gets at most one routine alert every 10 minutes and five alerts
an hour of any kind.

The first sign-in of each account after upgrading counts as a new device,
because Argus has no record of the devices used before.

## Configuration

In the console: **Settings › Alerts › Sign-in alerts**. In the config file:

```yaml
alerts:
  email: { ... }      # as for session alerts
  telegram: { ... }
  sign_in:            # optional; these are the defaults
    admins: true      # tell administrators
    person: true      # email the person who signed in
    when: new_device  # or: every
```
