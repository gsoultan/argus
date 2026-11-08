# Home page

`/` is Home (web/src/components/Home.tsx, route web/src/routes/index.tsx), the
first menu item and where the console opens. It used to redirect to Servers.

## Who sees what

- **Owner, admin, auditor** (`seesWholeFleet`): the live dashboard below.
  Needs you, six tiles, Sessions in the last 24 hours, Live now, Fleet health,
  argus-control (platform managers only, its own 60 s poll of Settings ›
  Status), Happening now (the audit log).
- **Approver**: Needs you from `/stats`, direct logins and pending requests
  (polls), then their own Home.
- **Everyone else**: their servers (ServerCard, first 6), their databases,
  their own requests, their recent sessions. Nothing about the fleet.

## The live dashboard (2026-11)

Server: `internal/control/home_snapshot.go` (what one snapshot holds, read
from the store and each agent's last usage report) and `home_hub.go` (builds
it once per organization, diffs, streams). Web: `lib/homeLive.ts` (store),
`components/home/Live.tsx` (tiles, chart, Live now, Fleet health, feed).

- **Rides the bell's stream.** `ready` names the stream; `POST
  /api/v1/alerts/stream/{id}/home {on}` switches Home on or off on it. Only the
  caller's own stream, in their organization, fleet roles only (404 / 403).
  Switching on again sends the whole dashboard: that is the resync. The tab
  holding the stream switches Home on while any tab shows it (tabs say so
  over the BroadcastChannel). Demoted at the 30 s cookie re-check: `home
  {off:true}` and it stops.
- **Frames**: `{v, full?, set}`; `set` is the top-level fields that changed.
  A version gap makes the tab ask again. A subscriber whose buffer is full is
  sent the whole dashboard next build, not cut off.
- **Cost is per organization**: fast half (servers, live, waiting, audit) every
  5 s, slow half (by-hour counts, sign-ins, usage) every 30 s, only while
  watched; the store's `changed` hook (audit appends, a session starting or
  ending) asks for a build sooner, at most one per 2 s. Unwatched, it samples
  once a minute for an hour to keep the lines, then forgets the organization.
- **Lines** are the last hour, one point a minute, in memory; `trends.since`
  says where they begin. They restart with argus-control. By-hour bars are
  from the store and survive a restart.
- **Truth rules**: usage older than 3 minutes is left out (System card's rule);
  a disk counts like df; Needs you raises a disk only at 95%, failed sign-ins
  at 5 in 15 minutes. Fixture mode says "Sample data" on the pill.
- `GET /api/v1/home` is the whole dashboard for the first paint and the 15 s
  fallback when no stream can be held; while watched it is the hub's last
  build and costs nothing.
- **Budgets**: `TestHomeNetworkBudget` (a busy fleet, an hour: about 120 B/s;
  the whole dashboard under 8 KB; fails over 300 B/s). `BenchmarkHomeSnapshot`
  on 1,000 servers and 10,000 sessions: fast about 1.4 ms, slow about 23 ms.
- Tab hidden 10 s: Home is let go, nothing is sent or read. Durations tick in
  the browser, once a second while visible.

## Decisions

- Heading greets by time of day and first name; tests match
  `/^Good (morning|afternoon|evening)/`.
- Links use `ButtonLink` / `AnchorLink` (components/links.tsx) so routes and
  params type-check.
- The dark command band, fleet map and recording ring were replaced by this
  (design in the project's plans/home-dashboard).
