import { test, expect } from '@playwright/test'
import { watchErrors } from './helpers'

const ROUTES: Array<[string, string]> = [
  ['/', 'Overview'],
  ['/sessions', 'Sessions'],
  ['/assets', 'Assets'],
  ['/requests', 'Access requests'],
  ['/audit', 'Audit log'],
  ['/users', 'Users & roles'],
  ['/settings', 'Settings'],
  ['/coverage', 'Coverage'],
  ['/connect', 'Connect'],
]

for (const [path, heading] of ROUTES) {
  test(`${path} renders with no errors`, async ({ page }) => {
    const errs = watchErrors(page)
    await page.goto(path)
    await expect(page.getByRole('heading', { name: heading, exact: true })).toBeVisible()
    // Give lazy chunks and workers a moment to settle before judging.
    await page.waitForLoadState('networkidle')
    errs.assertClean()
  })
}

test('the designed fonts actually load', async ({ page }) => {
  // The theme named Inter for months while nothing loaded it. This is the
  // check that would have noticed.
  await page.goto('/')
  // Force both families to be used, then wait for the loads they trigger.
  await page.evaluate(async () => {
    await Promise.all([
      document.fonts.load('13px "Inter Variable"', 'Argus'),
      document.fonts.load('12px "JetBrains Mono Variable"', 'SHA256:'),
    ])
  })
  const loaded = await page.evaluate(() =>
    [...document.fonts].filter((f) => f.status === 'loaded').map((f) => f.family),
  )
  expect(loaded, `loaded faces: ${loaded.join(', ') || 'none'}`).toContain('Inter Variable')
  expect(loaded, `loaded faces: ${loaded.join(', ') || 'none'}`).toContain('JetBrains Mono Variable')
  // And the page is rendered with them, not merely able to be.
  const body = await page.evaluate(() => getComputedStyle(document.body).fontFamily)
  expect(body).toContain('Inter Variable')
})

test('the audit chain verifies in the browser', async ({ page }) => {
  const errs = watchErrors(page)
  await page.goto('/audit')
  await expect(page.getByText(/Chain built/)).toBeVisible()
  await page.getByRole('button', { name: 'Verify chain' }).click()
  await expect(page.getByText('Chain intact')).toBeVisible()
  // WASM, not WebCrypto: hundreds of links in single-digit milliseconds.
  // textContent, not innerText: badges render uppercase via CSS.
  const ms = (await page.getByText(/\d+ms off main thread/i).textContent()) ?? ''
  expect(Number(ms.match(/(\d+)ms/i)![1])).toBeLessThan(200)
  errs.assertClean()
})

test('no status badge is ever truncated', async ({ page }) => {
  for (const path of ['/', '/sessions', '/assets']) {
    await page.goto(path)
    await page.waitForLoadState('networkidle')
    const clipped = await page.evaluate(() =>
      [...document.querySelectorAll('.mantine-Badge-root')].filter((b) => {
        const l = (b.querySelector('[class*=label]') as HTMLElement) ?? (b as HTMLElement)
        return l.scrollWidth > l.clientWidth + 1
      }).map((b) => (b as HTMLElement).innerText),
    )
    expect(clipped, `${path}: truncated badges`).toEqual([])
  }
})
