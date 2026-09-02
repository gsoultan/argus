# Argus

Privileged access management for Linux infrastructure. Brokered SSH with session
recording, credential injection, just-in-time approvals, and a tamper-evident
audit trail.

## Running locally

```bash
./scripts/setup.sh    # once
./scripts/dev.sh      # every day
```

Everything is a plain shell script under `scripts/` — no build tool, no task
runner, nothing to install. `dev.sh` checks tool versions, resolves stale port
holders, and streams every service into one prefixed log. Ctrl-C stops
everything, including grandchildren.

A `Makefile` exists as a thin wrapper (`make dev`, `make check`, …) if you
prefer it, but every target is a one-line delegation to the script — the
scripts are the interface.

The web UI runs against an in-memory mock API, so it is fully usable before any
backend exists — every function in `web/src/lib/api.ts` maps 1:1 to an endpoint
the Go control plane will serve.

| | |
|---|---|
| Web UI | http://localhost:5273 |
| Control plane API | http://localhost:8080 *(once built)* |
| SSH gateway | `localhost:2222` *(once built)* |

### The scripts

| Script | Does |
|---|---|
| `scripts/setup.sh` | First-time setup — verify toolchain, install dependencies |
| `scripts/dev.sh` | Run every service present locally |
| `scripts/deps.sh` | Postgres + MinIO lifecycle |
| `scripts/check.sh` | Typecheck, vet, test — what CI runs |
| `scripts/build.sh` | Production build |
| `scripts/lib.sh` | Shared helpers, sourced by the rest |

Each takes `--help`. Common flags:

```bash
./scripts/dev.sh web           # only named services
./scripts/dev.sh --force       # kill whatever holds a needed port
./scripts/dev.sh --clean       # reinstall web deps first
./scripts/check.sh web         # or: go
./scripts/build.sh web         # or: go
./scripts/deps.sh up|down|reset|status|logs
```

### Requirements

- **Bun** ≥ 1.4 — the only hard requirement for the web app
- **Node** — *not needed.* Bun runs Vite, `tsc` and the dev server; the whole
  toolchain is verified to work with no node on PATH. If you happen to have one
  installed, the scripts note its version but never depend on it.
- **Go** ≥ 1.25 — only once `cmd/` exists
- **Apple `container`** (or Docker) — only once the control plane exists, for
  Postgres and MinIO. `make deps-up` starts them.
  Install with `brew install --cask container`; start with `container system start`.

### Local dependencies

`scripts/deps.sh` owns Postgres and MinIO. There is deliberately **no compose
file**: Apple's `container` has no `compose` subcommand, and maintaining a
second definition of the same infrastructure only invites drift.

```bash
./scripts/deps.sh up       # create, start, wait until ready, ensure buckets
./scripts/deps.sh down     # stop, keep data
./scripts/deps.sh reset    # stop and delete all data
./scripts/deps.sh status
./scripts/deps.sh logs postgres
```

Two Apple-container specifics the script handles, both of which fail confusingly
if you hit them by hand:

- **`PGDATA` must be a subdirectory of the volume mount.** Apple container
  volumes are formatted filesystems containing `lost+found`, and `initdb`
  refuses to initialise into a non-empty directory.
- **There is no DNS for user-defined networks.** Containers cannot resolve each
  other by name, so the script addresses MinIO by IP. (`container system dns
  create` would fix it, but requires sudo.)

### Adding a service

`scripts/dev.sh` detects services rather than assuming them. Add one line to the
`SERVICES` registry:

```
name | port | colour | working dir | detect path | command
```

The `detect path` is what must exist for the service to be considered present,
so a half-built backend never breaks `make dev` for someone working on the UI.

## Layout

```
web/                  React 19 · Mantine 9 · TanStack Router/Query/Form · Vite 8
  src/types/domain.ts The wire contract the Go control plane implements
  src/lib/api.ts      Mock API — swap for fetch against argus-control
  src/workers/        Audit hash-chain verification, asciicast decoding
scripts/              setup · dev · deps · check · build (plain shell, no task runner)
Makefile              Optional thin wrapper over scripts/
```

## Security posture

Argus terminates SSH, which means the gateway holds session plaintext in memory.
That is the deliberate cost of working against hosts with no agent installed.
Treat gateway nodes as the highest-value asset in the fleet: no shared tenancy,
hardware-backed keys for the vault, and the audit log replicated somewhere the
gateway cannot write.

PTY-derived command detection is an audit aid, not a security boundary — a user
can obscure intent with base64 or a script whose body never reaches the screen.
The eBPF agent tier provides kernel-observed `execve` evidence. The UI labels
which one produced any given timeline and never presents them as equivalent.
