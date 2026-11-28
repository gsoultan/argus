import { ActionIcon, Badge, Box, Button, CopyButton, Drawer, Group, ScrollArea, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { Link } from '@tanstack/react-router'
import {
  IconCheck, IconCopy, IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand, IconLock, IconSelector, IconSettings,
  IconShieldCheck, IconUser, type Icon,
} from '@tabler/icons-react'
import { useState, type ReactNode } from 'react'
import { EngineMark } from '~/components/EngineMark'
import { EnvBadge } from '~/components/ChangeImpact'
import { writesNeedApproval } from '~/lib/changeApproval'
import { FS } from '~/theme'
import type { DBLevel, Database } from '~/types/domain'

/*
The frame every database workspace shares (plans/database-page-layout):
one header that says where you are and what you may do, and one rail that
is the page's only menu. Postgres, MySQL, SQL Server, MongoDB and Redis
each hand the rail their own list of panels; nothing here decides access,
it only shows the level the session already has.
*/

export type RailGroup = 'work' | 'care' | 'dba'

export interface RailItem {
  key: string
  label: string
  icon: Icon
  hint: string
  group: RailGroup
  /** Runs as the server's admin account: shown with a lock below DBA. */
  dba?: boolean
  /** A short count or age beside the name, such as "2h ago". */
  count?: { text: string; tone?: 'pending' | 'denied' }
}

const COUNT_INK = { pending: 'var(--mantine-color-amber-text)', denied: 'var(--mantine-color-rose-text)' }

function RailCount({ c }: { c: NonNullable<RailItem['count']> }) {
  return <Text size={FS.micro} fw={c.tone ? 600 : 400} c={c.tone ? undefined : 'dimmed'} style={{ color: c.tone ? COUNT_INK[c.tone] : undefined, whiteSpace: 'nowrap' }}>{c.text}</Text>
}

const GROUP_TITLE: Record<RailGroup, string> = { work: 'Work', care: 'Look after', dba: 'DBA' }
const GROUPS: RailGroup[] = ['work', 'care', 'dba']

/* ── Header ─────────────────────────────────────────────────────────────── */

export type SessionStatus = 'connecting' | 'ready' | 'closed'

/**
 * The database's one header: engine, name and environment first, the full
 * address on its own line, and a single strip for the approval rule, the
 * person's level and the recording. A production database keeps its rose
 * edge across the top.
 */
export function DatabaseHeader({ database, narrow, level, status, where, detail, canAsk, askLabel, onAskForAccess, settings, left }: {
  database: Database
  narrow: boolean
  level: DBLevel | ''
  status: SessionStatus
  /** The server's address, said once and never cut on a wide screen. */
  where: string
  /** Version, port, pooler or the account it runs as. */
  detail?: string
  canAsk: boolean
  askLabel?: string
  onAskForAccess?: () => void
  /** An organization admin: the gear to who can connect and the settings. */
  settings?: boolean
  /** A control before the mark, such as the phone's object tree button. */
  left?: ReactNode
}) {
  const prod = database.label === 'production'
  const approval = writesNeedApproval(database)
  const recorded = status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'
  const levelText = level ? level.toUpperCase() : '…'
  const strip = (
    <Group gap={0} wrap="nowrap" data-testid="trust-strip"
      style={{ border: '1px solid var(--color-line)', borderRadius: 10, background: 'var(--color-surface)', boxShadow: 'var(--shadow-card)', flexShrink: 0 }}>
      {approval && (
        <Tooltip label="A statement that changes it runs once someone else approves it. Reading never waits." withArrow>
          <Group gap={6} px={narrow ? 8 : 12} py={6} wrap="nowrap" style={{ borderRight: '1px solid var(--color-line)' }}>
            <IconShieldCheck size={15} color="var(--color-pending)" aria-hidden />
            <Text size={FS.meta} style={{ whiteSpace: 'nowrap' }}>{narrow ? 'Approval' : <>Changes need <b>approval</b></>}</Text>
          </Group>
        </Tooltip>
      )}
      <Group gap={6} px={narrow ? 8 : 12} py={6} wrap="nowrap" style={{ borderRight: '1px solid var(--color-line)' }}>
        <IconUser size={15} color="var(--color-brand)" aria-hidden />
        <Text size={FS.meta} style={{ whiteSpace: 'nowrap' }}>{narrow ? <b>{levelText}</b> : <>You can <b>{levelText}</b></>}</Text>
      </Group>
      <Group gap={7} px={narrow ? 8 : 12} py={6} wrap="nowrap">
        <Box w={8} h={8} style={{ borderRadius: 4, flexShrink: 0, background: status === 'ready' ? 'var(--color-denied)' : 'var(--mantine-color-dimmed)',
          boxShadow: status === 'ready' ? '0 0 0 3px var(--mantine-color-rose-light)' : undefined }} aria-hidden />
        <Text size={FS.meta} style={{ whiteSpace: 'nowrap' }}>{recorded}</Text>
      </Group>
    </Group>
  )
  const ask = canAsk && onAskForAccess && (
    <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />} style={{ flexShrink: 0 }}>
      {narrow ? 'Ask' : askLabel ?? 'Ask for write access'}
    </Button>
  )
  const gear = settings && (
    <Tooltip label="Who can connect, and this database's settings" withArrow>
      <Link to="/databases/$dbId/settings" params={{ dbId: database.id }} aria-label="Who can connect and settings"
        style={{ display: 'inline-flex', alignItems: 'center', justifyContent: 'center', width: 34, height: 34, borderRadius: 8, flexShrink: 0,
          border: '1px solid var(--color-line)', background: 'var(--color-surface)', color: 'var(--color-ink-soft)' }}>
        <IconSettings size={16} />
      </Link>
    </Tooltip>
  )
  return (
    <Box data-env={database.label} component="header"
      style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)', flexShrink: 0,
        boxShadow: prod ? 'inset 0 3px 0 var(--mantine-color-rose-6)' : undefined, paddingTop: prod ? 3 : 0 }}>
      <Group gap={narrow ? 'sm' : 'md'} px={narrow ? 'sm' : 'lg'} py={narrow ? 10 : 14} wrap={narrow ? 'wrap' : 'nowrap'}>
        {left}
        <EngineMark engine={database.engine} size={narrow ? 34 : 40} />
        <Box style={{ minWidth: 0, flex: 1 }}>
          <Group gap="sm" wrap="nowrap">
            <Text fw={650} size={narrow ? 'md' : 'lg'} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', letterSpacing: '-0.01em' }}>
              {database.dbname}
            </Text>
            <Box style={{ flexShrink: 0 }}><EnvBadge label={database.label} /></Box>
          </Group>
          <Group gap={8} wrap="nowrap" mt={2} style={{ minWidth: 0 }}>
            <Text size={FS.micro} c="dimmed" ff="monospace" truncate={narrow ? 'end' : undefined} style={{ minWidth: 0 }} title={where}>{where}</Text>
            {!narrow && (
              <CopyButton value={where}>
                {({ copied, copy }) => (
                  <ActionIcon size="xs" variant="subtle" color="slate" onClick={copy} aria-label="Copy the address">
                    {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
                  </ActionIcon>
                )}
              </CopyButton>
            )}
            {detail && <Text size={FS.micro} c="dimmed" style={{ whiteSpace: 'nowrap', flexShrink: 0 }}>· {detail}</Text>}
          </Group>
        </Box>
        {narrow ? (
          <Group gap={6} wrap="nowrap" w="100%">{strip}{ask}<Box ml="auto">{gear}</Box></Group>
        ) : (
          <Group gap="sm" wrap="nowrap" style={{ flexShrink: 0 }}>{ask}{strip}{gear}</Group>
        )}
      </Group>
    </Box>
  )
}

