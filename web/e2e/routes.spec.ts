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
    await errs.assertClean()
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
  await errs.assertClean()
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

/**
 * Row striping is a wash, not a fill.
 *
 * `--table-striped-color` was declared at `:root`, which looks like it sets the
 * stripe and does not: Mantine sets that variable on the table element itself,
 * so the rule was overridden and every other row was painted solid `slate.6` —
 * a mid blue-grey banding every list page, while a stylesheet claimed 1.4%
 * white. It is a `stripedColor` prop on the theme now.
 *
 * Asserted on the rendered pixel rather than the variable, because the variable
 * being right is exactly what was already believed.
 */
test('striped rows are a wash rather than a fill', async ({ page }) => {
  await page.goto('/sessions')
  await expect(page.getByRole('heading', { name: 'Sessions', exact: true })).toBeVisible()

  const alpha = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('tbody tr')]
    const striped = rows
      .map((r) => getComputedStyle(r).backgroundColor)
      .find((bg) => bg !== 'rgba(0, 0, 0, 0)' && bg !== 'transparent')
    if (!striped) return null
    const parts = striped.match(/[\d.]+/g) ?? []
    // rgb() with no alpha is fully opaque, which is the failure this catches.
    return parts.length === 4 ? Number(parts[3]) : 1
  })

  expect(alpha, 'no striped row found — is striped="even" still set?').not.toBeNull()
  expect(
    alpha,
    `A striped row is drawn at alpha ${alpha}. Anything approaching opaque means `
      + 'the stripe is a solid colour rather than a wash, which bands the whole '
      + 'table — see Table.defaultProps.stripedColor in src/theme.',
  ).toBeLessThan(0.06)
})
