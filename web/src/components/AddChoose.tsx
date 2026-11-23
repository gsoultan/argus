import { Badge, Box, Button, Group, SimpleGrid, Text, UnstyledButton } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconPlus, IconSearch, IconCheck } from '@tabler/icons-react'
import type { ReactNode } from 'react'
import { EngineMark } from '~/components/EngineMark'
import { isConfigured } from '~/lib/live'
import { applicationsQuery } from '~/lib/queries'
import { ENGINES, ENGINE_APPS } from '~/lib/engines'
import { FS } from '~/theme'
import type { Asset, DBEngine, ServerApp } from '~/types/domain'

/*
The first step of Add database and Add broker: which engine, then whether to
add one that is already there or create a new one. Each engine says what the
agent found running on the server, from its last report, so the choice starts
from what is there rather than from a form. Cards in a row are the same height
whatever their text.
*/

export type AddKind = 'database' | 'broker'
export type AddMode = 'existing' | 'create'

/** What the agent's report says about an engine on this server. */
export interface EngineFound {
  running: boolean
  /** "16.4 · port 5432", for the card. */
  detail: string
}

export function useEnginesFound(asset: Asset): (engine: DBEngine) => EngineFound | null {
  const { data } = useQuery({ ...applicationsQuery(asset.id), enabled: isConfigured() })
  const apps = data?.applications ?? []
  return (engine) => {
    if (!data) return null
    const kinds = ENGINE_APPS[engine]
    const found = apps.filter((a) => kinds.includes(a.kind) && !a.container)
    const run = found.find((a) => a.running) ?? found[0]
    if (!run) return { running: false, detail: '' }
    return { running: run.running, detail: appDetail(run) }
  }
}

function appDetail(a: ServerApp): string {
  const ports = [...new Set(a.listen.map((l) => l.port))].sort((x, y) => x - y)
  const version = a.version ? a.version.split(/[\s-]/)[0] : ''
  return [version, ports.length ? `port${ports.length > 1 ? 's' : ''} ${ports.slice(0, 3).join(', ')}` : '']
    .filter(Boolean).join(' · ')
}

const card = (on: boolean) => ({
  display: 'flex', flexDirection: 'column' as const, gap: 10, height: '100%', padding: 14, borderRadius: 12,
  border: `1px solid ${on ? 'var(--mantine-primary-color-filled)' : 'var(--color-line)'}`,
  boxShadow: on ? '0 0 0 1px var(--mantine-primary-color-filled) inset' : undefined,
  background: on ? 'var(--mantine-primary-color-light)' : 'var(--color-surface, var(--mantine-color-body))',
})

/** The engine cards, running ones first. */
export function EngineCards({ asset, engines, value, onChange }: {
  asset: Asset; engines: DBEngine[]; value: DBEngine; onChange: (e: DBEngine) => void
}) {
  const found = useEnginesFound(asset)
  const host = asset.hostname.split('.')[0]
  const order = [...engines].sort((a, b) => Number(found(b)?.running ?? false) - Number(found(a)?.running ?? false))
  return (
    <SimpleGrid cols={{ base: 1, xs: Math.min(engines.length, 2), sm: Math.min(engines.length, engines.length > 3 ? 3 : engines.length), md: engines.length }}
      spacing="sm" role="radiogroup" aria-label="Engine">
      {order.map((e) => {
        const f = found(e)
        const on = e === value
        return (
          <UnstyledButton key={e} role="radio" aria-checked={on} onClick={() => onChange(e)} style={card(on)}
            data-engine={e}>
            <Group gap={10} wrap="nowrap">
              <EngineMark engine={e} size={34} />
              <Text size={FS.body} fw={650} lh={1.25}>{engineName(e)}</Text>
            </Group>
            <Text size={FS.micro} mt="auto" c={f?.running ? 'teal' : 'dimmed'} lh={1.35}>
              {f === null ? 'Asking the agent…'
                : f.running ? <>● Running{f.detail ? ` · ${f.detail}` : ''}</>
                : f.detail ? `Installed, not running · ${f.detail}` : `Not found on ${host}`}
            </Text>
          </UnstyledButton>
        )
      })}
    </SimpleGrid>
  )
}

/** The names on the cards, with the engines a card also covers. */
export function engineName(e: DBEngine): string {
  return e === 'mysql' ? 'MySQL / MariaDB' : e === 'redis' ? 'Redis / Valkey' : ENGINES[e].label
}

