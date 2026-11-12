import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Badge, Box, Button, Card, Group, SimpleGrid, Stack, Table, Text, UnstyledButton } from '@mantine/core'
import { IconAlertTriangle, IconDeviceFloppy, IconInfoCircle } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import { LineChart, Meter } from '~/components/broker/charts'
import { notifyOk } from '~/lib/notify'
import type { StoreSession } from '~/components/useStoreSession'
import { formatRedis } from '~/components/useStoreSession'
import { attention, bytesText, infoNum, keyspace, ttlText, type RedisInfoData } from '~/lib/redisManage'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'
import type { StoreRequest } from '~/components/useStoreSession'

/*
The Redis overview: INFO every five seconds while the tab is open, a short
history of memory and commands per second drawn from those polls, the
keyspace, what needs attention (each line says what it read), and the slow
log. Nothing is kept once the tab closes.
*/

const POLL_MS = 5000
const KEEP = 120

export function RedisOverview({ s, canSave, change, go }: {
  s: StoreSession
  canSave: boolean
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  go: (tab: 'config' | 'users') => void
}) {
  const [d, setD] = useState<RedisInfoData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [mem, setMem] = useState<number[]>([])
  const [ops, setOps] = useState<number[]>([])
  const [saving, setSaving] = useState<string | null>(null)
  const slowRef = useRef<HTMLDivElement | null>(null)

  const poll = useCallback(async () => {
    const a = await s.ask({ op: 'info' })
    const got = dataOf<RedisInfoData>(a)
    if (!got) { setErr(a.error ?? 'INFO failed'); return }
    setErr(null)
    setD(got)
    setMem((m) => [...m, infoNum(got.info, 'used_memory') ?? 0].slice(-KEEP))
    setOps((o) => [...o, infoNum(got.info, 'instantaneous_ops_per_sec') ?? 0].slice(-KEEP))
  }, [s])

  useEffect(() => {
    if (s.status !== 'ready') return
    void poll()
    const t = window.setInterval(() => { if (!document.hidden) void poll() }, POLL_MS)
    return () => window.clearInterval(t)
  }, [s.status, poll])

  const save = async (name: 'bgsave' | 'bgrewriteaof') => {
    setSaving(name)
    const a = await change({ op: 'save', name })
    setSaving(null)
    if (a.type === 'result') notifyOk(name === 'bgsave' ? 'Snapshot started' : 'AOF rewrite started', formatRedis(a.value))
    else if (!a.notApproved) setErr(a.error ?? 'failed')
  }

  if (!d) {
    return <Box p="md">{err ? <Alert color="rose" icon={<IconAlertTriangle size={16} />}>{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading INFO…</Text>}</Box>
  }
  const info = d.info
  const used = infoNum(info, 'used_memory')
  const max = infoNum(info, 'maxmemory') ?? 0
  const hits = infoNum(info, 'keyspace_hits') ?? 0
  const misses = infoNum(info, 'keyspace_misses') ?? 0
  const hitRate = hits + misses > 0 ? Math.round((hits / (hits + misses)) * 100) : undefined
  const lastSave = infoNum(info, 'rdb_last_save_time')
  const sinceSave = lastSave ? Math.max(0, Math.round(Date.now() / 1000 - lastSave)) : undefined
  const items = attention(d)
  const ks = keyspace(info)

  const kpis: { label: string; value: string; hint?: string }[] = [
    { label: 'Memory', value: bytesText(used), hint: max ? `of ${bytesText(max)} · ${info.maxmemory_policy}` : 'no maxmemory' },
    { label: 'Ops / sec', value: (infoNum(info, 'instantaneous_ops_per_sec') ?? 0).toLocaleString(), hint: `${(infoNum(info, 'connected_clients') ?? 0).toLocaleString()} clients` },
    { label: 'Hit rate', value: hitRate === undefined ? '—' : `${hitRate}%`, hint: `${(infoNum(info, 'evicted_keys') ?? 0).toLocaleString()} evicted · ${(infoNum(info, 'expired_keys') ?? 0).toLocaleString()} expired` },
    { label: 'Last save', value: sinceSave === undefined ? '—' : `${ttlText(sinceSave)} ago`, hint: `${info.aof_enabled === '1' ? 'AOF on' : 'AOF off'} · ${info.rdb_last_bgsave_status === 'ok' ? 'RDB ok' : `RDB ${info.rdb_last_bgsave_status ?? '?'}`}` },
    { label: 'Role', value: info.role ?? '—', hint: info.role === 'master' ? `${info.connected_slaves ?? 0} replica${info.connected_slaves === '1' ? '' : 's'}` : `link ${info.master_link_status ?? '?'}` },
    { label: 'Up', value: ttlText(infoNum(info, 'uptime_in_seconds') ?? 0), hint: `Redis ${info.redis_version ?? ''}` },
  ]

  return (
    <Stack gap="md" p="md">
      {err && <Alert color="amber" icon={<IconAlertTriangle size={16} />}>{err} (showing the last answer)</Alert>}
      <SimpleGrid cols={{ base: 2, sm: 3, lg: 6 }} spacing="sm">
        {kpis.map((k) => (
          <Card key={k.label} padding="sm">
            <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600}>{k.label}</Text>
            <Text size={FS.figure} fw={650} lh={1.2}>{k.value}</Text>
            {k.hint && <Text size={FS.micro} c="dimmed" truncate>{k.hint}</Text>}
            {k.label === 'Memory' && max > 0 && used !== undefined && <Box mt={4}><Meter value={used} max={max} mark={max * 0.8} color={used / max >= 0.8 ? 'amber' : 'teal'} /></Box>}
          </Card>
        ))}
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
        <SectionCard title="Needs attention" icon={IconAlertTriangle} iconColor={items.length ? 'amber' : 'teal'}
          badge={items.length ? <Badge size="sm" color="amber" variant="light">{items.length}</Badge> : undefined}>
          <Stack gap={SP.cozy} px="md" pb="md">
            {items.length === 0 && <Text size={FS.meta} c="dimmed">Nothing here: memory, saves, the config file, the default user and the slow log all look fine.</Text>}
            {items.map((a) => (
              <UnstyledButton key={a.title} onClick={() => {
                if (a.go === 'slowlog') slowRef.current?.scrollIntoView({ behavior: 'smooth' })
                else if (a.go) go(a.go)
              }} style={{ borderLeft: `3px solid var(--mantine-color-${a.tone}-6)`, paddingLeft: 10 }}>
                <Text size={FS.body} fw={600}>{a.title}</Text>
                <Text size={FS.meta} c="dimmed">{a.detail}</Text>
              </UnstyledButton>
            ))}
          </Stack>
        </SectionCard>
        <SectionCard title="Memory and load" description={`The last ${Math.round((mem.length * POLL_MS) / 60000) || 1} min, from this page's own polls`}
          action={canSave ? (
            <Group gap={6}>
              <Button size="compact-xs" variant="default" leftSection={<IconDeviceFloppy size={12} />} loading={saving === 'bgsave'} onClick={() => void save('bgsave')}>Save now</Button>
              {info.aof_enabled === '1' && <Button size="compact-xs" variant="default" loading={saving === 'bgrewriteaof'} onClick={() => void save('bgrewriteaof')}>Rewrite AOF</Button>}
            </Group>
          ) : undefined}>
          <Box px="md" pb="md">
            <LineChart minutes={Math.max(1, Math.round((mem.length * POLL_MS) / 60000))} height={150} series={[
              { label: 'Memory MB', values: mem.map((m) => Math.round(m / 1048576)), color: 'var(--mantine-color-azure-6)' },
              { label: 'Ops/s', values: ops, color: 'var(--mantine-color-teal-6)', dashed: true },
            ]} />
          </Box>
        </SectionCard>
      </SimpleGrid>

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
        <SectionCard title="Keyspace" flush>
          <Table verticalSpacing={4} fz="xs">
            <Table.Thead><Table.Tr><Table.Th>Database</Table.Th><Table.Th ta="right">Keys</Table.Th><Table.Th ta="right">With a TTL</Table.Th><Table.Th ta="right">Average TTL</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {ks.map((k) => (
                <Table.Tr key={k.db}>
                  <Table.Td>db{k.db}</Table.Td>
                  <Table.Td ta="right">{k.keys.toLocaleString()}</Table.Td>
                  <Table.Td ta="right">{k.keys ? `${Math.round((k.expires / k.keys) * 100)}%` : '—'}</Table.Td>
                  <Table.Td ta="right">{k.avgTtl ? ttlText(Math.round(k.avgTtl / 1000)) : '—'}</Table.Td>
                </Table.Tr>
              ))}
              {ks.length === 0 && <Table.Tr><Table.Td colSpan={4}><Text size={FS.meta} c="dimmed">No keys in any database.</Text></Table.Td></Table.Tr>}
            </Table.Tbody>
          </Table>
        </SectionCard>
        <div ref={slowRef}>
          <SectionCard title="Slow log" description={`Commands over slowlog-log-slower-than; the last ${d.slowlog.length}`} icon={IconInfoCircle} flush>
            <Table verticalSpacing={4} fz="xs">
              <Table.Tbody>
                {d.slowlog.slice(0, 10).map((e) => (
                  <Table.Tr key={e.id}>
                    <Table.Td w={70} ta="right" style={{ whiteSpace: 'nowrap' }}>{e.micros >= 1000 ? `${Math.round(e.micros / 1000)} ms` : `${e.micros} µs`}</Table.Td>
                    <Table.Td ff="monospace" style={{ wordBreak: 'break-all' }}>{e.command.slice(0, 6).join(' ')}{e.command.length > 6 ? ' …' : ''}</Table.Td>
                    <Table.Td c="dimmed">{e.client ?? ''}</Table.Td>
                  </Table.Tr>
                ))}
                {d.slowlog.length === 0 && <Table.Tr><Table.Td><Text size={FS.meta} c="dimmed">Empty.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </SectionCard>
        </div>
      </SimpleGrid>
    </Stack>
  )
}
