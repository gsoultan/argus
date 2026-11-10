/**
 * What a key pressed in this browser sends to a remote Windows screen.
 *
 * A Windows or Linux keyboard passes through by physical position (the
 * KeyboardEvent `code`, which the agent turns into a scancode): Windows then
 * applies its own layout, so AltGr, dead keys and every national layout behave
 * as they do on a PC in front of it.
 *
 * A Mac keyboard needs translating, or every shortcut is wrong: ⌘C reached
 * Windows as Win+C. With Mac shortcuts on (the default on a Mac), ⌘ acts as
 * Ctrl, so ⌘C, ⌘V, ⌘Z, ⌘S and the rest do what the hands expect; ⌘⇧Z is
 * redo (Ctrl+Y); ⌘← / ⌘→ go to the start or end of the line; ⌘Tab is Alt+Tab
 * (only reaches the page in full screen); ⌃⌘ together is the Windows key; and
 * ⌃⌥⌫ is Ctrl+Alt+Del. Option is Alt by position already.
 *
 * macOS swallows the key-up of any key pressed while ⌘ is held. So a ⌘
 * shortcut is sent whole on key-down (press and release), and the key-up the
 * browser may or may not deliver later is ignored.
 *
 * With "type characters" on, for a remote whose layout differs from this
 * keyboard's, a key that produces a character on its own is sent as that
 * character (Unicode) rather than as a position, so what is printed on the key
 * is what arrives. Shortcuts and non-character keys still go by position.
 *
 * Pure: no DOM, no socket. RDPScreen feeds it events and sends what it returns.
 */

export type KeyOut =
  | { type: 'key'; code: string; down: boolean }
  | { type: 'char'; char: string }
  | { type: 'sync'; locks: number }

/** The parts of a KeyboardEvent this reads, so tests can build them. */
export interface KeyIn {
  code: string
  key: string
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  metaKey: boolean
  repeat?: boolean
  getModifierState?: (k: string) => boolean
}

export interface KeyboardPrefs {
  /** ⌘ as Ctrl, and the other Mac translations. Only matters on a Mac. */
  macShortcuts: boolean
  /** Characters rather than positions, for a remote on another layout. */
  chars: boolean
}

/** Lock bits as RDP's synchronize event carries them (TS_SYNC_*). */
export const LOCK_SCROLL = 1
export const LOCK_NUM = 2
export const LOCK_CAPS = 4

const META = new Set(['MetaLeft', 'MetaRight', 'OSLeft', 'OSRight'])
const CTRL = new Set(['ControlLeft', 'ControlRight'])
const MODIFIER = /^(Control|Shift|Alt)(Left|Right)$/

export function isMac(): boolean {
  if (typeof navigator === 'undefined') return false
  const ua = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData
  const p = ua?.platform || navigator.platform || navigator.userAgent
  return /mac|iphone|ipad|ipod/i.test(p)
}

const PREFS_KEY = 'argus.keyboard'

export function loadKeyboardPrefs(): KeyboardPrefs {
  const base: KeyboardPrefs = { macShortcuts: true, chars: false }
  try {
    return { ...base, ...(JSON.parse(window.localStorage.getItem(PREFS_KEY) ?? '{}') as Partial<KeyboardPrefs>) }
  } catch {
    return base
  }
}

export function saveKeyboardPrefs(p: KeyboardPrefs) {
  try {
    window.localStorage.setItem(PREFS_KEY, JSON.stringify(p))
  } catch {
    // Storage refused: the choice lasts for this page only.
  }
}

const tap = (code: string): KeyOut[] => [{ type: 'key', code, down: true }, { type: 'key', code, down: false }]

/** A chord pressed in order and released in reverse, as one would by hand. */
export function chord(...codes: string[]): KeyOut[] {
  return [
    ...codes.map((code): KeyOut => ({ type: 'key', code, down: true })),
    ...[...codes].reverse().map((code): KeyOut => ({ type: 'key', code, down: false })),
  ]
}

export class KeyMapper {
  /** Keys this mapper has told the remote are down, to release on blur. */
  private held = new Set<string>()
  /** Keys sent whole on key-down, whose key-up is to be ignored. */
  private tapped = new Set<string>()
  private metaDown = false
  /** Whether anything was pressed while ⌘ was down (else ⌃⌘ is Win). */
  private metaUsed = false
  /** Alt held on the remote for ⌘Tab, released with ⌘. */
  private altForTab = false
  private locks: number | null = null

  private mac: boolean
  private prefs: () => KeyboardPrefs

  constructor(mac: boolean, prefs: () => KeyboardPrefs) {
    this.mac = mac
    this.prefs = prefs
  }

  private mapMac(): boolean {
    return this.mac && this.prefs().macShortcuts
  }

  /** The lock keys as this event says they are, if it can say. */
  private lockState(e: KeyIn): number | null {
    if (!e.getModifierState) return null
    return (e.getModifierState('CapsLock') ? LOCK_CAPS : 0) |
      (e.getModifierState('NumLock') ? LOCK_NUM : 0) |
      (e.getModifierState('ScrollLock') ? LOCK_SCROLL : 0)
  }

