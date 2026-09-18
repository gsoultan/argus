# Web console architecture

`web/` — Vite 8 + React 19 + Mantine 9 + TanStack Router/Query, built with Bun.
No Node required. Dev server on 5290, proxying `/api` and `/auth` to the control
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

## Page composition

Routes own content; `components/page.tsx` owns layout. `PageHeader`, `PageBody`,
`Toolbar`, `SectionCard`, `EmptyState` and `DataTable` are the only shapes a
route should be assembling, and `components/nav.ts` is the single definition of
the sidebar's three sections — read by both `Shell` and `CommandPalette`. See
[design-system](design-system.md) before changing any of them.

## Two typecheck programs

`tsconfig.json` is application code only; `tsconfig.tooling.json` extends it for
`e2e/`, `vite.config.ts`, `vitest.config.ts` and `playwright.config.ts`, and is
the only one with `types: ["node"]` and `checkJs`. `bun run typecheck` runs both.

They are split because TypeScript loads a types package whole: once anything in
a program references `node:http`, `process` and `Buffer` are globals for every
file in it. With e2e and the configs in one program with `src`,
`process.env.HOME` inside a route typechecked clean — code that is undefined in
a browser. Verified both ways with a throwaway probe file.

## Bundle weight

Measured on a cold load of `/`, compressed, service worker blocked, against
`6916bcca`: **228.0 kB → 233.6 kB of JS+CSS (+5.6 kB)** for the azure palette,
the page-layout primitives, the grouped nav, the command palette, the fallback
banner and the router pending component. CSS moved 26.5 → 26.6 kB, so splitting
`app.tokens.css` out cost nothing.

Do not read the chunk table for this. It showed the entry chunk growing 33 kB,
which was `Shell.js` being folded into it — the all-chunk total moved 7.6 kB and
the real cold load moved less again. `performance.getEntriesByType('resource')`
and `encodedBodySize` is the number that matters; `content-length` is absent
from `vite preview` responses, so summing response headers reports ~0.

`CommandPalette` is `lazy()` for this reason: eager it cost 3.4 kB of every
cold load for a panel most sessions never open.

`app.css` is split: the Mantine import list lives there, everything Argus writes
itself lives in `app.tokens.css`, and `app.monolith.css` is a reference variant
built only by `e2e/cascade.spec.ts`. `ARGUS_CSS=monolith` swaps it in through a
`resolve.alias` entry that must stay ahead of the general `~` one. Keep the two
variants differing in exactly one thing — which Mantine stylesheets they pull in
— or the check starts reporting our own divergence as a cascade fault.

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
