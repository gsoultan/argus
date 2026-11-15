import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Freshness } from '~/components/Freshness'
import { renderWithProviders } from '~/test/render'

/** A live page says how old its figures are, and says so in amber when reads stop arriving. */
describe('Freshness', () => {
  it('says live and how long ago while reads arrive on time', () => {
    renderWithProviders(<Freshness at={Date.now() - 20_000} every={30_000} what="Status." />)
    expect(screen.getByTestId('freshness')).toHaveTextContent('Live · updated 20 s ago')
    expect(screen.getByTestId('freshness')).not.toHaveAttribute('data-stale')
  })

  it('turns amber once two reads in a row are missing', () => {
    renderWithProviders(<Freshness at={Date.now() - 3 * 60_000} every={30_000} what="Status." />)
    expect(screen.getByTestId('freshness')).toHaveTextContent('Not updated for 3 min')
    expect(screen.getByTestId('freshness')).toHaveAttribute('data-stale')
  })

  it('shows nothing before the first read', () => {
    renderWithProviders(<Freshness at={0} every={30_000} what="Status." />)
    expect(screen.queryByTestId('freshness')).toBeNull()
  })
})
