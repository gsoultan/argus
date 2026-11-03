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
