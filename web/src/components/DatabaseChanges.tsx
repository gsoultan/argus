import { useInfiniteQuery } from '@tanstack/react-query'
import { ActionIcon, Alert, Badge, Box, Button, Code, Group, Loader, ScrollArea, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconArrowLeft, IconRefresh } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { dataChanges, isConfigured, type DataChange } from '~/lib/live'

/*
The Data changes log: every committed statement from a console session that
inserted, updated or deleted rows, emptied or dropped a table, or put
something back from the trash, newest first. The control plane writes it as
it relays the session; this only reads it.
*/

const VERB: Record<string, { label: string; color: string }> = {
  insert: { label: 'Inserted', color: 'teal' }, update: { label: 'Updated', color: 'azure' },
  delete: { label: 'Deleted', color: 'rose' }, merge: { label: 'Merged', color: 'azure' },
  truncate: { label: 'Emptied', color: 'rose' }, drop: { label: 'Dropped', color: 'rose' },
  restore: { label: 'Restored', color: 'teal' }, write: { label: 'Changed', color: 'azure' },
}

const when = (iso: string) => {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
}

const rowsText = (c: DataChange) => (c.rows < 0 ? '' : c.rows === 1 ? '1 row' : `${c.rows.toLocaleString()} rows`)

export function ChangesPanel({ databaseId, narrow, onBack }: { databaseId: string; narrow: boolean; onBack: () => void }) {
  const q = useInfiniteQuery({
    queryKey: ['database', databaseId, 'changes'],
    queryFn: ({ pageParam }) => (isConfigured() ? dataChanges(databaseId, pageParam || undefined) : Promise.resolve([])),
    initialPageParam: 0,
    getNextPageParam: (last: DataChange[]) => (last.length >= 100 ? last[last.length - 1]!.seq : undefined),
  })
  const items = q.data?.pages.flat() ?? []

  return (
    <Box style={{ display: 'flex', flexDirection: 'column', minHeight: 0, flex: 1 }}>
      <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <ActionIcon variant="subtle" onClick={onBack} aria-label="Back to the query editor"><IconArrowLeft size={16} /></ActionIcon>
        <Box style={{ minWidth: 0, flex: 1 }}>
          <Text fw={650} size={FS.body}>Data changes</Text>
          {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>Every committed insert, update, delete, empty, drop and restore made from the console, newest first. Kept 180 days.</Text>}
        </Box>
        <Tooltip label="Refresh" withArrow>
          <ActionIcon variant="subtle" onClick={() => void q.refetch()} loading={q.isFetching && !q.isFetchingNextPage} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
        </Tooltip>
      </Group>
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap="md" p="md">
          {q.isError && <Alert color="rose" variant="light">{q.error instanceof Error ? q.error.message : 'The log could not be read.'}</Alert>}
          {q.isPending && <Loader size="sm" />}
          {!q.isPending && items.length === 0 && !q.isError && <Text size={FS.meta} c="dimmed">No data has been changed from the console yet.</Text>}
          {items.length > 0 && (narrow
            ? items.map((c) => (
                <Box key={c.seq} p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
                  <Group gap={6} wrap="nowrap">
                    <Badge size="xs" variant="light" color={VERB[c.verb]?.color ?? 'slate'} tt="none">{VERB[c.verb]?.label ?? c.verb}</Badge>
                    <Text size={FS.meta} ff="var(--font-mono)" truncate>{c.objects.join(', ')}</Text>
                    <Text size={FS.micro} c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap' }}>{rowsText(c)}</Text>
                  </Group>
                  <Text size={FS.micro} c="dimmed" mt={2}>{c.actor} · {when(c.at)}{c.approvalId ? ' · approved' : ''}</Text>
                  <Code block mt={6} style={{ whiteSpace: 'pre-wrap', fontSize: 11, maxHeight: 120, overflow: 'auto' }}>{c.sql}</Code>
                </Box>
              ))
            : (
              <Table striped highlightOnHover verticalSpacing={6} fz={FS.meta} aria-label="Data changes">
                <Table.Thead>
                  <Table.Tr><Table.Th>When</Table.Th><Table.Th>Who</Table.Th><Table.Th>What</Table.Th><Table.Th>Table</Table.Th><Table.Th ta="right">Rows</Table.Th><Table.Th>Statement</Table.Th></Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {items.map((c) => (
                    <Table.Tr key={c.seq}>
                      <Table.Td style={{ whiteSpace: 'nowrap' }}>{when(c.at)}</Table.Td>
                      <Table.Td ff="var(--font-mono)">{c.actor}</Table.Td>
                      <Table.Td>
                        <Group gap={4} wrap="nowrap">
                          <Badge size="xs" variant="light" color={VERB[c.verb]?.color ?? 'slate'} tt="none">{VERB[c.verb]?.label ?? c.verb}</Badge>
                          {c.approvalId && <Tooltip label="Ran under an approval, on production" withArrow><Badge size="xs" variant="outline" color="amber" tt="none">approved</Badge></Tooltip>}
                        </Group>
                      </Table.Td>
                      <Table.Td ff="var(--font-mono)">{c.objects.join(', ')}</Table.Td>
                      <Table.Td ta="right">{rowsText(c)}</Table.Td>
                      <Table.Td maw={420}>
                        <Tooltip label={<Text size={FS.micro} ff="var(--font-mono)" style={{ whiteSpace: 'pre-wrap' }}>{c.sql}</Text>} multiline maw={560} withinPortal>
                          <Text size={FS.micro} ff="var(--font-mono)" truncate>{c.sql.replace(/\s+/g, ' ')}</Text>
                        </Tooltip>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            ))}
          {q.hasNextPage && (
            <Button size="xs" variant="default" onClick={() => void q.fetchNextPage()} loading={q.isFetchingNextPage} style={{ alignSelf: 'center' }}>Older changes</Button>
          )}
          <Text size={FS.micro} c="dimmed">
            A change inside a transaction is listed once it commits, and never if it rolls back. The statement is shown as it was sent; the values that went with it,
            and what it returned, are not kept here. Admins and everyone assigned this database are told of each change in the bell.
          </Text>
        </Stack>
      </ScrollArea>
    </Box>
  )
}
