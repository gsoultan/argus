import { describe, expect, it } from 'vitest'
import { KeyMapper, LOCK_CAPS, LOCK_NUM, type KeyIn, type KeyOut, type KeyboardPrefs } from '~/lib/keymap'

const KEY: Record<string, string> = { KeyC: 'c', KeyV: 'v', KeyZ: 'z', KeyA: 'a', KeyQ: 'q', Digit2: '2', Tab: 'Tab' }

function ev(code: string, mods: Partial<KeyIn> = {}, locks: string[] = []): KeyIn {
  return {
    code, key: mods.key ?? KEY[code] ?? code, ctrlKey: false, altKey: false, shiftKey: false, metaKey: false,
    getModifierState: (k) => locks.includes(k), ...mods,
  }
}

function mapper(mac: boolean, prefs: Partial<KeyboardPrefs> = {}) {
  const p: KeyboardPrefs = { macShortcuts: true, chars: false, ...prefs }
  return new KeyMapper(mac, () => p)
}

const noSync = (out: KeyOut[]) => out.filter((o) => o.type !== 'sync')

const keys = (out: KeyOut[]) =>
  out.filter((o) => o.type === 'key').map((o) => (o.type === 'key' ? `${o.down ? '+' : '-'}${o.code}` : ''))

describe('a Windows or Linux keyboard', () => {
  it('passes keys through by position, so the remote layout applies', () => {
    const m = mapper(false)
    expect(keys(m.down(ev('ControlLeft', { ctrlKey: true })))).toEqual(['+ControlLeft'])
    expect(keys(m.down(ev('KeyC', { ctrlKey: true })))).toEqual(['+KeyC'])
    expect(keys(m.up(ev('KeyC', { ctrlKey: true })))).toEqual(['-KeyC'])
    expect(keys(m.up(ev('ControlLeft')))).toEqual(['-ControlLeft'])
  })

  it('keeps AltGr as the right Alt key, which the remote layout turns into @ or €', () => {
    const m = mapper(false)
    expect(keys(m.down(ev('AltRight', { altKey: true, ctrlKey: true })))).toEqual(['+AltRight'])
    expect(keys(m.down(ev('KeyQ', { altKey: true, ctrlKey: true, key: '@' })))).toEqual(['+KeyQ'])
  })

  it('treats the Windows key as the Windows key', () => {
    const m = mapper(false)
    expect(keys(m.down(ev('MetaLeft', { metaKey: true })))).toEqual(['+MetaLeft'])
    expect(keys(m.up(ev('MetaLeft')))).toEqual(['-MetaLeft'])
  })
})

