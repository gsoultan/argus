import { describe, expect, it } from 'vitest'
import { pageMoves, pageWords, parseKeys, superStreamKeys, superStreamPlan, superStreamProblem } from '~/lib/rabbitStreams'

const page = (lo: number, hi: number, first = 0, last = 100) => ({
  first, last, messages: Array.from({ length: hi - lo + 1 }, (_, i) => ({ offset: lo + i, body: '', encoding: 'string' as const, size: 0 })),
})

describe('stream pages', () => {
  it('moves older and newer, and stops at the ends', () => {
    expect(pageMoves(page(40, 59), 20)).toEqual({ older: 20, newer: 60 })
    expect(pageMoves(page(5, 24), 20)).toEqual({ older: 0, newer: 25 })
    expect(pageMoves(page(0, 19), 20).older).toBeNull()
    expect(pageMoves(page(81, 100), 20).newer).toBeNull()
    expect(pageMoves({ first: 0, last: 0, messages: [], empty: true }, 20)).toEqual({ older: null, newer: null })
  })
  it('says where the page is', () => {
    expect(pageWords(page(48211, 48230, 0, 53083))).toBe('48,211 to 48,230 of 0 to 53,083')
    expect(pageWords({ first: 0, last: 0, messages: [], empty: true })).toBe('The stream holds no messages.')
    expect(pageWords({ first: 3, last: 9, messages: [] })).toBe('No messages there. The stream holds offsets 3 to 9.')
  })
})

describe('super streams', () => {
  it('names the partitions', () => {
    expect(superStreamKeys('hash', 3, [])).toEqual(['0', '1', '2'])
    expect(superStreamKeys('keys', 3, parseKeys('eu, us\napac'))).toEqual(['eu', 'us', 'apac'])
  })
  it('checks as the agent does', () => {
    expect(superStreamProblem('payments', ['0', '1'], [])).toBeNull()
    expect(superStreamProblem('amq.x', ['0'], [])).toMatch(/RabbitMQ's own/)
    expect(superStreamProblem('p', [], [])).toMatch(/1 to 64/)
    expect(superStreamProblem('p', ['eu', 'eu'], [])).toMatch(/twice/)
    expect(superStreamProblem('p', ['e u'], [])).toMatch(/not a routing key/)
    expect(superStreamProblem('p', ['0'], ['p'])).toMatch(/exchange named p/)
    expect(superStreamProblem('p', ['0'], ['p-0'])).toMatch(/queue named p-0/)
  })
  it('previews what runs', () => {
    const plan = superStreamPlan('payments', ['0', '1', '2', '3'], 30, 0).join('\n')
    expect(plan).toContain('PUT exchange payments  direct')
    expect(plan).toContain('PUT queue payments-0 … payments-3')
    expect(plan).toContain('x-max-age 30D')
    expect(plan).toContain('… and 3 more')
  })
})
