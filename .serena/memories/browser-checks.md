# Browser checks

`web/e2e/` — Playwright against the production build (`bun run e2e`). It builds
the console with `VITE_CONTROL_URL=` (fixture mode), serves it with
`vite preview`, and runs in Chromium with `--enable-precise-memory-info` so
`performance.memory` is exact rather than quantised.

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