/* ── Rail ───────────────────────────────────────────────────────────────── */

const FOLD_KEY = 'argus-db-rail'

/** Whether the person pinned the rail open or shut; null follows the page. */
function readFold(): boolean | null {
  try {
    const v = window.localStorage.getItem(FOLD_KEY)
    return v === 'open' ? true : v === 'shut' ? false : null
  } catch { return null }
}

/**
 * The rail opens wide on an administration panel and folds to icons on the
 * editor, unless the person pinned it. Returns whether it is open now and
 * the toggle.
 */
export function useRailFold(onWork: boolean): [boolean, () => void] {
  const [pinned, setPinned] = useState<boolean | null>(readFold)
  const open = pinned ?? !onWork
  const toggle = () => {
    const next = !open
    // Pinning the state the page would choose anyway un-pins it.
    const keep = next === !onWork ? null : next
    setPinned(keep)
    try {
      if (keep === null) window.localStorage.removeItem(FOLD_KEY)
      else window.localStorage.setItem(FOLD_KEY, keep ? 'open' : 'shut')
    } catch { /* private window: the fold lasts this page only */ }
  }
  return [open, toggle]
}

function lockedFor(item: RailItem, level: DBLevel | '') { return Boolean(item.dba) && level !== 'dba' }

/** The page's one menu, on a wide screen. */
export function DatabaseRail({ items, active, level, open, onPick, onToggle, footer }: {
  items: RailItem[]
  active: string
  level: DBLevel | ''
  open: boolean
  onPick: (key: string) => void
  onToggle: () => void
  footer?: ReactNode
}) {
  const groups = GROUPS.filter((g) => items.some((i) => i.group === g))
  const anyDBA = items.some((i) => i.dba)
  const toggle = (
    <Tooltip label={open ? 'Fold the menu to icons' : 'Open the menu'} withArrow position="right">
      <ActionIcon variant="subtle" color="slate" onClick={onToggle} size={36} aria-label={open ? 'Fold the menu' : 'Open the menu'}>
        {open ? <IconLayoutSidebarLeftCollapse size={18} /> : <IconLayoutSidebarLeftExpand size={18} />}
      </ActionIcon>
    </Tooltip>
  )
  if (!open) {
    return (
      <Box component="nav" aria-label="Database" w={56} py="sm"
        style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', background: 'var(--color-surface)', display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4 }}>
        {groups.map((g, gi) => (
          <Box key={g} style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 4 }}>
            {gi > 0 && <Box w={24} h={1} my={6} style={{ background: 'var(--color-line)' }} />}
            {items.filter((i) => i.group === g).map((i) => {
              const on = i.key === active
              const Icon = i.icon
              return (
                <Tooltip key={i.key} label={lockedFor(i, level) ? `${i.label} · needs DBA access` : i.label} withArrow position="right">
                  <UnstyledButton onClick={() => onPick(i.key)} aria-label={i.label} aria-current={on ? 'page' : undefined}
                    w={40} h={40} style={{ position: 'relative', borderRadius: 10, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
                      background: on ? 'var(--color-brand-wash)' : undefined, color: on ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)',
                      opacity: lockedFor(i, level) ? 0.6 : 1 }}>
                    <Icon size={19} />
                    {i.count?.tone && <Box aria-hidden style={{ position: 'absolute', top: 7, right: 7, width: 7, height: 7, borderRadius: 7, background: COUNT_INK[i.count.tone] }} />}
                  </UnstyledButton>
                </Tooltip>
              )
            })}
          </Box>
        ))}
        <Box mt="auto">{toggle}</Box>
      </Box>
    )
  }
  return (
    <Box component="nav" aria-label="Database" w={232}
      style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', background: 'var(--color-surface)', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
      <ScrollArea style={{ flex: 1 }} type="auto">
        <Box px={10} py="sm">
          {groups.map((g) => (
            <Box key={g} mb={6}>
              <Group gap={6} px={10} pt={g === groups[0] ? 2 : 10} pb={6} wrap="nowrap">
                <Text size={FS.micro} c="dimmed" tt="uppercase" fw={650} style={{ letterSpacing: '.07em' }}>{GROUP_TITLE[g]}</Text>
                {g === 'dba' && anyDBA && (
                  level === 'dba'
                    ? <Group gap={3} ml="auto" wrap="nowrap"><IconCheck size={11} color="var(--color-verified)" /><Text size={FS.micro} fw={600} c="teal">You have it</Text></Group>
                    : <Group gap={3} ml="auto" wrap="nowrap"><IconLock size={11} color="var(--mantine-color-dimmed)" /><Text size={FS.micro} c="dimmed">Needs DBA</Text></Group>
                )}
              </Group>
              {items.filter((i) => i.group === g).map((i) => {
                const on = i.key === active
                const Icon = i.icon
                const locked = lockedFor(i, level)
                return (
                  <Tooltip key={i.key} label={locked ? 'Needs DBA access' : i.hint} withArrow position="right" multiline w={240} openDelay={400}>
                    <UnstyledButton onClick={() => onPick(i.key)} aria-current={on ? 'page' : undefined} px={10} py={7}
                      style={{ display: 'flex', alignItems: 'center', gap: 10, width: '100%', borderRadius: 8, marginBottom: 1, position: 'relative',
                        background: on ? 'var(--color-brand-wash)' : undefined,
                        color: on ? 'var(--color-brand-ink)' : 'var(--color-ink-soft)' }}>
                      {on && <Box style={{ position: 'absolute', left: -10, top: 7, bottom: 7, width: 3, borderRadius: '0 3px 3px 0', background: 'var(--color-brand)' }} />}
                      <Icon size={16} color={on ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)'} />
                      <Text size={FS.body} fw={on ? 600 : 400} style={{ flex: 1 }}>{i.label}</Text>
                      {i.count && !locked && <RailCount c={i.count} />}
                      {locked && <IconLock size={12} color="var(--mantine-color-dimmed)" aria-label="needs DBA access" />}
                    </UnstyledButton>
                  </Tooltip>
                )
              })}
            </Box>
          ))}
        </Box>
      </ScrollArea>
      <Group gap={6} px={10} py={8} wrap="nowrap" style={{ borderTop: '1px solid var(--color-line)' }}>
        <Box style={{ flex: 1, minWidth: 0 }}>{footer}</Box>
        {toggle}
      </Group>
    </Box>
  )
}

