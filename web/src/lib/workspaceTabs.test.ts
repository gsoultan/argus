import { beforeEach, describe, expect, it } from 'vitest'
import { blankTab, loadTabs, mayWrite, nameFromSQL, nextName, saveTabs, txAfter } from './workspaceTabs'

describe('tabs', () => {
  beforeEach(() => localStorage.clear())

  it('names new tabs with the lowest free number', () => {
    const a = blankTab([])
    const b = blankTab([a])
    expect([a.name, b.name]).toEqual(['Query 1', 'Query 2'])
    expect(nextName([b])).toBe('Query 1')
  })

  it('names a tab from its first comment', () => {
    expect(nameFromSQL('\n-- Unshipped orders\nselect 1')).toBe('Unshipped orders')
    expect(nameFromSQL('select 1 -- not a name')).toBeNull()
  })

  it('keeps text but never results, per database', () => {
    const t = { ...blankTab([], 'select 42'), maxRows: 10000 }
    saveTabs('db1', [t], t.key)
    const raw = localStorage.getItem('argus.workspace.tabs.db1')!
    expect(raw).toContain('select 42')
    expect(Object.keys(JSON.parse(raw).tabs[0]).sort()).toEqual(['maxRows', 'name', 'named', 'sql'])
    const back = loadTabs('db1')
    expect(back.tabs).toHaveLength(1)
    expect(back.tabs[0]!.sql).toBe('select 42')
    expect(back.tabs[0]!.maxRows).toBe(10000)
    expect(loadTabs('db2').tabs[0]!.sql).toBe('')
  })

  it('survives garbage in storage', () => {
    localStorage.setItem('argus.workspace.tabs.db1', '{nope')
    expect(loadTabs('db1').tabs).toHaveLength(1)
    localStorage.setItem('argus.workspace.tabs.db1', JSON.stringify({ tabs: [{ name: 1 }, { name: 'a', sql: 'x', maxRows: 7 }] }))
    const t = loadTabs('db1').tabs
    expect(t).toHaveLength(1)
    expect(t[0]!.maxRows).toBe(1000)
  })
})

describe('txAfter', () => {
  it('follows the person’s own transaction', () => {
    let open = false
    open = txAfter(open, 'BEGIN', true)
    expect(open).toBe(true)
    open = txAfter(open, 'update t set a = 1', false)
    expect(open).toBe(true)
    expect(txAfter(open, 'ROLLBACK TO SAVEPOINT s', true)).toBe(true)
    expect(txAfter(open, '-- done\nCOMMIT', true)).toBe(false)
    expect(txAfter(true, 'rollback', true)).toBe(false)
  })
})

describe('mayWrite', () => {
  it('tells a plain read from a write', () => {
    expect(mayWrite('select * from orders where note = \'update me\'')).toBe(false)
    expect(mayWrite('with x as (delete from t returning *) select * from x')).toBe(true)
    expect(mayWrite('select nextval(\'s\')')).toBe(true)
  })
})
