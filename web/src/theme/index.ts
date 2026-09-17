import { createTheme, type MantineColorsTuple, rem } from '@mantine/core'

/**
 * Argus is an operations console read in low light, often next to a terminal.
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

/** Neutral. Blue-tinted (hue ~217) so it belongs to the same family as azure. */
const slate: MantineColorsTuple = [
  '#eff4fb', '#dee7f3', '#c5d2e6', '#a3b3cc', '#7d8ea9',
  '#5b6b85', '#404f66', '#212c40', '#161f2f', '#0f1724',
]

/**
 * Brand. Deep enough that a filled button carries white text at 5.2:1, and far
 * enough from `sky` in both hue and lightness that a solid azure control is
 * never mistaken for a light "live" badge.
 */
const azure: MantineColorsTuple = [
  '#eaf2ff', '#d5e4ff', '#abc8ff', '#7ba8ff', '#5089fb',
  '#3272f2', '#2563eb', '#1b4ec4', '#163d97', '#102a68',
]

const teal: MantineColorsTuple = [
  '#e6fff8', '#c9fdef', '#94f8de', '#5df0cc', '#33e9bd',
  '#2dd4a7', '#1fb98f', '#159474', '#0a6f57', '#00483a',
]

const amber: MantineColorsTuple = [
  '#fff8e1', '#ffefc7', '#ffdd93', '#ffca5b', '#f8ba30',
  '#f0b429', '#d59a1c', '#a97814', '#7c570b', '#4d3502',
]

const rose: MantineColorsTuple = [
  '#ffe9ec', '#ffd1d7', '#ffa1af', '#ff6d84', '#f4576b',
  '#e33c53', '#c62d43', '#9c2134', '#741726', '#4a0a15',
]

const sky: MantineColorsTuple = [
  '#e3f6ff', '#c3e9ff', '#8ed6ff', '#54c1ff', '#38bdf8',
  '#1aa5e6', '#0b83bb', '#046492', '#00476b', '#002b42',
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
  title: rem(19),
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
  // 6 in both schemes: white on azure.6 is 5.2:1, where the lighter shade this
  // used to pick in dark mode left button labels at 4.0:1.
  primaryShade: { light: 6, dark: 6 },
  colors: { slate, azure, teal, amber, rose, sky, dark: slate },

  // "Inter Variable" is the family @fontsource-variable/inter registers. The
  // theme previously asked for "Inter", which no stylesheet ever defined, so
  // every surface silently fell back to the system font and the designed
  // typography never rendered anywhere.
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
  radius: { xs: rem(3), sm: rem(5), md: rem(7), lg: rem(11), xl: rem(16) },

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
      defaultProps: { radius: 'lg', centered: true, overlayProps: { blur: 3, opacity: 0.6 } },
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
    Button: { defaultProps: { size: 'xs' } },
    TextInput: { defaultProps: { size: 'xs' } },
    PasswordInput: { defaultProps: { size: 'xs' } },
    Textarea: { defaultProps: { size: 'xs' } },
    Select: { defaultProps: { size: 'xs' } },
    MultiSelect: { defaultProps: { size: 'xs' } },
    SegmentedControl: { defaultProps: { size: 'xs' } },
    Checkbox: { defaultProps: { size: 'xs' } },
    Radio: { defaultProps: { size: 'xs' } },
    Switch: { defaultProps: { size: 'sm' } },
    ThemeIcon: { defaultProps: { radius: 'sm', variant: 'light' } },

    /* ── Data display ───────────────────────────────────────────────────── */
    Table: {
      // Row height was set per table, at 6, 7, 8, 10 and "xs" across the eight
      // tables in the console — so moving between two list pages changed the
      // rhythm for no reason anyone had decided on.
      defaultProps: { verticalSpacing: SP.cozy, horizontalSpacing: 'md', highlightOnHover: true },
      styles: {
        th: {
          fontSize: FS.digest,
          textTransform: 'uppercase',
          letterSpacing: '0.05em',
          color: 'var(--mantine-color-slate-4)',
          fontWeight: 600,
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
        label: { overflow: 'visible', textOverflow: 'clip' },
        root: { whiteSpace: 'nowrap' },
      },
    },
    Tooltip: { defaultProps: { withArrow: true, openDelay: 300, radius: 'sm' } },
    Code: { styles: { root: { backgroundColor: 'var(--color-raised)' } } },
    NavLink: { styles: { root: { borderRadius: rem(7) } } },
  },
})
