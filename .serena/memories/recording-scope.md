# Recording scope

Per-asset console setting (migration `017`): `every_login` (default) or
`argus_only`. Decides only what happens to a **direct** sshd login on an
agent-managed host; a brokered session is recorded either way.

## How it reaches the host

The heartbeat reply carries `recording_scope`; the agent applies it and sends
back what it applied, stored as `assets.agent_recording_scope`. NULL there is
"unconfirmed" (old agent or no check-in yet), never "applied" — the console
card (`web/src/components/RecordingScope.tsx`) says which.

Strict on every doubt: no matching asset, several matching assets that
disagree, an unrecognised value, an undelivered heartbeat on a fresh start —
all mean `every_login`. An undelivered heartbeat keeps the last value heard.

## Who decides

The daemon, not the shim. The shim sends `start`; the daemon answers with a
`Decision` line before the shell runs. `record:false` only for
`Direct && argus_only`. The daemon then reports the login to
`/api/v1/report/login`, which writes a critical `session.direct_unrecorded`
audit entry, and notes it when the console's setting disagrees with the agent.

Version skew: an old daemon never sends a Decision; the shim waits
`decisionWait` (2s) and records. An error or a closed socket is fail-closed.

## Changing it

`PUT /api/v1/assets/{id}/recording-scope`, admin/owner, audited
(`asset.recording_scope_changed`, warning when loosening). Its own route, not a
field on the asset PATCH, so an older console's full-replace edit cannot reset it.

Known gap: the collector socket is 0666, so a local user can fabricate
`start` messages and with them audit entries. Pre-existing for recordings too.
