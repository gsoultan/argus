import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Drawer, Group, Loader, ScrollArea, Select, Stack, Table, Text, TextInput,
  UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import { IconBolt, IconMenu2, IconPlayerStop, IconPlus, IconRefresh, IconSearch } from '@tabler/icons-react'
import { redisArg } from '~/lib/redisArg'
import { FS, SP } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import { useCompletion } from '~/components/Autocomplete'
import { completeRedis } from '~/lib/redisComplete'
import { ConfirmByName, FatalBanner, StoreHeader } from '~/components/StoreChrome'
import { formatRedis, plain, useStoreSession, type RedisKey, type RedisValue, type StoreAnswer } from '~/components/useStoreSession'

/*
Redis in the browser: a key browser on the left (SCAN, never KEYS), the
selected key's value at the top, and a command line like redis-cli below it.

Nothing here decides what the person may do: Redis's ACL user for their level
does, and a refusal is Redis's own NOPERM, shown as it came. The one thing the
console adds is asking for the server's name before a command the agent will
only run confirmed.
*/

const NewKey = lazy(() => import('~/components/redis/RedisNewKey').then((m) => ({ default: m.NewKey })))

export interface RedisWorkspaceProps {
  database: Database
  access: DBAccess
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
}

// Key types are not findings, so they stay neutral: the semantic colours
// are kept for verified, pending, denied and live.
const typeBadge = { variant: 'outline', color: 'slate', radius: 'sm' } as const

interface Line { id: number; cmd: string; out: string; error?: boolean; ms?: number }

