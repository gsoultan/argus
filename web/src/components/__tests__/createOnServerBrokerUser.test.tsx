import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import type { Asset } from '~/types/domain'

/*
Create new on a RabbitMQ node, with the app's user made in the same step
(plans/broker-users/design.md, picture 08): the dialog offers the three
choices, sends the new user with its password, and shows the AMQP URL once.
*/

const api = vi.hoisted(() => ({ createOptions: vi.fn(), createOnServer: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  databasesOnAsset: async () => [],
}))
vi.mock('~/lib/createApi', async (orig) => ({ ...(await orig<typeof import('~/lib/createApi')>()), ...api }))

const { CreateOnServer } = await import('~/components/CreateOnServer')

describe('Create new vhost with an app user', () => {
  it('sends a new user with the vhost and shows its AMQP URL', async () => {
    localStorage.removeItem('argus.create.ownerKind')
    api.createOptions.mockResolvedValue({ state: 'ok', options: { taken: ['/'], users: [{ name: 'orders-app' }] } })
    api.createOnServer.mockImplementation(async (_id: string, req: { dryRun?: boolean }) => req.dryRun
      ? { state: 'ok', statements: ['rabbitmqctl add_vhost /payments'], steps: [] }
      : {
          state: 'ok', statements: [], steps: [{ text: 'Made the user payments_app', ok: true }],
          connection: { host: 'mq-01.corp', port: 5672, database: '/payments', user: 'payments_app' },
        })
    const asset = { id: 'a1', hostname: 'mq-01.corp', address: '10.0.0.7', port: 22 } as Asset
    ui(<CreateOnServer asset={asset} engine="rabbitmq" onClose={() => {}} onAddExisting={() => {}} />)
    expect(await screen.findByText('App user')).toBeTruthy()
    expect(screen.getByText('No app user')).toBeTruthy()
    expect(screen.getByText('A user on mq-01')).toBeTruthy()
    fireEvent.change(screen.getByPlaceholderText('/payments'), { target: { value: '/payments' } })
    fireEvent.click(screen.getByText('A new user'))
    expect(await screen.findByDisplayValue('payments_app')).toBeTruthy()
    const create = await screen.findByRole('button', { name: 'Create /payments and payments_app' })
    await waitFor(() => expect((create as HTMLButtonElement).disabled).toBe(false))
    fireEvent.click(create)
    await waitFor(() => expect(api.createOnServer).toHaveBeenCalledWith('a1', expect.objectContaining({
      engine: 'rabbitmq', name: '/payments', owner: { kind: 'new', name: 'payments_app' }, password: expect.any(String),
    })))
    const sent = api.createOnServer.mock.calls.find((c) => !c[1].dryRun)![1] as { password: string }
    const url = await screen.findByText(`amqp://payments_app:${encodeURIComponent(sent.password)}@mq-01.corp:5672/%2Fpayments`)
    expect(url).toBeTruthy()
    expect(screen.getByText('Vhost')).toBeTruthy()
  })
})
