# argus-secret: the secrets server

A separate binary (`cmd/argus-secret`, package `internal/vault`). It runs under
its own account `argus-secret` and reads its config from
`/etc/argus-secret/secret.yaml`. Data lives in `/var/lib/argus-secret`
(`secrets.db` bbolt, `root.key`, `tls/`). It is managed from argus-control and
works like AWS Secrets Manager for one organization. An organization can run
several of these servers, and each top-level folder lives on exactly one.
Design doc: Claude Doc "argus-secret: design proposal".

**The split is the point.** argus-control holds the rules and the audit log,
but never a value or a key. argus-secret is the only place a value opens. If
control is stolen, the thief gets rules and audit entries, not secrets.

- **Keys.** The root key file wraps one key per organization, and that key
  wraps a data key for each version. AAD binds every ciphertext to its row. A
  check value catches a wrong root key.
- **Data plane `/v1`.** Agents (`Authorization: Agent argus-agent-…`) and app
  tokens (`Bearer argus-app-…`) call it directly. argus-secret decides from
  the last rules bundle. Every value is sealed with `internal/secretwire`
  (X25519 + HKDF + AES-GCM, AAD `ReadAAD(name,version,field)`) to the
  caller's one-time key, so Cloudflare Tunnel and other proxies see only
  ciphertext. A refused read answers 404, the same as a missing secret.
- **Management plane `/manage/v1`.** Only argus-control calls it. Each request
  carries `Argus-Control-Signature: t=..,n=..,s=..`, an Ed25519 signature
  over method, URI, time, a random nonce, the `Argus-Actor` header and
  sha256(body). The window is ±2 minutes, with a replay cache. The nonce
  exists because Ed25519 is deterministic: without it, two identical calls
  in one second (add a server, then sync it) signed the same and the second
  was refused as a replay.
- **Rules.** A bundle (`vault.Bundle`) holds folders, servers with credential
  sha256 hashes and groups, apps with token hashes, and rules. Serial only
  goes up. Reads are refused once argus-control has not been heard from for
  `rules_max_age` (default 15m), and any signed management call counts as
  contact.
- **Read log.** Hash-chained (`hashchain.Next`) and written in the same
  transaction as the read. argus-control pulls it (`GET /manage/v1/log`) and
  acks it, and argus-secret prunes acked entries older than 30 days while
  keeping a base anchor. `VerifyLog` checks the chain.
- **Behind a proxy.** `behind_tls_proxy: true` serves plain HTTP on loopback
  only. `trusted_proxies` decides when CF-Connecting-IP and X-Forwarded-For
  are believed.
- **Layering.** `cmd/argus-secret/layering_test.go` allows only vault,
  secretwire, hashchain, secrets and tlsconfig, and CI runs it by name.
  `secretwire` is standard library only, so the agent can link it later.

## In argus-control (`secrets_servers.go`, `secrets_routes.go`, migration 119)

- **Management key.** `secretsManageKey(ctx)` = Ed25519 from
  sha256("argus-secret manage key v1" + the organization's grant key seed).
  It is derived, never stored, and the same on every replica.
  `GET /api/v1/secrets/servers` shows its public half and the organization id,
  which go into the server's `control_public_key` and `organization`.
- **`secrets_servers`.** URL (https only), TLS pin (empty means system roots,
  for a proxy's certificate), folders (one server per top-level folder,
  checked under a table lock), plus seal key, rules serial and hash, and log
  seq, head and gap. A server is added only after it answers a signed status
  call for this organization. **`secret_rules`** holds path, subjects, can and
  needs_approval.
- **Sync** (`RunSecretsSync`, every minute, plus `syncSoon` after a change,
  with a per-server mutex) does three things. It reads status, which also
  counts as contact. It pushes the bundle when its digest or the server's
  serial changed, with serial max(ours, theirs)+1. It collects the log: each
  entry must be seq+1, hash to its link and carry Prev == our head. Entries
  go into `audit_events` as `secret.<action>` (a refused read is a warning),
  then `log_seq` moves forward with a compare-and-set and argus-control acks.
  A mismatch sets `log_gap` and writes one `secrets.log_gap` critical event
  and alert, and collection stops until `POST
  .../servers/{id}/log/resume {reason}`.
- **Bundle.** Servers are live `agent_credentials` (secret_hash), with groups
  from the asset's group_name and tags. The id is the asset id, or the
  credential id when no asset matches. A revoked credential drops out at the
  next sync. Apps are empty until PR4.
- **Console relay.** `/api/v1/secrets*` signs for the person (teams as team
  ids, role) and passes the body through unchanged. Reads stay sealed to the
  browser's key, and writes are sealed by the browser to `seal-key`.
  argus-secret decides. Approved is never set yet, so a needsApproval rule
  refuses.
- Test: `TestSecretsServersEndToEnd`, against a real in-process argus-secret.

Phases: PR1 is the binary (done). PR2 is control (done, same PR). PR3 is the console micro-frontend at `/mf/secrets/<v>/`. PR4
is the agent's `secret get/run/file` and the socket, plus app tokens.
