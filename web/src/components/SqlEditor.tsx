import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react'
import { Box } from '@mantine/core'
import { EditorState, Prec, type Extension } from '@codemirror/state'
import {
  EditorView, drawSelection, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars,
  keymap, lineNumbers, placeholder as placeholderExt,
} from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { HighlightStyle, bracketMatching, indentOnInput, syntaxHighlighting } from '@codemirror/language'
import { tags as t } from '@lezer/highlight'
import { PostgreSQL, sql as sqlLang } from '@codemirror/lang-sql'
import {
  autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, completionStatus, startCompletion,
  type CompletionContext, type CompletionResult,
} from '@codemirror/autocomplete'
import { highlightSelectionMatches, search, searchKeymap } from '@codemirror/search'
import { lintGutter, setDiagnostics } from '@codemirror/lint'
import type { Completion, SuggestionKind } from '~/lib/sqlComplete'

/*
The query editor: CodeMirror 6 with the Postgres dialect. Line numbers,
highlighting, bracket matching, find and replace (⌘/Ctrl+F), undo history,
and the console's own autocomplete (the same completeSQL the old textarea
used, so what it suggests did not change).

The value is controlled: `value` in, `onChange` out. A change of `value`
from outside (another tab, a name picked in the object tree) replaces the
text; typing does not echo back.

The error mark is where Postgres said the statement failed, worked out by
the workspace (see pgErrorOffset) as an offset in this text.
*/

/** An error to mark: `from` is an offset in the editor's text. */
export interface EditorError { from: number; message: string }

export interface SqlEditorHandle {
  /** The selected text and where it starts, or the whole text from 0. */
  selection: () => { text: string; from: number }
  /** Replaces the selection with text and puts the cursor after it. */
  insert: (text: string) => void
  /** Replaces the selection, or everything when nothing is selected. */
  replaceSelectionOrAll: (text: string) => void
  focus: () => void
}

export interface SqlEditorProps {
  value: string
  onChange: (v: string) => void
  complete: (text: string, cursor: number, explicit: boolean) => Completion | null
  /** Changes when the data behind `complete` grows (columns arrived). */
  refresh?: unknown
  onRun: () => void
  onSave: () => void
  onFormat?: () => void
  error?: EditorError | null
  height: number
  narrow?: boolean
  placeholder?: string
}

const kindType: Record<SuggestionKind, string> = {
  keyword: 'keyword', function: 'function', schema: 'namespace', table: 'class', view: 'class',
  sequence: 'variable', column: 'property', alias: 'variable', command: 'keyword', key: 'constant',
}

const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.operatorKeyword, t.modifier], color: 'var(--mantine-color-azure-text)', fontWeight: 600 },
  { tag: [t.string, t.special(t.string)], color: 'var(--mantine-color-violet-text)' },
  { tag: [t.number, t.bool, t.null], color: 'var(--mantine-color-cyan-text)' },
  { tag: [t.lineComment, t.blockComment, t.comment], color: 'var(--color-ink-muted)', fontStyle: 'italic' },
  { tag: [t.typeName, t.standard(t.name)], color: 'var(--mantine-color-indigo-text)' },
  { tag: [t.special(t.name), t.quote], color: 'var(--color-ink)' },
  { tag: [t.punctuation, t.operator, t.paren, t.bracket], color: 'var(--color-ink-soft)' },
])

