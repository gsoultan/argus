import { describe, expect, it } from 'vitest'
import { defaultHealthCheck, groupSizeOf, healthCheckOf, healthProblem, healthWords, LB_NAME, type CloudResource } from './cloud'

describe('health checks', () => {
  it('say what they check in the control plane words', () => {
    expect(healthWords(defaultHealthCheck('HTTP'))).toBe('HTTP / answering 200, every 30s, healthy after 5, unhealthy after 2')
    expect(healthWords({ ...defaultHealthCheck('TCP'), port: '9000' })).toBe('TCP on port 9000, every 30s, healthy after 5, unhealthy after 2')
  })
  it('read one from a target group, or its defaults', () => {
    const r = { kind: 'target-group', id: 'tg', name: 'tg', region: 'r', details: { protocol: 'TCP' } } as CloudResource
    expect(healthCheckOf(r)).toEqual(defaultHealthCheck('TCP'))
    r.details = { protocol: 'HTTP', healthCheckProtocol: 'HTTP', healthCheckPath: '/ready', healthCheckInterval: '10', healthyThreshold: '2', unhealthyThreshold: '3', matcher: '200-299' }
    expect(healthWords(healthCheckOf(r))).toBe('HTTP /ready answering 200-299, every 10s, healthy after 2, unhealthy after 3')
  })
  it('refuse what AWS refuses', () => {
    const h = defaultHealthCheck('HTTP')
    expect(healthProblem(h)).toBeNull()
    expect(healthProblem({ ...h, path: 'health' })).toMatch(/starts with \//)
    expect(healthProblem({ ...h, matcher: 'ok' })).toMatch(/200-299/)
    expect(healthProblem({ ...h, port: '70000' })).toMatch(/1 to 65535/)
    expect(healthProblem({ ...h, intervalSeconds: 1 })).toMatch(/5 to 300/)
    expect(healthProblem({ ...h, healthy: 11 })).toMatch(/2 to 10/)
  })
})

it('names and group sizes', () => {
  expect(LB_NAME.test('pay-api')).toBe(true)
  expect(LB_NAME.test('-pay')).toBe(false)
  expect(LB_NAME.test('a'.repeat(33))).toBe(false)
  expect(groupSizeOf({ kind: 'scaling-group', id: 'g', name: 'g', region: 'r', details: { min: '1', max: '4', desired: '2', running: '2' } } as CloudResource))
    .toEqual({ min: 1, max: 4, desired: 2, running: 2 })
})
