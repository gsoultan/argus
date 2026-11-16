import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { UsageRow } from '~/components/myservers'
import { usageTone, type AssetUsageRow } from '~/lib/assetsUsage'
import { renderWithProviders } from '~/test/render'

/**
 * The usage row on an Assets card: three figures with their bars, coloured
 * at the running-low thresholds, and nothing invented for a figure the agent
 * did not send.
 */

const row: AssetUsageRow = {
  assetId: 'a1', usageAt: new Date().toISOString(),
  cpu: 18, memory: 61, disk: { mount: '/var/lib/postgresql', percent: 91 }, warnings: [],
}

describe('the usage row', () => {
  it('shows processor, memory and the fullest disk', () => {
    renderWithProviders(<UsageRow usage={row} />)
    expect(screen.getByTestId('usage-cpu')).toHaveTextContent('Processor18%')
    expect(screen.getByTestId('usage-memory')).toHaveTextContent('Memory61%')
    expect(screen.getByTestId('usage-disk')).toHaveTextContent('Disk /var/lib/postgresql91%')
    expect(screen.getByRole('progressbar', { name: 'Disk 91% full' })).toBeInTheDocument()
  })

  it('leaves out a figure the agent did not send rather than drawing zero', () => {
    renderWithProviders(<UsageRow usage={{ ...row, cpu: null }} />)
    expect(screen.queryByTestId('usage-cpu')).toBeNull()
    expect(screen.getByTestId('usage-memory')).toBeInTheDocument()
  })

  it('draws nothing when there is no figure at all', () => {
    renderWithProviders(<UsageRow usage={{ ...row, cpu: null, memory: null, disk: null }} />)
    expect(screen.queryByTestId('usage-row')).toBeNull()
  })

  it('colours at the running-low thresholds', () => {
    expect([usageTone(89), usageTone(90), usageTone(94), usageTone(95)]).toEqual(['azure', 'amber', 'amber', 'rose'])
  })
})
