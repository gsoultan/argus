import { useEffect, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Group, Modal, ScrollArea, Stack, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { IconArrowBackUp, IconBraces, IconLock } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { editorKind, parseTarget, sourceTable, TARGET_SQL, type EditTarget, type EditorKind, type Stmt, type TableColumn } from '~/lib/rowEdit'
import type { Cell, Col } from '~/lib/resultView'
import type { Collected } from '~/components/DatabaseAdmin'

/*
The pieces of editing rows in the results grid: reading the table a result
came from, the small editor a cell opens into, the form a row opens into
in the side panel (and on a phone, where there is no double-click), the bar
that counts the changes, and the dialog that shows the SQL they become.
*/

/** What a save did: every statement touched its one row, or nothing stayed. */
export type SaveResult =
  | { ok: true; returned: Collected[] }
  | { ok: false; at: number; error: string }

export interface EditSupport {
  /** The person's level allows writing. */
  allowed: boolean
  /** Another statement is running on the session. */
  busy: boolean
  /** The person's own statements left a transaction open. */
  inTransaction: boolean
  /** Cells the agent cut at its size limit, as `row:col`. */
  cut?: Set<string>
  save: (stmts: Stmt[]) => Promise<SaveResult>
}

type GridCollect = (sql: string, maxRows?: number, params?: (string | null)[]) => Promise<Collected>

/** The table every column of the result came from, read once per result. */
export function useEditTarget(cols: Col[], collect: GridCollect | undefined, key: number): EditTarget | null {
  const [target, setTarget] = useState<EditTarget | null>(null)
  const src = sourceTable(cols)
  const oid = 'oid' in src ? src.oid : 0
  useEffect(() => {
    setTarget(null)
    if (!collect || !oid) return
    let live = true
    void collect(TARGET_SQL, 2000, [String(oid)]).then((r) => {
      if (live && !r.error) setTarget(parseTarget(oid, r.rows))
    })
    return () => { live = false }
  }, [key, oid]) // eslint-disable-line react-hooks/exhaustive-deps
  return target
}

const BOOL_LABEL: Record<string, string> = { t: 'true', f: 'false' }

/** A value for a boolean or enum column: a short list, NULL included. */
function choicesFor(kind: EditorKind, col: TableColumn | undefined): { value: string; label: string }[] {
  if (kind === 'boolean') return [{ value: 't', label: 'true' }, { value: 'f', label: 'false' }]
  return (col?.enumValues ?? []).map((v) => ({ value: v, label: v }))
}

const NULL_OPT = '\u0000null'

/** Empty means NULL for a number or a time: '' is not one. */
const fromText = (kind: EditorKind, v: string): Cell => (v === '' && (kind === 'number' || kind === 'timestamp') ? null : v)

/** The editor a grid cell turns into on a double-click. */
export function CellInput({ kind, col, value, onCommit, onCancel }: {
  kind: EditorKind
  col: TableColumn | undefined
  value: Cell
  onCommit: (v: Cell) => void
  onCancel: () => void
}) {
  const style: React.CSSProperties = {
    width: '100%', minWidth: 120, height: 24, font: 'inherit', fontSize: 12, padding: '0 6px', borderRadius: 4,
    border: '1px solid var(--mantine-color-azure-filled)', background: 'var(--color-void)', color: 'inherit', outline: 'none',
  }
  if (kind === 'boolean' || kind === 'enum') {
    return (
      <select autoFocus style={style} defaultValue={value ?? NULL_OPT} aria-label={`New value for ${col?.name ?? 'the cell'}`}
        onChange={(e) => onCommit(e.currentTarget.value === NULL_OPT ? null : e.currentTarget.value)}
        onBlur={onCancel} onKeyDown={(e) => { if (e.key === 'Escape') onCancel() }}>
        {choicesFor(kind, col).map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        <option value={NULL_OPT}>NULL</option>
      </select>
    )
  }
  let done = false
  const commit = (v: Cell) => { if (done) return; done = true; onCommit(v) }
  return (
    <Group gap={2} wrap="nowrap">
      <input autoFocus style={style} defaultValue={value ?? ''} placeholder={value === null ? 'NULL' : ''}
        aria-label={`New value for ${col?.name ?? 'the cell'}`} inputMode={kind === 'number' ? 'decimal' : undefined}
        onKeyDown={(e) => {
          if (e.key === 'Enter') commit(fromText(kind, e.currentTarget.value))
          if (e.key === 'Escape') { done = true; onCancel() }
        }}
        onBlur={(e) => commit(fromText(kind, e.currentTarget.value))} />
      <Tooltip label="Set to NULL" withinPortal>
        <ActionIcon size="sm" variant="subtle" color="gray" aria-label="Set to NULL"
          onMouseDown={(e) => { e.preventDefault(); commit(null) }} onClick={() => commit(null)}>
          <Text span size={FS.micro} ff="var(--font-mono)">∅</Text>
        </ActionIcon>
      </Tooltip>
    </Group>
  )
}

/** One field of the row form. `unset` means Postgres fills it in (a new row). */
function FieldInput({ col, kind, value, unset, locked, onChange, onUnset, onOpen }: {
  col: TableColumn
  kind: EditorKind
  value: Cell
  unset?: boolean
  locked?: string
  onChange: (v: Cell) => void
  onUnset?: () => void
  onOpen: () => void
}) {
  const label = col.name
  if (locked) {
    return (
      <Group gap={6} wrap="nowrap">
        <IconLock size={12} color="var(--mantine-color-dimmed)" />
        <Text size={FS.meta} ff="var(--font-mono)" c="dimmed" truncate>{value ?? 'NULL'}</Text>
        <Text size={FS.micro} c="dimmed" ml="auto">{locked}</Text>
      </Group>
    )
  }
  if (unset) {
    return (
      <Button size="compact-xs" variant="default" onClick={() => onChange(col.notNull && !col.hasDefault ? '' : null)}
        aria-label={`Set ${label}`} style={{ fontWeight: 400 }}>
        {col.hasDefault ? 'Postgres default' : 'NULL'} · set a value
      </Button>
    )
  }
  if (kind === 'json' || kind === 'array') {
    return (
      <Group gap={6} wrap="nowrap">
        <Text size={FS.meta} ff="var(--font-mono)" c={value === null ? 'dimmed' : undefined} truncate style={{ flex: 1 }}>{value ?? 'NULL'}</Text>
        <Button size="compact-xs" variant="light" leftSection={<IconBraces size={12} />} onClick={onOpen} aria-label={`Edit ${label}`}>Edit</Button>
        {onUnset && <Button size="compact-xs" variant="subtle" color="gray" onClick={onUnset}>Leave to Postgres</Button>}
      </Group>
    )
  }
  if (kind === 'boolean' || kind === 'enum') {
    return (
      <Group gap={4} wrap="wrap">
        {[...choicesFor(kind, col), { value: NULL_OPT, label: 'NULL' }].map((o) => {
          const on = o.value === NULL_OPT ? value === null : value === o.value
          return (
            <Button key={o.value} size="compact-xs" variant={on ? 'filled' : 'default'} onClick={() => onChange(o.value === NULL_OPT ? null : o.value)}
              aria-pressed={on} style={{ fontFamily: 'var(--font-mono)', fontWeight: 500 }}>
              {BOOL_LABEL[o.value] ?? o.label}
            </Button>
          )
        })}
        {onUnset && <Button size="compact-xs" variant="subtle" color="gray" onClick={onUnset}>Leave to Postgres</Button>}
      </Group>
    )
  }
  const long = value !== null && (value.length > 60 || value.includes('\n'))
  const common = {
    size: 'xs' as const, value: value ?? '', placeholder: value === null ? 'NULL' : '', 'aria-label': label,
    styles: { input: { fontFamily: 'var(--font-mono)' } },
    onChange: (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => onChange(fromText(kind, e.currentTarget.value)),
  }
  return (
    <Group gap={4} wrap="nowrap" align="flex-start">
      <Box style={{ flex: 1 }}>
        {long ? <Textarea {...common} autosize minRows={2} maxRows={8} /> : <TextInput {...common} inputMode={kind === 'number' ? 'decimal' : undefined} />}
      </Box>
      <Tooltip label="Set to NULL" withinPortal>
        <ActionIcon size="md" variant="subtle" color="gray" aria-label={`Set ${label} to NULL`} onClick={() => onChange(null)} disabled={value === null}>
          <Text span size={FS.micro} ff="var(--font-mono)">∅</Text>
        </ActionIcon>
      </Tooltip>
      {onUnset && <Button size="compact-xs" variant="subtle" color="gray" mt={4} onClick={onUnset}>Leave to Postgres</Button>}
    </Group>
  )
}

export interface FormField {
  col: TableColumn
  value: Cell
  /** A new row's column left to Postgres. */
  unset?: boolean
  original?: Cell
  locked?: string
}

/** A row as a form: every field, its type, and an editor that fits it. */
export function RowForm({ fields, onChange, onUnset, onOpen }: {
  fields: FormField[]
  onChange: (attnum: number, v: Cell) => void
  onUnset?: (attnum: number) => void
  onOpen: (attnum: number) => void
}) {
  return (
    <Stack gap="sm">
      {fields.map((f) => {
        const changed = f.original !== undefined && f.value !== f.original
        const kind = editorKind(f.col.type, f.col.enumValues)
        return (
          <Box key={f.col.attnum} p={changed ? 6 : 0} style={changed ? { borderRadius: 6, background: 'var(--mantine-color-amber-light)' } : undefined}>
            <Group gap={6} wrap="nowrap" mb={4}>
              <Text size={FS.meta} fw={600} ff="var(--font-mono)">{f.col.name}</Text>
              <Text size={FS.micro} c="dimmed">{f.col.type}</Text>
              {f.col.notNull && !f.col.hasDefault && !f.locked && <Badge size="xs" variant="light" color="amber" tt="none">required</Badge>}
              {changed && (
                <Button size="compact-xs" variant="subtle" ml="auto" leftSection={<IconArrowBackUp size={12} />}
                  onClick={() => onChange(f.col.attnum, f.original ?? null)} aria-label={`Undo ${f.col.name}`}>
                  Undo
                </Button>
              )}
            </Group>
            <FieldInput col={f.col} kind={kind} value={f.value} unset={f.unset} locked={f.locked}
              onChange={(v) => onChange(f.col.attnum, v)} onOpen={() => onOpen(f.col.attnum)}
              onUnset={onUnset && !f.unset && (!f.col.notNull || f.col.hasDefault) ? () => onUnset(f.col.attnum) : undefined} />
            {changed && <Text size={FS.micro} c="dimmed" mt={2} ff="var(--font-mono)" truncate>was {f.original ?? 'NULL'}</Text>}
          </Box>
        )
      })}
    </Stack>
  )
}

/** The bar under the grid while there are changes to save. */
export function ChangesBar({ changedRows, newRows, busy, saving, error, onDiscard, onReview, onSave, narrow }: {
  changedRows: number
  newRows: number
  busy: boolean
  saving: boolean
  error: string | null
  onDiscard: () => void
  onReview: () => void
  onSave: () => void
  narrow: boolean
}) {
  const parts = [
    changedRows ? `${changedRows} ${changedRows === 1 ? 'row' : 'rows'} changed` : '',
    newRows ? `${newRows} new ${newRows === 1 ? 'row' : 'rows'}` : '',
  ].filter(Boolean)
  return (
    <Box px="md" py={6} role="region" aria-label="Unsaved changes"
      style={{ borderTop: '1px solid var(--color-line)', borderLeft: '3px solid var(--mantine-color-amber-filled)', background: 'var(--color-raised)' }}>
      <Group gap="sm" wrap={narrow ? 'wrap' : 'nowrap'}>
        <Text size={FS.meta} fw={600}>{parts.join(' · ')}</Text>
        {error
          ? <Text size={FS.micro} c={INK.denied} style={{ flex: 1 }}>{error}</Text>
          : <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>Not saved yet. Running another statement discards them.</Text>}
        <Group gap={6} wrap="nowrap" ml="auto">
          <Button size="compact-sm" variant="subtle" color="gray" onClick={onDiscard} disabled={saving}>Discard</Button>
          <Button size="compact-sm" variant="default" onClick={onReview} disabled={saving}>Review SQL</Button>
          <Tooltip label="Waits for the running statement to finish" disabled={!busy || saving} withinPortal>
            <Button size="compact-sm" onClick={onSave} loading={saving} disabled={busy && !saving}>Save</Button>
          </Tooltip>
        </Group>
      </Group>
    </Box>
  )
}

/** The statements a save will send, with their values, before anything runs. */
export function ReviewDialog({ opened, stmts, inTransaction, busy, saving, onClose, onSave }: {
  opened: boolean
  stmts: Stmt[]
  inTransaction: boolean
  busy: boolean
  saving: boolean
  onClose: () => void
  onSave: () => void
}) {
  return (
    <Modal opened={opened} onClose={onClose} size="xl" title={<Text fw={600}>Review the changes</Text>} scrollAreaComponent={ScrollArea.Autosize}>
      <Stack gap="md">
        <Text size={FS.meta} c="dimmed">
          {inTransaction
            ? 'These run inside the transaction you have open, behind a savepoint: they stay only if you COMMIT it.'
            : 'These run as one transaction. Each must change exactly one row, or none of them stays.'}
          {' '}Values travel as parameters, never as SQL. A row someone changed since you read it is not overwritten.
        </Text>
        {stmts.map((s, i) => (
          <Box key={i}>
            <Text size={FS.meta} fw={600} mb={4}>{i + 1}. {s.label}</Text>
            <Box component="pre" m={0} p={8} style={{ fontFamily: 'var(--font-mono)', fontSize: 12, whiteSpace: 'pre-wrap', borderRadius: 6, background: 'var(--color-raised)' }}>
              {s.sql}
            </Box>
            {s.params.length > 0 && (
              <Box mt={4} style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                {s.params.map((p, j) => (
                  <Text key={j} size={FS.micro} ff="var(--font-mono)" truncate>
                    <Text span c="dimmed" size={FS.micro} ff="var(--font-mono)">${j + 1} = </Text>
                    {p === null ? <Text span fs="italic" c="dimmed" size={FS.micro}>NULL</Text> : JSON.stringify(p)}
                  </Text>
                ))}
              </Box>
            )}
          </Box>
        ))}
        <Group justify="flex-end" gap="xs">
          <Button size="xs" variant="default" onClick={onClose}>Close</Button>
          <Button size="xs" onClick={onSave} loading={saving} disabled={busy && !saving}>
            Save {stmts.length} {stmts.length === 1 ? 'change' : 'changes'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
