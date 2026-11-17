import { useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, Code, CopyButton, Group, Select, Stack, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconCheck, IconCopy, IconX } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { inPrefix, parseSource, type PlanStep, type ReachView } from '~/lib/redisManage'
import { reachCheck, type ReachCheckAnswer } from '~/lib/live'
import { assetName } from '~/lib/assetType'
import type { Asset, DBEngine } from '~/types/domain'

/*
The parts every engine's "which machines can connect" screen shares: the
list of machines let in, the plan of what the agent will change, and the
test from the client's side, through another Argus server's agent.
*/

export function Copy({ value, label }: { value: string; label: string }) {
  return (
    <CopyButton value={value}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copied' : label}>
          <ActionIcon variant="subtle" size="sm" onClick={copy} aria-label={label}>{copied ? <IconCheck size={12} /> : <IconCopy size={12} />}</ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  )
}

export function plural(n: number, one: string, many: string): string { return n === 1 ? one : many }

/** An Argus server, with one of its private IPv4 addresses. */
export interface KnownServer { id: string; name: string; address: string }

export function knownServers(assets: Asset[] | undefined, self: string): KnownServer[] {
  const out: KnownServer[] = []
  for (const a of assets ?? []) {
    if (a.id === self) continue
    for (const ad of a.addresses ?? []) {
      if (ad.family === 'ipv4' && !ad.public) out.push({ id: a.id, name: assetName(a), address: ad.address })
    }
  }
  return out
}

/** Argus servers on one of the networks the database's server is on. */
export function nearServers(n: ReachView, servers: KnownServer[], except: string[] = []): KnownServer[] {
  return servers.filter((k) => !except.includes(k.address) && n.addresses.some((a) => a.private && inPrefix(k.address, a.prefix)))
}

const STEP_MARK: Record<PlanStep['state'], { icon: string; color: string }> = {
  ok: { icon: '✓', color: 'teal' },
  change: { icon: '+', color: 'azure' },
  skip: { icon: '–', color: 'slate' },
  blocked: { icon: '✕', color: 'rose' },
  warn: { icon: '!', color: 'amber' },
}

/** What the agent will change, step by step. */
export function PlanSteps({ steps }: { steps: PlanStep[] }) {
  return (
    <Stack gap={0}>
      {steps.map((st, i) => (
        <Group key={i} gap="sm" wrap="nowrap" align="flex-start" py={8} style={{ borderTop: i ? '1px solid var(--color-line)' : undefined }}>
          <Badge size="sm" circle variant={st.state === 'skip' ? 'outline' : 'light'} color={STEP_MARK[st.state].color}>{STEP_MARK[st.state].icon}</Badge>
          <Box style={{ flex: 1, minWidth: 0 }}>
            <Text size={FS.meta} fw={600}>{st.title}</Text>
            {st.detail && <Text size={FS.micro} c="dimmed" lh={1.45}>{st.detail}</Text>}
            {st.diff && <Code block mt={4} style={{ fontSize: 11 }}>{`- ${st.diff.from}\n+ ${st.diff.to}`}</Code>}
            {st.lines && st.lines.length > 0 && <Code block mt={4} style={{ fontSize: 11, whiteSpace: 'pre-wrap' }}>{st.lines.join('\n')}</Code>}
          </Box>
        </Group>
      ))}
    </Stack>
  )
}

/** The machines let in: added by address or range, or picked from Argus's servers. */
export function SourcePicker({ n, sources, setSources, servers }: {
  n: ReachView
  sources: string[]
  setSources: (f: (x: string[]) => string[]) => void
  servers: KnownServer[]
}) {
  const [typed, setTyped] = useState('')
  const [typedErr, setTypedErr] = useState<string | null>(null)
  const [names, setNames] = useState<Record<string, string>>({})
  const add = (raw: string) => {
    const p = parseSource(raw)
    if ('error' in p) { setTypedErr(p.error); return }
    setTypedErr(null)
    setTyped('')
    setSources((x) => x.includes(p.source) ? x : [...x, p.source].slice(0, 32))
  }
  const suggest = nearServers(n, servers, sources)
  return (
    <Stack gap={6}>
      {sources.map((src) => {
        const k = servers.find((x) => x.address === src)
        return (
          <Group key={src} gap={SP.snug} wrap="nowrap" px={8} py={4} style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
            <Badge size="xs" variant="light" color={k ? 'azure' : 'slate'}>{k ? 'Argus server' : src.includes('/') ? 'Network' : 'Address'}</Badge>
            <Text size="xs" ff="monospace">{src}</Text>
            <TextInput size="xs" variant="unstyled" placeholder={k ? k.name : 'what is it? (optional)'} value={k ? k.name : names[src] ?? ''}
              readOnly={!!k} aria-label={`Name for ${src}`} style={{ flex: 1 }}
              onChange={(e) => { const v = e.currentTarget.value; setNames((m) => ({ ...m, [src]: v })) }} />
            <ActionIcon variant="subtle" size="sm" aria-label={`Remove ${src}`} onClick={() => setSources((x) => x.filter((y) => y !== src))}><IconX size={12} /></ActionIcon>
          </Group>
        )
      })}
      <Group gap={6} wrap="nowrap" align="flex-start">
        <TextInput size="xs" style={{ flex: 1 }} placeholder="Add an address or range, like 10.0.0.72 or 10.0.4.0/24"
          aria-label="Add an address or range" value={typed} error={typedErr}
          onChange={(e) => { setTyped(e.currentTarget.value); setTypedErr(null) }}
          onKeyDown={(e) => { if (e.key === 'Enter' && typed.trim()) { e.preventDefault(); add(typed) } }} />
        <Button size="xs" variant="default" disabled={!typed.trim()} onClick={() => add(typed)}>Add</Button>
      </Group>
      {suggest.length > 0 && (
        <Group gap={4}>
          <Text size={FS.micro} c="dimmed">Argus servers on the same network:</Text>
          {suggest.slice(0, 6).map((k) => (
            <Button key={k.id + k.address} size="compact-xs" variant="default" onClick={() => setSources((x) => [...x, k.address])}>
              + {k.name} · {k.address}
            </Button>
          ))}
        </Group>
      )}
    </Stack>
  )
}

