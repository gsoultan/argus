import { test, expect } from '@playwright/test'

/**
 * Where recordings are copied, in Settings › Storage, against
 * e2e/authstub.mjs.
 *
 * What it guards: each service asks for what that service calls its
 * credentials -- an Azure storage account is not an access key -- R2 comes
 * with its one region, the archive offers only what the chosen service has,
 * and a connection test reports both of its outcomes.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const shots = process.env.ARGUS_SHOTS

test('object storage is chosen by service and tested before saving', async ({ page }) => {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await page.goto('/settings?section=storage')

  await page.getByRole('switch', { name: 'Copy recordings to object storage' }).check()
  const services = page.getByRole('radiogroup', { name: 'Storage service' })
  await expect(services.getByRole('radio', { name: 'Amazon S3' })).toBeChecked()

  // Cloudflare R2: S3's fields, R2's one region.
  await services.getByText('Cloudflare R2').click()
  await expect(page.getByRole('textbox', { name: 'Region' })).toHaveValue('auto')
  await expect(page.getByRole('textbox', { name: 'Access key ID' })).toBeVisible()
  await page.getByRole('textbox', { name: 'Endpoint' }).fill('0123abcd.r2.cloudflarestorage.com')
  await page.getByRole('textbox', { name: 'Bucket' }).fill('argus-recordings')
  const cls = page.getByRole('combobox', { name: 'Archive storage class' })
  await cls.click()
  await expect(page.getByRole('option', { name: 'Standard-IA' })).toBeVisible()
  await expect(page.getByRole('option', { name: 'Glacier Instant Retrieval' })).toHaveCount(0)
  await page.keyboard.press('Escape')

  // Azure: an account, its key and a container; tiers, not classes.
  await services.getByText('Azure Blob Storage').click()
  await expect(page.getByRole('textbox', { name: 'Storage account' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Region' })).toHaveCount(0)
  await page.getByRole('textbox', { name: 'Storage account' }).fill('northwindrec')
  await page.getByRole('textbox', { name: 'Container' }).fill('argus-recordings')
  await expect(page.getByRole('textbox', { name: 'Blob endpoint' }))
    .toHaveAttribute('placeholder', 'https://northwindrec.blob.core.windows.net')
  const tier = page.getByRole('combobox', { name: 'Archive access tier' })
  await tier.click()
  await page.getByRole('option', { name: 'Cool' }).click()
  await expect(tier).toHaveValue('Cool')

  // Without the key the test says why; with it, it says it worked.
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(page.getByText('Storage did not work')).toBeVisible()
  await expect(page.getByText(/needs the storage account's name and one of its access keys/)).toBeVisible()
  await page.getByLabel('Account key').fill('a2V5')
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(page.getByText('Storage works')).toBeVisible()
  if (shots) {
    await page.getByRole('heading', { name: 'Object storage' }).scrollIntoViewIfNeeded()
    await page.screenshot({ path: `${shots}/azure.png`, fullPage: false })
  }

  // Back to S3, the Azure tier is not carried over as an S3 class.
  await services.getByText('Amazon S3').click()
  await expect(cls).toHaveValue("Bucket's default")
  await expect(page.getByRole('textbox', { name: 'Endpoint' })).toHaveValue('s3.amazonaws.com')
})

test("outside the default organization, Storage is the organization's own", async ({ page, context }) => {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  // stub_run: a fresh organization for this browser and this try.
  await context.addCookies([
    { name: 'argus_org', value: 'acme', url: 'http://localhost:5511' },
    { name: 'stub_run', value: `${test.info().project.name}-${test.info().retry}-${Date.now()}`, url: 'http://localhost:5511' },
  ])
  await page.goto('/settings?section=storage')

  // The control plane's own say what an organization gets by default, and
  // nothing of where they are.
  const own = page.getByRole('switch', { name: "Keep recordings in this organization's own storage" })
  await expect(own).not.toBeChecked()
  await expect(page.getByText(/Recordings go to the control plane's object storage, kept apart/)).toBeVisible()
  await expect(page.getByRole('combobox', { name: 'Keep on servers for' }))
    .toHaveValue("Control plane's setting (7 days)")
  await expect(page.getByRole('switch', { name: 'Copy recordings to object storage' })).toHaveCount(0)

  await own.check()
  await page.getByRole('radiogroup', { name: 'Storage service' }).getByText('Cloudflare R2').click()
  await page.getByRole('textbox', { name: 'Endpoint' }).fill('acme.r2.cloudflarestorage.com')
  await page.getByRole('textbox', { name: 'Bucket' }).fill('acme-recordings')
  await page.getByRole('textbox', { name: 'Access key ID' }).fill('acmekey')
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(page.getByText(/needs a secret key/)).toBeVisible()
  await page.getByLabel('Secret access key').fill('acmesecret')
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(page.getByText('Storage works')).toBeVisible()
  await page.getByRole('combobox', { name: 'Keep on servers for' }).click()
  await page.getByRole('option', { name: '3 days' }).click()

  await expect(page.getByText(/Unsaved changes to where this organization's recordings are kept/)).toBeVisible()
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByText('Recording storage saved')).toBeVisible()
  await expect(page.getByText('Copying recordings across')).toBeVisible()
  await expect(page.getByText(/12 copied from the control plane's storage/)).toBeVisible()
  if (shots) {
    await page.locator('.mantine-Notification-root').evaluateAll((els) => els.forEach((e) => e.remove()))
    await page.screenshot({ path: `${shots}/organization-storage.png`, fullPage: true })
  }
})
