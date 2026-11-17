import { useCallback } from 'react'
import { Box, Modal } from '@mantine/core'
import { NetworkPanel } from '~/components/DatabaseNetwork'
import type { Admin, AdminOp } from '~/components/DatabaseAdmin'
import { useRedisChange } from '~/components/redis/useRedisChange'
import type { StoreSession } from '~/components/useStoreSession'
import type { Database } from '~/types/domain'

/*
Which machines can connect to a MongoDB: the same panel as Postgres and
MySQL, asked through the MongoDB session (network, network_set) instead of
the SQL session's admin operations. A change that waits for approval asks
for it the way a Redis change does, for the exact text the agent sent.
*/

export function MongoNetwork({ s, database, opened, onClose, narrow }: {
  s: StoreSession
  database: Database
  opened: boolean
  onClose: () => void
  narrow: boolean
}) {
  const { change, dialog } = useRedisChange(s, database)
  const admin = useCallback(async (op: AdminOp) => {
    const a = op.kind === 'network'
      ? await s.ask({ op: 'network' })
      : await change({ op: 'network_set', reach: op.reach, addresses: op.addresses ?? [], sources: op.sources ?? [],
        restart: !!op.restart, ...(op.confirm ? { confirm: op.confirm } : {}) })
    if (a.type === 'result') return { data: a.data, tag: a.tag, ms: a.ms }
    return { error: { error: a.error ?? 'failed' } }
  }, [s, change]) as Admin
  return (
    <Modal opened={opened} onClose={onClose} size="xl" fullScreen={narrow} withCloseButton={false} padding={0}>
      <Box style={{ display: 'flex', flexDirection: 'column', height: narrow ? '100dvh' : '80dvh' }}>
        <NetworkPanel admin={admin} level={s.ready?.level ?? ''} narrow={narrow} onBack={onClose}
          assetId={database.assetId} engine="mongodb" />
      </Box>
      {dialog}
    </Modal>
  )
}
