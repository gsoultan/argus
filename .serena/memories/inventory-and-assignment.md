# The inventory, and who is assigned to it

Read this before touching assets, the Connect page, `Dial`, or anything that
decides whether a session opens.

## What changed, and why it had to

An asset existed because someone hand-edited `inventory.json` on a gateway, and
**every signed-in account could open a session on every host in it** by naming
one. The principal list said which accounts Argus could broker on a host; it
never said *for whom*. On a product whose claim is knowing who reached what,
that was the missing half.

Now: an administrator adds a host in the console and assigns people to it. An
operator sees the hosts assigned to them, with the principals they may actually
assume, and nothing else.

## The shape

- **`assets` gained `source`, `credential_ref`, `domain`, `archived_at`**
  (migration 015). `source` is `console` or `gateway`: a gateway publishing its
  file inventory is refused over a console-owned row (`ErrConsoleOwned`), said
  out loud at both ends rather than swallowed — "my edits keep reverting" and
  "my inventory file is ignored" are one event seen from two sides.
- **`asset_assignments (asset_id, user_email, principals[])`**. Explicit, not
  computed from groups: "who can reach pay-01 as ops" is one query with one
  answer.
- **Retire, never delete.** `archived_at`. Sessions and audit entries name the
  host; a deleted row makes its history unreadable, which is what someone
  covering their tracks would reach for.

## The rules that must not drift

- **The asset's principal list bounds every assignment.** The intersection is
  computed at read time, not stored, so removing a principal from a host removes
  it from everyone assigned it without revisiting a single assignment.
  `AssetsForUser` narrows what the console shows the same way the gateway
  enforces it.
- **An approved access request is the other way in.** Assignment is standing
  access; a grant is one host, one principal, and it expires. A product whose
  answer to "I need this host for an hour" is "ask to be assigned it
  permanently" has no just-in-time access at all. Both `handleTicket` and
  `authorizePrincipal` honour either.
- **Elevated principals are decided by the grant, not the assignment.** The
  assignment gate applies to ordinary principals only — a grant is strictly
  narrower, so checking assignment first would just move the refusal, and with
  it the audit entry, away from the real reason.
- **Admins and owners are exempt**, as they already were from the approval
  chain. The gateway is told which accounts those are (`unrestricted` in the
  synced inventory) so `ssh(1)` and the browser cannot disagree about it.

## The gateway

`SyncInventory` mirrors `SyncPolicy`: one minute, and **a failed fetch keeps the
inventory in force**. An empty one would read as "every asset was deleted".

- **The credential is a name, never a path.** The control plane sends
  `credential_ref`; `vaultPath` resolves it inside `vault_dir` and refuses a
  separator or a `..`. Without that check, "add an asset" would be "make the
  gateway read any file it can as a private key". Resolution is by protocol:
  SSH → `KeyPath`, RDP → `CredentialDir`.
- **`inventory-cache.json` exists so a restart during an outage is a delay, not
  a lockout.** Without it a gateway that came back while the control plane was
  down would know of no assignments and have to refuse everything. 0600; holds
  no credential material but is still a map of the fleet.
- **`UnderControlPlane()` is set when the gateway is *configured* to sync, not
  when a sync first succeeds.** Otherwise an outage at start-up would silently
  downgrade it to "no assignments known, so nobody is restricted".
- **`authorize` (was `authorizeElevated`) is one decision.** Assigned and
  ordinary → open with no round trip. Anything else → ask the control plane,
  which also knows about grants and is fresher. Unanswerable is a refusal.

## Where it is not enforced, deliberately

- **A gateway with no control plane** has no assignments to check; the principal
  list is the whole control, as it always was on that path. `cmd/argus-gateway`
  says so at start-up — a reduction in what is enforced is never silent.
- **The native RDP listener authenticates nobody**, so there is no identity to
  hold an assignment. See [[core]] on `rdp.Request`. Elevated principals are
  refused there outright; the browser console is the path that authenticates
  before it brokers.

## What each role is told

`seesWholeFleet` (admin, owner, auditor) is the one rule, applied in four
places: `/api/v1/assets`, `/api/v1/stats`, `/api/v1/sessions` and the session
artefacts. Everyone else gets their assigned hosts and their own sessions.

- **Scoped, not gated.** The Overview still renders for an operator; every
  figure on it is now about something they can act on, and it agrees with the
  lists below it. The page copy changes with the role, because "fleet posture"
  over two hosts is the wrong frame around the right numbers.
- **`getSession`, `getRecording`, `getRDPReplay` and `presignRecording` check
  ownership** (`mayReadSession`). A filtered list with an unfiltered detail
  endpoint is not a control -- the id is in every recording link, and the
  recording is the most sensitive artefact here.
- **Someone else's session is 404, never 403.** A 403 confirms the id names a
  real session and whose it is, which is most of what the recording would have
  said.
- **`Stats` excludes archived assets** on both paths. A retired host that still
  counted would be an Overview reporting hosts no gateway will broker.
