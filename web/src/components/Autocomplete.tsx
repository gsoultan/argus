import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from 'react'
import { createPortal } from 'react-dom'
import type { Completion, Suggestion, SuggestionKind } from '~/lib/sqlComplete'

/*
An autocomplete list for a plain textarea or input: the editor stays a native
control (selection, undo, phone keyboards, ⌘/Ctrl+Enter all keep working) and
this adds a list under the caret.

Typing a word or a dot opens it, Ctrl+Space asks for it with nothing typed,
↑/↓ move, Enter or Tab take the highlighted entry, Esc closes it. What the
list holds comes from `complete`, so the same control serves SQL and Redis.
*/

export type CompleteFn = (text: string, cursor: number, explicit: boolean) => Completion | null

interface Open { c: Completion; explicit: boolean; active: number }

type Editor = HTMLTextAreaElement | HTMLInputElement

export interface CompletionControl {
  /** Call first in the editor's onKeyDown; true means the key was used. */
  onKeyDown: (e: React.KeyboardEvent<Editor>) => boolean
  /** Call from the editor's onChange after updating the value. */
  onTyped: () => void
  close: () => void
  /** Props for the editor element (ARIA wiring and closing on blur/click). */
  editorProps: {
    'aria-autocomplete': 'list'
    'aria-expanded': boolean
    'aria-controls': string | undefined
    'aria-activedescendant': string | undefined
    onBlur: () => void
    onClick: () => void
  }
  popup: ReactNode
}