/** Add one that is there, or create a new one, in the engine's words. */
export function HowCards({ engine, host, value, onChange }: {
  engine: DBEngine; host: string; value: AddMode; onChange: (m: AddMode) => void
}) {
  const w = howWords(engine, host)
  const opts: { v: AddMode; icon: ReactNode; title: string; text: string }[] = [
    { v: 'existing', icon: <IconSearch size={17} />, title: w.existing, text: w.existingText },
    { v: 'create', icon: <IconPlus size={17} />, title: w.create, text: w.createText },
  ]
  return (
    <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm" role="radiogroup" aria-label="What to do">
      {opts.map((o) => {
        const on = o.v === value
        return (
          <UnstyledButton key={o.v} role="radio" aria-checked={on} onClick={() => onChange(o.v)}
            style={{ ...card(on), flexDirection: 'row', alignItems: 'flex-start', gap: 12, padding: '14px 16px' }}>
            <Box style={{ width: 34, height: 34, borderRadius: 9, flexShrink: 0, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
              background: on ? 'var(--mantine-color-body)' : 'var(--color-raised)', color: on ? 'var(--mantine-primary-color-filled)' : undefined }}>
              {o.icon}
            </Box>
            <Box>
              <Text size={FS.body} fw={650}>{o.title}</Text>
              <Text size={FS.micro} c="dimmed" mt={2}>{o.text}</Text>
            </Box>
          </UnstyledButton>
        )
      })}
    </SimpleGrid>
  )
}

export function howWords(engine: DBEngine, host: string) {
  switch (engine) {
    case 'rabbitmq':
      return { existing: 'Add a vhost that is already there', existingText: `Pick from the vhosts on ${host}.`,
        create: 'Create a new vhost', createText: 'With its queue type and limits, then it is added.' }
    case 'kafka':
      return { existing: 'Give access to topics', existingText: 'Every topic, or the ones starting with a prefix.',
        create: 'Create a new topic', createText: 'With its partitions and retention, then it is added.' }
    case 'redis':
      return { existing: 'Add this Redis', existingText: `One entry covers every db of the Redis on ${host}.`,
        create: 'Create a new database', createText: 'Redis has a fixed set of numbered dbs; this explains how.' }
    default:
      return { existing: 'Add one that is already there', existingText: `Pick from what the agent finds on ${host}.`,
        create: 'Create a new database', createText: 'The agent makes it, then it is added.' }
  }
}

/** The step 1 choice, kept on top of step 2, with Change to go back. */
export function ChoiceLine({ asset, engine, mode, onChange }: {
  asset: Asset; engine: DBEngine; mode: AddMode; onChange: () => void
}) {
  const found = useEnginesFound(asset)(engine)
  const host = asset.hostname.split('.')[0]!
  const w = howWords(engine, host)
  return (
    <Group gap="sm" wrap="nowrap">
      <EngineMark engine={engine} size={34} />
      <Box style={{ minWidth: 0 }}>
        <Text size={FS.body} fw={650} truncate>{engineName(engine)} on {host}</Text>
        <Text size={FS.micro} c="dimmed" truncate>
          {mode === 'existing' ? w.existing : w.create}{found?.running && found.detail ? ` · ${found.detail}` : ''}
        </Text>
      </Box>
      <Button ml="auto" size="compact-sm" variant="subtle" onClick={onChange}>Change</Button>
    </Group>
  )
}

/** "1 Choose — 2 Details", in the dialog's title. */
export function Stepper({ step }: { step: 1 | 2 }) {
  const dot = (n: 1 | 2, label: string) => {
    const on = n === step
    const done = n < step
    return (
      <Group gap={6} wrap="nowrap" aria-current={on ? 'step' : undefined}>
        <Box style={{ width: 20, height: 20, borderRadius: '50%', display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          fontSize: 11, fontWeight: 700,
          background: on ? 'var(--mantine-primary-color-filled)' : done ? 'var(--mantine-color-teal-light)' : undefined,
          color: on ? 'var(--mantine-color-white)' : done ? 'var(--mantine-color-teal-6)' : 'var(--mantine-color-dimmed)',
          border: on || done ? undefined : '1.5px solid var(--color-line)' }}>
          {done ? <IconCheck size={12} /> : n}
        </Box>
        <Text size={FS.meta} fw={on ? 600 : 400} c={on ? undefined : 'dimmed'}>{label}</Text>
      </Group>
    )
  }
  return (
    <Group gap={10} wrap="nowrap" visibleFrom="xs">
      {dot(1, 'Choose')}
      <Box w={24} h={1.5} bg="var(--color-line)" />
      {dot(2, 'Details')}
    </Group>
  )
}

/** The dialog title: what, on which server, and the steps. */
export function AddTitle({ kind, host, step }: { kind: AddKind; host: string; step: 1 | 2 }) {
  return (
    <Group gap="sm" wrap="nowrap" w="100%">
      <Text fw={650} size="lg">{kind === 'broker' ? 'Add broker' : 'Add database'}</Text>
      <Badge variant="outline" color="gray" radius="sm" tt="none">{host}</Badge>
      <Box ml="auto" mr="md"><Stepper step={step} /></Box>
    </Group>
  )
}