describe('a Mac keyboard with Mac shortcuts', () => {
  it('sends ⌘C as Ctrl+C, whole, and never the Windows key', () => {
    const m = mapper(true)
    expect(keys(m.down(ev('MetaLeft', { metaKey: true })))).toEqual([])
    expect(keys(m.down(ev('KeyC', { metaKey: true })))).toEqual(['+ControlLeft', '+KeyC', '-KeyC', '-ControlLeft'])
    // macOS may or may not deliver C's key-up: either way nothing more goes.
    expect(keys(m.up(ev('KeyC')))).toEqual([])
    expect(keys(m.up(ev('MetaLeft')))).toEqual([])
  })

  it('sends ⌘V, ⌘Z and ⌘A as their Ctrl twins', () => {
    const m = mapper(true)
    m.down(ev('MetaLeft', { metaKey: true }))
    for (const code of ['KeyV', 'KeyZ', 'KeyA']) {
      expect(keys(m.down(ev(code, { metaKey: true })))).toEqual(['+ControlLeft', `+${code}`, `-${code}`, '-ControlLeft'])
    }
  })

  it('sends ⌘⇧Z as Ctrl+Y and leaves Shift as the hand has it', () => {
    const m = mapper(true)
    m.down(ev('MetaLeft', { metaKey: true }))
    expect(keys(m.down(ev('ShiftLeft', { metaKey: true, shiftKey: true })))).toEqual(['+ShiftLeft'])
    expect(keys(m.down(ev('KeyZ', { metaKey: true, shiftKey: true })))).toEqual(
      ['-ShiftLeft', '+ControlLeft', '+KeyY', '-KeyY', '-ControlLeft', '+ShiftLeft'],
    )
    expect(keys(m.up(ev('ShiftLeft')))).toEqual(['-ShiftLeft'])
  })

  it('moves to the start and end of the line with ⌘← and ⌘→', () => {
    const m = mapper(true)
    m.down(ev('MetaLeft', { metaKey: true }))
    expect(keys(m.down(ev('ArrowLeft', { metaKey: true })))).toEqual(['+Home', '-Home'])
    expect(keys(m.down(ev('ArrowRight', { metaKey: true })))).toEqual(['+End', '-End'])
    expect(keys(m.down(ev('ArrowUp', { metaKey: true })))).toEqual(['+ControlLeft', '+Home', '-Home', '-ControlLeft'])
  })

  it('holds Alt through ⌘Tab so the window switcher stays until ⌘ is let go', () => {
    const m = mapper(true)
    m.down(ev('MetaLeft', { metaKey: true }))
    expect(keys(m.down(ev('Tab', { metaKey: true })))).toEqual(['+AltLeft', '+Tab', '-Tab'])
    expect(keys(m.down(ev('Tab', { metaKey: true })))).toEqual(['+Tab', '-Tab'])
    expect(keys(m.up(ev('MetaLeft')))).toEqual(['-AltLeft'])
  })

  it('makes ⌃⌘ on its own the Windows key', () => {
    const m = mapper(true)
    expect(keys(m.down(ev('ControlLeft', { ctrlKey: true })))).toEqual(['+ControlLeft'])
    expect(keys(m.down(ev('MetaLeft', { ctrlKey: true, metaKey: true })))).toEqual([])
    expect(keys(m.up(ev('MetaLeft', { ctrlKey: true })))).toEqual(['-ControlLeft', '+MetaLeft', '-MetaLeft'])
    // Control's own key-up then finds nothing left to release.
    expect(keys(m.up(ev('ControlLeft')))).toEqual([])
  })

  it('makes ⌃⌥⌫ Ctrl+Alt+Del', () => {
    const m = mapper(true)
    m.down(ev('ControlLeft', { ctrlKey: true }))
    m.down(ev('AltLeft', { ctrlKey: true, altKey: true }))
    expect(keys(m.down(ev('Backspace', { ctrlKey: true, altKey: true })))).toEqual(['+Delete', '-Delete'])
  })

  it('leaves ⌘ as the Windows key when Mac shortcuts are off', () => {
    const m = mapper(true, { macShortcuts: false })
    expect(keys(m.down(ev('MetaLeft', { metaKey: true })))).toEqual(['+MetaLeft'])
    expect(keys(m.down(ev('KeyC', { metaKey: true })))).toEqual(['+KeyC'])
  })
})

describe('typing characters for another layout', () => {
  it('sends a character as itself and shortcuts by position', () => {
    const m = mapper(false, { chars: true })
    expect(noSync(m.down(ev('Digit2', { key: '"', shiftKey: true })))).toEqual([{ type: 'char', char: '"' }])
    expect(m.up(ev('Digit2'))).toEqual([])
    m.down(ev('ControlLeft', { ctrlKey: true }))
    expect(keys(m.down(ev('KeyC', { ctrlKey: true })))).toEqual(['+KeyC'])
  })

  it('waits on a dead key and sends the composed letter', () => {
    const m = mapper(false, { chars: true })
    expect(noSync(m.down(ev('Quote', { key: 'Dead' })))).toEqual([])
    expect(noSync(m.down(ev('KeyE', { key: 'é' })))).toEqual([{ type: 'char', char: 'é' }])
  })
})

describe('staying in step with the remote', () => {
  it('matches Caps Lock and Num Lock before the first key, and when they change', () => {
    const m = mapper(false)
    expect(m.down(ev('KeyA', {}, ['CapsLock', 'NumLock']))[0]).toEqual({ type: 'sync', locks: LOCK_CAPS | LOCK_NUM })
    m.up(ev('KeyA'))
    expect(m.down(ev('KeyA', {}, ['CapsLock', 'NumLock']))[0]).toEqual({ type: 'key', code: 'KeyA', down: true })
    expect(m.down(ev('KeyA', {}, ['NumLock']))[0]).toEqual({ type: 'sync', locks: LOCK_NUM })
  })

  it('lets go of every key still down when focus leaves the screen', () => {
    const m = mapper(false)
    m.down(ev('ShiftLeft', { shiftKey: true }))
    m.down(ev('KeyA', { shiftKey: true }))
    expect(keys(m.releaseAll()).sort()).toEqual(['-KeyA', '-ShiftLeft'])
    expect(m.releaseAll()).toEqual([])
  })
})
