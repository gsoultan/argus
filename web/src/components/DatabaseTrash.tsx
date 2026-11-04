import { useCallback, useEffect, useState } from 'react'
import { ActionIcon, Alert, Badge, Box, Button, Group, Loader, ScrollArea, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconArrowBackUp, IconArrowLeft, IconRefresh, IconTrash } from '@tabler/icons-react'
import { FS, INK, SP } from '~/theme'
import { notifyError, notifyOk } from '~/lib/notify'
import type { Collected } from '~/components/DatabaseAdmin'
import type { DBLevel } from '~/types/domain'

/*
The database's trash, as the console shows it.

What a DELETE, DROP or TRUNCATE removed in a session is kept in the database
itself (the agent's argus_trash schema): deleted rows for 30 days, a dropped
table or schema and an emptied table's rows for 7. Undo right after the
statement, or Restore from the Trash panel, puts it back, through a function
that checks the person could have done the real thing themselves.
*/

export interface TrashItem {
  id: number
  /** rows, table, schema or empty. */
  kind: string
  source: string
  rows?: number
  expires: string
}

type Collect = (sql: string, maxRows?: number, params?: (string | null)[]) => Promise<Collected>

export const RESTORE_SQL = 'SELECT * FROM argus_trash_fn.restore($1::bigint)'
export const TRASH_LIST_SQL = 'SELECT id, kind, source, rows, bytes, by_role, at, expires, restored, restored_by FROM argus_trash_fn.list()'

const day = (iso: string) => {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
}

/** What a removal did, in a sentence. */
export function trashSentence(it: TrashItem): string {
  const n = it.rows ?? 0
  switch (it.kind) {
    case 'rows': return `Deleted ${n.toLocaleString()} ${n === 1 ? 'row' : 'rows'} from ${it.source}. A copy is kept until ${day(it.expires)}.`
    case 'empty': return `Emptied ${it.source} (${n.toLocaleString()} rows). Its rows are kept in the trash until ${day(it.expires)}.`
    case 'schema': return `Moved schema ${it.source} to the trash until ${day(it.expires)}.`
    default: return `Moved ${it.source} to the trash until ${day(it.expires)}.`
  }
}

const restoreLabel = (kind: string) => (kind === 'rows' ? 'Undo' : 'Restore')

/** Puts one item back, and says how it went. */
async function restoreItem(collect: Collect, it: { id: number; kind: string; source: string }): Promise<boolean> {
  const r = await collect(RESTORE_SQL, 1, [String(it.id)])
  if (r.error) {
    notifyError('Not put back', new Error(r.error.error ?? 'Postgres refused it'))
    return false
  }
  notifyOk(it.kind === 'rows' ? 'Undone' : 'Restored',
    it.kind === 'rows' ? `The rows are back in ${it.source}.` : `${it.source} is back where it was.`)
  return true
}

/** Under a result: what its statement put in the trash, with Undo. */
export function TrashNotice({ items, collect, canRestore, busy }: {
  items: TrashItem[]
  collect: Collect
  canRestore: boolean
  /** Another statement is running. */
  busy: boolean
}) {
  const [done, setDone] = useState<Record<number, boolean>>({})
  const [working, setWorking] = useState<number | null>(null)
  return (
    <Stack gap={4} px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-raised)' }} aria-label="Kept in the trash">
      {items.map((it) => (
        <Group key={it.id} gap="sm" wrap="nowrap">
          <IconTrash size={14} color="var(--mantine-color-dimmed)" />
          <Text size={FS.meta} style={{ flex: 1 }}>{done[it.id] ? `Put back: ${it.source}.` : trashSentence(it)}</Text>
          {canRestore && !done[it.id] && (
            <Tooltip label="Waits for the running statement" disabled={!busy} withinPortal>
              <Button size="compact-xs" variant="light" leftSection={<IconArrowBackUp size={12} />}
                loading={working === it.id} disabled={busy}
                onClick={async () => {
                  setWorking(it.id)
                  const ok = await restoreItem(collect, it)
                  setWorking(null)
                  if (ok) setDone((d) => ({ ...d, [it.id]: true }))
                }}>
                {restoreLabel(it.kind)}
              </Button>
            </Tooltip>
          )}
        </Group>
      ))}
    </Stack>
  )
}

interface Listed {
  id: number
  kind: string
  source: string
  rows: number | null
  bytes: number | null
  by: string
  at: string
  expires: string
  restored: string | null
  restoredBy: string | null
}

const KIND: Record<string, string> = { rows: 'Deleted rows', table: 'Dropped table', schema: 'Dropped schema', empty: 'Emptied table' }

function size(b: number): string {
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++ }
  return `${b < 10 && i ? b.toFixed(1) : Math.round(b)} ${u[i]}`
}

