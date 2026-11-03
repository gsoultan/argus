import { test, expect, type Page } from '@playwright/test'

/**
 * Light and dark: follows the operating system until the person picks one,
 * and remembers the pick in this browser.
 */
test.use({ serviceWorkers: 'block' })

const scheme = (page: Page) =>
  page.evaluate(() => document.documentElement.getAttribute('data-mantine-color-scheme'))

test('follows the operating system by default', async ({ browser }) => {
  for (const colorScheme of ['light', 'dark'] as const) {
    const ctx = await browser.newContext({ colorScheme, serviceWorkers: 'block' })
    const page = await ctx.newPage()
    await page.goto('/assets')
    await expect(page.getByRole('heading', { name: 'Servers', exact: true })).toBeVisible()
    expect(await scheme(page)).toBe(colorScheme)
    await ctx.close()
  }
})

test('a picked scheme beats the system and survives a reload', async ({ browser }) => {
  const ctx = await browser.newContext({ colorScheme: 'light', serviceWorkers: 'block' })
  const page = await ctx.newPage()
  await page.goto('/assets')
  await page.getByRole('button', { name: /^Theme:/ }).click()
  await page.getByRole('menuitem', { name: 'Dark' }).click()
  expect(await scheme(page)).toBe('dark')

  // Set before the bundle runs, by public/color-scheme.js: a dark console must
  // not paint white first. Checked at DOMContentLoaded, before React mounts.
  await page.reload({ waitUntil: 'domcontentloaded' })
  expect(await scheme(page)).toBe('dark')
  await expect(page.getByRole('button', { name: 'Theme: Dark' })).toBeVisible()
  await ctx.close()
})

/**
 * The bug that started this: commands in the session timeline were drawn in a
 * near-white shade left over from the old dark console, 1.2:1 on a white card.
 * Measured on the rendered pixels in both schemes.
 */
for (const colorScheme of ['light', 'dark'] as const) {
  test(`command timeline is readable in ${colorScheme}`, async ({ browser }) => {
    const ctx = await browser.newContext({ colorScheme, serviceWorkers: 'block' })
    const page = await ctx.newPage()
    await page.goto('/sessions')
    const rows = page.locator('tbody tr[role="link"]')
    await expect(rows.first()).toBeVisible()
    // The first session with a command timeline in the seeded fixture. Wait
    // for the detail page itself, then ask, so a slow render is not mistaken
    // for a session without a timeline.
    let found = false
    for (let i = 0; i < 8 && !found; i++) {
      await page.goto('/sessions')
      await rows.nth(i).click()
      await page.waitForURL(/\/sessions\/[^/?]+$/)
      await expect(page.getByText('Recording integrity')).toBeVisible()
      found = (await page.getByText('Command timeline').count()) > 0
    }
    expect(found, 'no session in the fixture has a command timeline').toBe(true)

    const ratios = await page.evaluate(() => {
      const parse = (c: string) => (c.match(/[\d.]+/g) ?? []).map(Number)
      const lum = ([r, g, b]: number[]) => {
        const f = (v: number) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4 }
        return 0.2126 * f(r!) + 0.7152 * f(g!) + 0.0722 * f(b!)
      }
      const bgOf = (el: Element | null): number[] => {
        for (; el; el = el.parentElement) {
          const c = parse(getComputedStyle(el).backgroundColor)
          if (c.length === 3 || (c.length === 4 && c[3]! > 0.5)) return c
        }
        return [255, 255, 255]
      }
      const heading = [...document.querySelectorAll('*')].find((e) => e.textContent === 'Command timeline')
      const card = heading?.closest('.mantine-Card-root')
      const cmds = [...(card?.querySelectorAll('.mantine-Text-root[style*="overflow-wrap"]') ?? [])]
      return cmds.slice(0, 5).map((el) => {
        const a = lum(parse(getComputedStyle(el).color)), b = lum(bgOf(el))
        return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
      })
    })
    expect(ratios.length, 'found no command lines to measure').toBeGreaterThan(0)
    for (const r of ratios) expect(r).toBeGreaterThanOrEqual(4.5)
    await ctx.close()
  })
}
