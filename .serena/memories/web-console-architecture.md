# Web console architecture

`web/` — Vite 8 + React 19 + Mantine 9 + TanStack Router/Query, built with Bun.
No Node required. Dev server on 5273, proxying `/api` and `/auth` to the control
plane over **HTTPS** (it serves TLS whenever a cert is configured; proxying to
http:// fails every request and the console then misreports it as "no identity
provider configured").

## Data layer, in three parts

- `lib/live.ts` — the real control plane. `isConfigured()` is whether a URL is
  set; `isLive()` is whether it has ever answered.
- `lib/api.ts` — in-memory fixture with the same signatures, so the console is
  fully usable before the backend exists. `live.orFallback()` picks between them.
- `lib/queries.ts` — every TanStack query/mutation. Mutations invalidate by key
  prefix, so a new query under an existing prefix is refreshed for free.

Fixture state lives in module scope (`db` in `api.ts`), so it survives
client-side navigation and resets on a full page reload. Worth knowing when
testing persistence — a hard reload is not a valid persistence check.

## Where the workers are

Four, all in `web/src/workers/`. Two of them own state rather than shipping it
to the main thread: `replay.worker.ts` keeps decoded frames, `rdpreplay.worker.ts`
keeps the recording buffer. See [replay-player-design](replay-player-design.md).

`chain.worker.ts` verifies the audit hash chain with **hash-wasm**, not
WebCrypto. The chain is inherently serial, so `crypto.subtle.digest` cost one
promise and one allocation per link for ~100-byte inputs — scheduling dominated
hashing. 480 links verify in ~2 ms. `canonical()` must stay byte-identical to
the Go control plane; `lib/__tests__/chain.test.ts` asserts the WASM digest
still matches WebCrypto, which is the test that stops a silent "everything is
tampered" regression.

Nothing else in this console is a good WASM candidate. Recording decode is
dominated by `JSON.parse`, which is native and which WASM would slow down.

## Testing

Vitest + jsdom, `bun run test`. `src/test/setup.ts` polyfills `matchMedia` and
`ResizeObserver`, which jsdom lacks and Mantine needs on mount.
`src/test/render.tsx` wraps in Mantine + QueryClient but **not** the router —
TanStack Router resolves its first match asynchronously, so a synchronous render
against it yields an empty document and a confusing failure. Components that
need a router are exercised through their routes.

## Testing against a real Postgres

`internal/control` store tests skip unless `ARGUS_TEST_DATABASE_URL` is set:

    ./scripts/deps.sh up
    ARGUS_TEST_DATABASE_URL="postgres://argus:argus@localhost:5433/argus?sslmode=disable" go test ./...

They share one database rather than getting a fresh one, so a test that mutates
global state (the single-row `gateway_policy`, for instance) must restore it in
`t.Cleanup` or it loosens the fixture for whatever runs next.

## SSH test scaffolding

Use TCP loopback, never `net.Pipe`, for anything that completes an SSH
handshake. `net.Pipe` is unbuffered and synchronous; both ends write their
version banner before reading either, so both block in `Write` and the
handshake deadlocks before the test begins. `sshChannelPair` in
`internal/gateway/forward_test.go` is the working pattern.
