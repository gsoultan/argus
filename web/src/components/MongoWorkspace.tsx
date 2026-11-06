import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Drawer, Group, Loader, ScrollArea, Stack, Text, TextInput, UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import { IconLeaf, IconMenu2, IconPlayerStop, IconRefresh, IconSearch } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import { JsonView } from '~/components/JsonView'
import { ConfirmByName, FatalBanner, StoreHeader } from '~/components/StoreChrome'
import { useStoreSession, type RedisKey, type StoreAnswer } from '~/components/useStoreSession'

/*
MongoDB in the browser: the collections on the left, the selected one's first
documents at the top, and a command line below that takes what mongosh takes
for one command -- db.orders.find({ status: "paid" }).limit(20), or a command
document such as { dbStats: 1 }.

Nothing here decides what the person may do: their MongoDB user's roles for
their level do, and a refusal is MongoDB's own, shown as it came. The console
adds only the confirm before a command that wipes a collection or the
database, which the agent will run only with the server's name typed.
*/

export interface MongoWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
}

const kindBadge = { variant: 'outline', color: 'slate', radius: 'sm' } as const

interface Line { id: number; cmd: string; out: string; error?: boolean; ms?: number; tag?: string }

/** The documents an answer carries, as JSON text each. */
export function mongoDocs(a: StoreAnswer | null): unknown[] {
  return Array.isArray(a?.data) ? a!.data as unknown[] : []
}

/** How a command line answer reads in the terminal. */
export function formatMongo(a: StoreAnswer): string {
  const docs = mongoDocs(a)
  const body = docs.map((d) => JSON.stringify(d, null, 2)).join('\n')
  const more = a.truncated ? '\n… more documents than shown; narrow the filter or add .limit()' : ''
  if (a.type !== 'result') return `(refused) ${a.error ?? 'failed'}${body ? `\n${body}` : ''}`
  return (body || '(no documents)') + more
}

