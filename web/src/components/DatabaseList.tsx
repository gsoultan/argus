import { Badge, Box, Group, ScrollArea, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { IconDatabase, IconSearch } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { engineOf } from '~/lib/engines'
import { databasesQuery } from '~/lib/queries'
import { FS } from '~/theme'
import type { DBLabel } from '~/types/domain'

const LABEL_COLOR: Record<DBLabel, string> = { production: 'rose', staging: 'amber', development: 'teal' }

/**
 * Every database you can see, as a column of rows, for the left of a
 * database's workspace. The one you have open is the lit row.
 */
export function DatabaseList({ currentId }: { currentId: string }) {
  const { data } = useQuery(databasesQuery())
  const [q, setQ] = useState('')

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (data?.databases ?? [])
      .filter((d) => !needle || d.dbname.toLowerCase().includes(needle) || d.hostname.toLowerCase().includes(needle))
  }, [data, q])

  return (
    <>
      <Box p="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <TextInput
          size="sm"
          placeholder="Find a database"
          aria-label="Find a database in the list"
          leftSection={<IconSearch size={14} />}
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
        />
      </Box>
      <ScrollArea style={{ flex: 1 }} type="auto">
        {rows.map((d) => (
          <Link
            key={d.id}
            to="/databases/$dbId"
            params={{ dbId: d.id }}
            className="argus-splitrow"
            data-active={d.id === currentId || undefined}
            aria-current={d.id === currentId ? 'page' : undefined}
          >
            <IconDatabase size={16} color="var(--color-brand)" style={{ flexShrink: 0 }} />
            <Box style={{ minWidth: 0, flex: 1 }}>
              <Text size="sm" fw={600} truncate>{d.engine === 'redis' ? `db ${d.dbname}` : d.dbname}</Text>
              <Text size={FS.micro} c="dimmed" ff="monospace" truncate>{engineOf(d.engine).label} · {d.hostname}</Text>
            </Box>
            <Badge variant="light" color={LABEL_COLOR[d.label]} radius="sm" size="xs">{d.label}</Badge>
          </Link>
        ))}
        {rows.length === 0 && (
          <Group p="md"><Text size={FS.meta} c="dimmed">No database matches.</Text></Group>
        )}
      </ScrollArea>
    </>
  )
}
