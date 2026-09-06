import type { ITheme } from '@xterm/xterm'

/**
 * One palette for every terminal surface.
 *
 * A live session, a shadowed session and a replay of that same session must not
 * look like three different products — and more practically, an auditor
 * comparing a recording against what they watched should not have to wonder
 * whether a colour difference means anything. It was defined twice and diverged
 * only in the cursor colour; now the cursor is the parameter and the rest is
 * shared.
 */
export function termTheme(cursor: string): ITheme {
  return {
    background: '#05080c',
    foreground: '#e2e8f0',
    cursor,
    selectionBackground: 'rgba(45,212,167,0.25)',
    black: '#0a0e14',
    red: '#f4576b',
    green: '#2dd4a7',
    yellow: '#f0b429',
    blue: '#60a5fa',
    magenta: '#c084fc',
    cyan: '#38bdf8',
    white: '#cbd5e1',
    brightBlack: '#64748b',
    brightRed: '#fb7185',
    brightGreen: '#5eead4',
    brightYellow: '#fcd34d',
    brightBlue: '#93c5fd',
    brightMagenta: '#d8b4fe',
    brightCyan: '#7dd3fc',
    brightWhite: '#f1f5f9',
  }
}

/** Resolved at call time so the terminal uses the same webfont as the console. */
export function termFontFamily(): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue('--font-mono').trim()
  return v || 'ui-monospace, "SF Mono", Menlo, monospace'
}

/** Teal for a session you are driving, amber for one you are only watching. */
export const CURSOR_LIVE = '#2dd4a7'
export const CURSOR_OBSERVING = '#f0b429'
