# Browser checks

CI caches `~/.cache/ms-playwright` keyed on `web/bun.lock`. The browsers are
~400 MB across two engines; their system libraries are apt packages outside that
path, so a cache hit still runs `playwright install-deps`.

`web/e2e/` — Playwright against the production build (`bun run e2e`). It builds
the console with `VITE_CONTROL_URL=` (fixture mode), serves it with
`vite preview`, and runs in Chromium with `--enable-precise-memory-info` so
`performance.memory` is exact rather than quantised.

Three servers, and all three builds are chained into the *first* webServer
command rather than split across entries: 5510 the fixture build, 5511
`authstub.mjs` serving the control-plane build so the sign-in screen exists at
all, 5512 the reference build for the cascade check.

**Playwright starts webServer entries in order, waiting for each url before
launching the next.** `preview-monolith.mjs` depends on that — by the time it
runs, the build it serves is already complete. Two earlier versions tried to
prove freshness from inside that script, by requiring the marker file to be
newer than the process and then by watching it disappear and return; both
deadlock under sequential startup, because the build finished before the script
existed. Freshness is established upstream instead: the entry above deletes
`dist-monolith` before it builds.

## Why it exists

Every "fast, low memory" result for the console was first measured by hand. A
claim like that decays: the next change to the worker boundary or the terminal
buffer undoes it silently, and nothing short of a browser can tell. The suite
asserts the properties directly, on every change, in CI (`console-e2e` job).

## What it asserts

- every route renders with zero console errors and no failed asset
- `Inter Variable` and `JetBrains Mono Variable` are loaded *and* the body is
  rendered with them (the theme named Inter for months while nothing loaded it)
- the audit chain verifies in WASM in well under 200 ms
- no status badge is ever truncated (`BROKERED` vs `BYPASSED`)
- **the data-source badge** (`signin.spec.ts`): that the header does not claim a
  connection before one has answered. Route matters — on `/` the state is
  unreachable because the router's loader awaits `statsQuery` and nothing
  renders until it returns; `/connect` declares no loader, so the shell paints
  while the first call is in flight. And `page.route` cannot see requests a
  service worker re-issues, so the delay it depends on needs
  `test.use({ serviceWorkers: 'block' })` or it silently does nothing.
- **the cascade** (`cascade.spec.ts`): every route rendered by both the shipped
  build and a reference build using Mantine's concatenated stylesheet, with
  `getComputedStyle` compared property by property. Includes the two routes that
  need an id — the path is resolved by clicking the first row of the list, on
  the shipped build, and then used verbatim against both. See
  [design-system](design-system.md)
- **terminal replay**: an 8 MB generated asciicast (~50k+ frames) plays at 8x
  and is scrubbed; main-thread heap must stay under `after-decode × 1.35 + 24 MB`
- **desktop replay**: a ~40 MB generated display stream (300 rects + 6 full
  frames over 300 s) plays and is scrubbed backward — the heavy path for a
  delta stream — under the same ceiling

Measured by hand on a 35 MB / 213k-frame / 75-minute terminal recording: the
main-thread heap held flat at ~104 MB across decode, playback and seven seeks.

## Traps

- Badges render uppercase via CSS: assert with `textContent()`, not
  `innerText()`, and use `/i` on regexes.
- `document.fonts.check()` is not the assertion; call `document.fonts.load()`
  for both families first, then inspect `[...document.fonts]` for `loaded`.
- `bunx playwright test` must run from `web/` — from the repo root it finds no
  config and tries to execute the vitest files.
- The `?cast=<url>` and `?rdp=<url>&w=&h=` query params on a session page load
  a recording straight from a URL. They exist for exactly this: measuring the
  players against a large artefact with no control plane in the loop.
