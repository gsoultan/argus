import { describe, expect, it } from 'vitest'
import { itemLabel, summary, type J } from '~/lib/jsonSummary'

describe('JSON summaries', () => {
  it('names a list of objects by their items', () => {
    const rules: J[] = [{ desc: 'Approval for staff', name: 'IMPC_Staff' }, { name: 'IMPC_Intern' }, { id: 7 }, { title: 'Fourth' }]
    expect(itemLabel(rules[0]!)).toBe('IMPC_Staff')
    expect(summary(rules)).toBe('4 items: IMPC_Staff, IMPC_Intern, 7, …')
  })

  it('shows an object as its keys and short values', () => {
    expect(summary({ notify: true, levels: 3, owner: { name: 'HR' }, tags: ['a', 'b'] })).toBe('notify: true · levels: 3 · owner: {1} · tags: [2]')
    expect(summary([1, 2, 3])).toBe('[1,2,3]')
    expect(summary(null)).toBe('null')
  })
})
