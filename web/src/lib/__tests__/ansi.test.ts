import { describe, expect, it } from 'vitest'
import { extractCommands, stripAnsi, toSpans } from '~/lib/ansi'

const ESC = String.fromCharCode(27)
const BS = String.fromCharCode(8)
const sgr = (code: string, s: string) => `${ESC}[${code}m${s}${ESC}[0m`

describe('stripAnsi', () => {
  it('removes SGR and other CSI sequences but keeps the text', () => {
    expect(stripAnsi(sgr('32', 'ok'))).toBe('ok')
    expect(stripAnsi(`${ESC}[2Jcleared`)).toBe('cleared')
    expect(stripAnsi('plain')).toBe('plain')
  })
})

describe('toSpans', () => {
  it('splits styled runs and drops empty ones', () => {
    const spans = toSpans(`plain${sgr('31', 'red')}tail`)
    expect(spans.map((s) => s.text)).toEqual(['plain', 'red', 'tail'])
    expect(spans[1]!.fg).toBeDefined()
    expect(spans[0]!.fg).toBeUndefined()
  })

  it('treats a bare ESC[m as a reset', () => {
    const spans = toSpans(`${ESC}[1mbold${ESC}[mafter`)
    expect(spans[0]!.bold).toBe(true)
    expect(spans[1]!.bold).toBeUndefined()
  })
})

describe('extractCommands', () => {
  /** stdin is what the user actually typed, so it wins when present. */
  it('prefers stdin, terminating a command on carriage return', () => {
    const frames = [
      { t: 1, kind: 'i' as const, data: 'l' },
      { t: 1.1, kind: 'i' as const, data: 's' },
      { t: 1.2, kind: 'i' as const, data: '\r' },
      { t: 2, kind: 'o' as const, data: '$ not-this' },
    ]
    expect(extractCommands(frames)).toEqual([{ t: 1, cmd: 'ls' }])
  })

  /** Backspace is honoured so an edited command is not reported as typed. */
  it('applies backspace rather than recording the correction', () => {
    const chars = [...'rm -rf /tmpX', BS, '\r']
    const frames = chars.map((c, i) => ({ t: i / 10, kind: 'i' as const, data: c }))
    expect(extractCommands(frames)).toEqual([{ t: 0, cmd: 'rm -rf /tmp' }])
  })

  it('reports a trailing command that was never submitted', () => {
    const frames = [{ t: 3, kind: 'i' as const, data: 'shutdown now' }]
    expect(extractCommands(frames)).toEqual([{ t: 3, cmd: 'shutdown now' }])
  })

  /**
   * Output-only recordings are the common case for other recorders, so the
   * prompt-scraping fallback has to work across frame boundaries — a terminal
   * echoes one character per frame, so no frame holds a whole command line.
   */
  it('falls back to prompt scraping when there is no stdin', () => {
    const line = '$ systemctl restart nginx\r\n'
    const frames = [...line].map((c, i) => ({ t: i / 100, kind: 'o' as const, data: c }))
    expect(extractCommands(frames).map((c) => c.cmd)).toEqual(['systemctl restart nginx'])
  })

  it('strips colour from a scraped prompt line', () => {
    const frames = [
      { t: 0, kind: 'o' as const, data: `${sgr('32', 'ops@db-01')}:~$ whoami\r\n` },
    ]
    expect(extractCommands(frames).map((c) => c.cmd)).toEqual(['whoami'])
  })

  it('returns nothing for a recording with no commands in it', () => {
    expect(extractCommands([{ t: 0, kind: 'o' as const, data: 'just output\r\n' }])).toEqual([])
  })
})
