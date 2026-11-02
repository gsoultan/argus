import { describe, expect, it, vi, beforeEach } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import { TwoFactorSetup } from '~/components/TwoFactor'

const live = vi.hoisted(() => ({
  beginMFAEnrolment: vi.fn(),
  confirmMFAEnrolment: vi.fn(),
  logout: vi.fn(),
}))
vi.mock('~/lib/live', () => live)

/**
 * The enrolment flow is the first thing a privileged account meets, and the
 * only thing it may do until it is done. The password is sent with the first
 * request, the secret is shown and scannable, and the recovery codes cannot
 * be dismissed until they are kept.
 */
describe('TwoFactorSetup', () => {
  beforeEach(() => {
    live.beginMFAEnrolment.mockReset().mockResolvedValue({
      secret: 'JBSWY3DPEHPK3PXP',
      uri: 'otpauth://totp/Argus:a%40b.c?secret=JBSWY3DPEHPK3PXP&issuer=Argus',
    })
    live.confirmMFAEnrolment.mockReset().mockResolvedValue({
      recoveryCodes: ['aaaa-bbbb-cccc-dddd', 'eeee-ffff-gggg-hhhh'],
    })
  })

  it('asks for the password first and sends it with the enrolment request', async () => {
    ui(<TwoFactorSetup enrolled={false} onDone={() => {}} />)
    const button = screen.getByRole('button', { name: 'Continue' })
    expect(button).toBeDisabled()
    fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'hunter2 hunter2' } })
    fireEvent.click(button)
    await waitFor(() => expect(live.beginMFAEnrolment).toHaveBeenCalledWith('hunter2 hunter2', '', false))
    // The secret is shown as text and as a QR code.
    expect(await screen.findByText('JBSW Y3DP EHPK 3PXP')).toBeInTheDocument()
    expect(screen.getByRole('img', { name: /QR code/ })).toBeInTheDocument()
  })

  it('demands the current code when replacing an authenticator', () => {
    ui(<TwoFactorSetup enrolled onDone={() => {}} />)
    fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'hunter2 hunter2' } })
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled()
    expect(screen.getByLabelText('Code from your current authenticator')).toBeInTheDocument()
  })

  it('shows the recovery codes once and will not finish until they are kept', async () => {
    const onDone = vi.fn()
    ui(<TwoFactorSetup enrolled={false} onDone={onDone} />)
    fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'hunter2 hunter2' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    const code = await screen.findByLabelText('Code from the app')
    fireEvent.change(code, { target: { value: '123456' } })
    fireEvent.click(screen.getByRole('button', { name: 'Turn it on' }))
    await waitFor(() => expect(live.confirmMFAEnrolment).toHaveBeenCalledWith('123456'))

    expect(await screen.findByText('aaaa-bbbb-cccc-dddd')).toBeInTheDocument()
    const done = screen.getByRole('button', { name: /Copy or download them first/ })
    expect(done).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }))
    fireEvent.click(screen.getByRole('button', { name: 'Done' }))
    expect(onDone).toHaveBeenCalled()
  })

  it('shows the control plane\'s refusal verbatim', async () => {
    live.beginMFAEnrolment.mockRejectedValue(new Error('your password is not correct'))
    ui(<TwoFactorSetup enrolled={false} onDone={() => {}} />)
    fireEvent.change(screen.getByLabelText('Your password'), { target: { value: 'wrong wrong wrong' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await screen.findByText('your password is not correct')).toBeInTheDocument()
  })
})