const theme = (narrow: boolean) => EditorView.theme({
  '&': { height: '100%', fontSize: narrow ? '12px' : '13px', background: 'var(--color-surface)', color: 'var(--color-ink)' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'var(--font-mono)', lineHeight: '21px', overflow: 'auto' },
  '.cm-content': { caretColor: 'var(--color-ink)', padding: '8px 0' },
  // drawSelection hides the native caret and draws its own, which the base
  // theme paints black; follow the ink colour so it shows on a dark surface.
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--color-ink)', borderLeftWidth: '2px' },
  '.cm-gutters': { background: 'var(--color-raised)', color: 'var(--color-ink-muted)', border: 0, borderRight: '1px solid var(--color-line)' },
  '.cm-activeLine': { background: 'color-mix(in srgb, var(--color-brand) 5%, transparent)' },
  '.cm-activeLineGutter': { background: 'transparent', color: 'var(--color-ink)' },
  '.cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection': {
    background: 'color-mix(in srgb, var(--color-brand) 22%, transparent) !important',
  },
  '.cm-matchingBracket': { background: 'color-mix(in srgb, var(--color-brand) 18%, transparent)', outline: 'none' },
  '.cm-searchMatch': { background: 'color-mix(in srgb, var(--mantine-color-amber-5) 30%, transparent)' },
  '.cm-placeholder': { color: 'var(--color-ink-muted)' },
  '.cm-panels': { background: 'var(--color-raised)', color: 'var(--color-ink)', borderColor: 'var(--color-line)' },
  '.cm-panel input, .cm-panel button': { fontFamily: 'inherit', fontSize: '12px' },
  '.cm-tooltip': { background: 'var(--color-raised)', border: '1px solid var(--color-line)', borderRadius: '6px' },
  '.cm-tooltip-autocomplete > ul > li[aria-selected]': { background: 'var(--color-brand-wash)', color: 'var(--color-ink)' },
  '.cm-completionDetail': { color: 'var(--color-ink-muted)', fontStyle: 'normal', marginLeft: '8px' },
  '.cm-lintRange-error': { backgroundImage: 'none', textDecoration: 'wavy underline var(--mantine-color-rose-6)', textUnderlineOffset: '3px' },
  '.cm-diagnostic-error': { borderLeftColor: 'var(--mantine-color-rose-6)' },
})

/**
 * Where Postgres's error position lands in the editor. Postgres counts
 * characters from 1 in the statement it ran; a JS offset counts UTF-16 units.
 * `ran` is what was sent, `at` is where it began in the editor (a selection
 * runs from where it starts), and `back` maps an offset in `ran` back to one
 * in the editor's text when parameters were bound.
 */
export function pgErrorOffset(ran: string, position: number, at: number, back: (o: number) => number = (o) => o): number {
  return at + back(charOffset(ran, 0, Math.max(0, position - 1)))
}

/** The UTF-16 offset of the n-th character (0-based) of text from `at`. */
export function charOffset(text: string, at: number, n: number): number {
  let i = at
  for (let k = 0; k < n && i < text.length; k++) {
    const c = text.charCodeAt(i)
    i += c >= 0xd800 && c <= 0xdbff ? 2 : 1
  }
  return Math.min(i, text.length)
}

