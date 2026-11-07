import { useState } from 'react'
import { ActionIcon, Box, Button, Group, Progress, Stack, Text, Tooltip } from '@mantine/core'
import {
  IconArrowDown, IconArrowUp, IconCheck, IconChevronDown, IconChevronUp, IconFileZip, IconRefresh, IconX,
} from '@tabler/icons-react'
import { formatSize } from '~/lib/files/paths'
import { FS, INK } from '~/theme'
import type { Transfer } from './useTransfers'

/**
 * What is moving, what is waiting, and what needs an answer. A name clash
 * waits in its own row with Replace, Keep both or Skip while the rest of the
 * queue carries on.
 */
export function TransfersTray({ items, onResolve, onRetry, onCancel, onClear, docked }: {
  items: Transfer[]
  onResolve: (id: number, choice: 'replace' | 'both' | 'skip') => void
  onRetry: (id: number) => void
  onCancel: (id: number) => void
  onClear: () => void
  /** Phone: along the bottom edge rather than floating in a corner. */
  docked?: boolean
}) {
  const [open, setOpen] = useState(true)
  if (items.length === 0) return null
  const busy = items.filter((t) => t.status === 'active' || t.status === 'waiting').length
  const ask = items.filter((t) => t.status === 'conflict').length
  const failed = items.filter((t) => t.status === 'failed').length
  const title = busy
    ? `${busy} ${busy === 1 ? 'transfer' : 'transfers'} in progress`
    : ask ? `${ask} ${ask === 1 ? 'file needs' : 'files need'} an answer`
      : failed ? `${failed} failed` : 'Transfers done'

  return (
    <Box className={docked ? 'argus-files-tray docked' : 'argus-files-tray'} role="region" aria-label="Transfers">
      <Group gap="sm" px="md" py="sm" wrap="nowrap" className="argus-files-tray-head">
        <Text fw={600} size="sm" style={{ flex: 1 }}>{title}</Text>
        {!busy && (
          <Button size="compact-xs" variant="subtle" color="slate" onClick={onClear}>Clear</Button>
        )}
        <ActionIcon variant="subtle" color="slate" onClick={() => setOpen((o) => !o)}
          aria-label={open ? 'Hide transfers' : 'Show transfers'}>
          {open ? <IconChevronDown size={16} /> : <IconChevronUp size={16} />}
        </ActionIcon>
      </Group>
      {open && (
        <Stack gap={0} className="argus-files-tray-list">
          {items.map((t) => <Row key={t.id} t={t} onResolve={onResolve} onRetry={onRetry} onCancel={onCancel} />)}
        </Stack>
      )}
    </Box>
  )
}

function Row({ t, onResolve, onRetry, onCancel }: {
  t: Transfer
  onResolve: (id: number, choice: 'replace' | 'both' | 'skip') => void
  onRetry: (id: number) => void
  onCancel: (id: number) => void
}) {
  const Icon = t.kind === 'zip' ? IconFileZip : t.kind === 'upload' ? IconArrowUp : IconArrowDown
  const pct = t.size > 0 ? Math.min(100, Math.round((t.bytes / t.size) * 100)) : t.status === 'done' ? 100 : 0
  const elapsed = t.startedAt ? (Date.now() - t.startedAt) / 1000 : 0
  const rate = elapsed > 0.5 ? t.bytes / elapsed : 0
  const left = rate > 0 && t.size > t.bytes ? Math.ceil((t.size - t.bytes) / rate) : null

  return (
    <Box px="md" py="sm" className="argus-files-tray-row" data-status={t.status}>
      <Group gap="sm" wrap="nowrap">
        {t.status === 'done'
          ? <IconCheck size={16} color={INK.verified} style={{ flexShrink: 0 }} />
          : <Icon size={16} style={{ flexShrink: 0, opacity: 0.7 }} />}
        <Text size="sm" truncate style={{ flex: 1, minWidth: 0 }} fw={t.status === 'active' ? 500 : 400}>{t.name}</Text>
        <Text size={FS.micro} c={t.status === 'conflict' ? INK.pending : t.status === 'failed' ? INK.denied : 'dimmed'}
          style={{ flexShrink: 0 }}>
          {t.status === 'active' ? `${pct}%`
            : t.status === 'waiting' ? 'Waiting'
              : t.status === 'conflict' ? 'Already exists'
                : t.status === 'failed' ? 'Failed'
                  : t.status === 'skipped' ? 'Skipped'
                    : formatSize(t.bytes)}
        </Text>
        {t.status === 'waiting' && (
          <Tooltip label="Take out of the queue">
            <ActionIcon size="sm" variant="subtle" color="slate" onClick={() => onCancel(t.id)} aria-label={`Cancel ${t.name}`}>
              <IconX size={14} />
            </ActionIcon>
          </Tooltip>
        )}
        {t.status === 'failed' && (t.kind !== 'upload' || t.file) && (
          <Tooltip label="Try again">
            <ActionIcon size="sm" variant="subtle" color="slate" onClick={() => onRetry(t.id)} aria-label={`Retry ${t.name}`}>
              <IconRefresh size={14} />
            </ActionIcon>
          </Tooltip>
        )}
      </Group>
      {t.status === 'active' && (
        <>
          <Progress value={pct} size="sm" mt={8} aria-label={`${t.name}: ${pct}%`} />
          <Text size={FS.micro} c="dimmed" mt={4}>
            {formatSize(t.bytes)} of {formatSize(t.size)}
            {rate > 0 ? ` · ${formatSize(rate)}/s` : ''}
            {left !== null ? ` · about ${left < 60 ? `${left} s` : `${Math.ceil(left / 60)} min`} left` : ''}
          </Text>
        </>
      )}
      {t.status === 'conflict' && (
        <Group gap="xs" mt={8}>
          <Button size="compact-sm" color="amber" onClick={() => onResolve(t.id, 'replace')}>Replace</Button>
          <Button size="compact-sm" variant="default" onClick={() => onResolve(t.id, 'both')}>Keep both</Button>
          <Button size="compact-sm" variant="subtle" color="slate" onClick={() => onResolve(t.id, 'skip')}>Skip</Button>
        </Group>
      )}
      {t.status === 'failed' && t.error && (
        <Text size={FS.micro} c={INK.denied} mt={4} lh={1.4}>{t.error}</Text>
      )}
      {t.status === 'done' && t.sha256 && (
        <Text size={FS.micro} c="dimmed" mt={2} ff="monospace" truncate>sha256 {t.sha256.slice(0, 16)}…</Text>
      )}
    </Box>
  )
}
