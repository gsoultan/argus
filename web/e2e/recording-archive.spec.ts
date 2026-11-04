import { test, expect } from '@playwright/test'

/**
 * The recording archive in Settings › Recordings, against e2e/authstub.mjs.
 *
 * What it guards: the archive age is a choice the operator can see and make,
 * and the storage classes offered are only those a recording can be replayed
 * from at once. Glacier and Deep Archive would leave a recording the console
 * lists and cannot open.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

test('the archive age and storage class are set from Settings', async ({ page }) => {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await page.goto('/settings?section=storage')

  const after = page.getByRole('combobox', { name: 'Archive recordings older than' })
  await expect(after).toHaveValue('30 days (default)')
  // No bucket, no storage class: a directory has none.
  await expect(page.getByRole('combobox', { name: 'Archive storage class' })).toBeHidden()

  await after.click()
  await page.getByRole('option', { name: '90 days' }).click()
  await expect(after).toHaveValue('90 days')
  await expect(page.getByText('unsaved').first()).toBeVisible()

  await page.getByRole('switch', { name: 'Copy recordings to a bucket' }).check()
  const cls = page.getByRole('combobox', { name: 'Archive storage class' })
  await expect(cls).toHaveValue("Bucket's default")
  await cls.click()
  await expect(page.getByRole('option', { name: 'Glacier Instant Retrieval' })).toBeVisible()
  await expect(page.getByRole('option', { name: /deep archive/i })).toHaveCount(0)
  await expect(page.getByRole('option', { name: /^glacier$/i })).toHaveCount(0)
})
