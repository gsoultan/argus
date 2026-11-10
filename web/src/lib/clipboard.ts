/**
 * Copy and paste between this browser and a remote desktop.
 *
 * The control plane says, on the session's ready, which way text may cross
 * (the asset type's rule, set in Settings › Asset types):
 *
 *   both  copy on the server lands here, and what is copied here pastes there
 *   to    paste into the server only
 *   from  copy out of the server only
 *   off   neither
 *
 * Copied on the server, the text is written to this clipboard while the tab
 * has focus, or the next time it does. Copied here, it reaches the server
 * when the screen takes focus (Chrome and Edge, once the browser has allowed
 * reading the clipboard) and in any browser at the paste shortcut: Ctrl+V,
 * or ⌘V on a Mac, reads the clipboard first and sends the key after, so the
 * server pastes what you just copied.
 *
 * "Paste as typing" types the clipboard instead, for a box that refuses paste
 * (the Windows sign-in screen) or a screen with no clipboard (screen sharing).
 */

import type { KeyOut } from '~/lib/keymap'

export type ClipboardRule = 'both' | 'to' | 'from' | 'off'

/** The most text one crossing carries, in UTF-8 bytes, as the server holds it. */
export const CLIPBOARD_MAX = 128 << 10

export function clipboardAllows(rule: ClipboardRule | undefined, toServer: boolean): boolean {
  if (rule === 'both') return true
  if (rule === 'to') return toServer
  if (rule === 'from') return !toServer
  return false
}

export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length
}

/** The most text paste-as-typing types, so a stray clipboard cannot type for minutes. */
export const TYPE_MAX = 5000

/**
 * The clipboard as keystrokes: characters as themselves, line breaks as
 * Enter and tabs as Tab, so a multi-line paste lands as lines.
 */
export function typingFor(text: string): KeyOut[] {
  const out: KeyOut[] = []
  const tap = (code: string) => out.push({ type: 'key', code, down: true }, { type: 'key', code, down: false })
  for (const ch of [...text.replace(/\r\n?/g, '\n')].slice(0, TYPE_MAX)) {
    if (ch === '\n') tap('Enter')
    else if (ch === '\t') tap('Tab')
    else out.push({ type: 'char', char: ch })
  }
  return out
}

/** Whether the key is this keyboard's paste shortcut. */
export function isPasteChord(e: { code: string; ctrlKey: boolean; metaKey: boolean; altKey: boolean }, mac: boolean): boolean {
  if (e.code !== 'KeyV' || e.altKey) return false
  return mac ? e.metaKey || e.ctrlKey : e.ctrlKey
}

/** Reads the clipboard, or null if the browser will not hand it over in time. */
export async function readClipboard(timeoutMs = 4000): Promise<string | null> {
  const read = navigator.clipboard?.readText?.bind(navigator.clipboard)
  if (!read) return null
  try {
    return await Promise.race([
      read(),
      new Promise<null>((r) => setTimeout(() => r(null), timeoutMs)),
    ])
  } catch {
    return null
  }
}

/** Whether reading needs no prompt now (Chrome and Edge once allowed). */
export async function canReadQuietly(): Promise<boolean> {
  try {
    const p = await navigator.permissions?.query({ name: 'clipboard-read' as PermissionName })
    return p?.state === 'granted'
  } catch {
    return false
  }
}

export async function writeClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