/**
 * Connect from the client's side: an Argus server among the machines that
 * may connect tries through its own agent; any other machine gets the
 * command to run there.
 */
export function ClientTest({ n, engine, sources, servers, command }: {
  n: ReachView
  engine: DBEngine
  sources: string[]
  servers: KnownServer[]
  /** The command to run on another machine, for an address of this server. */
  command: (ip: string) => string
}) {
  const target = n.addresses.find((a) => a.state === 'signin' || a.state === 'open')?.ip
  const [results, setResults] = useState<Record<string, ReachCheckAnswer | 'busy'>>({})
  const [pick, setPick] = useState<string | null>(null)
  if (!target) return null
  const listed = sources.map((src) => ({ src, k: servers.find((x) => x.address === src) }))
  const others = nearServers(n, servers)
  const picked = others.find((x) => x.id === pick)

  const run = async (k: KnownServer) => {
    setResults((r) => ({ ...r, [k.id]: 'busy' }))
    try {
      const ans = await reachCheck(k.id, { host: target, port: n.port, engine })
      setResults((r) => ({ ...r, [k.id]: ans }))
    } catch (e) {
      setResults((r) => ({ ...r, [k.id]: { state: 'failed', error: e instanceof Error ? e.message : String(e) } }))
    }
  }
  const row = (key: string, label: ReactNode, k: KnownServer | undefined) => {
    const r = k ? results[k.id] : undefined
    const res = r && r !== 'busy' ? r : null
    const good = res?.state === 'ok' && res.connected && res.answer !== 'protected'
    let detail: string | undefined
    if (!k) detail = 'not an Argus server: run the command below there'
    else if (!res) detail = `can try ${target}:${n.port} from there`
    else if (res.state !== 'ok') detail = res.error
    else if (!res.connected) detail = res.detail
    else if (res.answer === 'signin') detail = `connected in ${res.ms} ms; it asked for a sign-in`
    else if (res.answer === 'open') detail = `connected in ${res.ms} ms; it answered with no sign-in`
    else detail = res.detail ?? `connected in ${res.ms} ms`
    return (
      <Group key={key} gap={SP.snug} wrap="nowrap" px={8} py={6} style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
        <Text size="xs" fw={700} c={!res ? 'dimmed' : good ? 'teal' : 'rose'} w={14}>{!res ? '?' : good ? '✓' : '✕'}</Text>
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Text size="xs">{label}</Text>
          <Text size={FS.micro} c="dimmed">{detail}</Text>
        </Box>
        {k && <Button size="compact-xs" variant="subtle" loading={r === 'busy'} onClick={() => void run(k)}>{res ? 'Test again' : 'Test'}</Button>}
      </Group>
    )
  }
  const cmd = command(target)
  return (
    <Stack gap={6}>
      <Text size="sm" fw={600}>Test from another machine</Text>
      {listed.map(({ src, k }) => row(src, <><Text span ff="monospace" size="xs">{src}</Text>{k && ` · ${k.name}`}</>, k))}
      {listed.length === 0 && others.length > 0 && (
        <Select size="xs" placeholder="Test from an Argus server" data={others.map((k) => ({ value: k.id, label: `${k.name} · ${k.address}` }))}
          value={pick} onChange={setPick} aria-label="Test from an Argus server" style={{ maxWidth: 360 }} />
      )}
      {listed.length === 0 && picked && row(picked.id, `${picked.name} · ${picked.address}`, picked)}
      <Group gap={4} wrap="nowrap"><Code style={{ flex: 1, fontSize: 11.5 }}>{cmd}</Code><Copy value={cmd} label="Copy command" /></Group>
    </Stack>
  )
}
