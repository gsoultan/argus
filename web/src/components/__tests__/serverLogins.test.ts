import { describe, expect, it } from 'vitest'
import { loginStatus } from '~/components/server/ServerLogins'
import type { ServerLogin } from '~/types/domain'

const login = (over: Partial<ServerLogin>): ServerLogin => ({
  name: 'deploy', state: 'active', createdBy: 'a@x', createdAt: '2026-11-01T00:00:00Z',
  changedBy: 'a@x', changedAt: '2026-11-01T00:00:00Z', agent: '', ...over,
})

// What was asked is never shown as done until the agent says it is.
describe('loginStatus', () => {
  it('waits for the agent before saying ready', () => {
    expect(loginStatus(login({})).label).toBe('Waiting for the agent')
    expect(loginStatus(login({ agent: 'ready' })).label).toBe('Ready')
  })
  it('says locking until the agent has locked it', () => {
    expect(loginStatus(login({ state: 'locked', agent: 'ready' })).label).toBe('Locking')
    expect(loginStatus(login({ state: 'locked', agent: 'locked' })).label).toBe('Locked')
  })
  it('says a name the server already had is taken, with the agent’s reason', () => {
    const s = loginStatus(login({ agent: 'taken', agentDetail: 'not ours' }))
    expect(s.label).toBe('Name taken')
    expect(s.detail).toBe('not ours')
  })
  it('says deleting until the agent has deleted it', () => {
    expect(loginStatus(login({ state: 'delete', agent: 'locked' })).label).toBe('Deleting')
    expect(loginStatus(login({ state: 'deleted', agent: 'deleted' })).label).toBe('Deleted')
  })
})