export function RedisWorkspace({ database, access, getTicket, endpoint, onAskForAccess }: RedisWorkspaceProps) {
  const narrow = useMediaQuery('(max-width: 48em)') ?? false
  const [keysOpen, keysDrawer] = useDisclosure(false)
  const s = useStoreSession(database, getTicket, endpoint)
  const level = s.ready?.level ?? access.level

  // Key browser.
  const [pattern, setPattern] = useState('*')
  const [type, setType] = useState<string | null>(null)
  const [keys, setKeys] = useState<RedisKey[]>([])
  const [cursor, setCursor] = useState<string>('0')
  const [total, setTotal] = useState<number | undefined>()
  const [scanning, setScanning] = useState(false)
  const [scanErr, setScanErr] = useState<string | null>(null)

  const scan = useCallback(async (from: string, append: boolean) => {
    setScanning(true)
    // A few SCAN steps until a page is full or the walk ends, so a sparse
    // pattern does not show an empty first page.
    let c = from
    const found: RedisKey[] = []
    for (let step = 0; step < 10; step++) {
      const a = await s.ask({ op: 'scan', cursor: c, pattern, type: type ?? '', count: 200 })
      if (a.type !== 'result') {
        setScanErr(a.error ?? 'the scan failed')
        setScanning(false)
        return
      }
      if (c === '0' && a.total !== undefined) setTotal(a.total)
      found.push(...(a.keys ?? []))
      c = a.cursor ?? '0'
      if (c === '0' || found.length >= 100) break
    }
    setScanErr(null)
    setKeys((k) => (append ? [...k, ...found] : found))
    setCursor(c)
    setScanning(false)
  }, [s, pattern, type])

  useEffect(() => {
    if (s.status === 'ready') void scan('0', false)
    // Only when the session becomes ready; later scans are the person's.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.status])

  // The selected key's value.
  const [selected, setSelected] = useState<RedisKey | null>(null)
  const [value, setValue] = useState<StoreAnswer | null>(null)
  const open = useCallback(async (k: RedisKey) => {
    setSelected(k)
    setValue(null)
    if (narrow) keysDrawer.close()
    setValue(await s.ask({ op: 'value', key: k.name, keyQ: k.q ?? false, count: 200 }))
  }, [s, narrow, keysDrawer])

  // The command line.
  const [cmd, setCmd] = useState('')
  const [lines, setLines] = useState<Line[]>([])
  const [running, setRunning] = useState<number | null>(null)
  const [hist, setHist] = useState<string[]>([])
  const [histAt, setHistAt] = useState(-1)
  const [confirming, setConfirming] = useState<{ cmd: string; why: string } | null>(null)
  const outRef = useRef<HTMLDivElement | null>(null)

  const runCmd = useCallback(async (text: string, confirm?: string): Promise<boolean> => {
    const c = text.trim()
    if (!c) return false
    const a = s.ask({ op: 'run', command: c, ...(confirm ? { confirm } : {}) })
    const id = s.lastId()
    setRunning(id)
    const ans = await a
    setRunning(null)
    if (ans.needsConfirm) {
      setConfirming({ cmd: c, why: ans.error ?? '' })
      return false
    }
    const out = ans.type === 'result' ? formatRedis(ans.value) : `(refused) ${ans.error ?? 'failed'}`
    const bad = ans.type !== 'result' || ans.value?.t === 'e'
    setLines((l) => [...l, { id, cmd: c, out, error: bad, ms: ans.ms }].slice(-200))
    requestAnimationFrame(() => { const el = outRef.current; if (el) el.scrollTop = el.scrollHeight })
    return !bad
  }, [s])
  const [making, setMaking] = useState(false)
  const keyExists = useCallback(async (k: string) => {
    const a = await s.ask({ op: 'run', command: `EXISTS ${redisArg(k)}` })
    return a.type === 'result' && a.value?.t === 'i' && (a.value.i ?? 0) > 0
  }, [s])

  const submit = () => {
    const c = cmd.trim()
    if (!c || running !== null) return
    setHist((h) => [c, ...h.filter((x) => x !== c)].slice(0, 50))
    setHistAt(-1)
    setCmd('')
    void runCmd(c)
  }
  // Autocomplete: commands, subcommands, and the key names the browser on
  // the left has already listed. Nothing extra is fetched for it.
  const cmdRef = useRef<HTMLInputElement | null>(null)
  const keyNames = useMemo(() => keys.map((k) => k.name), [keys])
  const completeHere = useCallback((t: string, c: number, explicit: boolean) =>
    completeRedis(t, c, keyNames, explicit), [keyNames])
  const completion = useCompletion({ ref: cmdRef, value: cmd, setValue: setCmd, complete: completeHere })

  const onKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    // An open list takes Enter and the arrows; history needs it closed.
    if (completion.onKeyDown(e)) return
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
        <TextInput size="xs" value={pattern} onChange={(e) => setPattern(e.currentTarget.value)} style={{ flex: 1 }}
          leftSection={<IconSearch size={12} />} placeholder="user:*" aria-label="Key pattern"
          onKeyDown={(e) => { if (e.key === 'Enter') void scan('0', false) }} />
        <ActionIcon variant="default" onClick={() => void scan('0', false)} aria-label="Find keys" loading={scanning}>
          <IconRefresh size={14} />
        </ActionIcon>
      </Group>
      <Group gap={6} wrap="nowrap">
        <Select size="xs" placeholder="Every type" clearable value={type} onChange={setType} aria-label="Key type" style={{ flex: 1 }}
          data={['string', 'hash', 'list', 'set', 'zset', 'stream']} />
        {s.status === 'ready' && level !== 'read' && (
          <Button size="xs" variant="default" leftSection={<IconPlus size={12} />} onClick={() => setMaking(true)}>Key</Button>
        )}
      </Group>
      <Text size={FS.micro} c="dimmed">
        {total !== undefined ? `${total.toLocaleString()} keys in db ${database.dbname}` : 'Keys'} · found with SCAN, so Redis never stops for it
      </Text>
      {scanErr && <Text size={FS.micro} c="rose">{scanErr}</Text>}
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Stack gap={0}>
          {keys.map((k) => (
            <UnstyledButton key={(k.q ? 'q:' : '') + k.name} onClick={() => void open(k)} px={6} py={4}
              style={{ borderRadius: 6, background: selected?.name === k.name ? 'var(--color-brand-soft, var(--mantine-color-azure-light))' : undefined }}>
              <Group gap={6} wrap="nowrap">
                <Badge size="xs" {...typeBadge} w={52}>{k.type}</Badge>
                <Text size={FS.meta} ff="monospace" truncate style={{ flex: 1 }} title={k.name}>{k.name}</Text>
                {k.ttl >= 0 && <Text size={FS.micro} c="dimmed">{ttlText(k.ttl)}</Text>}
              </Group>
            </UnstyledButton>
          ))}
          {keys.length === 0 && !scanning && <Text size={FS.meta} c="dimmed" p={6}>No keys match.</Text>}
        </Stack>
        {cursor !== '0' && (
          <Button size="compact-xs" variant="subtle" mt={6} onClick={() => void scan(cursor, true)} loading={scanning}>
            More
          </Button>
        )}
      </ScrollArea>
    </Stack>
  )

  return (
    <Box style={{ display: 'flex', height: narrow ? 'auto' : 'calc(100dvh - 120px)', minHeight: 480 }}>
      {narrow ? (
        <Drawer opened={keysOpen} onClose={keysDrawer.close} size="85%" padding="xs" title="Keys">{browser}</Drawer>
      ) : (
        <Box w={300} style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', overflow: 'hidden' }}>{browser}</Box>
      )}
      <Box style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <StoreHeader database={database} access={access} level={level} status={s.status} narrow={narrow}
          onAskForAccess={onAskForAccess} icon={<IconBolt size={16} color="var(--color-brand)" />}
          title={`Redis · db ${database.dbname}`}
          detail={`${database.hostname}${s.ready?.version ? ` · ${s.ready.server === 'valkey' ? 'Valkey' : 'Redis'} ${s.ready.version}` : ''}${s.ready?.user ? ` · as ${s.ready.user}` : ''}`}
          left={narrow ? <ActionIcon variant="subtle" onClick={keysDrawer.open} aria-label="Open the key browser"><IconMenu2 size={18} /></ActionIcon> : undefined} />
        <FatalBanner fatal={s.fatal} />

        <Box style={{ flex: '1 1 50%', minHeight: narrow ? 200 : 0, overflow: 'auto', borderBottom: '1px solid var(--color-line)' }} p="md">
          <ValueView selected={selected} value={value} onReload={selected ? () => void open(selected) : undefined} />
        </Box>

        <Box style={{ flex: '1 1 50%', minHeight: narrow ? 260 : 0, display: 'flex', flexDirection: 'column', background: 'var(--color-surface-sunken, #0f1115)' }}>
          <Box ref={outRef} style={{ flex: 1, overflow: 'auto', padding: 12, fontFamily: 'var(--font-mono)', fontSize: 12.5, lineHeight: '19px', color: '#d6dae1' }}>
            {lines.length === 0 && (
              <div style={{ color: '#8b93a1' }}>
                Commands run as your own Redis user at {level || '…'}. Every one is recorded before it runs.
                Try <span style={{ color: '#d6dae1' }}>GET user:1</span> or <span style={{ color: '#d6dae1' }}>TTL user:1</span>.
              </div>
            )}
            {lines.map((l) => (
              <div key={l.id} style={{ marginBottom: 6 }}>
                <div><span style={{ color: '#6aa9ff' }}>{database.hostname.split('.')[0]}[{database.dbname}]&gt;</span> {l.cmd}</div>
                <pre style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: l.error ? '#ff8a8a' : undefined }}>{l.out}</pre>
              </div>
            ))}
          </Box>
          <Group gap={6} p={8} wrap="nowrap" style={{ borderTop: '1px solid #262a33' }}>
            <TextInput ref={cmdRef} {...completion.editorProps} value={cmd} onChange={(e) => { completion.onTyped(); setCmd(e.currentTarget.value) }} onKeyDown={onKey} style={{ flex: 1 }}
              placeholder="GET user:1   (Enter runs, ↑ for history, Ctrl+Space suggests)" aria-label="Redis command" spellCheck={false}
              autoCapitalize="off" autoCorrect="off" disabled={s.status !== 'ready'}
              styles={{ input: { fontFamily: 'var(--font-mono)', background: '#171a21', color: '#e7eaf0', borderColor: '#2b303b' } }} />
            {running !== null ? (
              <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} onClick={() => s.cancel(running)}>Cancel</Button>
            ) : (
              <Button size="xs" onClick={submit} disabled={s.status !== 'ready' || !cmd.trim()}>Run</Button>
            )}
            {completion.popup}
          </Group>
        </Box>
      </Box>
      {making && (
        <Suspense fallback={null}>
          <NewKey opened onClose={() => { setMaking(false); void scan('0', false) }} exists={keyExists} onRun={(l) => runCmd(l)} />
        </Suspense>
      )}
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

