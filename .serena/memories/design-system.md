# Design system

## Two colour systems, kept apart

This is the rule the rest of the palette hangs off.

- **azure is the brand.** Buttons, active navigation, links, focus rings, the
  logo, player transport, the command palette's selection. It means "a control
  you can operate", never "a finding".
- **teal / amber / rose / sky are semantic** and nothing else may use them:
  verified / pending / denied / live. They are the only colours allowed to pull
  the eye, so a badge always means something.

Until 2026-09-18 `primaryColor` was **teal**, which is also "verified" — so
every ordinary button on the screen was the same green as a pinned-host-key
badge, and the console's loudest signal was being spent on Cancel. Splitting the
brand out into azure is what makes the semantic four legible again. When adding
a colour to anything, first decide which of the two systems it belongs to.

`slate` is the neutral, tinted toward the same blue (hue ~217) so surfaces,
borders and secondary text read as one family rather than as grey beside a blue
accent. `dark` is aliased to it.

**Never use Mantine's or Tailwind's stock `red` / `yellow` / `gray` / `green` /
`blue` / `sky`.** They render in visibly different hues from these ramps. Eight
`className="text-teal-400"`-style usages had accumulated — including a live dot
sitting directly beside a Badge painted from the theme, so the two blues did not
match.

### Contrast floors that were chosen, not inherited

- `primaryShade` is **6 in both schemes**. White on azure.6 is 5.2:1; the
  lighter shade Mantine picks by default in dark mode left button labels at
  4.0:1.
- `--mantine-color-dimmed` is pinned to `slate.4` in `app.css`. Mantine derives
  dimmed from `dark-2`, which in this palette is a near-white used for primary
  text, so "dimmed" was barely dimmer than body copy.

## Surfaces

Four elevations, declared in `app.css`: `--color-void` (page) < `--color-surface`
(cards, header, navbar) < `--color-raised` (inset panels, code, hover) <
`--color-line` (borders). `--color-brand` / `--color-brand-bright` sit alongside
the four semantic vars.

## Type scale

`FS` in `web/src/theme/index.ts`. Named steps below and between Mantine's own,
because a dense console is mostly secondary text and those were once 36 inline
`size="10px"` literals plus one-off 9, 11, 19, 27, 28 and 42px values.

`micro` (10) labels, timestamps, hints · `digest` (11) hashes · `meta` (12)
table cells and inline metadata · `body` (13) running prose · `figure` (27) stat
numerals · `title` (19) page headers · `display` (42) the 404 numeral.

`body` exists because page descriptions, alert bodies and card explanations were
all set at `xs`/12px — the same size as a table cell — so the sentences that
explain what a page *is* were the least readable text on it.

## Spacing

`SP` in the theme: `hair` 2 · `tight` 4 · `snug` 6 · `cozy` 8. Anything larger
uses the theme's own `spacing` scale (xs 8, sm 12, md 16, lg 20, xl 28). There
were 86 raw pixel values across the routes with no scale behind them.

## Defaults live in the theme, not in every call site

`size="xs"` appeared on all but a handful of the console's controls, so it is
now the `defaultProps` for Button, TextInput, Select, Textarea, MultiSelect,
PasswordInput, SegmentedControl, Checkbox and Radio. A new control is consistent
by omission rather than by remembering.

`Table` carries `verticalSpacing: SP.cozy`. Row height had been set per table at
6, 7, 8, 10 and `"xs"` across the eight tables, so moving between two list pages
changed the rhythm for no reason anyone had decided on.

## Page chrome — `web/src/components/page.tsx`

Every route is built from these. Before they existed each route composed its own
header, filter row, card header and empty state inline.

`PageHeader` (sticky; crumbs, title, status, description, actions) ·
`PageBody` · `Toolbar` · `SectionCard` · `EmptyState` · `DataTable` ·
`TableSkeleton`.

- **Navigation goes in `crumbs`, never in `actions`.** The two detail pages had
  a "Back" button as the first of four equally weighted buttons, the last of
  which terminates a live session.
- **The title is the only `h1` and carries no adornment.** `status` renders
  beside it but outside the heading, or it lands in the accessible name and
  changes what `getByRole('heading', { name })` matches — which both the route
  tests and the e2e suite rely on.
- **Filters belong in `Toolbar`, above the data they narrow.** They had lived in
  three places, including inside the page header's action slot on Sessions,
  where a filter sat beside buttons that perform operations.
- **`DataTable` owns the three states a table can be in.** Every table used to
  render an empty `<tbody>` while its query was in flight, which is
  pixel-identical to "this fleet has no hosts".
- **`Stat` renders a skeleton for a count it does not have.** Coverage used
  `cov?.unreviewedHosts ?? 0`, so the screen whose whole job is "does Argus see
  everything?" answered "no gaps anywhere" while the request was in flight — the
  most reassuring possible reading of not knowing. Callers must pass `undefined`
  rather than collapsing it to zero.
- **Neither `/connect` nor `/coverage` has a route loader, deliberately.** A
  loader is awaited before the router's first render, so it blanks the console —
  shell included — until it resolves; measured at ~2.8s against a slow control
  plane. Connect's chunk also carries a terminal emulator. An honest loading
  state beats a blank page, and it is what keeps the header's `Connecting…`
  state reachable at all: every route that *does* block has already proven the
  control plane is there by the time the shell paints.
