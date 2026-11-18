import { describe, expect, it } from 'vitest'
import { grantedLess, noticeHeading, noticeShort, warningsIn, type PGNotice } from '~/lib/pgNotices'

const nothing: PGNotice = { severity: 'WARNING', code: '01007', message: 'no privileges were granted for "etl"' }
const notAll: PGNotice = { severity: 'WARNING', code: '01007', message: 'not all privileges were granted for "job"' }
const info: PGNotice = { severity: 'NOTICE', code: '42P07', message: 'relation "t" already exists, skipping' }

describe('pgNotices', () => {
  it('names a GRANT that granted nothing, rather than calling it a success', () => {
    expect(grantedLess([nothing])).toBe(true)
    expect(noticeHeading([nothing, nothing])).toBe('Postgres ran this, but granted nothing')
    expect(noticeShort([nothing])).toBe('nothing granted')
  })
  it('tells a partial grant apart', () => {
    expect(noticeHeading([nothing, notAll])).toBe('Postgres ran this, but did not grant everything it names')
    expect(noticeShort([notAll])).toBe('not all granted')
  })
  it('keeps a notice as information, not a warning', () => {
    expect(warningsIn([info])).toEqual([])
    expect(noticeShort([info])).toBe('1 notice')
    expect(noticeHeading([info])).toBe('Postgres ran this, and said')
  })
  it('says nothing when Postgres said nothing', () => {
    expect(noticeHeading(undefined)).toBeNull()
    expect(noticeShort([])).toBeNull()
  })
})