  private press(code: string): KeyOut {
    this.held.add(code)
    return { type: 'key', code, down: true }
  }

  private release(code: string): KeyOut {
    this.held.delete(code)
    return { type: 'key', code, down: false }
  }

  down(e: KeyIn): KeyOut[] {
    const out: KeyOut[] = []
    // Caps Lock and Num Lock as this keyboard has them, before the first key
    // after focus and whenever they change, so the remote's match.
    const locks = this.lockState(e)
    if (locks !== null && locks !== this.locks && e.code !== 'CapsLock' && e.code !== 'NumLock' && e.code !== 'ScrollLock') {
      this.locks = locks
      out.push({ type: 'sync', locks })
    }

    if (this.mapMac()) {
      if (META.has(e.code)) {
        // Held here, not sent: ⌘ is not the Windows key.
        if (!this.metaDown) {
          this.metaDown = true
          this.metaUsed = false
        }
        return out
      }
      if (this.metaDown) {
        // Modifiers pressed with ⌘ go down as themselves and up when let go.
        if (MODIFIER.test(e.code)) return [...out, this.press(e.code)]
        this.metaUsed = true
        this.tapped.add(e.code)
        return [...out, ...this.macShortcut(e)]
      }
      // ⌃⌥⌫ is how a Mac keyboard says Ctrl+Alt+Del.
      if (e.code === 'Backspace' && e.ctrlKey && e.altKey) {
        this.tapped.add(e.code)
        return [...out, ...tap('Delete')]
      }
    }

    if (e.repeat && this.held.has(e.code)) {
      return [...out, { type: 'key', code: e.code, down: true }]
    }
    if (this.prefs().chars && !e.ctrlKey && !e.altKey && !e.metaKey && e.key.length > 0 && [...e.key].length === 1) {
      // A character on its own: sent as itself, whatever the remote layout.
      this.tapped.add(e.code)
      return [...out, { type: 'char', char: e.key }]
    }
    if (e.key === 'Dead' && this.prefs().chars) {
      // The accent waits for the next key, which arrives composed.
      return out
    }
    return [...out, this.press(e.code)]
  }

  up(e: KeyIn): KeyOut[] {
    if (this.mapMac() && META.has(e.code)) {
      this.metaDown = false
      const out: KeyOut[] = []
      if (this.altForTab) {
        this.altForTab = false
        out.push(this.release('AltLeft'))
      }
      // ⌃⌘ pressed and let go with nothing between: the Windows key.
      const ctrl = [...this.held].find((c) => CTRL.has(c))
      if (!this.metaUsed && ctrl) {
        out.push(this.release(ctrl), ...tap('MetaLeft'))
      }
      this.metaUsed = false
      return out
    }
    if (this.tapped.delete(e.code)) return []
    if (!this.held.has(e.code)) return []
    return [this.release(e.code)]
  }

  /** Everything still down on the remote, let go: focus left the screen. */
  releaseAll(): KeyOut[] {
    const out = [...this.held].map((code): KeyOut => ({ type: 'key', code, down: false }))
    this.held.clear()
    this.tapped.clear()
    this.metaDown = false
    this.metaUsed = false
    this.altForTab = false
    // Ask again on the next key: they may have changed while away.
    this.locks = null
    return out
  }

  /** A key pressed with ⌘ held, on a Mac, as Windows expects it. */
  private macShortcut(e: KeyIn): KeyOut[] {
    const ctrlHeld = [...this.held].some((c) => CTRL.has(c))
    const withCtrl = (...codes: string[]) => (ctrlHeld ? chord(...codes) : chord('ControlLeft', ...codes))
    switch (e.code) {
      case 'Tab':
        // ⌘Tab: Alt+Tab, Alt held until ⌘ is let go so the switcher stays.
        if (!this.altForTab) {
          this.altForTab = true
          return [this.press('AltLeft'), ...tap('Tab')]
        }
        return tap('Tab')
      case 'ArrowLeft': return tap('Home')
      case 'ArrowRight': return tap('End')
      case 'ArrowUp': return withCtrl('Home')
      case 'ArrowDown': return withCtrl('End')
      case 'Backspace': return chord('ShiftLeft', 'Home').concat(tap('Delete'))
      case 'KeyZ':
        if (e.shiftKey) {
          // Redo. Shift is down on the remote; let it go for Ctrl+Y and
          // put it back, so the hand still holding it is not contradicted.
          const shift = [...this.held].find((c) => c === 'ShiftLeft' || c === 'ShiftRight') ?? 'ShiftLeft'
          return [{ type: 'key', code: shift, down: false }, ...withCtrl('KeyY'), { type: 'key', code: shift, down: true }]
        }
        return withCtrl('KeyZ')
      default:
        return withCtrl(e.code)
    }
  }
}
