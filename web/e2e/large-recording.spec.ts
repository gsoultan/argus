import { test, expect } from '@playwright/test'

/**
 * A recording far larger than a terminal session usually is, against
 * e2e/authstub.mjs.
 *
 * What it guards: the player used to download a 257 MB recording unasked and
 * sit on "Decoding recording…" until the tab gave up. Now it says how large the
 * recording is and downloads it only when asked.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

test('a huge recording is downloaded only when asked, then plays', async ({ page, request }) => {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()

  const before = (await (await request.get('/__stub/huge-fetches')).json()).count as number
  await page.goto('/sessions/s-huge-transfer')
  await expect(page.getByText(/^This recording is [\d.]+ MB$/)).toBeVisible()
  await expect(page.getByText('Decoding recording…')).toHaveCount(0)
  // Not loaded is not "no commands".
  await expect(page.getByText('Commands appear once the recording is loaded.')).toBeVisible()
  await expect(page.getByText('No commands detected.')).toHaveCount(0)
  expect((await (await request.get('/__stub/huge-fetches')).json()).count).toBe(before)

  await page.getByRole('button', { name: /download and replay/i }).click()
  await expect(page.getByText('asciicast v2')).toBeVisible()
  expect((await (await request.get('/__stub/huge-fetches')).json()).count).toBe(before + 1)
})
