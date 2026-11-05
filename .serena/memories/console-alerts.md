# Console alerts: the bell in the header

`internal/control/console_alerts.go` (store, rule, fan-out),
`console_alert_routes.go` (HTTP + SSE), `console_alert_sources.go` (what raises
one and its wording). Web: `components/AlertBell.tsx` (eager: bell, count,
stream, pop-ups), `components/AlertPanel.tsx` (lazy: list, per-person
settings), `lib/alerts.ts` (queries, fixture, the shared stream).

- **Not evidence.** A row in `console_alerts` points at an event the audit chain
  already holds; swept after 30 days by the per-organization sweep.
- **Who reads a row is its `audience`**: `deciders` (approver/admin/owner),
  `admins` (seesWholeFleet: admin/owner/auditor), or `person` (recipient).
  Nobody is alerted about their own act (`actor`). The rule exists twice,
  `alertVisibleSQL` and `alertVisible`; `TestAlertVisibilityIsTheSameInSQLAndInMemory`
  holds them together. Change both or neither.
- **The stream is a nudge, not the source of truth.** The console refetches the
  list and the unread count on every event and on reconnect, so a dropped event
  costs a pop-up, never a wrong badge. Cross-replica: one poll per
  organization per 3s while anyone on this replica listens.
- **Streams are bounded** (8 per account, 2000 per replica), re-check the cookie
  every 30s (sign-out, disable, demotion), and are ended by
  `RegisterOnShutdown(api.StopAlertStreams)`, or a settings reload would wait
  out Shutdown's whole deadline.
- **One EventSource per browser**, not per tab: Web Locks picks the tab that
  holds it and a BroadcastChannel shares what it hears. Over HTTP/1.1 a stream
  per tab eats the six connections a browser allows a host.
- **Preferences are per person** (`console_alert_prefs`), opened from the bell
  because Settings is guarded to the fleet roles. Security cannot be muted.
  Desktop notifications are per browser (localStorage + permission).
- Before this, `@mantine/notifications/styles.layer.css` was never imported, so
  every toast in the console rendered below the page, off screen.
- **Alerts that ask someone to act close themselves (2026-11).** An alert
  carries a `subject` (`request:<id>`, `direct-login:<id>`); when that request
  is approved, denied or cancelled by its asker, `closeAlerts` sets `closed`
  ("Approved by …", "Cancelled by …") and `closed_at`. A closed alert is not
  unread, does not chime, and the stream sends a `closed` event with its seqs
  so open consoles drop the pop-up and refetch Approvals. The cross-replica
  poll picks up closings in the same query as new alerts (`seq > last OR
  closed_at > closedAt`). The asker's own "was approved/denied" alert has no
  subject: it is news, not a task.
- **Requests can be cancelled** by the person who asked
  (`POST /api/v1/requests/{id}/cancel`, state `cancelled`, audit
  `request.cancel`). The statement-approval dialog cancels its ask when closed
  while waiting, because nothing else can use a later approval. Deciding a
  settled request answers 409 with who settled it (`requestClosed`).
  `GET /api/v1/requests/{id}` is what an alert's `/approvals?req=` link opens,
  so a stale link shows how it ended rather than nothing.
- Before this, every Deny crashed after committing (the audit text
  dereferenced a denial's nil expiry) and a proxy answered 502. Handlers now
  run under `recoverPanics`, which answers 500 instead of dropping the
  connection.
