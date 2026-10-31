import { createTheme, type MantineColorsTuple, rem } from '@mantine/core'

/**
 * Argus is light: a pale grey page, white cards, one blue for everything you
 * can press. The terminal and the remote desktop stay dark, because they are
 * the server's screen, not the console's.
 *
 * Two colour systems, and keeping them apart is the whole design:
 *
 * - **azure** is the *brand*. Buttons, active navigation, links, focus rings.
 *   It says "this is a control you can operate", never "this is a finding".
 * - **teal / amber / rose / sky** are *semantic* and nothing else ever uses
 *   them: verified / pending / denied / live. They are the only colours that
 *   are allowed to pull the eye, so a badge always means something.
 *
 * Before, teal was both the primary colour and "verified", so every ordinary
 * button on the screen was the same green as a pinned-host-key badge — the
 * console's loudest signal, spent on a Cancel button. Splitting the brand out
 * into azure is what makes the semantic four legible again.
 *
 * `slate` is the neutral, tinted toward the same blue so surfaces, borders and
 * secondary text sit in one family rather than reading as grey next to a blue
 * accent. `dark` is aliased to it.
 *
 * **Never use Mantine's or Tailwind's stock `red` / `yellow` / `gray` / `green`
 * / `blue`.** They render in visibly different hues from these ramps, and the
 * terminal and RDP surfaces once did exactly this — which is why they looked
 * like a bolted-on product next to every other badge.
 */

/**
 * Neutral. Graphite with only a trace of cool tint.
 *
 * The earlier ramp leaned hard into navy, so every surface, border and line of
 * secondary text was blue and the brand had nothing to stand out against. A
 * quieter neutral is what lets the one brand colour and the four semantic ones
 * read as signals again.
 */
const slate: MantineColorsTuple = [
  '#f6f7f9', '#eef0f3', '#e4e7ec', '#d0d5dd', '#98a2b3',
  '#667085', '#475467', '#344054', '#1d2939', '#101828',
]

/**
 * Brand: an indigo "iris", for the watchman the product is named after.
 *
 * The name stays `azure` so no call site changes. The hue moved off pure blue
 * because the old brand sat right next to `sky` ("live"), so a primary button
 * and a live-session badge read as the same colour. White on shade 6 is 5.0:1.
 */
const azure: MantineColorsTuple = [
  '#eff6ff', '#dbeafe', '#bfdbfe', '#93c5fd', '#60a5fa',
  '#3b82f6', '#2563eb', '#1d4ed8', '#1e40af', '#1e3a8a',
]

const teal: MantineColorsTuple = [
  '#ecfdf3', '#d1fadf', '#a6f4c5', '#6ce9a6', '#32d583',
  '#12b76a', '#039855', '#027a48', '#05603a', '#054f31',
]

const amber: MantineColorsTuple = [
  '#fffaeb', '#fef0c7', '#fedf89', '#fec84b', '#fdb022',
  '#f79009', '#dc6803', '#b54708', '#93370d', '#7a2e0e',
]

const rose: MantineColorsTuple = [
  '#fef3f2', '#fee4e2', '#fecdca', '#fda29b', '#f97066',
  '#f04438', '#d92d20', '#b42318', '#912018', '#7a271a',
]

const sky: MantineColorsTuple = [
  '#f0f9ff', '#e0f2fe', '#b9e6fe', '#7cd4fd', '#36bffa',
  '#0ba5ec', '#0086c9', '#026aa2', '#065986', '#0b4a6f',
]

/**
 * The type scale, including the steps below Mantine's `xs`.
 *
 * Argus is a dense console and a large share of its text is secondary: field
 * labels, timestamps, hints, hash digests. Those were written as inline
 * `size="10px"` in 36 places, alongside one-off 9, 11, 19, 27, 28 and 42px
 * values — a scale that existed only in aggregate and could not be adjusted.
 * Naming the steps makes it one decision instead of thirty-six.
 */
export const FS = {
  /** Dense secondary text: field labels, timestamps, card hints. */
  micro: rem(10),
  /** Hash digests and other glyph-compared strings. */
  digest: rem(11),
  /** Table cells and inline metadata. Mantine's `xs`, named for intent. */
  meta: rem(12),
  /**
   * Running prose: page descriptions, alert bodies, card explanations.
   *
   * These were all set at `xs`/12px — the same size as a table cell — so the
   * sentences that explain what a page *is* were the least readable text on
   * it. One step up is still dense and is markedly easier to read.
   */
  body: rem(13),
  /** Headline figure on a dashboard stat card. */
  figure: rem(27),
  /** Page title in PageHeader — matches headings.h2. */
  title: rem(22),
  /** The 404 numeral, and nothing else. */
  display: rem(42),
} as const

/**
 * Sub-`xs` spacing, on a 2px grid.
 *
 * There were 86 raw pixel values across the routes (`gap={7}`, `gap={9}`,
 * `gap={11}`, `mt={3}`…) with no scale behind them. Odd values round up to the
 * next even step.
 */
export const SP = {
  /** 2px — between a label and the value directly under it. */
  hair: 2,
  /** 4px — inside a badge cluster. */
  tight: 4,
  /** 6px — icon to its label. */
  snug: 6,
  /** 8px — between related lines in a field stack. */
  cozy: 8,
} as const

