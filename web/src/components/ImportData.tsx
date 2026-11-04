import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, FileButton, Group, List, Modal, Progress, ScrollArea, SegmentedControl, Stack, Table, Text,
} from '@mantine/core'
import { IconDownload, IconFileImport, IconFileSpreadsheet } from '@tabler/icons-react'
import { FS } from '~/theme'
import { downloadBlob, downloadText } from '~/lib/download'
import {
  MAX_IMPORT_BYTES, MAX_IMPORT_ROWS, canUpsert, columnNote, csvTemplate, importStatements, mapHeader, parseCSV, planImport, required,
  templateColumns, xlsxTemplateSheets, type Cell, type OnConflict,
} from '~/lib/importData'
import { readXlsx, writeXlsx } from '~/lib/xlsx'
import type { EditTarget } from '~/lib/rowEdit'

/*
Import from a CSV or Excel file into a table. Get the template first, fill
it in, pick it; the console checks every value and shows what it found, and
only then sends the rows, in one transaction.
*/

export type ImportStmt = { sql: string; params: Cell[]; first: number; last: number }
export type ImportRun = (stmts: ImportStmt[], progress: (done: number) => void) =>
  Promise<{ ok: true; rows: number } | { ok: false; at: number; error: string }>

interface Loaded { name: string; header: Cell[]; data: Cell[][] }

