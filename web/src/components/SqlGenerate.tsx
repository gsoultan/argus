import { createContext, useContext, useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Autocomplete, Box, Button, Checkbox, Code, Group, Menu, Modal, ScrollArea, Stack, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconCode, IconDots, IconFileImport, IconPlus, IconTableOptions, IconX } from '@tabler/icons-react'
import { FS } from '~/theme'
import {
  COMMON_TYPES, DML_LABEL, alterTable, blankColumn, createTable, designProblems, type Design, type DesignColumn, type DmlKind,
} from '~/lib/sqlGen'

/*
The schema tree's generators: a menu on each object that writes its DDL or a
statement for it into a new tab, and the table designer. The workspace does
the reading (as the person's role, recorded like anything else) and opens
the tab; nothing here runs anything.
*/

export type GenAction =
  | { what: 'ddl' | DmlKind | 'design' | 'import'; kind: string; schema: string; name: string }
  | { what: 'new-table'; schema: string }

export interface GenContext {
  act: (a: GenAction) => void
  /** The person may change the database: the designer is offered. */
  canWrite: boolean
}

export const GenerateContext = createContext<GenContext | null>(null)

const DML: DmlKind[] = ['select', 'insert', 'update', 'delete', 'upsert']
const HAS_DDL = new Set(['table', 'partitioned table', 'view', 'matview', 'function', 'procedure', 'sequence'])

/** The ⋯ menu beside an object in the tree. */
export function ObjectGenMenu({ kind, schema, name }: { kind: string; schema: string; name: string }) {
  const g = useContext(GenerateContext)
  if (!g || !HAS_DDL.has(kind)) return null
  const table = kind === 'table' || kind === 'partitioned table'
  const go = (what: GenAction['what']) => g.act({ what, kind, schema, name } as GenAction)
  return (
    <Menu position="bottom-end" withinPortal shadow="md">
      <Menu.Target>
        <ActionIcon size="xs" variant="subtle" aria-label={`Generate for ${name}`}><IconDots size={11} /></ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>Open in a new tab</Menu.Label>
        <Menu.Item leftSection={<IconCode size={13} />} onClick={() => go('ddl')}>DDL (CREATE {kind === 'matview' ? 'MATERIALIZED VIEW' : kind.toUpperCase()})</Menu.Item>
        {table && DML.map((k) => <Menu.Item key={k} onClick={() => go(k)}>{DML_LABEL[k]}</Menu.Item>)}
        {table && g.canWrite && (
          <>
            <Menu.Divider />
            <Menu.Item leftSection={<IconTableOptions size={13} />} onClick={() => go('design')}>Design table…</Menu.Item>
            <Menu.Item leftSection={<IconFileImport size={13} />} onClick={() => go('import')}>Import from CSV or Excel…</Menu.Item>
          </>
        )}
      </Menu.Dropdown>
    </Menu>
  )
}

/** The + beside a schema: a new table there. */
export function NewTableButton({ schema }: { schema: string }) {
  const g = useContext(GenerateContext)
  if (!g?.canWrite) return null
  return (
    <Tooltip label={`New table in ${schema}`} withArrow>
      <ActionIcon size="xs" variant="subtle" aria-label={`New table in ${schema}`} onClick={() => g.act({ what: 'new-table', schema })}>
        <IconPlus size={11} />
      </ActionIcon>
    </Tooltip>
  )
}

/**
 * The table designer. Opened on a table it writes the ALTER TABLE statements
 * that change it; opened on a schema, the CREATE TABLE. Either way the
 * statements open in a new tab, to be read and run there.
 */
