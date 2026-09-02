import { createTheme, type MantineColorsTuple, rem } from '@mantine/core'

/**
 * Argus is an operations console read in low light, often next to a terminal.
 * The palette is deliberately desaturated so the four *semantic* colours —
 * verified / pending / denied / live — are the only things that pull the eye.
 */

const slate: MantineColorsTuple = [
  '#f1f5f9', '#e2e8f0', '#cbd5e1', '#94a3b8', '#64748b',
  '#475569', '#334155', '#212b38', '#161d27', '#10151d',
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

export const theme = createTheme({
  primaryColor: 'teal',
  primaryShade: { light: 6, dark: 5 },
  colors: { slate, teal, amber, rose, sky, dark: slate },

  fontFamily:
    'Inter, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
  fontFamilyMonospace: '"JetBrains Mono", ui-monospace, "SF Mono", Menlo, monospace',

  headings: {
    fontWeight: '600',
    sizes: {
      h1: { fontSize: rem(24), lineHeight: '1.3' },
      h2: { fontSize: rem(19), lineHeight: '1.35' },
      h3: { fontSize: rem(15), lineHeight: '1.4' },
    },
  },

  defaultRadius: 'md',
  radius: { xs: rem(3), sm: rem(5), md: rem(7), lg: rem(11), xl: rem(16) },

  components: {
    Card: {
      defaultProps: { withBorder: true, radius: 'md', padding: 'lg' },
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
    },
    Table: {
      styles: {
        th: {
          fontSize: rem(11),
          textTransform: 'uppercase',
          letterSpacing: '0.05em',
          color: 'var(--mantine-color-slate-4)',
          fontWeight: 600,
        },
      },
    },
    Badge: { defaultProps: { radius: 'sm', variant: 'light' } },
    Tooltip: { defaultProps: { withArrow: true, openDelay: 300, radius: 'sm' } },
    Code: { styles: { root: { backgroundColor: 'var(--color-raised)' } } },
  },
})
