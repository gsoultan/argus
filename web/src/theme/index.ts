import { createTheme, type CSSVariablesResolver, type MantineColorsTuple, rem } from '@mantine/core'

/**
 * Argus has two schemes, light and dark, and follows the operating system until
 * the person picks one (the sun/moon menu in the header; see ColorSchemeMenu).
 * Light is a pale grey page, white cards, one blue for everything you can
 * press; dark is the same layout on graphite. The terminal and the remote
 * desktop stay dark in both, because they are the server's screen, not the
 * console's.
 *
 * Text colours are never written as a fixed shade (`c="amber.4"`). A shade
 * that reads on one background is unreadable on the other: the command
 * timeline drew commands in `slate.2`, a near-white left over from when the
 * console was dark, and on the light scheme they were 1.2:1 against the card.
 * Use `INK` below, which resolves per scheme and is checked against WCAG AA
 * (4.5:1) on every surface it can sit on.
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
  // 5 is secondary text on the light scheme. It was #667085, which is 4.97:1 on
  // a card but 4.5:1 on the page and 4.4:1 on an inset panel -- under AA in the
  // two places a timestamp most often sits. #5d6679 is 5.2:1 on all three.
  '#5d6679', '#475467', '#344054', '#1d2939', '#101828',
]

/**
 * The dark scheme's neutral, the same graphite as `slate` run the other way.
 * Mantine reads `dark` by index: 0 is body text, 2 secondary text, 4 borders,
 * 6 inputs, 7 the page. Secondary text (#98a2b3) is 6.1:1 on the lightest
 * surface it sits on.
 */
