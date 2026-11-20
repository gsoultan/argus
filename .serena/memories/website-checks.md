# Website checks: is a site up, and how much of the time

`internal/control/websites.go` (store, loop, the confirmation rule, alerts),
`website_routes.go` (HTTP), `internal/tunnel/sitecheck.go` (the one fetch,
`SiteProbe`, shared by control plane and agent), `internal/agent/sitecheck.go`
(the agent's `sitecheck` tunnel service). Web: `routes/websites.tsx`,
`components/Websites.tsx`, `lib/websites.ts`. Migration 099.

## The rule: never call a site down when it is not

A failure must survive four questions before it is "down":

1. **Retries.** Two more tries, 20 s apart, in the same round. Any success is up.
2. **Argus's own network.** For a failure Argus's network could cause (DNS,
   connect, timeout) the canaries are asked: TCP to 1.1.1.1/8.8.8.8/9.9.9.9:443
   and lookups of three resolver names. All silent → round `unknown`: not
   down, not counted in uptime. A canary that never answered here (no internet)
   is ignored, and a private-IP URL skips the TCP canary. The loop refreshes
   the canaries every 5 min while any check runs: a canary first looked at
   during the failure would look like one that never answers.
3. **Chosen servers.** Up to three assets per check; their agents fetch it
   too. Any success → round `partial` (counts as up; the list says "Argus
   cannot reach it; another server can"). A server that cannot be asked is
   named in the detail and does not block the outage.
4. **Two rounds.** Down at `websiteDownAfter` (2) failed rounds in a row. The
   outage starts at the first; its `pending` round becomes `down`. A good round
   turns leftover `pending` rounds into `up` (blips).

Slow is not a failure; only no answer within the check's timeout (30 s
default, 5–60) is. Status is any < 400 after ≤5 redirects unless one is set;
optional keyword in the first MB.

## Facts worth knowing

- Uptime = seconds up (`up`+`partial`) / seconds up or down, from
  `website_rounds` (kept 35 days). Unknown and pending rounds and time with no
  control server running count neither way.
- Alerts: kind `website` (mutable), audience admins, subject `website:<id>`;
  "is down" (critical) is closed by "back up after N minutes". Both go to the
  alert channels via `Alerts.SecurityEvent`, like backups.
- Adding/changing/removing a check is audited (`website.add|change|remove`);
  rounds and outages are not audit events. Read: admin/owner/auditor. Write:
  admin/owner.
- Replicas: `TakeDueWebsiteChecks` leases with FOR UPDATE SKIP LOCKED (5 min);
  a round cut by shutdown releases the lease and records nothing.
- No probe ever connects to link-local/metadata addresses: refused in
  `ValidSiteURL`, at dial time (`Control`), and before handing a literal to a
  proxy. Nothing of the page body leaves the machine that fetched it.
- No fixture: without a control plane the page says so, never shows invented sites.

Not done yet: alerting on certificate expiry (the date is stored and shown),
per-check alert recipients, public status page.

## Telegram routing (per feature, per site)
- Website alerts go to the bell and to Telegram only; they are not emailed and do not go through Dispatcher.SecurityEvent.
- Feature setting: `website_alert_settings` (one row; missing = on, every chat). GET/PUT /api/v1/websites/settings (admin/owner, audited `website.alerts`).
- Per site: `website_checks.telegram` = default (follow the feature) | off | chats (`telegram_chats`).
- `websiteTelegramChats` resolves the chats, always intersected with the chats configured in Settings › Alerts. Only the default organization uses Telegram, since the bot is platform-wide.
- POST /api/v1/websites/{id}/test-alert sends a test now (409 with the reason if none, 502 on a Telegram error).
- Test: TestWebsiteTelegramPerFeatureAndPerSite (fake Telegram server via notify.Telegram.WithAPI).
