# Approving your own request (2026-11)

Every approval means a second person agreed; whoever asked never decides
their own. The exception (`internal/control/self_approval.go`): the person
who asked may approve their own request when **nobody else holds a role that
decides that kind**, typing `approve my own request`. Recorded as
`approval.self_approved` at warning severity, beside the usual approved row.

- Kinds: *requests* (access requests, db changes and restores, cloud
  changes, schedules, cloud access) are decided by approver/admin/owner, plus
  a database's other editors where editors approve; *runs* (package and
  software runs, disk grows, log cleanups) by admin/owner only, so an
  approver account does not stop the only admin approving a run.
- Counted per decision from enabled `users` rows in the organization's own
  schema (`Store.SoleDecider`). A second account that could decide brings the
  normal rule back at once, for requests already waiting too.
- Denying your own is still refused: you cancel it instead.
- The stores take `own bool` (`DecideRequest`, `Decide{Package,Disk,Log}Run`)
  and refuse with `ErrOwnRequest` / `ErrSelfApproval`; handlers check
  `approveOwn`, then retry with `own` true. Decisions without a body accept
  `{"confirmSelf": "..."}`.
- Console: `GET /api/v1/approvals/self` → `useDecidesAlone(kind)`;
  `ApproveOwn` is the button and typed dialog, `AloneOr` swaps the
  "another admin approves it" sentence where people ask.
- Script review keeps its older version of the same exception
  (`review my own script`, admins counted).
