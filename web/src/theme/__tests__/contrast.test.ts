/// <reference types="node" />
// Reads the stylesheet from disk (vitest runs from web/): it does not process
// CSS, so a ?raw import of it comes back empty.
import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { cssVariablesResolver, theme } from '~/theme'

/**
 * Every colour of text the console uses, on every surface it can sit on, in
 * both schemes, is WCAG AA: 4.5:1.
 *
 * The bug this exists for: the command timeline drew commands in `slate.2`, a
 * shade chosen when the console was dark. On the light scheme that is 1.2:1
 * against a white card, and nobody could read which commands had run. A fixed
 * shade is right in one scheme and wrong in the other, so text colours resolve
 * per scheme, and this checks what they resolve to.
 */

function surfaces(block: string) {
  const pick = (name: string) => {
    const m = block.match(new RegExp(`--color-${name}:\\s*(#[0-9a-f]{6})`, 'i'))
    if (!m) throw new Error(`--color-${name} not found`)
    return m[1]!
  }
  return { void: pick('void'), surface: pick('surface'), raised: pick('raised') }
}

const css = readFileSync(`${process.cwd()}/src/app.tokens.css`, 'utf8')

const LIGHT = surfaces(css.slice(css.indexOf('@theme'), css.indexOf('}', css.indexOf('@theme'))))
const darkStart = css.indexOf(":root[data-mantine-color-scheme='dark']")
const DARK = surfaces(css.slice(darkStart, css.indexOf('}', darkStart)))

function luminance(hex: string) {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
    .map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
  return 0.2126 * c[0]! + 0.7152 * c[1]! + 0.0722 * c[2]!
}
function contrast(a: string, b: string) {
  const [x, y] = [luminance(a), luminance(b)].sort((p, q) => q - p)
  return (x! + 0.05) / (y! + 0.05)
}

/** Follows var(--mantine-color-<name>-<n>) and the resolver's own variables down to a hex. */
function resolve(value: string, scheme: Record<string, string>): string {
  if (value.startsWith('#')) return value
  const m = value.match(/^var\((--[\w-]+)\)$/)
  if (!m) throw new Error(`cannot resolve ${value}`)
  const v = m[1]!
  const shade = v.match(/^--mantine-color-([a-z]+)-(\d)$/)
  if (shade) return theme.colors![shade[1]!]![Number(shade[2])]!
  if (scheme[v]) return resolve(scheme[v]!, scheme)
  if (v === '--mantine-color-text') return theme.black!
  throw new Error(`unknown variable ${v}`)
}

const vars = cssVariablesResolver(theme as never)
const INKS = [
  '--mantine-color-dimmed',
  '--mantine-color-azure-text',
  '--mantine-color-teal-text',
  '--mantine-color-amber-text',
  '--mantine-color-rose-text',
  '--mantine-color-sky-text',
]

describe.each([
  ['light', vars.light as Record<string, string>, LIGHT],
  ['dark', vars.dark as Record<string, string>, DARK],
] as const)('%s scheme', (_name, scheme, bgs) => {
  for (const ink of [...INKS, '--mantine-color-text']) {
    it(`${ink} is AA on the page, a card and an inset panel`, () => {
      const fg = ink === '--mantine-color-text' && _name === 'light' ? theme.black! : resolve(scheme[ink] ?? `var(${ink})`, scheme)
      for (const [where, bg] of Object.entries(bgs)) {
        expect(contrast(fg, bg), `${ink} (${fg}) on ${where} (${bg})`).toBeGreaterThanOrEqual(4.5)
      }
    })
  }
})
