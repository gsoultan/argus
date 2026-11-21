import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen } from '@testing-library/react'
import { renderWithProviders } from '~/test/render'
import { ItemSwitcher } from '~/components/ItemSwitcher'

const items = [
  { value: 'a', label: 'orders · Postgres · db-01' },
  { value: 'b', label: 'users · Postgres · db-02' },
  { value: 'c', label: 'cache · Redis · db-03' },
]

/** What took the place of the list beside a database or an asset. */
describe('Previous, next and search over one kind of item', () => {
  it('steps to the neighbours and stops at the ends', () => {
    const onGo = vi.fn()
    renderWithProviders(<ItemSwitcher what="database" items={items} currentId="a" onGo={onGo} />)
    expect(screen.getByRole('button', { name: 'Previous database' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Next database' }))
    expect(onGo).toHaveBeenCalledWith('b')
  })

  it('is not drawn when there is nothing to move to', () => {
    renderWithProviders(<ItemSwitcher what="broker" items={items.slice(0, 1)} currentId="a" onGo={() => {}} />)
    expect(screen.queryByTestId('broker-switcher')).toBeNull()
  })
})
