# PWA and service worker

Configured in `web/vite.config.ts` under `VitePWA`.

## The bug worth remembering

`navigateFallbackDenylist` originally listed only `/^\/api\//`. Workbox binds a
navigation route to `index.html`, and `loginURL()` navigates to `/auth/login` —
same origin, not denylisted. So the worker served cached `index.html` instead,
and **SSO sign-in looped forever with no error**.

It only broke once the worker had installed, so the first visit always worked
and every visit after it did not. Both `/api/` and `/auth/` are now denylisted
*and* registered `NetworkOnly`.

Rule: anything the control plane serves must be denied the navigation fallback.

## Update strategy

`registerType: 'prompt'`, not `autoUpdate`. This console is used while watching
a live session; swapping the app out mid-stream drops the socket with no
explanation and discards a half-written termination reason.
`components/PWAUpdate.tsx` offers the reload and lets the operator pick when.

## Precaching

Only the shell — `index.html`, the entry chunk, CSS, SVG, and the **Latin** font
subsets. Precaching everything meant a first visit pulled xterm.js, every route
the user never opened, and Cyrillic/Greek/Vietnamese font subsets: 50 entries,
1.4 MB. It is 11 entries and ~707 KB now.

Route chunks are content-hashed, so `StaleWhileRevalidate` is always correct for
them. Fonts are immutable — `CacheFirst`.

Recordings and audit data are **NetworkOnly**, always. They are privileged and
must not sit in a cache on a shared machine.

## Code splitting

`React.lazy` guards the expensive components: `LiveTerminal`, `ShadowTerminal`,
`RDPScreen`, `RDPReplay`, `Replay`. xterm.js is ~83 kB gzipped and no session
needs both it and the desktop canvas — an SSH replay never draws a desktop, an
RDP replay never loads xterm, and the live-watch components are reachable only
from a modal on an active session.

Verify a chunk is genuinely lazy by checking it appears in the built route's
`__vite__mapDeps` array rather than in its static `from"./..."` imports.
