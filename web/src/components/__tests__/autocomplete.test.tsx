import { useCallback, useRef, useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { act, fireEvent, screen } from '@testing-library/react'
import { renderWithProviders } from '~/test/render'
import { useCompletion } from '~/components/Autocomplete'
import { catalogFrom, completeSQL, type SQLColumn } from '~/lib/sqlComplete'

const catalog = catalogFrom([
  { name: 'public', kind: 'path', detail: '1', schema: 'public' },
  { name: 'orders', kind: 'table', schema: 'public' },
])

/** A textarea wired the way the query workspace wires its editor. */
function Editor({ onRun, columns }: { onRun: () => void; columns: { current: SQLColumn[] | undefined; bump?: () => void } }) {
  const ref = useRef<HTMLTextAreaElement | null>(null)
  const [sql, setSql] = useState('')
  const [v, setV] = useState(0)
  columns.bump = () => setV((x) => x + 1)
  const complete = useCallback((t: string, c: number, explicit: boolean) =>
    completeSQL(t, c, catalog, () => columns.current, explicit), [columns])
  const ac = useCompletion({ ref, value: sql, setValue: setSql, complete, refresh: v })
  return (
    <>
      <textarea
        ref={ref}
        aria-label="SQL"
        value={sql}
        {...ac.editorProps}
        onChange={(e) => { ac.onTyped(); setSql(e.currentTarget.value) }}
        onKeyDown={(e) => {
          if (ac.onKeyDown(e)) return
          if (e.ctrlKey && e.key === 'Enter') onRun()
        }}
      />
      {ac.popup}
    </>
  )
}

function type(el: HTMLTextAreaElement, value: string) {
  fireEvent.change(el, { target: { value, selectionStart: value.length, selectionEnd: value.length } })
  el.selectionStart = el.selectionEnd = value.length
}

describe('editor autocomplete', () => {
  it('opens while typing, takes the highlighted entry on Enter, and leaves ⌘/Ctrl+Enter to run', async () => {
    const onRun = vi.fn()
    renderWithProviders(<Editor onRun={onRun} columns={{ current: [] }} />)
    const el = screen.getByLabelText('SQL') as HTMLTextAreaElement
    el.focus()
    type(el, 'sel')
    const list = await screen.findByRole('listbox')
    expect(list.textContent).toContain('SELECT')
    expect(el.getAttribute('aria-expanded')).toBe('true')

    fireEvent.keyDown(el, { key: 'Enter' })
    expect(el.value).toBe('select')
    expect(screen.queryByRole('listbox')).toBeNull()

    // With the list open, Ctrl+Enter still runs the statement.
    type(el, 'select * from ord')
    await screen.findByRole('listbox')
    fireEvent.keyDown(el, { key: 'Enter', ctrlKey: true })
    expect(onRun).toHaveBeenCalledTimes(1)
    expect(el.value).toBe('select * from ord')
  })

  it('moves with the arrows and closes on Escape without changing the text', async () => {
    renderWithProviders(<Editor onRun={() => {}} columns={{ current: [] }} />)
    const el = screen.getByLabelText('SQL') as HTMLTextAreaElement
    el.focus()
    type(el, 'SELECT * FROM orders WHERE true OR')
    // "OR" typed in full: no list to swallow the Enter for a new line.
    expect(screen.queryByRole('listbox')).toBeNull()
    type(el, 'SELECT * FROM orders WHERE true O')
    await screen.findByRole('listbox')
    const before = screen.getAllByRole('option').findIndex((o) => o.getAttribute('aria-selected') === 'true')
    fireEvent.keyDown(el, { key: 'ArrowDown' })
    const after = screen.getAllByRole('option').findIndex((o) => o.getAttribute('aria-selected') === 'true')
    expect(after).toBe(before + 1)
    fireEvent.keyDown(el, { key: 'Escape' })
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(el.value).toBe('SELECT * FROM orders WHERE true O')
  })

  it('shows a table\'s columns once they arrive', async () => {
    const columns: { current: SQLColumn[] | undefined; bump?: () => void } = { current: undefined }
    renderWithProviders(<Editor onRun={() => {}} columns={columns} />)
    const el = screen.getByLabelText('SQL') as HTMLTextAreaElement
    el.focus()
    type(el, 'SELECT o. FROM orders o')
    // Nothing to show yet: the columns are still on their way.
    el.selectionStart = el.selectionEnd = 'SELECT o.'.length
    fireEvent.keyDown(el, { key: ' ', ctrlKey: true })
    expect(screen.queryByRole('listbox')).toBeNull()

    columns.current = [{ name: 'id', type: 'bigint' }, { name: 'total', type: 'numeric' }]
    act(() => columns.bump!())
    const list = await screen.findByRole('listbox')
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['Cidbigint', 'Ctotalnumeric'])
    fireEvent.click(screen.getAllByRole('option')[1]!)
    expect(list.isConnected).toBe(false)
    expect(el.value).toBe('SELECT o.total FROM orders o')
  })
})