export function useCompletion({ ref, value, setValue, complete, refresh }: {
  ref: RefObject<Editor | null>
  value: string
  setValue: (v: string) => void
  complete: CompleteFn
  /** Changes when the data behind `complete` grows (columns arrived). */
  refresh?: unknown
}): CompletionControl {
  const [open, setOpen] = useState<Open | null>(null)
  const typed = useRef(false)
  // The last list was missing data that was asked for (a table's columns);
  // when it arrives the list is worked out again, even if it showed nothing.
  const waiting = useRef<{ explicit: boolean } | null>(null)
  const listId = useId()

  const compute = useCallback((explicit: boolean) => {
    const el = ref.current
    if (!el || el.selectionStart !== el.selectionEnd) { setOpen(null); return }
    const c = complete(el.value, el.selectionStart ?? el.value.length, explicit)
    waiting.current = c?.needs?.length ? { explicit } : null
    if (!c || c.items.length === 0) { setOpen(null); return }
    // A word typed out in full needs no list, and the list would take the
    // Enter meant for a new line.
    if (!explicit && c.items[0]!.insert.toLowerCase() === c.prefix.toLowerCase()) { setOpen(null); return }
    setOpen((o) => ({ c, explicit, active: o && o.c.from === c.from ? Math.min(o.active, c.items.length - 1) : 0 }))
  }, [complete, ref])

  // After typing, the list follows the caret.
  useEffect(() => {
    if (!typed.current) return
    typed.current = false
    compute(open?.explicit ?? false)
    // Only on a typed change of value.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value])

  // New data (a table's columns) refreshes an open list in place.
  const openRef = useRef(open)
  openRef.current = open
  useEffect(() => {
    const again = openRef.current ?? (document.activeElement === ref.current ? waiting.current : null)
    if (again) compute(again.explicit)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh])

  const close = useCallback(() => setOpen(null), [])

  const accept = useCallback((s: Suggestion) => {
    const o = openRef.current
    const el = ref.current
    if (!o || !el) return
    const text = el.value
    const next = text.slice(0, o.c.from) + s.insert + text.slice(o.c.to)
    const caret = o.c.from + s.insert.length
    setOpen(null)
    setValue(next)
    requestAnimationFrame(() => {
      el.focus()
      el.selectionStart = el.selectionEnd = caret
    })
  }, [ref, setValue])

  const onKeyDown = useCallback((e: React.KeyboardEvent<Editor>) => {
    if (e.key === ' ' && e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault()
      compute(true)
      return true
    }
    const o = openRef.current
    if (!o) return false
    if (e.metaKey || e.ctrlKey || e.altKey) {
      setOpen(null)
      return false
    }
    const n = o.c.items.length
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault()
        setOpen({ ...o, active: (o.active + 1) % n })
        return true
      case 'ArrowUp':
        e.preventDefault()
        setOpen({ ...o, active: (o.active - 1 + n) % n })
        return true
      case 'PageDown':
        e.preventDefault()
        setOpen({ ...o, active: Math.min(n - 1, o.active + 8) })
        return true
      case 'PageUp':
        e.preventDefault()
        setOpen({ ...o, active: Math.max(0, o.active - 8) })
        return true
      case 'Enter':
      case 'Tab':
        if (e.shiftKey) return false
        e.preventDefault()
        accept(o.c.items[o.active]!)
        return true
      case 'Escape':
        e.preventDefault()
        e.stopPropagation()
        setOpen(null)
        return true
      case 'ArrowLeft':
      case 'ArrowRight':
      case 'Home':
      case 'End':
        setOpen(null)
        return false
    }
    return false
  }, [accept, compute])

  const onTyped = useCallback(() => { typed.current = true }, [])

  const optionId = (i: number) => `${listId}-o${i}`
  return {
    onKeyDown,
    onTyped,
    close,
    editorProps: {
      'aria-autocomplete': 'list',
      'aria-expanded': open !== null,
      'aria-controls': open ? listId : undefined,
      'aria-activedescendant': open ? optionId(open.active) : undefined,
      onBlur: close,
      onClick: close,
    },
    popup: open && ref.current ? (
      <CompletionList el={ref.current} open={open} id={listId} optionId={optionId}
        onPick={accept} onHover={(i) => setOpen({ ...open, active: i })} onDismiss={close} />
    ) : null,
  }
}

/* ── The list ───────────────────────────────────────────────────────────── */

const KIND_MARK: Record<SuggestionKind, [mark: string, title: string]> = {
  keyword: ['K', 'keyword'], schema: ['S', 'schema'], table: ['T', 'table'], view: ['V', 'view'],
  sequence: ['#', 'sequence'], column: ['C', 'column'], function: ['ƒ', 'function'], alias: ['A', 'table in this statement'],
  command: ['>', 'command'], key: ['k', 'key'],
}

const WIDTH = 380
const MAX_H = 264

function CompletionList({ el, open, id, optionId, onPick, onHover, onDismiss }: {
  el: Editor
  open: Open
  id: string
  optionId: (i: number) => string
  onPick: (s: Suggestion) => void
  onHover: (i: number) => void
  onDismiss: () => void
}) {
  const [pos, setPos] = useState<{ left: number; top: number; above: boolean } | null>(null)
  const listRef = useRef<HTMLDivElement | null>(null)

  useLayoutEffect(() => {
    const caret = caretPosition(el, open.c.from)
    const r = el.getBoundingClientRect()
    const vw = window.innerWidth
    const vh = window.innerHeight
    const x = r.left + caret.left - el.scrollLeft
    const y = r.top + caret.top - el.scrollTop
    const left = Math.max(8, Math.min(x, vw - Math.min(WIDTH, vw - 16) - 8))
    const below = y + caret.height + 4
    const above = below + MAX_H > vh && y - MAX_H - 4 > 0
    setPos({ left, top: above ? y - 4 : below, above })
  }, [el, open.c.from, open.c.items.length])

  // The list is placed once, at the caret; scrolling the page would leave it
  // behind, so a scroll or resize closes it instead.
  useEffect(() => {
    const onScroll = (e: Event) => { if (!listRef.current?.contains(e.target as Node)) onDismiss() }
    window.addEventListener('scroll', onScroll, true)
    window.addEventListener('resize', onDismiss)
    return () => {
      window.removeEventListener('scroll', onScroll, true)
      window.removeEventListener('resize', onDismiss)
    }
  }, [onDismiss])

  useEffect(() => {
    document.getElementById(optionId(open.active))?.scrollIntoView({ block: 'nearest' })
  }, [open.active, optionId])

  if (!pos) return null
  const typed = open.c.prefix.toLowerCase()
  return createPortal(
    <div
      ref={listRef}
      id={id}
      role="listbox"
      aria-label="Suggestions"
      data-testid="completion-list"
      style={{
        position: 'fixed', left: pos.left, top: pos.top, transform: pos.above ? 'translateY(-100%)' : undefined,
        width: `min(${WIDTH}px, calc(100vw - 16px))`, maxHeight: MAX_H, overflowY: 'auto', zIndex: 400,
        background: 'var(--color-surface)', border: '1px solid var(--color-line)', borderRadius: 8,
        boxShadow: '0 8px 24px rgba(0,0,0,.28)', padding: 4, fontFamily: 'var(--font-mono)', fontSize: 12,
      }}
    >
      {open.c.items.map((s, i) => {
        const [mark, title] = KIND_MARK[s.kind]
        const active = i === open.active
        const at = typed ? s.label.toLowerCase().indexOf(typed) : -1
        return (
          <div
            key={`${s.kind}:${s.insert}:${i}`}
            id={optionId(i)}
            role="option"
            aria-selected={active}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => onPick(s)}
            onMouseMove={() => { if (!active) onHover(i) }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8, padding: '3px 6px', borderRadius: 6, cursor: 'pointer',
              background: active ? 'var(--color-raised)' : undefined,
              boxShadow: active ? 'inset 2px 0 0 var(--color-brand)' : undefined,
            }}
          >
            <span title={title} aria-hidden style={{
              width: 16, height: 16, flexShrink: 0, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
              borderRadius: 4, fontSize: 10, fontWeight: 700, border: '1px solid var(--color-line)',
              color: 'var(--mantine-color-dimmed)',
            }}>{mark}</span>
            <span style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', minWidth: 0 }}>
              {at >= 0 ? <>
                {s.label.slice(0, at)}<b>{s.label.slice(at, at + typed.length)}</b>{s.label.slice(at + typed.length)}
              </> : s.label}
            </span>
            {s.detail && (
              <span style={{ marginLeft: 'auto', paddingLeft: 8, color: 'var(--mantine-color-dimmed)', fontSize: 11, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: '55%' }}>
                {s.detail}
              </span>
            )}
          </div>
        )
      })}
    </div>,
    document.body,
  )
}

