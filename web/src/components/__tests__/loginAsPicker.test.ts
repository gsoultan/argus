import { describe, expect, it } from 'vitest'
import { accountChoices, defaultChoice } from '~/components/LoginAsPicker'
import type { RequestableHost } from '~/types/domain'

const host = (hostname: string, accounts: [string, boolean][], known = true): RequestableHost => ({
  hostname, known, agentReported: true,
  accounts: accounts.map(([name, onServer]) => ({ name, onServer, elevated: name === 'root' })),
})

describe('accountChoices', () => {
  it('offers only the server’s own accounts, never a fixed list', () => {
    const got = accountChoices([host('web-01.corp', [['gembit', true], ['root', true]])])
    expect(got.map((c) => c.name)).toEqual(['gembit', 'root'])
    expect(got.every((c) => !c.unavailable)).toBe(true)
    expect(got.find((c) => c.name === 'root')?.elevated).toBe(true)
  })

  it('names an account the agent did not find, and why it cannot be picked', () => {
    const [deploy] = accountChoices([host('web-01.corp', [['deploy', false]])])
    expect(deploy?.unavailable).toMatch(/Not found on web-01/)
  })

  it('needs an account on every picked server', () => {
    const got = accountChoices([
      host('web-01.corp', [['ops', true], ['ubuntu', true]]),
      host('web-02.corp', [['ops', true]]),
      host('elsewhere', [], false),
    ])
    expect(got.find((c) => c.name === 'ops')?.unavailable).toBe('')
    expect(got.find((c) => c.name === 'ubuntu')?.unavailable).toMatch(/Not a login account on web-02/)
  })
})

describe('defaultChoice', () => {
  const choices = accountChoices([host('web-01', [['root', true], ['ops', true], ['deploy', false]])])
  it('keeps a pick that is still available', () => expect(defaultChoice(choices, 'root')).toBe('root'))
  it('moves off an account the server lacks, to an everyday one', () =>
    expect(defaultChoice(choices, 'deploy')).toBe('ops'))
  it('leaves nothing picked when nothing can be', () =>
    expect(defaultChoice(accountChoices([host('a', [['deploy', false]])]), 'deploy')).toBe(''))
})
