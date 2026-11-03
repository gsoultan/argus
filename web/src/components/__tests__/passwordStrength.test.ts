import { passwordAcceptable } from '~/lib/passwordStrength'

// The same cases as internal/auth/strength_test.go: the console must not
// tick every box for a password the control plane then refuses.
describe('passwordAcceptable', () => {
  const ctx = { email: 'dana.lee@example.com', name: 'Dana Lee' }
  it.each([
    ['short1A!', false],
    ['Password2024!', false],
    ['P@ssw0rd!!!!xY', false],
    ['Dana.Lee#2026x', false],
    ['Example!2026xyZ', false],
    ['aaaaaaaaaaaaaa', false],
    ['Abcdefg!23456', false],
    ['Qwerty!9Lm#zz', false],
    ['lowercaseonlyhere', false],
    ['horse cable violet river', true],
    ['Tr4in-Coffee-Nap', true],
    [' Tr4in-Coffee-Nap', false],
    ['the dragon sleeps under seven hills', true],
  ])('%s → %s', (pw, ok) => {
    expect(passwordAcceptable(pw, ctx)).toBe(ok)
  })
})
