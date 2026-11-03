# New accounts, passwords and two-step sign-in

## Adding a person

An administrator adds a person with an email address, a name and a role. No
password is typed: Argus makes an 18-character temporary password and emails
it to the person together with the console address. It works for 72 hours by
default (**Settings › Account security**, or `temporary_password_ttl`).

Without email set up, the console shows the temporary password once so the
administrator can hand it over. It is never stored in clear, logged or put in
the audit trail.

From **Teams › People › Security**, "Send a new temporary password" replaces a
password and signs the person out everywhere. Without email, only an invited
person who has not yet chosen their own password can be given one on screen,
so no administrator can quietly take over a working account. Owners can only
be handled by owners, and nobody can do it to themselves.

## Choosing a password

Signing in with a temporary password opens one screen only: choose a new
password. Every other request is refused until then. The rules, checked by the
control plane and shown ticked off as they are met:

- 12 to 256 characters, no leading or trailing space
- not a common password, even with letters swapped for digits or symbols
- does not contain the person's name or email address
- at least 6 different characters and no runs like `aaaaa` or `12345`
- three kinds of character (lower, upper, digits, symbols), unless it is
  20 characters or longer

The same rules apply whenever anyone changes their password ("Change my
password" in the account menu). An expired temporary password is refused at
sign-in; an administrator sends a new one.

## Two-step sign-in

`require_mfa` now defaults to `all`: every account enrols an authenticator app
straight after choosing its password and cannot do anything else first.
`privileged` limits it to owners and administrators; `off` turns it off.

## Protections

- **Session hijacking.** A session cookie works only in the browser family it
  was issued to. A copied cookie used elsewhere is refused, the session is
  revoked, and administrators are alerted. `allow_browser_change: true`
  turns this off. Cookies stay `HttpOnly`, `Secure` and `SameSite=Lax`, with an origin check on every change.
- **Brute force.** Per-address and per-account limits on password and code
  attempts. Both account limits are in
  **Settings › Account security**.
- **Denial of service.** Password hashing is limited to a few at a time, so a
  flood of sign-in attempts cannot exhaust memory; request size and rate
  limits are unchanged.
- **Remote code execution.** The control plane runs no shell commands from
  requests; the agent runs only fixed commands with validated arguments.
