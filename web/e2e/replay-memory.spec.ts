import { test, expect } from '@playwright/test'
import { firstSessionPath, heapMB, makeCast, watchErrors } from './helpers'

/**
 * The property the replay rewrite exists to guarantee: the main thread does
 * not hold the recording. The frames live in the worker and the terminal's
 * buffer is capped, so the heap must not track the size of what is played.
 *
 * A generated recording rather than a fixture on disk: ~8 MB is enough to
 * make the old player's failure mode (a heap that grew with every frame, a
 * render that re-parsed the whole scrollback) unmistakable, and small enough
 * to decode in a few seconds in CI.
 */
test('terminal replay holds a flat main-thread heap on a large recording', async ({ page }) => {
  test.slow()
  const cast = makeCast(8 * 1024 * 1024)
  await page.route('**/e2e.cast', (route) =>
    route.fulfill({ status: 200, contentType: 'text/plain', body: cast }),
  )
  const errs = watchErrors(page)

  const path = await firstSessionPath(page, /SSH/)
  await page.goto(`${path}?cast=/e2e.cast`)
  await expect(page.getByText(/decoded in \d+ms/)).toBeVisible({ timeout: 60_000 })

  const frames = Number((await page.getByText(/([\d,]+) frames/).innerText()).replace(/\D/g, ''))
  expect(frames).toBeGreaterThan(50_000)

  const afterDecode = await heapMB(page)

  // Play at 8x for a while, then scrub back and forth.
  await page.getByRole('combobox', { name: 'Playback speed' }).click()
  await page.getByRole('option', { name: '8x' }).click()
  await page.getByRole('button', { name: 'Play' }).click()
  await page.waitForTimeout(8_000)
  const duringPlayback = await heapMB(page)
  await page.getByRole('button', { name: 'Pause' }).click()

  for (let i = 0; i < 4; i++) await page.keyboard.press('Shift+ArrowRight')
  for (let i = 0; i < 6; i++) await page.keyboard.press('Shift+ArrowLeft')
  await page.waitForTimeout(1_500)
  const afterSeeks = await heapMB(page)

  // The old player would have grown by hundreds of MB here.
  const ceiling = afterDecode * 1.35 + 24
  expect(duringPlayback, `heap grew during playback: ${afterDecode} -> ${duringPlayback} MB`).toBeLessThan(ceiling)
  expect(afterSeeks, `heap grew after seeks: ${afterDecode} -> ${afterSeeks} MB`).toBeLessThan(ceiling)

  // And the terminal buffer is bounded, not the whole scrollback.
  const rows = await page.locator('.xterm-rows > div').count()
  expect(rows).toBeLessThan(80)

  errs.assertClean()
})
