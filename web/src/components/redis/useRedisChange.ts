import { useCallback, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { meQuery } from '~/lib/queries'
import { useStatementApproval } from '~/components/StatementApproval'
import type { StoreAnswer, StoreRequest, StoreSession } from '~/components/useStoreSession'
import type { Database } from '~/types/domain'

/*
One way to send a Redis change. When the agent answers that the change waits
for approval (a bulk change over 100 keys, a configuration or user change on a
database whose changes need it), the same approval asks as a Postgres
statement are used: the canonical text the agent sent is what is asked for,
and once someone else approves it the request goes again with the approval's
id. The control plane checks the text matches; nothing here decides.
*/

export interface ChangeAnswer extends StoreAnswer {
  /** No approval came, so nothing was sent again. */
  notApproved?: boolean
}

export function useRedisChange(s: StoreSession, database: Database): {
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  dialog: ReactNode
} {
  const { data: me } = useQuery(meQuery())
  const { ask, dialog } = useStatementApproval(database.id, database.dbname, { label: database.label, me: me?.email })
  const change = useCallback(async (req: StoreRequest): Promise<ChangeAnswer> => {
    const a = await s.ask(req)
    if (!a.needsApproval) return a
    const id = await ask([{ sql: a.needsApproval.text, rows: a.needsApproval.cap }], { why: a.error })
    if (!id) return { ...a, notApproved: true, error: 'not run: it was not approved' }
    return s.ask({ ...req, approval: id })
  }, [s, ask])
  return { change, dialog }
}

/** The agent's data on a result, typed by the caller; null on anything else. */
export function dataOf<T>(a: StoreAnswer | null | undefined): T | null {
  return a && a.type === 'result' && a.data !== undefined ? (a.data as T) : null
}
