import { useCallback } from 'react'
import { NetworkPanel, type NetworkEngine } from '~/components/DatabaseNetwork'
import type { Admin, AdminOp } from '~/components/DatabaseAdmin'
import { useRedisChange } from '~/components/redis/useRedisChange'
import type { StoreSession } from '~/components/useStoreSession'
import type { Database } from '~/types/domain'

/*
Which machines can connect, for an engine whose console is a store session
(MongoDB, RabbitMQ, Kafka): the same panel as Postgres and MySQL, asked
through the session's network and network_set operations instead of the
SQL session's admin operations. A change that waits for approval asks for
it the way a Redis change does, for the exact text the agent sent.
*/

export function StoreNetworkPanel({ s, database, engine, narrow, onBack }: {
  s: StoreSession
  database: Database
  engine: Extract<NetworkEngine, 'mongodb' | 'rabbitmq' | 'kafka'>
  narrow: boolean
  onBack?: () => void
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
    <>
      <NetworkPanel admin={admin} level={s.ready?.level ?? ''} narrow={narrow} onBack={onBack} assetId={database.assetId} engine={engine} />
      {dialog}
    </>
  )
}