export function TableDesigner({ start, narrow, onClose, onOpen }: {
  /** The table as it is, or a blank design for a new one (no `was` on its columns). */
  start: Design
  narrow: boolean
  onClose: () => void
  onOpen: (sql: string, name: string) => void
}) {
  const creating = start.columns.every((c) => c.was === undefined)
  const [d, setD] = useState<Design>(start)
  const set = (i: number, patch: Partial<DesignColumn>) =>
    setD((x) => ({ ...x, columns: x.columns.map((c, j) => (j === i ? { ...c, ...patch, ...(patch.pk ? { notNull: true } : {}) } : c)) }))
  const problems = designProblems(d)
  const sql = useMemo(() => (problems.length ? '' : creating ? createTable(d) : alterTable(start, d).join('\n')), [d, start, creating, problems.length])

  const nameCell = (c: DesignColumn, i: number) => (
    <TextInput size="xs" value={c.name} aria-label={`Name of column ${i + 1}`} ff="monospace"
      onChange={(e) => set(i, { name: e.currentTarget.value })} />
  )
  const typeCell = (c: DesignColumn, i: number) => (
    <Autocomplete size="xs" value={c.type} data={COMMON_TYPES} aria-label={`Type of ${c.name || `column ${i + 1}`}`} ff="monospace"
      onChange={(v) => set(i, { type: v })} comboboxProps={{ withinPortal: true }} />
  )
  const defaultCell = (c: DesignColumn, i: number) => (
    <TextInput size="xs" value={c.default} placeholder="none" aria-label={`Default of ${c.name || `column ${i + 1}`}`} ff="monospace"
      onChange={(e) => set(i, { default: e.currentTarget.value })} />
  )
  const keyBox = (c: DesignColumn, i: number) => (
    <Checkbox size="xs" label={narrow ? 'Key' : undefined} aria-label={`${c.name} is in the primary key`} checked={c.pk} onChange={(e) => set(i, { pk: e.currentTarget.checked })} />
  )
  const notNullBox = (c: DesignColumn, i: number) => (
    <Checkbox size="xs" label={narrow ? 'Not null' : undefined} aria-label={`${c.name} is not null`} checked={c.notNull || c.pk} disabled={c.pk}
      onChange={(e) => set(i, { notNull: e.currentTarget.checked })} />
  )
  const uniqueBox = (c: DesignColumn, i: number) => (
    <Checkbox size="xs" label={narrow ? 'Unique' : undefined} aria-label={`${c.name} is unique`} checked={c.unique} disabled={c.pk}
      onChange={(e) => set(i, { unique: e.currentTarget.checked })} />
  )
  const remove = (c: DesignColumn, i: number) => (
    <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Remove ${c.name || `column ${i + 1}`}`}
      onClick={() => setD((x) => ({ ...x, columns: x.columns.filter((_, j) => j !== i) }))}><IconX size={13} /></ActionIcon>
  )

  return (
    <Modal opened onClose={onClose} size="xl" fullScreen={narrow} centered
      title={<Text fw={650}>{creating ? `New table in ${d.schema}` : `Design ${d.schema}.${d.name}`}</Text>}>
      <Stack gap="sm">
        {creating && (
          <TextInput label="Table name" value={d.name} ff="monospace" data-autofocus
            onChange={(e) => { const name = e.currentTarget.value; setD((x) => ({ ...x, name })) }} />
        )}
        {narrow ? (
          <Stack gap="xs">
            {d.columns.map((c, i) => (
              <Box key={i} p="xs" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
                <Group gap="xs" wrap="nowrap" align="flex-end">
                  <Box style={{ flex: 1 }}>{nameCell(c, i)}</Box>{remove(c, i)}
                </Group>
                <Group gap="xs" mt={6} grow>{typeCell(c, i)}{defaultCell(c, i)}</Group>
                <Group gap="md" mt={6}>{keyBox(c, i)}{notNullBox(c, i)}{uniqueBox(c, i)}</Group>
              </Box>
            ))}
          </Stack>
        ) : (
          <ScrollArea.Autosize mah={360} type="auto">
            <Table verticalSpacing={4} fz={FS.meta}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Column</Table.Th><Table.Th>Type</Table.Th><Table.Th>Default</Table.Th>
                  <Table.Th ta="center">Key</Table.Th><Table.Th ta="center">Not null</Table.Th><Table.Th ta="center">Unique</Table.Th><Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {d.columns.map((c, i) => (
                  <Table.Tr key={i}>
                    <Table.Td>{nameCell(c, i)}</Table.Td><Table.Td w={180}>{typeCell(c, i)}</Table.Td><Table.Td>{defaultCell(c, i)}</Table.Td>
                    <Table.Td><Group justify="center">{keyBox(c, i)}</Group></Table.Td>
                    <Table.Td><Group justify="center">{notNullBox(c, i)}</Group></Table.Td>
                    <Table.Td><Group justify="center">{uniqueBox(c, i)}</Group></Table.Td>
                    <Table.Td>{remove(c, i)}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </ScrollArea.Autosize>
        )}
        <Button size="xs" variant="light" leftSection={<IconPlus size={13} />} style={{ alignSelf: 'flex-start' }}
          onClick={() => setD((x) => ({ ...x, columns: [...x.columns, blankColumn(x.columns.length + 1)] }))}>Add column</Button>
        {problems.length > 0 && <Alert color="amber" variant="light">{problems.join(' ')}</Alert>}
        {!problems.length && (
          <Box>
            <Text size={FS.micro} c="dimmed" mb={4}>
              {creating ? 'The statement:' : sql ? 'The statements, in the order they run:' : 'Nothing has changed yet.'}
            </Text>
            {sql && <ScrollArea.Autosize mah={200} type="auto"><Code block style={{ whiteSpace: 'pre-wrap', fontSize: 12 }}>{sql}</Code></ScrollArea.Autosize>}
          </Box>
        )}
        <Text size={FS.micro} c="dimmed">
          This writes the SQL and opens it in a new tab; nothing changes until you run it there, where it is checked, recorded and,
          on production, approved like anything else. Changing a column's type rewrites the table and locks it while it does.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!sql} onClick={() => onOpen(sql, creating ? `create ${d.name}` : `alter ${d.name}`)}>Open in a new tab</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
