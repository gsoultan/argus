import { test, expect } from '@playwright/test'

/**
 * Setting up email and Telegram alerts from Settings, against e2e/authstub.mjs.
 *
 * The bug this exists for: "Set up Email" only picked the Email tab, which was
 * already the open one, and the mail server fields stayed hidden behind a
 * switch that was off. The button did nothing anyone could see, and there was
 * no form to fill in.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

async function openAlerts(page: import('@playwright/test').Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await page.goto('/settings?section=alerts')
  await expect(page.getByText('Alert channels')).toBeVisible()
}

test('"Set up Email" opens the mail server form', async ({ page }) => {
  await openAlerts(page)
  await expect(page.getByRole('textbox', { name: 'SMTP server' })).toBeHidden()

  await page.getByRole('button', { name: 'Set up Email' }).click()

  const host = page.getByRole('textbox', { name: 'SMTP server' })
  await expect(host).toBeVisible()
  await expect(host).toBeFocused()
  await expect(page.getByRole('switch', { name: 'Send email' })).toBeChecked()
  await expect(page.getByLabel('From')).toBeVisible()
  // Nothing is saved behind the operator's back: the draft is marked unsaved.
  await expect(page.getByText('unsaved').first()).toBeVisible()
})

test('"Set up Telegram" opens the bot form', async ({ page }) => {
  await openAlerts(page)

  await page.getByRole('button', { name: 'Set up Telegram' }).click()

  const token = page.getByLabel('Bot token')
  await expect(token).toBeVisible()
  await expect(token).toBeFocused()
  await expect(page.getByLabel('Send to Telegram')).toBeChecked()
  await expect(page.getByLabel('Chat ids').first()).toBeVisible()
})

test('email can go through Panmail, and a test send says where it went', async ({ page }) => {
  await openAlerts(page)
  await page.getByRole('button', { name: 'Set up Email' }).click()

  await page.getByText('Panmail', { exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'SMTP server' })).toBeHidden()
  await page.getByLabel('Panmail address').fill('https://mail.northwind.id')
  await page.getByLabel('Provider id').fill('0f8b8f4e-0000-4000-8000-000000000000')
  await page.getByRole('textbox', { name: 'From', exact: true }).fill('argus@northwind.id')
  const to = page.getByRole('combobox', { name: 'To', exact: true })
  await to.fill('sec@northwind.id')
  await to.press('Enter')

  // Without the key the form says so, and the test button waits for it.
  await expect(page.getByText(/Still needed: API key/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Send test email' })).toBeDisabled()

  await page.getByLabel('API key').fill('pm_test_key')
  await page.getByRole('button', { name: 'Send test email' }).click()
  await expect(page.getByText('Test email sent')).toBeVisible()
  await expect(page.getByText(/Panmail queued it for sec@northwind.id/)).toBeVisible()
})