/** The Trash panel: everything kept, and Restore. */
export function TrashPanel({ collect, level, narrow, onBack }: {
  collect: Collect
  level: DBLevel | ''
  narrow: boolean
  onBack: () => void
}) {
  const [items, setItems] = useState<Listed[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [working, setWorking] = useState<number | null>(null)
  const load = useCallback(async () => {
    setLoading(true)
    const r = await collect(TRASH_LIST_SQL, 500)
    setLoading(false)
    if (r.error) {
      setError(/argus_trash_fn/.test(r.error.error ?? '')
        ? 'This database has no trash yet. It is set up the next time someone opens a session on it with an up-to-date agent.'
        : r.error.error ?? 'Postgres refused it')
      return
    }
    setError(null)
    setItems(r.rows.map((x) => ({
      id: Number(x[0]), kind: x[1] ?? '', source: x[2] ?? '', rows: x[3] === null ? null : Number(x[3]),
      bytes: x[4] === null ? null : Number(x[4]), by: x[5] ?? '', at: x[6] ?? '', expires: x[7] ?? '',
      restored: x[8] ?? null, restoredBy: x[9] ?? null,
    })))
  }, [collect])
  useEffect(() => { void load() }, [load])
  const canRestore = level !== '' && level !== 'read'

  return (
    <Box style={{ display: 'flex', flexDirection: 'column', minHeight: 0, flex: 1 }}>
      <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <ActionIcon variant="subtle" onClick={onBack} aria-label="Back to the query editor"><IconArrowLeft size={16} /></ActionIcon>
        <Box style={{ minWidth: 0, flex: 1 }}>
          <Text fw={650} size={FS.body}>Trash</Text>
          {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>Deleted rows are kept for 30 days; dropped tables and schemas, and emptied tables' rows, for 7.</Text>}
        </Box>
        <Tooltip label="Refresh" withArrow>
          <ActionIcon variant="subtle" onClick={() => void load()} loading={loading} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
        </Tooltip>
      </Group>
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap="md" p="md">
          {error && <Alert color="amber" variant="light">{error}</Alert>}
          {items === null && !error && <Loader size="sm" />}
          {items?.length === 0 && <Text size={FS.meta} c="dimmed">Nothing is in the trash.</Text>}
          {items && items.length > 0 && (narrow
            ? items.map((it) => (
                <Box key={it.id} p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
                  <Group justify="space-between" wrap="nowrap" gap="xs">
                    <Text size={FS.meta} fw={600} ff="var(--font-mono)" truncate>{it.source}</Text>
                    <Badge size="xs" variant="light" color="slate" tt="none">{KIND[it.kind] ?? it.kind}</Badge>
                  </Group>
                  <Text size={FS.micro} c="dimmed" mt={2}>
                    {it.rows !== null && `${it.rows.toLocaleString()} rows · `}by {it.by} · {day(it.at)} · until {day(it.expires)}
                  </Text>
                  <Group mt="xs" justify="flex-end">{action(it)}</Group>
                </Box>
              ))
            : (
              <Table striped highlightOnHover verticalSpacing={6} fz={FS.meta}>
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>What</Table.Th><Table.Th>Kind</Table.Th><Table.Th ta="right">Rows</Table.Th><Table.Th ta="right">Size</Table.Th>
                    <Table.Th>By</Table.Th><Table.Th>When</Table.Th><Table.Th>Kept until</Table.Th><Table.Th />
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {items.map((it) => (
                    <Table.Tr key={it.id}>
                      <Table.Td ff="var(--font-mono)">{it.source}</Table.Td>
                      <Table.Td>{KIND[it.kind] ?? it.kind}</Table.Td>
                      <Table.Td ta="right">{it.rows?.toLocaleString() ?? ''}</Table.Td>
                      <Table.Td ta="right">{it.bytes ? size(it.bytes) : ''}</Table.Td>
                      <Table.Td ff="var(--font-mono)">{it.by}</Table.Td>
                      <Table.Td>{day(it.at)}</Table.Td>
                      <Table.Td>{day(it.expires)}</Table.Td>
                      <Table.Td ta="right">{action(it)}</Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            ))}
          <Text size={FS.micro} c="dimmed">
            Putting something back needs the same right as the real thing: to own a dropped table or schema, or to insert into the table rows go back to.
            The trash lives in the database, so its rows stay behind the database's own grants and backups, never in a recording.
          </Text>
        </Stack>
      </ScrollArea>
    </Box>
  )

  function action(it: Listed) {
    if (it.restored) return <Text size={FS.micro} c={INK.verified}>Put back by {it.restoredBy}</Text>
    if (!canRestore) return null
    return (
      <Button size="compact-xs" variant="light" leftSection={<IconArrowBackUp size={12} />} loading={working === it.id}
        onClick={async () => {
          setWorking(it.id)
          const ok = await restoreItem(collect, it)
          setWorking(null)
          if (ok) void load()
        }}>
        {restoreLabel(it.kind)}
      </Button>
    )
  }
}
