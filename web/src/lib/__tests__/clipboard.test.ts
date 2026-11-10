import { afterEach, describe, expect, it, vi } from 'vitest'
import { clipboardAllows, isPasteChord, readClipboard, typingFor, utf8Length } from '~/lib/clipboard'

const ev = (code: string, m: { ctrl?: boolean; meta?: boolean; alt?: boolean } = {}) =>
  ({ code, ctrlKey: !!m.ctrl, metaKey: !!m.meta, altKey: !!m.alt })

afterEach(() => vi.restoreAllMocks())

describe('the clipboard rule', () => {
  it('lets text cross only the ways the asset type allows', () => {
    expect(clipboardAllows('both', true) && clipboardAllows('both', false)).toBe(true)
    expect([clipboardAllows('to', true), clipboardAllows('to', false)]).toEqual([true, false])
    expect([clipboardAllows('from', true), clipboardAllows('from', false)]).toEqual([false, true])
    expect(clipboardAllows('off', true) || clipboardAllows(undefined, false)).toBe(false)
  })
})

describe('the paste shortcut', () => {
  it('is Ctrl+V on a PC, and ⌘V or Ctrl+V on a Mac, never with Alt', () => {
    expect(isPasteChord(ev('KeyV', { ctrl: true }), false)).toBe(true)
    expect(isPasteChord(ev('KeyV', { meta: true }), false)).toBe(false)
    expect(isPasteChord(ev('KeyV', { meta: true }), true)).toBe(true)
    expect(isPasteChord(ev('KeyV', { ctrl: true, alt: true }), false)).toBe(false)
    expect(isPasteChord(ev('KeyC', { ctrl: true }), false)).toBe(false)
  })
})

describe('paste as typing', () => {
  it('types characters, and line breaks and tabs as their keys', () => {
    const keys = typingFor('a\tb\r\nc😀')
    expect(keys).toEqual([
      { type: 'char', char: 'a' },
      { type: 'key', code: 'Tab', down: true }, { type: 'key', code: 'Tab', down: false },
      { type: 'char', char: 'b' },
      { type: 'key', code: 'Enter', down: true }, { type: 'key', code: 'Enter', down: false },
      { type: 'char', char: 'c' },
      { type: 'char', char: '😀' },
    ])
  })

  it('stops at a sane length', () => {
    expect(typingFor('x'.repeat(20_000))).toHaveLength(5000)
  })
})

describe('reading the clipboard', () => {
  it('gives null rather than throwing when the browser refuses', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true, value: { readText: () => Promise.reject(new Error('denied')) },
    })
    expect(await readClipboard()).toBeNull()
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { readText: async () => 'copied' } })
    expect(await readClipboard()).toBe('copied')
  })

  it('counts bytes as the server does', () => {
    expect(utf8Length('é😀')).toBe(6)
  })
})
