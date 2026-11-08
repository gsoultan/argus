import { useState } from 'react'
import { Alert, Box, Button, Group, NumberInput, SegmentedControl, Skeleton, Stack, Table, Text } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { SectionCard } from '~/components/page'
import { get, isConfigured, send } from '~/lib/live'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'

/*
 * Settings › Asset types: who is asked before a session opens, per type.
 * An asset can differ from its type on its own edit form. A device is not
 * here: the Argus app on it asks as its control mode says.
 */

export type ConnectMode = 'never' | 'ask' | 'tell'

export interface ConnectRule {
  desktop: ConnectMode
  terminal: Exclude<ConnectMode, 'ask'>
  seconds: number
}

export interface ConnectRules {
  server: ConnectRule
  workstation: ConnectRule
}

const KEY = ['settings', 'asset-types'] as const

export const connectRules = (): Promise<ConnectRules> => get('/api/v1/settings/asset-types')
const saveConnectRules = (r: ConnectRules): Promise<ConnectRules> => send('PUT', '/api/v1/settings/asset-types', r)

const DESKTOP = [
  { value: 'never', label: 'Never ask' },
  { value: 'ask', label: 'Ask first' },
  { value: 'tell', label: 'Tell them' },
]
const TERMINAL = [
  { value: 'never', label: 'Nothing' },
  { value: 'tell', label: 'Tell them' },
]

export function AssetTypeSettings({ canEdit }: { canEdit: boolean }) {
  const qc = useQueryClient()
  const { data: saved, isLoading } = useQuery({ queryKey: KEY, queryFn: connectRules, enabled: isConfigured() })
  const [draft, setDraft] = useState<ConnectRules | null>(null)
  const save = useMutation({
    mutationFn: saveConnectRules,
    onSuccess: (r) => {
      qc.setQueryData(KEY, r)
      setDraft(null)
    },
  })

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>These rules are set on a control plane. Connect this console to Argus to set them.</Text>
      </Alert>
    )
  }
  if (isLoading || !saved) return <Skeleton h={260} />

  const r = draft ?? saved
  const changed = draft !== null && JSON.stringify(draft) !== JSON.stringify(saved)
  const set = (kind: keyof ConnectRules, patch: Partial<ConnectRule>) =>
    setDraft({ ...r, [kind]: { ...r[kind], ...patch } })

  const row = (kind: keyof ConnectRules, label: string, blurb: string) => (
    <Table.Tr key={kind}>
      <Table.Td>
        <Text size="sm" fw={600}>{label}</Text>
        <Text size={FS.meta} c="dimmed">{blurb}</Text>
      </Table.Td>
      <Table.Td>
        <SegmentedControl size="xs" disabled={!canEdit} aria-label={`${label}: desktop and screen`}
          value={r[kind].desktop} data={DESKTOP}
          onChange={(v) => set(kind, { desktop: v as ConnectMode })} />
      </Table.Td>
      <Table.Td>
        <SegmentedControl size="xs" disabled={!canEdit} aria-label={`${label}: terminal`}
          value={r[kind].terminal} data={TERMINAL}
          onChange={(v) => set(kind, { terminal: v as ConnectRule['terminal'] })} />
      </Table.Td>
      <Table.Td>
        <NumberInput size="xs" w={90} min={10} max={120} suffix=" s" disabled={!canEdit || r[kind].desktop !== 'ask'}
          aria-label={`${label}: seconds to answer`} value={r[kind].seconds}
          onChange={(v) => set(kind, { seconds: typeof v === 'number' ? v : Number(v) || 60 })} />
      </Table.Td>
    </Table.Tr>
  )

  return (
    <SectionCard title="Before connecting" description="Who is asked, or told, before a session opens on each type of asset. An asset can differ from its type on its own edit form.">
      <Stack gap={SP.cozy}>
        <Table.ScrollContainer minWidth={620}>
          <Table verticalSpacing="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Type</Table.Th>
                <Table.Th>Remote Desktop or screen</Table.Th>
                <Table.Th>Terminal</Table.Th>
                <Table.Th>No answer after</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {row('server', 'Servers', 'Nobody sits at them.')}
              {row('workstation', 'Workstations', 'A laptop or PC someone uses.')}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          Ask first puts a question on the screen of whoever is using the machine, naming who is connecting;
          no answer is a no. Tell them opens at once and shows a notice. Nobody signed in means nobody is asked.
          A terminal is never asked: it is a login of its own and takes nothing from the person at the screen.
          Devices follow their own control mode.
        </Text>
        {canEdit && (
          <Box>
            <Group justify="flex-end" gap={SP.cozy}>
              <Button variant="default" disabled={!changed || save.isPending} onClick={() => setDraft(null)}>Discard</Button>
              <Button disabled={!changed} loading={save.isPending}
                onClick={() => void run(() => save.mutateAsync(r), {
                  failure: 'Not saved',
                  success: ['Saved', 'The new rules apply to the next session that opens.'],
                })}>
                Save
              </Button>
            </Group>
          </Box>
        )}
      </Stack>
    </SectionCard>
  )
}