function ttlText(s: number): string {
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${Math.round(s / 3600)}h`
  return `${Math.round(s / 86400)}d`
}

function ValueView({ selected, value, onReload }: { selected: RedisKey | null; value: StoreAnswer | null; onReload?: () => void }) {
  if (!selected) {
    return <Text size={FS.meta} c="dimmed">Pick a key on the left to see its value. Reading a value is recorded, like a command.</Text>
  }
  if (!value) return <Loader size="sm" />
  const head = (
    <Group gap="xs" mb="sm" wrap="wrap">
      <Text fw={650} ff="monospace" size={FS.body} style={{ wordBreak: 'break-all' }}>{selected.name}</Text>
      {value.keyType && <Badge size="sm" {...typeBadge}>{value.keyType}</Badge>}
      <Text size={FS.micro} c="dimmed">
        {value.ttl !== undefined && value.ttl >= 0 ? `expires in ${ttlText(value.ttl)}` : 'no expiry'}
        {value.total !== undefined && value.keyType !== 'string' ? ` · ${value.total.toLocaleString()} items` : ''}
        {value.truncated ? ' · showing the first ones' : ''}
        {value.ms !== undefined ? ` · ${value.ms} ms` : ''}
      </Text>
      {onReload && <Button size="compact-xs" variant="subtle" onClick={onReload}>Reload</Button>}
    </Group>
  )
  if (value.type !== 'result') return <>{head}<Text size={FS.meta} c="rose">{value.error}</Text></>
  if (value.error) return <>{head}<Text size={FS.meta} c="dimmed">{value.error}</Text></>
  const v = value.value
  const items = v?.a ?? []
  const pairs = (vs: RedisValue[]) => {
    const out: [RedisValue, RedisValue][] = []
    for (let i = 0; i + 1 < vs.length; i += 2) out.push([vs[i]!, vs[i + 1]!])
    return out
  }
  const mono = { fontFamily: 'var(--font-mono)', fontSize: 12.5, whiteSpace: 'pre-wrap' as const, wordBreak: 'break-all' as const }
  switch (value.keyType) {
    case 'string':
      return <>{head}<Box p="sm" style={{ ...mono, border: '1px solid var(--color-line)', borderRadius: 8 }}>{plain(v)}{v?.cut ? ' …(cut)' : ''}</Box></>
    case 'hash':
    case 'zset':
      return (
        <>{head}
          <Table withTableBorder verticalSpacing={4} fz="xs">
            <Table.Thead><Table.Tr><Table.Th>{value.keyType === 'hash' ? 'Field' : 'Member'}</Table.Th><Table.Th>{value.keyType === 'hash' ? 'Value' : 'Score'}</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {pairs(items).map(([a, b], i) => (
                <Table.Tr key={i}><Table.Td style={mono}>{plain(a)}</Table.Td><Table.Td style={mono}>{plain(b)}</Table.Td></Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </>
      )
    case 'list':
    case 'set':
      return (
        <>{head}
          <Table withTableBorder verticalSpacing={4} fz="xs">
            <Table.Tbody>
              {items.map((e, i) => (
                <Table.Tr key={i}>
                  {value.keyType === 'list' && <Table.Td w={60} c="dimmed">{i}</Table.Td>}
                  <Table.Td style={mono}>{plain(e)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </>
      )
    default:
      return <>{head}<Box style={mono}>{formatRedis(v)}</Box></>
  }
}
