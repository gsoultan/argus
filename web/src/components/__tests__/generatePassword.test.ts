import { describe, expect, it } from 'vitest'
import { generatePassword } from '~/components/Setup'

describe('generatePassword', () => {
  it('is long enough for the control plane and free of look-alike characters', () => {
    for (let i = 0; i < 200; i++) {
      const pw = generatePassword()
      expect(pw).toMatch(/^[a-km-zA-HJ-NP-Z2-9]{6}(-[a-km-zA-HJ-NP-Z2-9]{6}){3}$/)
      expect(pw.length).toBeGreaterThanOrEqual(12)
    }
  })

  it('does not repeat itself', () => {
    const seen = new Set(Array.from({ length: 500 }, generatePassword))
    expect(seen.size).toBe(500)
  })
})