/**
 * The rail on a phone: one button that names where you are, opening a sheet
 * with the same sections. `right` holds the panel's main action.
 */
export function RailPicker({ items, active, level, onPick, right }: {
  items: RailItem[]
  active: string
  level: DBLevel | ''
  onPick: (key: string) => void
  right?: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const now = items.find((i) => i.key === active) ?? items[0]
  const groups = GROUPS.filter((g) => items.some((i) => i.group === g))
  if (!now) return null
  const NowIcon = now.icon
  return (
    <>
      <Group gap={8} px="sm" py={8} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)' }}>
        <UnstyledButton onClick={() => setOpen(true)} aria-label={`Section: ${now.label}. Change`} aria-haspopup="dialog"
          style={{ flex: 1, height: 38, border: '1px solid var(--color-line)', borderRadius: 10, display: 'flex', alignItems: 'center', gap: 8, padding: '0 12px' }}>
          <NowIcon size={16} color="var(--color-brand)" />
          <Text fw={600} size={FS.body} style={{ flex: 1 }}>{now.label}</Text>
          <IconSelector size={15} color="var(--mantine-color-dimmed)" />
        </UnstyledButton>
        {right}
      </Group>
      <Drawer opened={open} onClose={() => setOpen(false)} position="bottom" title="Go to"
        styles={{ content: { borderRadius: '18px 18px 0 0', height: 'auto', maxHeight: '90dvh', flex: 'none', width: '100%' } }}>
        {groups.map((g) => (
          <Box key={g} mb="xs">
            <Text size={FS.micro} c="dimmed" tt="uppercase" fw={650} px={12} py={6} style={{ letterSpacing: '.07em' }}>{GROUP_TITLE[g]}</Text>
            {items.filter((i) => i.group === g).map((i) => {
              const on = i.key === active
              const Icon = i.icon
              return (
                <UnstyledButton key={i.key} onClick={() => { onPick(i.key); setOpen(false) }} aria-current={on ? 'page' : undefined}
                  px={12} py={10} style={{ display: 'flex', alignItems: 'center', gap: 12, width: '100%', borderRadius: 8,
                    background: on ? 'var(--color-brand-wash)' : undefined, color: on ? 'var(--color-brand-ink)' : undefined }}>
                  <Icon size={18} color={on ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)'} />
                  <Text size="sm" fw={on ? 600 : 400} style={{ flex: 1 }}>{i.label}</Text>
                  {i.count && !lockedFor(i, level) && <RailCount c={i.count} />}
                  {lockedFor(i, level) && <Badge size="xs" variant="light" color="slate" leftSection={<IconLock size={10} />}>DBA</Badge>}
                </UnstyledButton>
              )
            })}
          </Box>
        ))}
      </Drawer>
    </>
  )
}

