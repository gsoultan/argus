# Home page

`/` is Home (web/src/components/Home.tsx, route web/src/routes/index.tsx), the
first menu item and where the console opens. It used to redirect to Servers.

## Who sees what

- **Needs you** — approvers and up (`canSee(role, 'approvers')`). Only cards
  with something in them; one line "Nothing is waiting on you." otherwise.
  - Approvals: `stats.requestsPending` + held direct logins, the same sum as the
    menu badge in Shell.tsx. Detail names the oldest pending request.
  - Servers waiting to be added: `coverage.unreviewedHosts`, admin only.
  - argus-control problems: `/api/v1/platform/status` problems, platform
    managers only and only with a control plane configured. Links to
    Settings › Status (`/settings?section=status`), which stays the full page.
- **Fleet / Now / Latest in the audit log** — `seesWholeFleet` (owner, admin,
  auditor). Tiles from `/api/v1/stats`; Live now from active sessions minus
  silent ones, each with Watch; argus-control summary card for platform
  managers (shares `statusKey` with the Status page so it is one fetch);
  `recentAuditQuery(6)` = `GET /api/v1/audit?limit=6`.
- **Everyone else** — their servers (ServerCard, first 6), their databases
  (Open), their own requests (filtered to their email, since an approver gets
  everyone's), their recent sessions.

## Decisions

- No trend charts: no endpoint serves a series. Tiles use what /stats has.
- Heading greets by time of day and first name; tests match
  `/^Good (morning|afternoon|evening)/`.
- Links use `ButtonLink` / `AnchorLink` (components/links.tsx) so routes and
  params type-check.