/* ── Caret position ─────────────────────────────────────────────────────── */

const COPY = [
  'boxSizing', 'width', 'borderTopWidth', 'borderRightWidth', 'borderBottomWidth', 'borderLeftWidth',
  'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'fontStyle', 'fontVariant', 'fontWeight',
  'fontStretch', 'fontSize', 'lineHeight', 'fontFamily', 'textAlign', 'textTransform', 'textIndent',
  'letterSpacing', 'wordSpacing', 'tabSize',
] as const

/**
 * Where character `pos` sits inside the editor, relative to its border box
 * and before scrolling: a hidden copy with the same box and font, filled up to
 * `pos`, measured at a marker.
 */
export function caretPosition(el: Editor, pos: number): { left: number; top: number; height: number } {
  const cs = window.getComputedStyle(el)
  const div = document.createElement('div')
  const st = div.style
  for (const p of COPY) st[p] = cs[p]
  st.position = 'absolute'
  st.visibility = 'hidden'
  st.top = '0'
  st.left = '-9999px'
  st.overflow = 'hidden'
  const area = el.tagName === 'TEXTAREA'
  st.whiteSpace = area ? 'pre-wrap' : 'pre'
  st.overflowWrap = area ? 'break-word' : 'normal'
  div.textContent = el.value.slice(0, pos)
  const mark = document.createElement('span')
  mark.textContent = el.value.slice(pos) || '.'
  div.appendChild(mark)
  document.body.appendChild(div)
  const lh = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.4 || 18
  const out = { left: mark.offsetLeft, top: mark.offsetTop, height: lh }
  document.body.removeChild(div)
  return out
}
