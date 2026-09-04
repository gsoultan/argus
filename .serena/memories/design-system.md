# Design system

## Palette

Deliberately desaturated so the four semantic colours are the only things that
pull the eye: **teal** = verified, **amber** = pending, **rose** = denied,
**sky** = live. `slate` is the neutral, and `dark` is aliased to it.

**Never use Mantine's stock `red` / `yellow` / `gray` / `green`.** They render in
visibly different hues from the theme ramps, and the terminal and RDP components
once did exactly this — which is why those surfaces looked like a bolted-on
product next to every other badge.

## Type scale

`FS` in `web/src/theme/index.ts`. Named steps below Mantine's `xs`, because a
dense console is mostly secondary text and those were once 36 inline
`size="10px"` literals plus one-off 9, 11, 19, 27, 28 and 42px values.

`micro` (10) labels, timestamps, hints · `digest` (11) hashes · `figure` (27)
stat numerals · `title` (19) page headers · `display` (42) the 404 numeral.

## Spacing

Sub-`xs` spacing is on a **2px grid**: 0, 2, 4, 6, 8, 10, 12. There were 86 raw
pixel values across the routes (`gap={7}`, `gap={9}`, `gap={11}`, `mt={3}`…)
with no scale behind them. Odd values round up to the next even step.

## Shared primitives — use these, do not re-declare them

`web/src/components/primitives.tsx` holds `Field`, `Stat` and `rowNav`. All
three existed twice before and had already drifted: two `Field`s differing by a
pixel of margin, two `Stat` cards with different type scales and one missing its
icon.

`rowNav(onActivate)` is what makes a table row behave like the link it looks
like — click, Enter, Space, `tabIndex`, `role`. Spread it or the row is
unreachable by keyboard.

## Fonts

Inter and JetBrains Mono are **self-hosted** via `@fontsource-variable/*`, not a
CDN: the console must render identically air-gapped, and a third-party font
request from a PAM console leaks which operator is looking at it and when. The
theme previously named `Inter`, which nothing ever loaded, so every surface
silently fell back to the system font.

Family names are `Inter Variable` and `JetBrains Mono Variable`, referenced
through `--font-sans` / `--font-mono` so the xterm surfaces match the document.

## Accessibility floor

Every icon-only control needs an `aria-label` — including Mantine's `Slider`,
whose thumb is a `div` and needs `thumbLabel`. Wide tables need
`Table.ScrollContainer` or they simply overflow on a narrow screen. Assert
labels with `getByRole(role, { name })`, not `getByLabelText`: Mantine renders a
hidden native input alongside its own, and both carry the label.

## Mantine CSS is imported per component, and the order is load-bearing

`web/src/app.css` imports ~47 individual `@mantine/core/styles/*.layer.css`
files rather than the 267 kB monolith, which cut the stylesheet from 39.5 kB to
27 kB gzipped.

**The import order is Mantine's own, not alphabetical.** Several components are
built on others at the same specificity — Card on Paper, NavLink on
UnstyledButton — so the base must come first. Sorted by name, Card lands before
Paper and loses the cascade: it computes `display: block` instead of `flex`.
That is a broken layout which compiles, type-checks, and passes every test that
does not measure rendered geometry. It was caught only by diffing
`getComputedStyle` across both builds in a real browser.

Regenerate the order from the byte offset of each component's first hashed
class inside `@mantine/core/styles.layer.css`. Adding a Mantine component means
adding its stylesheet here; nothing will fail, it will simply render unstyled.

The verification worth repeating for any CSS change of this shape: build both
variants, walk every route in a browser, and compare `getComputedStyle` on a
fixed set of elements property by property. Screenshot diffing is noisier and
pixel-identical output is not the same as identical cascade.