- **An empty state says what to do next.** "No assets match." alone reads the
  same whether the filter is too narrow, the fleet is empty, or the request
  failed.
- **A dashboard alert states the finding and hands over.** Its actions sit
  beside the prose rather than under it, and the paragraph lives on the page you
  act on. Two full-width explanations stacked over their own badge rows pushed
  the fleet counters off a laptop screen. Measure the result: capping the text
  at a readable width *without* moving the actions made it 20px taller, because
  a narrower measure wraps more.

## Shared primitives — `web/src/components/primitives.tsx`

`Field`, `Stat`, `Target` and `rowNav`. All had been duplicated and had drifted.

- `Stat` uppercases its label **in JavaScript, not with `tt="uppercase"`** —
  the route tests read those strings, and a CSS transform leaves the DOM text in
  sentence case where `getByText('LIVE SESSIONS')` cannot find it.
- `Stat` shows a chevron when it navigates. Coverage and Users render the same
  card with no destination, and a hover border was the only thing separating
  "this drills down" from "this is a number".
- `Target` (`principal@host`) sets `white-space: nowrap`. Hostnames are full of
  hyphens and the browser breaks at every one, so `deploy@edge-02` split across
  two lines where `ops@db-03` did not and no two rows matched in height.
- `rowNav(onActivate)` is what makes a table row behave like the link it looks
  like — click, Enter, Space, `tabIndex`, `role`. Spread it or the row is
  unreachable by keyboard.

## Assets and Coverage stay separate

Asked in 2026-09-18 and decided: **do not merge them.** Assets lists the hosts
Argus manages and each one's trust state. Coverage answers whether Argus sees
everything — including, in the other direction, agents reporting from machines
that are not in the inventory at all. The inventory cannot show those by
construction, and a page that showed only the first gap would look complete
while missing whole machines.

What was actually wrong is that the boundary was invisible and the drill-down
did not exist: Coverage counted hosts with no agent and could not say which
ones, because the inventory had no agent filter to be pointed at. So

- `AssetQuery` gained `agentState` and `bypassPosture`,
- `/assets` filters live in the URL (`?agent=stale`, `?hostKey=changed`), with
  unrecognised values dropped in `validateSearch` so a hand-edited link cannot
  filter the table by something none of the controls can display,
- Coverage's stat cards drill into those filters, and its unmonitored alert
  links to the hosts it counted,
- the Assets header carries a Coverage link stating what the inventory does
  *not* answer.

**"Hosts outside the inventory" is deliberately not a link.** It is the one
thing Assets structurally cannot show, and leaving it inert is what makes the
split legible.

A counter only links to a filter when the two mean the same set. The Overview's
"Unverified hosts" counts `hostKeyState !== 'pinned'` — unpinned *and* changed —
and no single filter value carries that, so it stays unfiltered rather than
landing on a shorter list than the number that was clicked.

## Information architecture — `web/src/components/nav.ts`

Nine destinations, grouped **Operate / Fleet / Governance**. One flat list gave
no hint that Connect and Audit log are used by different people for different
reasons, so every visit meant reading all nine.

`NAV_SECTIONS` is the single source for both the sidebar and the command
palette. **Paths did not change** — this is a reading order, not a routing
change, so every bookmark, deep link and test still resolves.

Cmd-K opens `CommandPalette`, which searches pages, hosts, sessions and people.
It is built on Modal + TextInput rather than `@mantine/spotlight`, to avoid both
a dependency and another entry in the load-bearing stylesheet order below.

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

The focus ring is `--color-brand-bright`, not `--color-live`: a focus ring marks
a control, and the live colour is spoken for by sessions actually in progress.

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

### That verification is now a test

`web/e2e/cascade.spec.ts`. `app.monolith.css` is the same application built with
Mantine's concatenated stylesheet — its own order, by construction — and the
spec walks every route in both builds comparing `getComputedStyle` property by
property. Screenshot diffing is noisier, and pixel-identical output is not the
same as identical cascade.

The probe set is derived from the DOM (every `mantine-*` and `argus-*` class
present) rather than hand-listed, so a component added tomorrow is covered
without anyone remembering. Both failure modes are checked by deliberately
breaking them: moving `Paper.layer.css` after `Card.layer.css` reports
`mantine-Card-root { display: block }`, and deleting `Badge.layer.css` reports
twelve differences on the badge label.

`/sessions/$id` and `/assets/$id` are covered too, and they are not decoration:
they hold the replay player, the field stacks and the command timeline, and
several components render nowhere else. Deleting `Slider.layer.css` — which only
`PlayerControls` uses — fails the session detail while all nine list routes pass.
Their paths cannot be written down in advance, so the spec clicks the first row
of the list on the shipped build and uses the resulting path against both; the
fixture is seeded, so it is the same record every run.

It runs in CI through `bun run e2e`, which is an unfiltered `playwright test` —
a new spec under `e2e/` is picked up without touching the workflow.

It samples until two consecutive reads agree instead of after a fixed delay.
Styles arrive asynchronously — stylesheet, then web fonts, then whatever React
mounts last — and this compares widths and heights. A fixed settle caught a
table scroll container at the browser's default 16px on WebKit and called it a
cascade fault. A genuine difference is stable and still fails.