export const theme = createTheme({
  primaryColor: 'azure',
  // 6 in both schemes: white on azure.6 is 5.0:1, where a lighter shade would
  // leave button labels short of AA.
  primaryShade: { light: 6, dark: 6 },
  colors: { slate, azure, teal, amber, rose, sky },
  black: '#101828',

  // IBM Plex Sans, from @fontsource-variable/ibm-plex-sans. An engineering face
  // with open, distinct letterforms (Il1, 0O) that holds up at the small sizes
  // this console lives at, and sits naturally beside JetBrains Mono, which
  // carries every hostname and digest. The family name must match what the
  // package registers, or every surface silently falls back to the system font.
  fontFamily: 'var(--font-sans)',
  fontFamilyMonospace: 'var(--font-mono)',

  fontSizes: {
    xs: FS.meta,
    sm: rem(14),
    md: rem(15),
    lg: rem(17),
    xl: rem(20),
  },

  headings: {
    fontWeight: '600',
    sizes: {
      h1: { fontSize: rem(24), lineHeight: '1.3' },
      h2: { fontSize: FS.title, lineHeight: '1.35' },
      h3: { fontSize: rem(15), lineHeight: '1.4' },
      h4: { fontSize: rem(13), lineHeight: '1.4' },
    },
  },

  // Named rather than inherited, because the console's own rhythm is tighter
  // than Mantine's default at the small end and looser at the large.
  spacing: {
    xs: rem(8),
    sm: rem(12),
    md: rem(16),
    lg: rem(20),
    xl: rem(28),
  },

  defaultRadius: 'md',
  radius: { xs: rem(4), sm: rem(6), md: rem(10), lg: rem(12), xl: rem(16) },

  components: {
    /* ── Surfaces ───────────────────────────────────────────────────────── */
    Card: {
      defaultProps: { withBorder: true, radius: 'md', padding: 'md' },
      styles: {
        root: {
          backgroundColor: 'var(--color-surface)',
          borderColor: 'var(--color-line)',
        },
      },
    },
    Paper: {
      styles: {
        root: { backgroundColor: 'var(--color-surface)', borderColor: 'var(--color-line)' },
      },
    },
    Modal: {
      defaultProps: { radius: 'lg', centered: true, overlayProps: { blur: 2, opacity: 0.35 } },
      styles: {
        content: { border: '1px solid var(--color-line)' },
        title: { fontWeight: 600, fontSize: rem(14) },
      },
    },
    Alert: {
      defaultProps: { variant: 'light', radius: 'md' },
      styles: { title: { fontSize: rem(13), fontWeight: 600 } },
    },

    /* ── Controls ───────────────────────────────────────────────────────── */
    // `size="xs"` appeared on all but a handful of the console's controls. As a
    // default it is one decision instead of ~120, and a new control is
    // consistent by omission rather than by remembering.
    // The few controls a page is *for* (Connect, Add a server) ask for `sm`
    // explicitly, so the main action is never the same size as a filter.
    Button: { defaultProps: { size: 'sm' } },
    TextInput: { defaultProps: { size: 'sm' } },
    PasswordInput: { defaultProps: { size: 'sm' } },
    Textarea: { defaultProps: { size: 'sm' } },
    Select: { defaultProps: { size: 'sm' } },
    MultiSelect: { defaultProps: { size: 'sm' } },
    SegmentedControl: { defaultProps: { size: 'sm' } },
    Checkbox: { defaultProps: { size: 'sm' } },
    Radio: { defaultProps: { size: 'sm' } },
    Switch: { defaultProps: { size: 'sm' } },
    ThemeIcon: { defaultProps: { radius: 'sm', variant: 'light' } },

    /* ── Data display ───────────────────────────────────────────────────── */
    Table: {
      // Row height was set per table, at 6, 7, 8, 10 and "xs" across the eight
      // tables in the console — so moving between two list pages changed the
      // rhythm for no reason anyone had decided on.
      //
      // The two colours are props, not CSS variables. Mantine sets
      // `--table-striped-color` on the table element itself, so a `:root` rule
      // declaring it is overridden and does nothing — which is what happened
      // here: rows were being painted solid `slate.6`, a mid blue-grey, while a
      // stylesheet claimed they were 1.4% white. Lightening the neutral ramp
      // made that louder, and it was banding every list page.
      defaultProps: {
        verticalSpacing: SP.cozy,
        horizontalSpacing: 'md',
        highlightOnHover: true,
        stripedColor: 'rgba(16, 24, 40, 0.018)',
        highlightOnHoverColor: 'rgba(37, 99, 235, 0.04)',
      },
      styles: {
        // Sentence case. Uppercase headers on every table made each list read
        // as shouting, and are slower to scan than the words they spell.
        th: {
          fontSize: FS.meta,
          color: 'var(--mantine-color-slate-5)',
          fontWeight: 500,
          whiteSpace: 'nowrap',
        },
      },
    },
    Badge: {
      defaultProps: { radius: 'sm', variant: 'light' },
      styles: {
        // Mantine truncates a Badge label when its column is squeezed. Every
        // badge in this console is a status word, and a truncated one is worse
        // than no badge at all: "brokered" and "bypassed" both render as
        // "BROKE…", which is the single distinction the Origin column exists
        // to make. Wide tables already sit in a Table.ScrollContainer, so the
        // cost of not truncating is a horizontal scrollbar — not a lost signal.
        //
        // Sentence case and normal weight, too: Mantine's uppercase bold badge
        // turned a row of statuses into a row of alarms.
        label: {
          overflow: 'visible', textOverflow: 'clip', textTransform: 'none',
          fontWeight: 500, letterSpacing: 0,
        },
        root: { whiteSpace: 'nowrap' },
      },
    },
    Tooltip: { defaultProps: { withArrow: true, openDelay: 300, radius: 'sm' } },
    Code: { styles: { root: { backgroundColor: 'var(--color-raised)' } } },
    NavLink: { styles: { root: { borderRadius: rem(7) } } },
  },
})