/** The rail's foot: the link to who can connect and the settings, for an admin. */
export function RailSettingsLink({ dbId }: { dbId: string }) {
  return (
    <Group gap={6} wrap="nowrap">
      <IconSettings size={14} color="var(--mantine-color-dimmed)" />
      <Link to="/databases/$dbId/settings" params={{ dbId }} title="Who can connect, and this database's settings"
        style={{ fontSize: 12, fontWeight: 500, color: 'var(--color-brand)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
        Who can connect
      </Link>
    </Group>
  )
}

/* ── Summary tiles ──────────────────────────────────────────────────────── */

export interface SummaryTile {
  label: string
  value: string
  /** A few words after the number: "running", "1.2 GB", "daily". */
  note?: string
  /** Only when the number means something: verified, pending or denied. */
  tone?: 'verified' | 'pending' | 'denied'
}

const TONE: Record<NonNullable<SummaryTile['tone']>, { ink: string; wash: string; edge: string }> = {
  verified: { ink: 'var(--mantine-color-teal-text)', wash: 'transparent', edge: 'var(--color-line)' },
  pending: { ink: 'var(--mantine-color-amber-text)', wash: 'transparent', edge: 'var(--mantine-color-amber-light-hover)' },
  denied: { ink: 'var(--mantine-color-rose-text)', wash: 'var(--mantine-color-rose-light)', edge: 'var(--mantine-color-rose-light-hover)' },
}

/**
 * Up to four numbers a panel already loads, under its title. A tile adds no
 * query of its own: it only says again, larger, what the panel read.
 */
export function SummaryTiles({ tiles, narrow }: { tiles: SummaryTile[]; narrow: boolean }) {
  return (
    <Box data-testid="summary-tiles" style={{ display: 'grid', gap: narrow ? 8 : 12,
      gridTemplateColumns: `repeat(${narrow ? Math.min(2, tiles.length) : tiles.length}, minmax(0, 1fr))` }}>
      {tiles.map((t) => {
        const tone = t.tone ? TONE[t.tone] : null
        return (
          <Box key={t.label} px={14} py={10}
            style={{ borderRadius: 12, border: `1px solid ${tone?.edge ?? 'var(--color-line)'}`, background: tone?.wash && tone.wash !== 'transparent' ? tone.wash : 'var(--color-surface)',
              boxShadow: 'var(--shadow-card)', minWidth: 0 }}>
            <Text size={FS.micro} c="dimmed" fw={500} truncate>{t.label}</Text>
            <Group gap={8} wrap="nowrap" align="baseline" mt={2}>
              <Text fw={650} size="xl" style={{ letterSpacing: '-0.02em', color: tone?.ink, whiteSpace: 'nowrap' }}>{t.value}</Text>
              {t.note && <Text size={FS.micro} c="dimmed" truncate>{t.note}</Text>}
            </Group>
          </Box>
        )
      })}
    </Box>
  )
}
