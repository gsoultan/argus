import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Code, Group, Modal, Radio, Stack, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconAlertTriangle, IconLock, IconSearch } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import {
  SETTING_GROUPS, settingWarning, type RedisConfigData, type RedisConfigResult, type RedisSetting,
} from '~/lib/redisManage'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'

/*
Redis's settings as it runs them, beside what redis.conf says. A setting that
differs from the file is one a restart would undo, and is marked. Changes are
drafted, reviewed together, and applied with CONFIG SET; optionally also
written to redis.conf with CONFIG REWRITE. Settings that move files, ports or
passwords are locked: they are changed on the server, not from here.
*/

export function RedisConfig({ s, canSet, change }: {
  s: StoreSession
  canSet: boolean
  change: (req: StoreRequest) => Promise<ChangeAnswer>
}) {
  const [cfg, setCfg] = useState<RedisConfigData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [review, setReview] = useState(false)
  const [rewrite, setRewrite] = useState<'running' | 'file'>('file')
  const [applying, setApplying] = useState(false)
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null)

  const load = useCallback(async () => {
    const a = await s.ask({ op: 'config_get' })
    const d = dataOf<RedisConfigData>(a)
    if (!d) setErr(a.error ?? 'could not read the configuration')
    else { setErr(null); setCfg(d) }
  }, [s])
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  const byName = useMemo(() => new Map((cfg?.settings ?? []).map((x) => [x.name, x])), [cfg])
  const changes = Object.entries(draft).filter(([k, v]) => byName.get(k)?.value !== v)
  const drifted = (cfg?.settings ?? []).filter((x) => x.drift)

  const apply = async (items: [string, string][], rw: boolean) => {
    setApplying(true)
    setResult(null)
    const a = await change({ op: 'config_set', config: items.map(([name, value]) => ({ name, value })), rewrite: rw })
    setApplying(false)
    setReview(false)
    if (a.notApproved) { setResult({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = dataOf<RedisConfigResult>(a)
    if (!r) { setResult({ ok: false, text: a.error ?? 'failed' }); return }
    setDraft({})
    setResult({
      ok: !r.rewriteError,
      text: `${items.length} setting${items.length === 1 ? '' : 's'} changed in the running Redis.${rw ? (r.rewritten ? ' redis.conf was rewritten.' : '') : ' redis.conf was left as it is, so a restart undoes it.'}${r.rewriteError ? ` Writing redis.conf failed: ${r.rewriteError}` : ''}`,
    })
    void load()
  }

  if (!cfg) return <Box p="md">{err ? <Alert color="rose">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the configuration…</Text>}</Box>

  const match = (x: RedisSetting) => !q || x.name.includes(q.toLowerCase())
  const groups = SETTING_GROUPS.map((g) => ({
    ...g,
    rows: cfg.settings.filter((x) => (g.id === 'locked' ? !x.settable : x.settable && x.group === g.id) && match(x)),
  })).filter((g) => g.rows.length > 0)

  return (
    <Stack gap="md" p="md">
      <Group justify="space-between" wrap="wrap">
        <TextInput size="xs" leftSection={<IconSearch size={12} />} placeholder="Find a setting" value={q} onChange={(e) => setQ(e.currentTarget.value)} w={260} aria-label="Find a setting" />
        <Text size={FS.micro} c="dimmed">
          {cfg.file ? `${cfg.file}${cfg.fileRead ? '' : ' (could not be read, so differences are not shown)'}` : 'Redis was started without a config file, so CONFIG REWRITE cannot save changes'}
        </Text>
      </Group>
      {result && <Alert color={result.ok ? 'teal' : 'rose'} withCloseButton onClose={() => setResult(null)}>{result.text}</Alert>}
      {drifted.length > 0 && (
        <Alert color="rose" icon={<IconAlertTriangle size={16} />} title={`${drifted.length} setting${drifted.length === 1 ? ' differs' : 's differ'} from redis.conf`}>
          <Text size={FS.meta}>A restart would undo {drifted.length === 1 ? 'it' : 'them'}: {drifted.map((x) => x.name).join(', ')}.</Text>
          {canSet && cfg.file && (
            <Button mt={6} size="compact-xs" variant="default" loading={applying}
              onClick={() => void apply(drifted.filter((x) => x.settable).map((x) => [x.name, x.value]), true)}>
              Keep the running values: write them to redis.conf
            </Button>
          )}
        </Alert>
      )}
      {groups.map((g) => (
        <SectionCard key={g.id} title={g.label} icon={g.id === 'locked' ? IconLock : undefined}
          description={g.id === 'locked' ? 'Files, ports, TLS and passwords: changed on the server itself' : undefined} flush>
          <Table verticalSpacing={4} fz="xs">
            <Table.Thead>
              <Table.Tr><Table.Th w="30%">Setting</Table.Th><Table.Th>Running</Table.Th><Table.Th w="22%">redis.conf</Table.Th><Table.Th w="14%">Default</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {g.rows.map((x) => {
                const d = draft[x.name]
                const warn = d !== undefined && d !== x.value ? settingWarning(x.name, d) : null
                return (
                  <Table.Tr key={x.name}>
                    <Table.Td ff="monospace">
                      <Group gap={4} wrap="nowrap">
                        {x.name}
                        {x.drift && <Tooltip label="Differs from redis.conf: a restart would undo it"><Badge size="xs" color="rose" variant="light">differs</Badge></Tooltip>}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      {canSet && x.settable && !x.secret ? (
                        <TextInput size="xs" value={d ?? x.value} aria-label={x.name}
                          error={warn ?? undefined}
                          styles={{ input: { fontFamily: 'var(--font-mono)', fontWeight: d !== undefined && d !== x.value ? 650 : undefined } }}
                          onChange={(e) => { const v = e.currentTarget.value; setDraft((m) => ({ ...m, [x.name]: v })) }} />
                      ) : <Text size="xs" ff="monospace">{x.secret ? (x.set ? '(set)' : '(not set)') : x.value || '""'}</Text>}
                    </Table.Td>
                    <Table.Td ff="monospace" c="dimmed">{x.secret ? '' : x.inFile ? (x.file || '""') : '—'}</Table.Td>
                    <Table.Td ff="monospace" c="dimmed">{x.default ?? ''}</Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </SectionCard>
      ))}
      {changes.length > 0 && (
        <Box p="sm" style={{ position: 'sticky', bottom: 0, border: '1px solid var(--color-line)', borderRadius: 8, background: 'var(--mantine-color-body)' }}>
          <Group justify="space-between">
            <Text size={FS.meta}>{changes.length} setting{changes.length === 1 ? '' : 's'} changed, not applied yet</Text>
            <Group gap={6}>
              <Button size="xs" variant="default" onClick={() => setDraft({})}>Discard</Button>
              <Button size="xs" onClick={() => setReview(true)}>Review</Button>
            </Group>
          </Group>
        </Box>
      )}
      <Modal opened={review} onClose={() => setReview(false)} title="Apply configuration changes" size="lg">
        <Stack gap="sm">
          <Table verticalSpacing={4} fz="xs" withTableBorder>
            <Table.Thead><Table.Tr><Table.Th>Setting</Table.Th><Table.Th>Now</Table.Th><Table.Th>New</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {changes.map(([k, v]) => (
                <Table.Tr key={k}>
                  <Table.Td ff="monospace">{k}</Table.Td>
                  <Table.Td ff="monospace" c="dimmed">{byName.get(k)?.value || '""'}</Table.Td>
                  <Table.Td ff="monospace">{v || '""'}{settingWarning(k, v) && <Text size={FS.micro} c="amber">{settingWarning(k, v)}</Text>}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          <Radio.Group value={rewrite} onChange={(v) => setRewrite(v as 'running' | 'file')}>
            <Stack gap={SP.cozy}>
              <Radio value="file" disabled={!cfg.file} label="The running Redis and redis.conf" description="CONFIG SET, then CONFIG REWRITE so a restart keeps it" />
              <Radio value="running" label="The running Redis only" description="A restart goes back to redis.conf; it will show as differing until then" />
            </Stack>
          </Radio.Group>
          <Code block>{changes.map(([k, v]) => `CONFIG SET ${k} ${v || '""'}`).join('\n')}{rewrite === 'file' && cfg.file ? '\nCONFIG REWRITE' : ''}</Code>
          {s.ready?.changesNeedApproval && <Text size={FS.micro} c="dimmed">This database's changes wait for someone else to approve exactly these commands.</Text>}
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={() => setReview(false)}>Cancel</Button>
            <Button size="xs" loading={applying} onClick={() => void apply(changes, rewrite === 'file' && !!cfg.file)}>Apply</Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  )
}
