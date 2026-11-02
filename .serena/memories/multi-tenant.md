# Organizations: several on one control plane

Read this before touching `Store`, `API.scope`, signers, agent credentials, the
tunnel hub, recordings, or anything that runs on its own context in
`cmd/argus-control`. Design and threat table: `plans/multi-tenant.md` in the
project files.

## The one rule

**Isolation is decided by which connection a query runs on, never by a WHERE
clause.** Each organization's tables live in a schema of their own (`t_<id>`,
twelve `[a-z0-9]` minted by Argus, never from input). Its pool opens every
connection with `search_path` set to that schema alone, no `public` fallback,
so a table missing from an organization's schema is an error rather than a
read of the default organization's. `Store.db(ctx)` picks the pool from the
organization on the context; the SQL in `internal/control` does not know
organizations exist.

The install's existing schema **is** the default organization (`id = default`).
`argus_platform.tenants` is the registry. Upgrading moves no row; cookies,
agent credentials and recording keys from before stay valid.

## Where the organization comes from

`API.scope` wraps every mux and puts exactly one `*Tenant` on the request
context before any handler runs:

- agent credential or enrolment token: its prefix, `argus-agent-<id>.<secret>`
  (`secretTenant`). No prefix means default; the hash covers the whole string,
  so re-addressing a secret makes it unknown, not moved.
- cookie, ticket or challenge: the `org` claim, read **unverified** by
  `auth.PeekTenant` and used only to choose which key to verify with.
- `X-Argus-Org` header: sign-in and nothing else; ignored once a cookie exists.
- the QUIC tunnel has no request, so `admitQUIC` uses `scopeSecret`; `postEnroll`
  re-scopes from the token in its body, which `scope` could not see.

Unknown organization → 401 (404 when a person typed the slug). Disabled → 403
everywhere, including every agent report; `TunnelHub.closeTenant` drops its
tunnels at once and `tunnelSeen` would anyway at the next ping.

**`Store.Strict` is on in production.** A query whose context names no
organization gets a closed pool and fails. Tests from before organizations run
non-strict and land on the default. Anything that runs on its own context --
sweeps, tunnel bookkeeping, the CLI -- uses `PlatformContext` or
`ForEachTenant`; `liveTunnel.ctx()` and `liveSession.tenant` carry it for the
hub and the live registry.

## Keys

- `Signer.ForTenant(id)`: HMAC(root, "argus tenant key v1:"+id). The default is
  the root signer itself. Every signed value carries `org` and the verifier
  checks it against its own, so a value from one organization fails in another
  with no handler comparing anything. Redemption state is the root's: burnt
  once is burnt everywhere.
- `tunnel.DeriveGrantKey(root, id)` for session grants; agents pin the key of
  the organization they enrolled into.
- Static `user_tokens` work for the default organization only.

## Maps keyed by anything an organization supplies

`TunnelHub.byHost`/`byCred`, `liveRegistry.m` and the agent credential cache
are keyed by (organization, name) via `tunnelKey`. Two organizations can both
have `db-01`. A test that reads these maps directly has to use `tunnelKey`.

## Recordings

`storage.ForTenant(store, id)`: a non-default organization writes under
`tenants/<id>/` and reads refuse any key outside its prefix; the default refuses
anything under `tenants/`. The check exists because the key a read asks for came
from a session row an **agent** reported. `PresignedURL` checks `Owns` too.

## Audit

One `audit_events` and one chain per organization; the advisory lock is
`hashtext('argus_audit:' || current_schema())`. Creating or disabling an
organization lands in the platform's chain and, for creation, as the first
event of the new chain. `backup run`/`verify` check every chain.

## Who manages organizations

`platformOwner`: an owner signed in to the default organization. Routes under
`/api/v1/platform/organizations` answer 404, not 403, to anyone else so an
organization's own admin learns nothing about a platform above them. There is
deliberately no route that reads across organizations.

## What is still platform-wide

Config file and Settings, first-run setup, backups, alert channels (session
alerts go out for the default organization only). Follow-ups are listed at the
end of the design.