const fileBase = (table: string) => table.replace(/"/g, '').replace(/[^A-Za-z0-9_.-]+/g, '_')

export function ImportData({ target, narrow, onClose, run }: {
  target: EditTarget
  narrow: boolean
  onClose: () => void
  run: ImportRun
}) {
  const [file, setFile] = useState<Loaded | null>(null)
  const [readError, setReadError] = useState<string | null>(null)
  const [reading, setReading] = useState(false)
  const [onConflict, setOnConflict] = useState<OnConflict>('error')
  const [state, setState] = useState<{ kind: 'idle' } | { kind: 'running'; done: number; total: number }
    | { kind: 'done'; rows: number } | { kind: 'failed'; error: string; rows: string }>({ kind: 'idle' })
  const cols = templateColumns(target)

  const mapping = useMemo(() => (file ? mapHeader(target, file.header) : null), [file, target])
  const plan = useMemo(() => (file && mapping ? planImport(mapping, file.data) : null), [file, mapping])
  const upsert = plan ? canUpsert(target, plan) : false

  const pick = async (f: File | null) => {
    if (!f) return
    setReadError(null); setFile(null); setState({ kind: 'idle' })
    if (f.size > MAX_IMPORT_BYTES) { setReadError(`The file is ${(f.size / 1048576).toFixed(1)} MB; an import takes up to ${MAX_IMPORT_BYTES / 1048576} MB.`); return }
    setReading(true)
    try {
      let rows: Cell[][]
      if (/\.xlsx$/i.test(f.name)) rows = (await readXlsx(new Uint8Array(await f.arrayBuffer()))).rows.filter((r) => r.some((v) => v !== null))
      else if (/\.xls$/i.test(f.name)) throw new Error('this is the old Excel format (.xls); save it as .xlsx or CSV and pick it again')
      else rows = parseCSV(await f.text())
      if (rows.length === 0) throw new Error('the file is empty')
      if (rows.length - 1 > MAX_IMPORT_ROWS) throw new Error(`the file has ${(rows.length - 1).toLocaleString()} rows; an import takes up to ${MAX_IMPORT_ROWS.toLocaleString()}`)
      setFile({ name: f.name, header: rows[0]!, data: rows.slice(1) })
    } catch (e) {
      setReadError(`Could not read ${f.name}: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setReading(false)
    }
  }

  const blocked = !plan || plan.rows.length === 0 || plan.columns.length === 0 || (mapping?.missing.length ?? 0) > 0 || plan.problems.length > 0
  const go = async () => {
    if (!plan || blocked) return
    const stmts = importStatements(target, plan, onConflict === 'update' && !upsert ? 'error' : onConflict)
    setState({ kind: 'running', done: 0, total: plan.rows.length })
    const r = await run(stmts, (done) => setState({ kind: 'running', done, total: plan.rows.length }))
    if (r.ok) setState({ kind: 'done', rows: r.rows })
    else {
      const s = stmts[r.at]
      setState({ kind: 'failed', error: r.error, rows: s ? `rows ${s.first}–${s.last} of the file` : 'the import' })
    }
  }
  const running = state.kind === 'running'

  return (
    <Modal opened onClose={() => !running && onClose()} size="xl" fullScreen={narrow} centered closeOnClickOutside={!running}
      title={<Group gap="xs"><IconFileImport size={18} /><Text fw={650}>Import into {target.table}</Text></Group>}>
      <Stack gap="md">
        <Box>
          <Text fw={600} size={FS.body}>1. Get the template</Text>
          <Text size={FS.meta} c="dimmed" mb="xs">
            Its header row is the table's columns. Fill in a row per record and keep the header as it is; columns you leave out are filled by their default, or NULL.
          </Text>
          <Group gap="xs">
            <Button size="xs" variant="light" leftSection={<IconFileSpreadsheet size={14} />}
              onClick={() => downloadBlob(new Blob([writeXlsx(xlsxTemplateSheets(target)) as BlobPart], { type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' }), `${fileBase(target.table)}-template.xlsx`)}>
              Excel template
            </Button>
            <Button size="xs" variant="light" leftSection={<IconDownload size={14} />}
              onClick={() => downloadText(csvTemplate(target), `${fileBase(target.table)}-template.csv`, 'text/csv')}>
              CSV template
            </Button>
          </Group>
          <ScrollArea.Autosize mah={140} mt="xs" type="auto">
            <List fz={FS.meta} spacing={2}>
              {cols.map((c) => (
                <List.Item key={c.name}>
                  <Text span ff="monospace" size={FS.meta} fw={required(c) ? 600 : 400}>{c.name}</Text>
                  <Text span size={FS.micro} c="dimmed"> {c.type}: {columnNote(c)}</Text>
                </List.Item>
              ))}
            </List>
          </ScrollArea.Autosize>
        </Box>

        <Box>
          <Text fw={600} size={FS.body} mb="xs">2. Pick the filled-in file</Text>
          <Group gap="sm">
            <FileButton onChange={(f) => void pick(f)} accept=".csv,.tsv,.txt,.xlsx,text/csv,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet">
              {(props) => <Button {...props} size="xs" variant="default" loading={reading} disabled={running}>Choose a CSV or Excel file</Button>}
            </FileButton>
            {file && <Text size={FS.meta} ff="monospace">{file.name}</Text>}
          </Group>
          {readError && <Alert mt="xs" color="rose" variant="light">{readError}</Alert>}
        </Box>

        {file && mapping && plan && (
          <Box>
            <Text fw={600} size={FS.body} mb={4}>3. Check what it found</Text>
            <Group gap={6} mb="xs">
              <Badge variant="light" color="azure" tt="none">{plan.rows.length.toLocaleString()} rows</Badge>
              <Badge variant="light" color="slate" tt="none">{plan.columns.length} of {cols.length} columns</Badge>
              {plan.problems.length > 0 && <Badge variant="light" color="rose" tt="none">{plan.problems.length.toLocaleString()} problems</Badge>}
            </Group>
            {mapping.missing.length > 0 && <Alert color="rose" variant="light" mb="xs">The file has no {mapping.missing.join(', ')}, which the table requires. Add {mapping.missing.length === 1 ? 'that column' : 'those columns'} to the header.</Alert>}
            {mapping.unknown.length > 0 && <Alert color="amber" variant="light" mb="xs">Left out, because the table has no such column: {mapping.unknown.join(', ')}.</Alert>}
            {mapping.twice.length > 0 && <Alert color="amber" variant="light" mb="xs">Named twice, only the first is used: {mapping.twice.join(', ')}.</Alert>}
            {plan.problems.length > 0 && (
              <Alert color="rose" variant="light" mb="xs" title="Fix these in the file, then pick it again">
                <List fz={FS.meta} spacing={0}>
                  {plan.problems.slice(0, 12).map((p, i) => <List.Item key={i}>Row {p.row}, {p.column}: {p.message}</List.Item>)}
                </List>
                {plan.problems.length > 12 && <Text size={FS.micro} mt={4}>… and {(plan.problems.length - 12).toLocaleString()} more.</Text>}
              </Alert>
            )}
            {plan.rows.length > 0 && plan.columns.length > 0 && (
              <ScrollArea type="auto" mah={220}>
                <Table striped withTableBorder fz={FS.micro} verticalSpacing={2} aria-label="Preview">
                  <Table.Thead><Table.Tr>{plan.columns.map((c) => <Table.Th key={c.name} ff="monospace">{c.name}</Table.Th>)}</Table.Tr></Table.Thead>
                  <Table.Tbody>
                    {plan.rows.slice(0, 10).map((r, i) => (
                      <Table.Tr key={i}>{r.map((v, k) => <Table.Td key={k} ff="monospace" c={v === null ? 'dimmed' : undefined}>{v ?? (plan.columns[k]!.hasDefault ? 'default' : 'NULL')}</Table.Td>)}</Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </ScrollArea>
            )}
            {plan.rows.length > 10 && <Text size={FS.micro} c="dimmed" mt={4}>The first 10 rows of {plan.rows.length.toLocaleString()}.</Text>}
          </Box>
        )}

        {plan && !blocked && (
          <Box>
            <Text fw={600} size={FS.body} mb="xs">4. When a row is already there</Text>
            <SegmentedControl size="xs" value={onConflict} onChange={(v) => setOnConflict(v as OnConflict)} disabled={running} fullWidth={narrow}
              orientation={narrow ? 'vertical' : 'horizontal'}
              data={[
                { value: 'error', label: 'Stop, import nothing' },
                { value: 'skip', label: 'Skip it' },
                { value: 'update', label: 'Update it from the file', disabled: !upsert },
              ]} />
            {!upsert && <Text size={FS.micro} c="dimmed" mt={4}>Updating needs the primary key in the file.</Text>}
          </Box>
        )}

        {state.kind === 'running' && (
          <Box>
            <Progress value={(state.done / Math.max(1, state.total)) * 100} animated />
            <Text size={FS.micro} c="dimmed" mt={4}>Sent {state.done.toLocaleString()} of {state.total.toLocaleString()} rows; nothing is kept until the last is in.</Text>
          </Box>
        )}
        {state.kind === 'done' && <Alert color="teal" variant="light">Imported: {state.rows.toLocaleString()} rows went into {target.table}.</Alert>}
        {state.kind === 'failed' && <Alert color="rose" variant="light" title="Nothing was imported">Postgres refused {state.rows}: {state.error}</Alert>}

        <Text size={FS.micro} c="dimmed">
          The rows go in one transaction, so a failure keeps none of them. Each statement is recorded like any other, and on production the import waits for approval.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose} disabled={running}>{state.kind === 'done' ? 'Close' : 'Cancel'}</Button>
          {state.kind !== 'done' && (
            <Button onClick={() => void go()} loading={running} disabled={blocked}>
              Import {plan && !blocked ? `${plan.rows.length.toLocaleString()} rows` : ''}
            </Button>
          )}
        </Group>
      </Stack>
    </Modal>
  )
}
