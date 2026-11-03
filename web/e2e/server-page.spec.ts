import { test, expect, type Page } from '@playwright/test'
import { watchErrors } from './helpers'

/**
 * A server's page is five tabs, and every one has to work on a phone as well
 * as on a desk: no console errors, and nothing wider than the screen, which on
 * a phone means a sideways scroll on the whole page.
 */

// Each tab is its own navigation, so no service worker may take the page:
// one that did would hide failed assets from the listeners (see helpers.ts).
test.use({ serviceWorkers: 'block' })

const TABS = ['overview', 'access', 'sessions', 'apps', 'settings'] as const

/** The first server's page, reached the way an admin reaches it. */
async function serverPath(page: Page): Promise<string> {
  await page.goto('/assets')
  await page.locator('a[aria-label^="Manage "]').first().click()
  await page.waitForURL(/\/assets\/[^/?]+/)
  return new URL(page.url()).pathname
}

for (const [label, viewport] of [
  ['a desk', { width: 1440, height: 900 }],
  ['a phone', { width: 390, height: 844 }],
] as const) {
  test(`every tab of a server's page renders cleanly on ${label}`, async ({ page }) => {
    await page.setViewportSize(viewport)
    const path = await serverPath(page)
    for (const tab of TABS) {
      const errs = watchErrors(page)
      await page.goto(tab === 'overview' ? path : `${path}?tab=${tab}`)
      await expect(page.getByRole('tab', { selected: true })).toHaveText(new RegExp(
        { overview: 'Overview', access: 'Access', sessions: 'Sessions', apps: 'What runs here', settings: 'Settings' }[tab],
      ))
      await page.waitForLoadState('networkidle')
      await errs.assertClean()
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
      expect(overflow, `${tab} is ${overflow}px wider than the screen`).toBeLessThanOrEqual(0)
    }
  })
}

test('the tab is kept in the address', async ({ page }) => {
  const path = await serverPath(page)
  await page.getByRole('tab', { name: /^Access/ }).click()
  await expect(page).toHaveURL(new RegExp(`${path}\\?tab=access$`))
  await page.reload()
  await expect(page.getByRole('tab', { selected: true })).toHaveText(/Access/)
  await page.getByRole('tab', { name: /^Overview/ }).click()
  await expect(page).toHaveURL(new RegExp(`${path}$`))
})
