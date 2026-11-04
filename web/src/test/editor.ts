import { act, screen } from '@testing-library/react'
import { EditorView } from '@codemirror/view'

/** The query editor's view, found by its label. */
export function editorView(): EditorView {
  const v = EditorView.findFromDOM(screen.getByLabelText('SQL'))
  if (!v) throw new Error('no SQL editor')
  return v
}

/** Replaces the editor's text as typing would, and puts the cursor at `cursor` (the end by default). */
export function setEditor(text: string, cursor = text.length) {
  const v = editorView()
  act(() => {
    v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: text }, selection: { anchor: cursor }, userEvent: 'input.type' })
  })
}
