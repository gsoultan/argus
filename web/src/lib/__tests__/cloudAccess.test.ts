import { describe, expect, it } from 'vitest'
import type { CloudAccount } from '~/lib/cloud'
import { accessNeedsApproval, consoleRoleMissing, keysBlock, minutesText } from '~/lib/cloudAccess'

describe('console access', () => {
  it('needs a second person by environment and level, as the control plane says', () => {
    expect(accessNeedsApproval('production', 'read')).toBe(true)
    expect(accessNeedsApproval('staging', 'read')).toBe(false)
    expect(accessNeedsApproval('staging', 'operate')).toBe(true)
    expect(accessNeedsApproval('development', 'operate')).toBe(false)
  })
  it('knows a stack without the console role', () => {
    const a = { manageMissing: ['role:ArgusConsoleOperate'] } as unknown as CloudAccount
    expect(consoleRoleMissing(a, 'operate')).toBe(true)
    expect(consoleRoleMissing(a, 'read')).toBe(false)
  })
  it('writes keys the CLI reads', () => {
    expect(keysBlock({ accessKeyId: 'ASIA1', secretAccessKey: 's', sessionToken: 't', expires: '', region: 'ap-southeast-3' }))
      .toBe("export AWS_ACCESS_KEY_ID='ASIA1'\nexport AWS_SECRET_ACCESS_KEY='s'\nexport AWS_SESSION_TOKEN='t'\nexport AWS_REGION='ap-southeast-3'")
    expect(minutesText(30)).toBe('30 minutes')
    expect(minutesText(480)).toBe('8 hours')
  })
})
