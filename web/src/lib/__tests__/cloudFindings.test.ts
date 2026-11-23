import { describe, expect, it } from 'vitest'
import type { CloudChange, CloudRule } from '~/lib/cloud'
import { ruleChangeFor, type CloudFinding } from '~/lib/cloudFindings'

const SSH: CloudRule = { inbound: true, protocol: 'tcp', fromPort: 22, toPort: 22, source: '0.0.0.0/0' }

const finding: CloudFinding = {
  accountId: 'ca1', accountName: 'prod', environment: 'production', id: 'open:sg-web:tcp/22-22/0.0.0.0/0', check: 'rule-open', group: 'risk',
  severity: 'high', source: 'cloud', kind: 'firewall', resourceId: 'sg-web', name: 'sg-web', region: 'us-east-1', title: 'SSH open', why: '',
  fix: 'firewall-rules', rule: SSH, basis: 'b',
}

const change = (c: Partial<CloudChange>): CloudChange => ({
  id: 'c1', accountId: 'ca1', accountName: 'prod', region: 'us-east-1', action: 'firewall-rules', level: 'change', environment: 'production',
  resourceId: 'sg-web', resourceName: 'sg-web', spec: { add: [], remove: [{ ...SSH, description: 'old' }] }, plan: {}, planHash: '', summary: 'Change rules',
  reason: '', state: 'awaiting_approval', requestedBy: 'a', requestedAt: '2026-11-23T04:00:00Z', decidedBy: null, decidedAt: null, appliedAt: null,
  requestId: '', result: {}, error: '', ...c,
})

describe('a firewall finding already being fixed', () => {
  it('is found while its removal waits for approval or is being made', () => {
    expect(ruleChangeFor(finding, [change({})])?.id).toBe('c1')
    expect(ruleChangeFor(finding, [change({ state: 'applying' })])?.id).toBe('c1')
    // A Google firewall rule whose last allow is taken away is deleted.
    expect(ruleChangeFor(finding, [change({ action: 'firewall-delete', spec: {} })])?.id).toBe('c1')
  })

  it('is found once made, until the group is read again', () => {
    const made = change({ state: 'applied', appliedAt: '2026-11-23T04:01:00.5Z' })
    expect(ruleChangeFor(finding, [made], '2026-11-23T04:00:30Z')?.id).toBe('c1')
    // Read since, and still listed: the fix is offered again.
    expect(ruleChangeFor(finding, [made], '2026-11-23T04:02:00Z')).toBeUndefined()
  })

  it('is not another rule, group or account, nor a change that ended without making it', () => {
    expect(ruleChangeFor(finding, [change({ spec: { remove: [{ ...SSH, fromPort: 23, toPort: 23 }] } })])).toBeUndefined()
    expect(ruleChangeFor(finding, [change({ resourceId: 'sg-api' })])).toBeUndefined()
    expect(ruleChangeFor(finding, [change({ accountId: 'ca2' })])).toBeUndefined()
    for (const state of ['failed', 'declined', 'cancelled', 'lapsed'] as const) expect(ruleChangeFor(finding, [change({ state })])).toBeUndefined()
    expect(ruleChangeFor({ ...finding, fix: 'stop', rule: undefined }, [change({})])).toBeUndefined()
    expect(ruleChangeFor(finding, undefined)).toBeUndefined()
  })
})
