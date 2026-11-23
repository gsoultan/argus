import {
  ActionIcon, Box, Button, Code, CopyButton, Group, Modal, PasswordInput, SegmentedControl, SimpleGrid, Stack, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCheck, IconCopy } from '@tabler/icons-react'
import { useEffect, useState, type ReactNode } from 'react'
import { get, isConfigured } from '~/lib/live'
import type { BrokerUserEvent } from '~/lib/kafkaUsers'
import { generatePassword } from '~/lib/password'
import { FS, SP } from '~/theme'
import type { Database } from '~/types/domain'

/*
What a broker's Users tab is made of on every engine: the password control,
cards to choose from, a section heading, a confirm that asks for the user's
name, a step of the sign-in check, and who made a user, from Activity.
*/

export const MONO = { input: { fontFamily: 'var(--font-mono)' } }

export function useUserEvents(database: Database) {
  return useQuery({
    queryKey: ['broker-users', database.id],
    queryFn: () => get<{ events: BrokerUserEvent[] }>(`/api/v1/databases/${encodeURIComponent(database.id)}/broker-users`),
    enabled: isConfigured(),
    staleTime: 10_000,
  })
}

export function madeText(e?: BrokerUserEvent): string {
  if (!e) return 'Outside Argus'
  const who = e.by.split('@')[0]
  return `Argus · ${who}, ${new Date(e.at).toLocaleDateString(undefined, { day: 'numeric', month: 'short' })}`
}

/** Generate a password or type one; the same control as the Redis users. */
export function PasswordChoice({ value, onChange }: { value: string; onChange: (pw: string) => void }) {
  const [mode, setMode] = useState<'generate' | 'type'>('generate')
  const [generated, setGenerated] = useState(() => generatePassword())
  const [typed, setTyped] = useState('')
  useEffect(() => { onChange(mode === 'generate' ? generated : typed) }, [mode, generated, typed]) // eslint-disable-line react-hooks/exhaustive-deps
  const short = mode === 'type' && typed.length > 0 && typed.length < 12
  return (
    <Stack gap={4}>
      <Group gap="xs" wrap="nowrap" align="center">
        <SegmentedControl size="xs" value={mode} onChange={(v) => setMode(v as 'generate' | 'type')} w={190}
          data={[{ value: 'generate', label: 'Generate' }, { value: 'type', label: 'Type one' }]} />
        {mode === 'generate' ? (
          <Group gap={6} wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
            <Code style={{ flex: 1, fontSize: 13, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{generated}</Code>
            <CopyButton value={generated}>
              {({ copied, copy }) => <Tooltip label={copied ? 'Copied' : 'Copy'}><ActionIcon variant="default" onClick={copy} aria-label="Copy password">{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon></Tooltip>}
            </CopyButton>
            <Button size="compact-xs" variant="subtle" onClick={() => setGenerated(generatePassword())}>New</Button>
          </Group>
        ) : (
          <PasswordInput size="xs" style={{ flex: 1 }} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} aria-label="Password" error={short ? 'At least 12 characters' : undefined} />
        )}
      </Group>
      <Text size={FS.micro} c="dimmed">Shown again with the connection details once it is made. Argus does not keep it{value ? '' : ''}.</Text>
    </Stack>
  )
}

/** Cards of equal height whatever their text, one chosen. */
export function ChoiceCards<T extends string>({ value, onChange, options, cols }: {
  value: T; onChange: (v: T) => void; options: { value: T; label: string; hint: string }[]; cols?: number
}) {
  return (
    <SimpleGrid cols={{ base: 2, sm: cols ?? options.length }} spacing={6} role="radiogroup">
      {options.map((o) => {
        const on = o.value === value
        return (
          <UnstyledButton key={o.value} role="radio" aria-checked={on} onClick={() => onChange(o.value)} p={8}
            style={{ height: '100%', borderRadius: 9, border: `1px solid ${on ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'}`,
              background: on ? 'var(--mantine-color-azure-light)' : undefined, boxShadow: on ? '0 0 0 1px var(--mantine-color-azure-6) inset' : undefined }}>
            <Text size={FS.meta} fw={600}>{o.label}</Text>
            <Text size={FS.micro} c="dimmed">{o.hint}</Text>
          </UnstyledButton>
        )
      })}
    </SimpleGrid>
  )
}

export function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <Stack gap={SP.cozy}>
      <Text size={FS.meta} fw={600}>{title}{hint && <Text span size={FS.micro} c="dimmed" fw={400}> · {hint}</Text>}</Text>
      {children}
    </Stack>
  )
}

export function ConfirmTyped({ opened, name, title, text, action, onCancel, onConfirm }: {
  opened: boolean; name: string; title: string; text: string; action: string; onCancel: () => void; onConfirm: () => Promise<void>
}) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => setTyped(''), [opened])
  return (
    <Modal opened={opened} onClose={onCancel} title={title} size="sm">
      <Stack gap="sm">
        <Text size={FS.meta}>{text}</Text>
        <TextInput label={`Type ${name} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} styles={MONO} />
        <Group justify="flex-end">
          <Button variant="default" size="xs" onClick={onCancel}>Cancel</Button>
          <Button color="rose" size="xs" disabled={typed !== name} loading={busy} onClick={async () => { setBusy(true); await onConfirm(); setBusy(false) }}>{action}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

export function Step({ ok, text }: { ok: boolean | null; text: string }) {
  return (
    <Group gap={8} wrap="nowrap" align="flex-start">
      <Box w={18} h={18} style={{ borderRadius: 9, flexShrink: 0, display: 'grid', placeItems: 'center', fontSize: 11, fontWeight: 700,
        background: ok === null ? undefined : ok ? 'var(--mantine-color-teal-light)' : 'var(--mantine-color-rose-light)',
        color: ok ? 'var(--mantine-color-teal-6)' : 'var(--mantine-color-rose-6)', border: ok === null ? '2px solid var(--mantine-color-azure-6)' : undefined }}>
        {ok === null ? '' : ok ? '✓' : '!'}
      </Box>
      <Text size={FS.meta}>{text}</Text>
    </Group>
  )
}
