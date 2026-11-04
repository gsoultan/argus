import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { RESTORE_SQL, TRASH_LIST_SQL, TrashNotice, TrashPanel } from '~/components/DatabaseTrash'
import type { Collected } from '~/components/DatabaseAdmin'
import { renderWithProviders } from '~/test/render'

/**
 * What a DELETE, DROP or TRUNCATE put in the trash is said under its result
 * with Undo, and listed in the Trash panel with Restore. Putting something
 * back is one statement with the item's number as a parameter.
 */

describe('Trash', () => {
  it('offers Undo under a DELETE, and sends the restore', async () => {
    const collect = vi.fn(async (): Promise<Collected> => ({ cols: [], rows: [['1']] }))
    renderWithProviders(<TrashNotice collect={collect} canRestore busy={false}
      items={[{ id: 7, kind: 'rows', source: 'public.orders', rows: 3, expires: '2026-12-04T06:00:00Z' }]} />)
    expect(screen.getByText(/Deleted 3 rows from public.orders\. A copy is kept until/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Undo' }))
    await waitFor(() => expect(collect).toHaveBeenCalledWith(RESTORE_SQL, 1, ['7']))
    expect(await screen.findByText('Put back: public.orders.')).toBeInTheDocument()
  })

  it('offers nothing to put back at read access', () => {
    renderWithProviders(<TrashNotice collect={vi.fn()} canRestore={false} busy={false}
      items={[{ id: 8, kind: 'table', source: 'public.old', expires: '2026-11-11T06:00:00Z' }]} />)
    expect(screen.getByText(/Moved public.old to the trash until/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull()
  })

  it('lists the trash and restores from it', async () => {
    const collect = vi.fn(async (sql: string): Promise<Collected> => sql === TRASH_LIST_SQL
      ? { cols: [], rows: [
          ['3', 'table', 'public.audit_old', '1200', '81920', 'ana_sari', '2026-11-04T06:00:00Z', '2026-11-11T06:00:00Z', null, null],
          ['2', 'rows', 'public.orders', '3', null, 'ana_sari', '2026-11-03T06:00:00Z', '2026-12-03T06:00:00Z', '2026-11-03T07:00:00Z', 'budi'],
        ] }
      : { cols: [], rows: [['3']] })
    renderWithProviders(<TrashPanel collect={collect} level="owner" narrow={false} onBack={vi.fn()} />)
    expect(await screen.findByText('public.audit_old')).toBeInTheDocument()
    expect(screen.getByText('Dropped table')).toBeInTheDocument()
    expect(screen.getByText('80 KB')).toBeInTheDocument()
    expect(screen.getByText('Put back by budi')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }))
    await waitFor(() => expect(collect).toHaveBeenCalledWith(RESTORE_SQL, 1, ['3']))
  })

  it('says when the database has no trash yet', async () => {
    const collect = vi.fn(async (): Promise<Collected> => ({ cols: [], rows: [], error: { error: 'schema "argus_trash_fn" does not exist' } }))
    renderWithProviders(<TrashPanel collect={collect} level="read" narrow onBack={vi.fn()} />)
    expect(await screen.findByText(/This database has no trash yet/)).toBeInTheDocument()
  })
})
