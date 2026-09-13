# Upgrades, account identity, and the bootstrap token

## Static console tokens are the sharpest edge in this codebase

`user_tokens:` in the control plane config maps a bearer string to an email. It
bypasses the password, the second factor and the role. It was once disabled only
when OIDC was configured -- the wrong test the moment Argus grew its own
accounts, because a password+MFA deployment still honoured it and handed out
`Role: "admin"` regardless of whose address the token named. OIDC has since
been removed entirely, so local accounts are the only thing the gate can ask
about.

Now: the role comes from the named account, and `cmd/argus-control/main.go`
**refuses to start** when `user_tokens` is set alongside any local
account. Refuses rather than ignores -- a deployment that thinks its dev token
works will use it.

The only supported case is bootstrap: no accounts at all. It warns.

**If you add another authentication path, revisit `API.authenticate`.** The
pattern to watch for is a guard that names one mechanism instead of asking the
real question ("is there a real way in?"). `a.oidc != nil` was exactly that
guard, and removing OIDC deleted it rather than fixing it -- the question is now
carried by `StaticTokensDisabled` alone.

## Email identity: store and compare the same string

`users.email` has a case-sensitive UNIQUE constraint. Every lookup in
`accounts.go` matches `lower(email)`. Those disagreed, so `Alice@corp.com` and
`alice@corp.com` were two accounts answering one login, and `SetPassword` /
`BeginMFAEnrolment` (no LIMIT on `WHERE lower(email) = ...`) wrote to both.

`008_email_case.sql` normalises and moves uniqueness onto `lower(email)`. It
**refuses** on pre-existing case-collisions rather than merging, because that
means choosing which role, password and audit history survive.

`normaliseEmail` is the one form addresses are stored in. Use it on any new
write path.

## Migrations must be tested against populated data

Every migration had only ever run on an empty schema -- the one situation no
customer is in. `migrate()` is split into `migrationNames()` +
`applyMigrations()` so a subset can be applied.

`internal/control/migration_test.go` stops at version k, seeds through the
schema as it existed then, applies the rest, and checks rows survived, the
audit chain still verifies **to the same head**, and re-running is a no-op.
Runs for every boundary. A second test asserts a fresh install and an upgraded
database reach byte-identical schemas.

Seed with the *oldest* column set: later migrations add columns with defaults,
so 001-era inserts stay valid at every version. `sessions.id` has no default --
the gateway mints it, so seeds must too.

Needs `ARGUS_TEST_DATABASE_URL` and a role with CREATEDB. It fails rather than
skips if it cannot create a database.

**Test cleanup that uses a closed pool fails silently.** `scratchDB` deferred
`pool.Close()` then reused the pool from `t.Cleanup` with the error discarded;
69 scratch databases accumulated before `CREATE DATABASE` blocked long enough
to time other tests out. Each admin statement now opens its own connection.

## Two gateways

Verified 2026-09-07. Two instances, one control plane, 100 concurrent sessions
each: 200/200, every session id distinct, audit chain intact after concurrent
writes from both. Pins come from the control plane; a terminal ticket redeemed
on one gateway is refused by the other with 401. Config:
`dev/argus2.yaml.example`. See [[capacity-and-drills]].

## The agent makes the strongest claim in the product

eBPF fidelity means "every execve is in this file". `activeSession.recordExec`
used to log-and-drop a failed write while the session kept the claim. It now
degrades to `pty`. `internal/agent` had no coverage of the session path at all,
which is how it survived -- the same bug shape as the gateway's `relay()`.

## ./... includes web/node_modules

Go does not skip `node_modules`, and `flatted` ships Go source. `check.sh` and
CI list packages via `go list ./... | grep -v /node_modules/`.
