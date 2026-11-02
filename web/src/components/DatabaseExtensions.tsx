import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Checkbox, Code, Group, Loader, ScrollArea, Select, Table, Text, TextInput,
} from '@mantine/core'
import { IconRefresh } from '@tabler/icons-react'
import { FS } from '~/theme'
import { notifyOk } from '~/lib/notify'
import {
  AdminRefused, NeedsDBA, PanelFrame, type Admin, type AdminFailure, type AdminOp, type AdminPanelProps,
} from '~/components/DatabaseAdmin'

/*
Extensions: what this database has installed, what the server can install,
and the three operations on them. Every mutation shows the exact statement
before its button; the statement comes from the agent (a preview of the same
builder that will run it), so the console never guesses at SQL.
*/

export interface PGExtension {
  name: string
  installedVersion?: string
  defaultVersion?: string
  versions?: string[]
  schema?: string
  relocatable?: boolean
  trusted?: boolean
  superuser?: boolean
  comment?: string
}

type Form =
  | { mode: 'install'; ext: PGExtension }
  | { mode: 'update'; ext: PGExtension }
  | { mode: 'remove'; ext: PGExtension }

/** The statement an operation would run, redacted, from the agent. */
export function usePreview(admin: Admin, op: AdminOp | null): { sql: string; error?: AdminFailure } {
  const [state, setState] = useState<{ sql: string; error?: AdminFailure }>({ sql: '' })
  const key = op ? JSON.stringify(op) : ''
  useEffect(() => {
    if (!op) { setState({ sql: '' }); return }
    let live = true
    const t = setTimeout(() => {
      void admin<{ sql: string }>({ ...op, preview: true }).then((r) => {
        if (!live) return
        setState(r.error ? { sql: '', error: r.error } : { sql: r.data?.sql ?? '' })
      })
    }, 200)
    return () => { live = false; clearTimeout(t) }
    // op is keyed by its JSON so a re-created object does not refetch.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [admin, key])
  return state
}

/** A newer version than the installed one, when the server has it. */
export function updateTarget(e: PGExtension): string | undefined {
  if (!e.installedVersion || !e.versions?.length) return undefined
  const last = e.versions[e.versions.length - 1]!
  if (last === e.installedVersion || e.versions.indexOf(last) <= e.versions.indexOf(e.installedVersion)) return undefined
  return last
}

export function ExtensionsPanel({ admin, collect, level, narrow, onBack, onAskForAccess }: AdminPanelProps) {
  const [data, setData] = useState<PGExtension[] | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [asOf, setAsOf] = useState<Date | null>(null)
  const [busy, setBusy] = useState(false)
  const [filter, setFilter] = useState('')
  const [form, setForm] = useState<Form | null>(null)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<PGExtension[]>({ kind: 'extension.list' })
    if (!alive.current) return
    if (r.error) setFailure(r.error)
    else { setFailure(null); setData(r.data ?? []) }
    setAsOf(new Date())
    setBusy(false)
  }, [admin])
  useEffect(() => { if (level === 'dba') void refresh() }, [refresh, level])

  if (level !== 'dba') {
    return (
      <PanelFrame title="Extensions" narrow={narrow} onBack={onBack} blurb="What is installed in this database and what the server can install.">
        <NeedsDBA level={level} what="Installing, updating and removing extensions" onAskForAccess={onAskForAccess} />
      </PanelFrame>
    )
  }

  const installed = (data ?? []).filter((e) => e.installedVersion)
  const q = filter.trim().toLowerCase()
  const available = (data ?? []).filter((e) => !e.installedVersion && (!q || e.name.toLowerCase().includes(q) || (e.comment ?? '').toLowerCase().includes(q)))
  const updatable = installed.filter(updateTarget).length

  return (
    <PanelFrame title="Extensions" narrow={narrow} onBack={onBack}
      blurb="Installed here, and available on this server. Install, update and remove run through the server’s postgres account; each is in this session’s recording."
      right={<ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>}>
      {form ? (
        <ExtensionForm form={form} admin={admin} collect={collect} onDone={() => { setForm(null); void refresh() }} onCancel={() => setForm(null)} />
      ) : (
        <>
          <Group gap="md" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
            {data && <Text size={FS.meta}><b>{installed.length}</b> installed</Text>}
            {data && <Text size={FS.meta} c={updatable ? 'azure' : undefined}><b>{updatable}</b> can be updated</Text>}
            {data && <Text size={FS.meta}><b>{data.length - installed.length}</b> available</Text>}
            {asOf && <Text size={FS.micro} c="dimmed" ml="auto">as of {asOf.toLocaleTimeString()}</Text>}
          </Group>
          {failure ? <Box p="md"><AdminRefused error={failure} /></Box> : !data ? <Loader size={16} m="md" /> : (
            <ScrollArea style={{ flex: 1 }} type="auto">
              <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} px="md" pt="sm" style={{ letterSpacing: '.06em' }}>Installed</Text>
              <Table verticalSpacing={4} fz={12} striped highlightOnHover>
                <Table.Thead>
                  <Table.Tr><Table.Th>Extension</Table.Th><Table.Th>Version</Table.Th>{!narrow && <Table.Th>Schema</Table.Th>}<Table.Th>Update</Table.Th><Table.Th></Table.Th></Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {installed.map((e) => {
                    const to = updateTarget(e)
                    const builtIn = e.name === 'plpgsql'
                    return (
                      <Table.Tr key={e.name}>
                        <Table.Td>
                          <Text size={FS.meta} ff="monospace" fw={600}>{e.name}</Text>
                          {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>{e.comment}</Text>}
                        </Table.Td>
                        <Table.Td><Text size={FS.meta} ff="monospace">{e.installedVersion}</Text></Table.Td>
                        {!narrow && <Table.Td><Text size={FS.meta} ff="monospace">{e.schema}</Text></Table.Td>}
                        <Table.Td>
                          {to ? (
                            <Button size="compact-xs" variant="light" onClick={() => setForm({ mode: 'update', ext: e })}>update to {to}</Button>
                          ) : <Text size={FS.micro} c="dimmed">up to date</Text>}
                        </Table.Td>
                        <Table.Td ta="right">
                          {builtIn ? <Text size={FS.micro} c="dimmed">built in</Text> : (
                            <Button size="compact-xs" variant="subtle" color="rose" onClick={() => setForm({ mode: 'remove', ext: e })}>Remove</Button>
                          )}
                        </Table.Td>
                      </Table.Tr>
                    )
                  })}
                </Table.Tbody>
              </Table>
              <Group px="md" pt="md" pb={4} gap="sm" wrap="nowrap">
                <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} style={{ letterSpacing: '.06em', flex: 1 }}>Available on this server</Text>
                <TextInput size="xs" placeholder={`Filter ${data.length - installed.length} extensions…`} value={filter} onChange={(e) => setFilter(e.currentTarget.value)} w={narrow ? 160 : 260} aria-label="Filter available extensions" />
              </Group>
              <Table verticalSpacing={4} fz={12} striped highlightOnHover>
                <Table.Thead>
                  <Table.Tr><Table.Th>Extension</Table.Th><Table.Th>Default version</Table.Th><Table.Th>Needs</Table.Th><Table.Th></Table.Th></Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {available.map((e) => (
                    <Table.Tr key={e.name}>
                      <Table.Td>
                        <Text size={FS.meta} ff="monospace" fw={600}>{e.name}</Text>
                        {!narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>{e.comment}</Text>}
                      </Table.Td>
                      <Table.Td><Text size={FS.meta} ff="monospace">{e.defaultVersion}</Text></Table.Td>
                      <Table.Td>
                        <Badge size="xs" variant="light" radius="sm" color={e.trusted ? 'teal' : e.superuser ? 'amber' : 'slate'}>
                          {e.trusted ? 'trusted' : e.superuser ? 'superuser' : 'create on database'}
                        </Badge>
                      </Table.Td>
                      <Table.Td ta="right"><Button size="compact-xs" variant="default" onClick={() => setForm({ mode: 'install', ext: e })}>Install</Button></Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
              {available.length === 0 && <Text size={FS.meta} c="dimmed" p="md">{q ? 'Nothing matches.' : 'Everything the server has is installed.'}</Text>}
              <Text size={FS.micro} c="dimmed" p="md">
                “Trusted” and “superuser” are Postgres’s own flags: either installs the same way here, because the helper runs as the server’s postgres account; the label says what the extension can reach.
              </Text>
            </ScrollArea>
          )}
        </>
      )}
    </PanelFrame>
  )
}

function ExtensionForm({ form, admin, collect, onDone, onCancel }: {
  form: Form; admin: Admin; collect: AdminPanelProps['collect']; onDone: () => void; onCancel: () => void
}) {
  const { ext } = form
  const [schema, setSchema] = useState<string>(ext.relocatable === false ? '' : 'public')
  const [schemas, setSchemas] = useState<string[]>(['public'])
  const [version, setVersion] = useState<string>(form.mode === 'update' ? (updateTarget(ext) ?? '') : (ext.defaultVersion ?? ''))
  const [cascade, setCascade] = useState(false)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  useEffect(() => {
    if (form.mode !== 'install') return
    void collect(`SELECT nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\\_%' AND nspname <> 'information_schema' ORDER BY 1`, 200)
      .then((r) => { if (!r.error) setSchemas(r.rows.map((x) => x[0] ?? '').filter(Boolean)) })
  }, [collect, form.mode])

  const op: AdminOp = form.mode === 'install'
    ? { kind: 'extension.create', name: ext.name, schema: schema || undefined, version: version || undefined, cascade }
    : form.mode === 'update'
      ? { kind: 'extension.update', name: ext.name, version: version || undefined }
      : { kind: 'extension.drop', name: ext.name, cascade }
  const preview = usePreview(admin, op)
  const title = { install: `Install ${ext.name}`, update: `Update ${ext.name}`, remove: `Remove ${ext.name}` }[form.mode]
  const verb = { install: 'Install', update: 'Update', remove: 'Remove' }[form.mode]
  const canRun = !busy && !!preview.sql && (form.mode !== 'remove' || typed === ext.name)

  const run = async () => {
    setBusy(true)
    const r = await admin({ ...op })
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(`${ext.name} ${form.mode === 'install' ? 'installed' : form.mode === 'update' ? 'updated' : 'removed'}`, r.tag ?? 'Done')
    onDone()
  }

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={640}>
        <Text fw={650} size={FS.body}>{title}</Text>
        <Text size={FS.meta} c="dimmed">{ext.comment}{form.mode === 'install' && ext.superuser ? ' · needs superuser, which the helper has' : ''}</Text>
        {form.mode === 'install' && (
          <Group grow mt="sm" align="flex-start">
            <Select size="xs" label="Schema" data={schemas} value={schema} onChange={(v) => setSchema(v ?? '')} disabled={ext.relocatable === false && !ext.installedVersion && false} searchable />
            <Select size="xs" label="Version" data={ext.versions ?? []} value={version} onChange={(v) => setVersion(v ?? '')} />
          </Group>
        )}
        {form.mode === 'update' && (
          <Select size="xs" mt="sm" label="Version" data={(ext.versions ?? []).filter((v) => v !== ext.installedVersion)} value={version} onChange={(v) => setVersion(v ?? '')} />
        )}
        {form.mode === 'install' && (
          <Checkbox size="xs" mt="sm" checked={cascade} onChange={(e) => setCascade(e.currentTarget.checked)}
            label={<>Also install what it depends on (<Code fz={11}>CASCADE</Code>)</>} />
        )}
        {form.mode === 'remove' && (
          <>
            <TextInput size="xs" mt="sm" label={`Type ${ext.name} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
            <Checkbox size="xs" mt="sm" checked={cascade} onChange={(e) => setCascade(e.currentTarget.checked)}
              label={<>Also drop everything that depends on it (<Code fz={11}>CASCADE</Code>). Leave this off first: Postgres then lists what depends on it and nothing is dropped.</>} />
          </>
        )}
        <Text size={FS.micro} c="dimmed" mt="md">This statement runs, as the server’s postgres account, and is recorded in this session</Text>
        <Code block mt={4} fz={12}>{preview.sql || (preview.error ? `— ${preview.error.error}` : '…')}</Code>
        {failure && <Box mt="sm"><AdminRefused error={failure} /></Box>}
        <Group mt="md" gap="xs">
          <Button size="xs" color={form.mode === 'remove' ? 'rose' : undefined} onClick={() => void run()} disabled={!canRun} loading={busy}>{verb}</Button>
          <Button size="xs" variant="default" onClick={onCancel}>Cancel</Button>
        </Group>
      </Box>
    </ScrollArea>
  )
}