function countText(n: number | undefined): string {
  if (n === undefined) return ''
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

export function MongoWorkspace({ database, access, getTicket, endpoint, onAskForAccess }: MongoWorkspaceProps) {
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const [listOpen, listDrawer] = useDisclosure(false)
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level

  // Collections.
  const [filter, setFilter] = useState('')
  const [colls, setColls] = useState<RedisKey[]>([])
  const [listing, setListing] = useState(false)
  const [listErr, setListErr] = useState<string | null>(null)
  const list = useCallback(async () => {
    setListing(true)
    const a = await s.ask({ op: 'scan' })
    setListing(false)
    if (a.type !== 'result') { setListErr(a.error ?? 'could not list collections'); return }
    setListErr(null)
    setColls(a.keys ?? [])
  }, [s])
  useEffect(() => {
    if (s.status === 'ready') void list()
    // Only when the session becomes ready; later refreshes are the person's.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.status])
  const shown = useMemo(() => {
    const n = filter.trim().toLowerCase()
    return n ? colls.filter((c) => c.name.toLowerCase().includes(n)) : colls
  }, [colls, filter])

  // The selected collection's first documents.
  const [selected, setSelected] = useState<RedisKey | null>(null)
  const [docs, setDocs] = useState<StoreAnswer | null>(null)
  const open = useCallback(async (c: RedisKey) => {
    setSelected(c)
    setDocs(null)
    if (narrow) listDrawer.close()
    setDocs(await s.ask({ op: 'value', key: c.name, count: 50 }))
  }, [s, narrow, listDrawer])

  // The command line.
  const [cmd, setCmd] = useState('')
  const [lines, setLines] = useState<Line[]>([])
  const [running, setRunning] = useState<number | null>(null)
  const [hist, setHist] = useState<string[]>([])
  const [histAt, setHistAt] = useState(-1)
  const [confirming, setConfirming] = useState<{ cmd: string; why: string } | null>(null)
  const outRef = useRef<HTMLDivElement | null>(null)

  const runCmd = useCallback(async (text: string, confirm?: string) => {
    const c = text.trim()
    if (!c) return
    const a = s.ask({ op: 'run', command: c, ...(confirm ? { confirm } : {}) })
    const id = s.lastId()
    setRunning(id)
    const ans = await a
    setRunning(null)
    if (ans.needsConfirm) {
      setConfirming({ cmd: c, why: ans.error ?? '' })
      return
    }
    setLines((l) => [...l, { id, cmd: c, out: formatMongo(ans), error: ans.type !== 'result', ms: ans.ms, tag: ans.tag }].slice(-200))
    requestAnimationFrame(() => { const el = outRef.current; if (el) el.scrollTop = el.scrollHeight })
  }, [s])

  const submit = () => {
    const c = cmd.trim()
    if (!c || running !== null) return
    setHist((h) => [c, ...h.filter((x) => x !== c)].slice(0, 50))
    setHistAt(-1)
    setCmd('')
    void runCmd(c)
  }
  const onKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') { e.preventDefault(); submit() }
    if (e.key === 'ArrowUp' && hist.length) {
      e.preventDefault()
      const i = Math.min(histAt + 1, hist.length - 1)
      setHistAt(i); setCmd(hist[i]!)
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      const i = histAt - 1
      setHistAt(Math.max(i, -1)); setCmd(i >= 0 ? hist[i]! : '')
    }
  }

  const browser = (
    <Stack gap={SP.cozy} p="xs" h="100%">
      <Group gap={6} wrap="nowrap">
        <TextInput size="xs" value={filter} onChange={(e) => setFilter(e.currentTarget.value)} style={{ flex: 1 }}
          leftSection={<IconSearch size={12} />} placeholder="Find a collection" aria-label="Find a collection" />
        <ActionIcon variant="default" onClick={() => void list()} aria-label="List collections again" loading={listing}>
          <IconRefresh size={14} />
        </ActionIcon>
      </Group>
      <Text size={FS.micro} c="dimmed">
        {colls.length ? `${colls.length} collection${colls.length === 1 ? '' : 's'} you can see in ${database.dbname}` : 'Collections'}
      </Text>
      {listErr && <Text size={FS.micro} c="rose">{listErr}</Text>}
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap={0}>
          {shown.map((c) => (
            <UnstyledButton key={c.name} onClick={() => void open(c)} px={6} py={4}
              style={{ borderRadius: 6, background: selected?.name === c.name ? 'var(--color-brand-soft, var(--mantine-color-azure-light))' : undefined }}>
              <Group gap={6} wrap="nowrap">
                <Text size={FS.meta} ff="monospace" truncate style={{ flex: 1 }} title={c.name}>{c.name}</Text>
                {c.type !== 'collection' && <Badge size="xs" {...kindBadge}>{c.type}</Badge>}
                {c.count !== undefined && <Text size={FS.micro} c="dimmed" title={`about ${c.count.toLocaleString()} documents`}>≈{countText(c.count)}</Text>}
              </Group>
            </UnstyledButton>
          ))}
          {shown.length === 0 && !listing && <Text size={FS.meta} c="dimmed" p={6}>{filter ? 'Nothing matches.' : 'No collections yet.'}</Text>}
        </Stack>
      </ScrollArea>
    </Stack>
  )

  const prompt = `${database.hostname.split('.')[0]}:${database.dbname}>`

  return (
    <Box style={{ display: 'flex', height: narrow ? 'auto' : 'calc(100dvh - 120px)', minHeight: 480 }}>
      {narrow ? (
        <Drawer opened={listOpen} onClose={listDrawer.close} size="85%" padding="xs" title="Collections">{browser}</Drawer>
      ) : (
        <Box w={280} style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', overflow: 'hidden' }}>{browser}</Box>
      )}
      <Box style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <StoreHeader database={database} access={access} level={level} status={s.status} narrow={narrow}
          onAskForAccess={onAskForAccess} icon={<IconLeaf size={16} color="var(--color-brand)" />}
          title={`MongoDB · ${database.dbname}`}
          detail={`${database.hostname}${s.ready?.version ? ` · MongoDB ${s.ready.version}` : ''}${s.ready?.user ? ` · as ${s.ready.user}` : ''}`}
          left={narrow ? <ActionIcon variant="subtle" onClick={listDrawer.open} aria-label="Open the collections"><IconMenu2 size={18} /></ActionIcon> : undefined} />
        <FatalBanner fatal={s.fatal} />

        <Box style={{ flex: '1 1 50%', minHeight: narrow ? 220 : 0, overflow: 'auto', borderBottom: '1px solid var(--color-line)' }} p="md">
          <DocsView selected={selected} docs={docs} onReload={selected ? () => void open(selected) : undefined} />
        </Box>

        <Box style={{ flex: '1 1 50%', minHeight: narrow ? 260 : 0, display: 'flex', flexDirection: 'column', background: 'var(--color-surface-sunken, #0f1115)' }}>
          <Box ref={outRef} style={{ flex: 1, overflow: 'auto', padding: 12, fontFamily: 'var(--font-mono)', fontSize: 12.5, lineHeight: '19px', color: '#d6dae1' }}>
            {lines.length === 0 && (
              <div style={{ color: '#8b93a1' }}>
                Commands run as your own MongoDB user at {level || '…'}. Every one is recorded before it runs.
                Try <span style={{ color: '#d6dae1' }}>db.{colls[0]?.name ?? 'orders'}.find({'{}'}).limit(10)</span> or{' '}
                <span style={{ color: '#d6dae1' }}>{'{ dbStats: 1 }'}</span>.
              </div>
            )}
            {lines.map((l) => (
              <div key={l.id} style={{ marginBottom: 8 }}>
                <div><span style={{ color: '#6aa9ff' }}>{prompt}</span> {l.cmd}</div>
                {(l.tag || l.ms !== undefined) && (
                  <div style={{ color: '#8b93a1' }}>{[l.tag, l.ms !== undefined ? `${l.ms} ms` : ''].filter(Boolean).join(' · ')}</div>
                )}
                <pre style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', color: l.error ? '#ff8a8a' : undefined }}>{l.out}</pre>
              </div>
            ))}
          </Box>
          <Group gap={6} p={8} wrap="nowrap" style={{ borderTop: '1px solid #262a33' }}>
            <TextInput value={cmd} onChange={(e) => setCmd(e.currentTarget.value)} onKeyDown={onKey} style={{ flex: 1 }}
              placeholder={narrow ? 'db.orders.find({})' : 'db.orders.find({ status: "paid" }).limit(20)   (Enter runs, ↑ for history)'}
              aria-label="MongoDB command" spellCheck={false} autoCapitalize="off" autoCorrect="off" disabled={s.status !== 'ready'}
              styles={{ input: { fontFamily: 'var(--font-mono)', background: '#171a21', color: '#e7eaf0', borderColor: '#2b303b' } }} />
            {running !== null ? (
              <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} onClick={() => s.cancel(running)}>Cancel</Button>
            ) : (
              <Button size="xs" onClick={submit} disabled={s.status !== 'ready' || !cmd.trim()}>Run</Button>
            )}
          </Group>
        </Box>
      </Box>
      <ConfirmByName opened={confirming !== null} hostname={database.hostname}
        what={confirming ? `${confirming.cmd} on ${database.hostname}.` : ''}
        onCancel={() => {
          if (confirming) setLines((l) => [...l, { id: Date.now(), cmd: confirming.cmd, out: '(not run) you cancelled the confirm', error: true }])
          setConfirming(null)
        }}
        onConfirm={(typed) => { const c = confirming; setConfirming(null); if (c) void runCmd(c.cmd, typed) }} />
    </Box>
  )
}

function DocsView({ selected, docs, onReload }: { selected: RedisKey | null; docs: StoreAnswer | null; onReload?: () => void }) {
  if (!selected) {
    return <Text size={FS.meta} c="dimmed">Pick a collection on the left to see its first documents. Opening one is recorded, like a command.</Text>
  }
  if (!docs) return <Loader size="sm" />
  const list = mongoDocs(docs)
  const head = (
    <Group gap="xs" mb="sm" wrap="wrap">
      <Text fw={650} ff="monospace" size={FS.body} style={{ wordBreak: 'break-all' }}>{selected.name}</Text>
      {selected.type !== 'collection' && <Badge size="sm" {...kindBadge}>{selected.type}</Badge>}
      <Text size={FS.micro} c="dimmed">
        {docs.total !== undefined ? `${docs.total.toLocaleString()} documents` : ''}
        {docs.truncated ? ` · showing the first ${list.length}` : ''}
        {docs.ms !== undefined ? ` · ${docs.ms} ms` : ''}
      </Text>
      {onReload && <Button size="compact-xs" variant="subtle" onClick={onReload}>Reload</Button>}
    </Group>
  )
  if (docs.type !== 'result') return <>{head}<Text size={FS.meta} c="rose">{docs.error}</Text></>
  if (list.length === 0) return <>{head}<Text size={FS.meta} c="dimmed">No documents.</Text></>
  return <>{head}<JsonView raw={JSON.stringify(list)} column={selected.name} /></>
}
