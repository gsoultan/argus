/**
 * Minimal SGR (Select Graphic Rendition) renderer for session replay.
 *
 * This handles the subset of ANSI that shell output actually uses: 8/16 colour
 * foreground and background, bold, dim, italic, underline, inverse, and reset.
 * Cursor movement and full screen addressing are deliberately NOT handled —
 * this is a linear scrollback view, not a terminal emulator. Sessions that use
 * full-screen TUIs (vim, htop, less) will render their raw redraw stream, which
 * is honest: it shows the auditor exactly what bytes crossed the wire.
 *
 * A full emulator (xterm.js headless + serialize addon) is the upgrade path
 * when TUI-heavy replay matters.
 */

export interface Span {
  text: string
  fg?: string
  bg?: string
  bold?: boolean
  dim?: boolean
  italic?: boolean
  underline?: boolean
}

const FG: Record<number, string> = {
  30: '#4b5563', 31: '#f4576b', 32: '#2dd4a7', 33: '#f0b429',
  34: '#60a5fa', 35: '#c084fc', 36: '#38bdf8', 37: '#cbd5e1',
  90: '#64748b', 91: '#fb7185', 92: '#5eead4', 93: '#fcd34d',
  94: '#93c5fd', 95: '#d8b4fe', 96: '#7dd3fc', 97: '#f1f5f9',
}

const BG: Record<number, string> = {
  40: '#111827', 41: '#7f1d2b', 42: '#065f46', 43: '#78350f',
  44: '#1e3a8a', 45: '#4c1d95', 46: '#075985', 47: '#334155',
}

const ESC = String.fromCharCode(27)
// SGR sequences only: ESC [ <params> m
const SGR = new RegExp(`${ESC}\\[([0-9;]*)m`, 'g')
// Everything else we strip rather than render as garbage.
const OTHER_CSI = new RegExp(`${ESC}\\[[0-9;?]*[A-Za-z]|${ESC}\\][^\\u0007]*\\u0007|${ESC}[()][A-Za-z0-9]`, 'g')

type Style = Omit<Span, 'text'>

function applyCodes(style: Style, codes: number[]): Style {
  let next: Style = { ...style }
  for (const c of codes) {
    if (c === 0) next = {}
    else if (c === 1) next.bold = true
    else if (c === 2) next.dim = true
    else if (c === 3) next.italic = true
    else if (c === 4) next.underline = true
    else if (c === 22) { delete next.bold; delete next.dim }
    else if (c === 23) delete next.italic
    else if (c === 24) delete next.underline
    else if (c === 39) delete next.fg
    else if (c === 49) delete next.bg
    else if (FG[c]) next.fg = FG[c]
    else if (BG[c]) next.bg = BG[c]
  }
  return next
}

/** Splits ANSI-bearing text into styled spans, preserving newlines. */
export function toSpans(input: string): Span[] {
  const spans: Span[] = []
  let style: Style = {}
  let cursor = 0

  SGR.lastIndex = 0
  let match: RegExpExecArray | null
  while ((match = SGR.exec(input)) !== null) {
    if (match.index > cursor) {
      spans.push({ ...style, text: input.slice(cursor, match.index) })
    }
    const raw = match[1] ?? ''
    const codes = raw === '' ? [0] : raw.split(';').map((n) => Number(n) || 0)
    style = applyCodes(style, codes)
    cursor = match.index + match[0].length
  }
  if (cursor < input.length) spans.push({ ...style, text: input.slice(cursor) })

  return spans
    .map((s) => ({ ...s, text: s.text.replace(OTHER_CSI, '') }))
    .filter((s) => s.text.length > 0)
}

/** Strips all escape sequences — used for search and command extraction. */
export function stripAnsi(input: string): string {
  return input.replace(SGR, '').replace(OTHER_CSI, '')
}

const DEL = String.fromCharCode(127)
const BS = String.fromCharCode(8)

export interface TimedCommand {
  t: number
  cmd: string
}

/**
 * Recovers the command timeline from decoded asciicast frames.
 *
 * Two sources, in order of trustworthiness:
 *
 * 1. **stdin frames** (`"i"`). These are literally what the user typed, so we
 *    accumulate keystrokes until a carriage return terminates the line. Most
 *    accurate, but many recorders — Teleport's node recording among them —
 *    capture PTY *output* only and emit no stdin at all.
 *
 * 2. **Prompt scraping** on accumulated stdout. Fallback for output-only
 *    recordings. Frames must be joined before matching: a terminal echoes one
 *    character per frame, so no single frame ever holds a whole command line.
 *
 * Both are heuristics on a PTY stream and neither is a security boundary — the
 * caveat Teleport and Warpgate both document. Kernel-observed execve events
 * from the eBPF agent are the real evidence; this is the fallback for hosts
 * without it.
 */
export function extractCommands(
  frames: readonly { t: number; kind: 'o' | 'i'; data: string }[],
): TimedCommand[] {
  const typed = frames.filter((f) => f.kind === 'i')

  if (typed.length > 0) {
    const out: TimedCommand[] = []
    let buf = ''
    let startedAt = 0
    for (const f of typed) {
      for (const ch of f.data) {
        if (ch === '\r' || ch === '\n') {
          const cmd = stripAnsi(buf).trim()
          if (cmd) out.push({ t: startedAt, cmd })
          buf = ''
        } else if (ch === DEL || ch === BS) {
          buf = buf.slice(0, -1) // honour backspace so edits don't leak
        } else {
          if (buf === '') startedAt = f.t
          buf += ch
        }
      }
    }
    const tail = stripAnsi(buf).trim()
    if (tail) out.push({ t: startedAt, cmd: tail })
    return out
  }

  // Output-only fallback: join stdout, then scrape lines following a prompt.
  const out: TimedCommand[] = []
  let acc = ''
  let lineStart = 0
  for (const f of frames) {
    if (f.kind !== 'o') continue
    if (acc === '') lineStart = f.t
    acc += f.data
    const parts = acc.split(/\r?\n/)
    acc = parts.pop() ?? ''
    for (const line of parts) {
      const m = /[$#]\s+(\S.*)$/.exec(stripAnsi(line))
      if (m?.[1]) out.push({ t: lineStart, cmd: m[1].trim() })
    }
    lineStart = f.t
  }
  const m = /[$#]\s+(\S.*)$/.exec(stripAnsi(acc))
  if (m?.[1]) out.push({ t: lineStart, cmd: m[1].trim() })
  return out
}