/** The span an error at `from` marks: the word that starts there. */
export function errorSpan(text: string, from: number): { from: number; to: number } {
  from = Math.max(0, Math.min(from, text.length))
  let to = from
  while (to < text.length && /[\p{L}\p{N}_$."]/u.test(text.charAt(to))) to++
  if (to === from) to = Math.min(text.length, from + 1)
  return { from, to }
}

export const SqlEditor = forwardRef<SqlEditorHandle, SqlEditorProps>(function SqlEditor(props, ref) {
  const host = useRef<HTMLDivElement | null>(null)
  const view = useRef<EditorView | null>(null)
  // The latest props, for extensions made once.
  const live = useRef(props)
  live.current = props
  // The last list was missing columns; when they arrive it is worked out again.
  const waiting = useRef(false)

  useEffect(() => {
    const source = (ctx: CompletionContext): CompletionResult | null => {
      const c = live.current.complete(ctx.state.doc.toString(), ctx.pos, ctx.explicit)
      waiting.current = Boolean(c?.needs?.length)
      if (!c || c.items.length === 0) return null
      // A word typed out in full needs no list (Enter is then a new line).
      if (!ctx.explicit && c.items.length === 1 && c.items[0]!.insert.toLowerCase() === c.prefix.toLowerCase()) return null
      return {
        from: c.from, to: c.to, filter: false,
        options: c.items.map((s, i) => ({ label: s.label, apply: s.insert, type: kindType[s.kind] ?? 'text', detail: s.detail, boost: -i })),
      }
    }
    const exts: Extension[] = [
      lineNumbers(), highlightActiveLineGutter(), highlightSpecialChars(), history(), drawSelection(),
      indentOnInput(), bracketMatching(), closeBrackets(), highlightActiveLine(), highlightSelectionMatches(),
      search({ top: true }), lintGutter(),
      sqlLang({ dialect: PostgreSQL }),
      syntaxHighlighting(highlight),
      autocompletion({ override: [source], icons: false, activateOnTypingDelay: 60 }),
      Prec.highest(keymap.of([
        { key: 'Mod-Enter', run: () => { live.current.onRun(); return true } },
        { key: 'Mod-s', run: () => { live.current.onSave(); return true }, preventDefault: true },
        { key: 'Shift-Alt-f', run: () => { live.current.onFormat?.(); return true } },
        { key: 'Mod-Space', run: startCompletion },
      ])),
      keymap.of([...closeBracketsKeymap, ...completionKeymap, ...searchKeymap, ...historyKeymap, indentWithTab, ...defaultKeymap]),
      EditorView.updateListener.of((u) => {
        if (!u.docChanged) return
        const text = u.state.doc.toString()
        if (text !== live.current.value) live.current.onChange(text)
      }),
      EditorView.contentAttributes.of({ 'aria-label': 'SQL', 'aria-multiline': 'true', spellcheck: 'false', autocapitalize: 'off', autocorrect: 'off' }),
      theme(Boolean(props.narrow)),
    ]
    if (props.placeholder) exts.push(placeholderExt(props.placeholder))
    const v = new EditorView({ parent: host.current!, state: EditorState.create({ doc: props.value, extensions: exts }) })
    view.current = v
    return () => { v.destroy(); view.current = null }
    // Made once per narrow/placeholder; the value is synced below.
  }, [props.narrow, props.placeholder])

  // Outside changes to the value replace the text.
  useEffect(() => {
    const v = view.current
    if (!v) return
    const cur = v.state.doc.toString()
    if (cur !== props.value) v.dispatch({ changes: { from: 0, to: cur.length, insert: props.value } })
  }, [props.value])

  // Columns arrived for a list that wanted them: ask again.
  useEffect(() => {
    const v = view.current
    if (v && waiting.current && (v.hasFocus || completionStatus(v.state) !== null)) startCompletion(v)
  }, [props.refresh])

  useEffect(() => {
    const v = view.current
    if (!v) return
    const e = props.error
    if (!e) { v.dispatch(setDiagnostics(v.state, [])); return }
    const { from, to } = errorSpan(v.state.doc.toString(), e.from)
    v.dispatch(setDiagnostics(v.state, [{ from, to, severity: 'error', message: e.message }]))
  }, [props.error])

  useImperativeHandle(ref, () => ({
    selection: () => {
      const v = view.current
      if (!v) return { text: live.current.value, from: 0 }
      const r = v.state.selection.main
      return r.empty ? { text: v.state.doc.toString(), from: 0 } : { text: v.state.sliceDoc(r.from, r.to), from: r.from }
    },
    insert: (text) => {
      const v = view.current
      if (!v) { live.current.onChange(live.current.value + text); return }
      v.dispatch(v.state.replaceSelection(text))
      v.focus()
    },
    replaceSelectionOrAll: (text) => {
      const v = view.current
      if (!v) { live.current.onChange(text); return }
      const r = v.state.selection.main
      if (r.empty) v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: text } })
      else v.dispatch({ changes: { from: r.from, to: r.to, insert: text }, selection: { anchor: r.from, head: r.from + text.length } })
    },
    focus: () => view.current?.focus(),
  }), [])

  return <Box ref={host} style={{ height: props.height, minHeight: 80 }} data-testid="sql-editor" />
})