const dark: MantineColorsTuple = [
  '#e4e7ec', '#c3c9d3', '#98a2b3', '#8a94a6', '#343b48',
  '#2a303b', '#1e232c', '#161a21', '#0f1218', '#0a0c10',
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
 * Text in a colour, per scheme. Every value is AA (4.5:1) or better on the
 * page, a card and an inset panel in its own scheme:
 *
 * | ink      | light            | dark             |
 * | :------- | :--------------- | :--------------- |
 * | brand    | azure.6  4.7–5.2 | azure.4  6.2–7.4 |
 * | verified | teal.7   4.9–5.4 | teal.4   8.2–9.8 |
 * | pending  | amber.7  4.9–5.4 | amber.4  8.6–10  |
 * | denied   | rose.7   6.0–6.6 | rose.4   5.7–6.7 |
 * | live     | sky.7    5.3–5.9 | sky.4    7.5–8.9 |
 *
 * The stock shades these replace (teal.6, amber.6, sky.6 in light; anything
 * from 0 to 4 written for the old dark console) ran from 3.2:1 down to 1.2:1.
 * They are Mantine's own `--mantine-color-<name>-text` variables, re-pointed
 * by `cssVariablesResolver` below, so `variant="subtle"` buttons and anchors
 * pick up the same values.
 */
/** localStorage key for the chosen scheme. public/color-scheme.js reads it too. */
export const COLOR_SCHEME_KEY = 'argus-color-scheme'

export const INK = {
  brand: 'var(--mantine-color-azure-text)',
  verified: 'var(--mantine-color-teal-text)',
  pending: 'var(--mantine-color-amber-text)',
  denied: 'var(--mantine-color-rose-text)',
  live: 'var(--mantine-color-sky-text)',
} as const

const SEMANTIC = ['teal', 'amber', 'rose', 'sky'] as const

/** `rgba()` of a palette shade, for the translucent dark-scheme tints. */
const tint = (hex: string, a: number) => {
  const n = parseInt(hex.slice(1), 16)
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${a})`
}

/**
 * The dark scheme's `variant="light"` (every badge, icon tile and avatar).
 * Mantine's default is shade 9 darkened by half -- near-black, so on graphite
 * a badge read as a hole in the card rather than a tag on it. A translucent
 * shade 6 keeps the hue visible, and shade 2 text on it is 7:1 or better.
 */
const darkLight = (name: string, ramp: MantineColorsTuple) => ({
  [`--mantine-color-${name}-light`]: tint(ramp[6], 0.16),
  [`--mantine-color-${name}-light-hover`]: tint(ramp[6], 0.24),
  [`--mantine-color-${name}-light-color`]: `var(--mantine-color-${name}-2)`,
})

/**
 * Scheme-dependent variables Mantine derives from the palette, set to values
 * that were measured rather than inherited. See INK, and `dark` above.
 */
export const cssVariablesResolver: CSSVariablesResolver = () => ({
  variables: {},
  light: {
    '--mantine-color-dimmed': 'var(--mantine-color-slate-5)',
    '--mantine-color-placeholder': 'var(--mantine-color-slate-5)',
    '--mantine-color-default-border': 'var(--mantine-color-slate-3)',
    '--mantine-color-default-hover': 'var(--mantine-color-slate-0)',
    '--mantine-color-disabled': 'var(--mantine-color-slate-1)',
    '--mantine-color-disabled-color': 'var(--mantine-color-slate-5)',
    '--mantine-color-disabled-border': 'var(--mantine-color-slate-2)',
    '--mantine-color-error': 'var(--mantine-color-rose-7)',
    '--mantine-color-anchor': 'var(--mantine-color-azure-6)',
    '--mantine-color-azure-text': 'var(--mantine-color-azure-6)',
    ...Object.fromEntries(SEMANTIC.map((c) => [`--mantine-color-${c}-text`, `var(--mantine-color-${c}-7)`])),
  },
  dark: {
    '--mantine-color-text': 'var(--mantine-color-dark-0)',
    '--mantine-color-dimmed': 'var(--mantine-color-dark-2)',
    '--mantine-color-placeholder': 'var(--mantine-color-dark-3)',
    '--mantine-color-error': 'var(--mantine-color-rose-4)',
    '--mantine-color-anchor': 'var(--mantine-color-azure-4)',
    '--mantine-color-azure-text': 'var(--mantine-color-azure-4)',
    ...Object.fromEntries(SEMANTIC.map((c) => [`--mantine-color-${c}-text`, `var(--mantine-color-${c}-4)`])),
    ...darkLight('azure', azure),
    ...darkLight('teal', teal),
    ...darkLight('amber', amber),
    ...darkLight('rose', rose),
    ...darkLight('sky', sky),
    ...darkLight('slate', slate),
  },
})

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
  /**
   * Dense secondary text: field labels, timestamps, card hints.
   *
   * 11, not 10. At 10px Plex's x-height is under 5px, and this step carries
   * most of the console's secondary text in a colour that is already one step
   * dimmer than body copy; the two together made labels the hardest thing on
   * the screen to read. 218 call sites, one decision.
   */
  micro: rem(11),
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
  colors: { slate, dark, azure, teal, amber, rose, sky },
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
        // A title that names a server can be one long unbroken hostname: let
        // it wrap anywhere rather than push the close button off the dialog.
        title: { fontWeight: 600, fontSize: rem(14), minWidth: 0, overflowWrap: 'anywhere' },
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
    // The eye is a real button: reachable with Tab and announced as a toggle
    // (Mantine sets aria-pressed), so revealing a password is not mouse-only.
    // Mantine's default takes it out of the tab order.
    PasswordInput: {
      defaultProps: {
        size: 'sm',
        visibilityToggleFocusable: true,
        visibilityToggleButtonProps: { 'aria-label': 'Show password' },
      },
    },
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
        // Translucent, so the same value works on a white card and a graphite
        // one: a faint wash of the text colour, and of the brand on hover.
        stripedColor: 'color-mix(in srgb, var(--mantine-color-text) 2.5%, transparent)',
        highlightOnHoverColor: 'color-mix(in srgb, var(--mantine-color-azure-5) 7%, transparent)',
      },
      styles: {
        // Sentence case. Uppercase headers on every table made each list read
        // as shouting, and are slower to scan than the words they spell.
        th: {
          fontSize: FS.meta,
          color: 'var(--mantine-color-dimmed)',
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
