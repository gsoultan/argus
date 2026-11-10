import { test, expect } from '@playwright/test'

/**
 * Settings › Cloud identities, against e2e/authstub.mjs: a control plane off
 * AWS says it has no identity, takes an access key, and says what AWS
 * answered before anything is saved.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

test('an access key is entered and tested from Settings', async ({ page }) => {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await page.goto('/settings?section=cloud')

  await expect(page.getByText('No identity', { exact: true })).toBeVisible()
  await expect(page.getByText(/no EC2 IMDS role found/)).toBeVisible()

  await page.getByLabel('An access key').check()
  await page.getByLabel(/Access key id/).fill('AKIAIOSFODNN7EXAMPLE')
  await expect(page.getByText(/Still needed: secret access key/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Test' })).toBeDisabled()
  await page.getByLabel(/Secret access key/).fill('wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY')
  await page.getByRole('button', { name: 'Test' }).click()
  await expect(page.getByText('AWS knows this identity')).toBeVisible()
  await expect(page.getByText('arn:aws:iam::123456789012:user/argus-control').first()).toBeVisible()
  await expect(page.getByText('unsaved').first()).toBeVisible()
})
