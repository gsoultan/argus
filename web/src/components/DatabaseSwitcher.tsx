import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { ItemSwitcher } from '~/components/ItemSwitcher'
import { engineOf, isBroker } from '~/lib/engines'
import { databasesQuery } from '~/lib/queries'

/**
 * Previous, next and a search over every database, or every broker, this
 * person can see: what the list down the side of a database's workspace was
 * for, without taking its width from the workspace.
 */
export function DatabaseSwitcher({ currentId, brokers = false }: { currentId: string; brokers?: boolean }) {
  const navigate = useNavigate()
  const { data } = useQuery(databasesQuery())
  const items = (data?.databases ?? [])
    .filter((d) => isBroker(d.engine) === brokers)
    .map((d) => ({
      value: d.id,
      label: `${d.engine === 'redis' ? `db ${d.dbname}` : d.dbname} · ${engineOf(d.engine).label} · ${d.hostname.split('.')[0]}`,
    }))
    .sort((a, b) => a.label.localeCompare(b.label))
  return (
    <ItemSwitcher what={brokers ? 'broker' : 'database'} items={items} currentId={currentId} width={280}
      onGo={(id) => void (brokers
        ? navigate({ to: '/brokers/$brokerId', params: { brokerId: id } })
        : navigate({ to: '/databases/$dbId', params: { dbId: id } }))} />
  )
}
