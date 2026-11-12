import { test, expect, type Page } from '@playwright/test'

/**
 * Settings › Cloud identities, against e2e/authstub.mjs: a list of
 * identities, one of them the default; a new one is tested before anything
 * is saved; one an account uses cannot be removed.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS

async function signIn(page: Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
}

const rowOf = (page: Page, name: string) => page.getByRole('row').filter({ has: page.getByText(name, { exact: true }) })

test('identities are listed, added and tested from Settings', async ({ page }) => {
  await signIn(page)
  await page.goto('/settings?section=cloud')

  const iam = rowOf(page, 'Northwind IAM user')
  await expect(iam.getByText('Default')).toBeVisible()
  await expect(iam.getByText('1 account · Northwind')).toBeVisible()
  await expect(rowOf(page, 'Main').getByText('No answer')).toBeVisible()
  if (SHOTS) {
    await page.screenshot({ path: `${SHOTS}/settings-list.png`, fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.screenshot({ path: `${SHOTS}/settings-list-phone.png`, fullPage: true })
    await page.setViewportSize({ width: 1280, height: 720 })
  }

  await page.getByRole('button', { name: 'Add identity' }).click()
  const drawer = page.getByRole('dialog')
  await drawer.getByLabel(/^Name/).fill('Payments IAM user')
  await expect(drawer.getByLabel('From where argus-control runs')).toBeDisabled()
  await drawer.getByLabel(/Access key id/).fill('AKIAIOSFODNN7EXAMPL2')
  await expect(drawer.getByText(/Still needed: secret access key/)).toBeVisible()
  await drawer.getByLabel(/Secret access key/).fill('wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY')
  await drawer.getByRole('button', { name: 'Test' }).click()
  await expect(drawer.getByText('AWS knows this identity')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/settings-add.png` })
  await drawer.getByRole('button', { name: 'Add' }).click()

  await expect(rowOf(page, 'Payments IAM user').getByText('Not saved')).toBeVisible()
  await expect(page.getByText('unsaved').first()).toBeVisible()
})

test('an identity an account uses cannot be removed', async ({ page }) => {
  await signIn(page)
  await page.goto('/settings?section=cloud')
  await expect(rowOf(page, 'Northwind IAM user').getByText('1 account · Northwind')).toBeVisible()
  await rowOf(page, 'Northwind IAM user').getByRole('button', { name: 'Remove' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText('Northwind IAM user is still used by 1 account')).toBeVisible()
  await expect(dialog.getByRole('button', { name: 'Remove' })).toHaveCount(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/settings-remove-blocked.png` })
})

test('a new account picks which identity it connects as', async ({ page }) => {
  await signIn(page)
  await page.goto('/cloud/connect')
  await expect(page.getByText('Argus connects as')).toBeVisible()
  await expect(page.getByRole('radio', { name: /Main/ })).toBeDisabled()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/connect-picker.png`, fullPage: true })
  await page.goto('/cloud')
  await expect(page.getByText(/Connects as Northwind IAM user/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Change identity' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/cloud-accounts.png`, fullPage: true })
})
