import { Alert, Button, Group, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconBulb } from '@tabler/icons-react'
import { useState } from 'react'
import { SectionCard } from '~/components/page'
import { envOf } from '~/lib/cloud'
import { assetSuggestionQuery, suggestionAction, type ServerSuggestion } from '~/lib/cloudSuggestions'
import { meQuery } from '~/lib/queries'
import { canSee, seesWholeFleet } from '~/lib/roles'
import { FS } from '~/theme'
import { ResizeChange } from './InstanceActions'

/*
 * A suggestion from Argus's own metrics: a busy server behind a load
 * balancer gets "add a server behind it", a busy one alone "resize up", one
 * idle for two weeks "resize down". It never acts: pressing it opens the
 * scale group wizard or the resize with the answers filled in, which then goes through
 * the usual plan and approval. "Not now" hides it on this browser for a day.
 */

const SNOOZE_MS = 24 * 3600_000

function snoozeKey(s: ServerSuggestion) {
  return `argus.suggestion.${s.assetId}.${s.kind}`
}

function snoozed(s: ServerSuggestion): boolean {
  try {
    const at = Number(localStorage.getItem(snoozeKey(s)) ?? 0)
    return Date.now() - at < SNOOZE_MS
  } catch {
    return false
  }
}

/** The suggestion's buttons: the change it names, and, on the banner, Not now. */
function Act({ s, onSnooze, size = 'sm' }: { s: ServerSuggestion; onSnooze?: () => void; size?: 'xs' | 'sm' }) {
  const { data: me } = useQuery(meQuery())
  const navigate = useNavigate()
  const [resizing, setResizing] = useState(false)
  const admin = canSee(me?.role, 'admin')
  const it = s.instance
  return (
    <Group gap="xs" wrap="nowrap">
      {admin && (
        <Button size={size} color={s.kind === 'resize-down' ? 'teal' : undefined}
          onClick={() => s.kind === 'add-server'
            ? void navigate({ to: '/cloud/scale/new', search: { account: it.accountId, region: it.resource.region, from: it.resource.id } })
            : setResizing(true)}>
          {suggestionAction(s)}
        </Button>
      )}
      {onSnooze && <Button size={size} variant="subtle" onClick={onSnooze}>Not now</Button>}
      {resizing && <ResizeChange it={it} env={envOf(it.resource.tags, it.environment)} initial={s.size} onClose={() => setResizing(false)} />}
    </Group>
  )
}

/** On a server's Overview and Metrics tabs, when Argus has a suggestion for it. */
export function SuggestionBanner({ assetId }: { assetId: string }) {
  const { data: me } = useQuery(meQuery())
  const { data: s } = useQuery(assetSuggestionQuery(assetId, seesWholeFleet(me?.role)))
  const [, setTick] = useState(0)
  if (!s || snoozed(s)) return null
  const snooze = () => {
    try { localStorage.setItem(snoozeKey(s), String(Date.now())) } catch { /* the banner just stays */ }
    setTick((n) => n + 1)
  }
  return (
    <Alert color={s.kind === 'resize-down' ? 'teal' : 'amber'} variant="light" icon={<IconBulb size={18} />} p="sm" data-testid="suggestion">
      <Group justify="space-between" wrap="wrap" gap="sm">
        <Stack gap={2} maw={720}>
          <Text size={FS.body} fw={700}>{s.title}</Text>
          <Text size={FS.meta}>{s.detail}</Text>
          {s.kind === 'add-server' && (
            <Text size={FS.micro} c="dimmed">It opens a scale group made from this server: servers like it join {s.balancer} when it is busy, and go when it is quiet.</Text>
          )}
        </Stack>
        <Act s={s} onSnooze={snooze} />
      </Group>
    </Alert>
  )
}

/** Every suggestion, on Cloud › Scaling. */
export function SuggestionsCard({ items }: { items?: ServerSuggestion[] }) {
  if (!items?.length) return null
  return (
    <SectionCard title="Suggested from Argus metrics" icon={IconBulb}>
      <Stack gap="sm" data-testid="suggestions">
        {items.map((s) => (
          <Group key={s.assetId + s.kind} justify="space-between" wrap="wrap" gap="sm">
            <Stack gap={2} maw={760}>
              <Text size={FS.body} fw={600}>{s.title}</Text>
              <Text size={FS.meta} c="dimmed">{s.detail}</Text>
            </Stack>
            <Act s={s} size="xs" />
          </Group>
        ))}
        <Text size={FS.micro} c="dimmed">A suggestion never acts by itself. Pressing it opens the change with the answers filled in.</Text>
      </Stack>
    </SectionCard>
  )
}
