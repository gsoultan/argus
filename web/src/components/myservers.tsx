import './ssh/ssh.css'
import { lazy, Suspense, useMemo, useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, Card, Group, Menu, Progress, SegmentedControl, Select, SimpleGrid, Skeleton, Stack, Text, TextInput,
  ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import {
  IconAppWindow, IconBrandWindows, IconChevronDown, IconClock, IconDeviceDesktop, IconDeviceLaptop, IconDeviceMobile, IconFolder,
  IconLayoutGrid, IconLock, IconSearch, IconTable, IconServer2, IconServerOff, IconSettings, IconTerminal2,
} from '@tabler/icons-react'
import { AssetAddresses } from '~/components/AssetAddresses'
import { DownloadSshConfigButton } from '~/components/ssh/OwnTerminal'
import { shortCommand } from '~/lib/sshConfig'
import { notifyOk } from '~/lib/notify'
import { EmptyState } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { now, relTime } from '~/components/primitives'
import { assetsQuery, meQuery, myServersQuery, requestsQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { useSessions } from '~/lib/sessions'
import type { Asset, MyServer } from '~/types/domain'
import { ANSWER_LABEL, answerOf, isUp, kindOf, offlineReason, reachOf, shortName, type Answer, type Reach } from '~/lib/reach'
import { ASSET_TYPES, ASSET_TYPE_LABEL, assetHostnameAside, assetShortName, assetTypeOf, type AssetType } from '~/lib/assetType'
import { needsUpdates, updatesWaitingQuery, type UpdatesWaitingRow } from '~/lib/updatesWaiting'
import { warningBadge, warningText, warningTone } from '~/lib/resourcesLow'
import { assetsUsageQuery, usageTone, type AssetUsageRow } from '~/lib/assetsUsage'
import { usePreference } from '~/lib/preferences'

/** Fetched only by someone who picks the table. */
import type { GroupBy } from '~/components/AssetTable'
const AssetTable = lazy(() => import('~/components/AssetTable').then((m) => ({ default: m.AssetTable })))

// Re-exported: every page that shows a server's status imports them from here.
export { ANSWER_LABEL, answerOf, isUp, kindOf, reachOf, shortName, type Answer, type Reach }
import { FS, SP, INK } from '~/theme'

type Show = 'all' | Answer
type Kind = 'all' | 'linux' | 'windows' | 'android'
type TypeFilter = 'all' | AssetType

const SHOWN: Answer[] = ['online', 'down', 'blocked']

/** What the Connect button opens: a phone's screen, a desktop or a terminal. */
function OpenIcon({ kind, rdp }: { kind: ReturnType<typeof kindOf>; rdp: boolean }) {
  if (kind === 'android') return <IconDeviceMobile size={16} />
  return rdp ? <IconDeviceDesktop size={16} /> : <IconTerminal2 size={16} />
}

const REACH: Record<Reach, { label: string; color: string; hint: string }> = {
  online: {
    label: 'Online',
    color: 'teal',
    hint: 'Its Argus agent is checking in.',
  },
  slow: {
    label: 'Slow',
    color: 'amber',
    hint: 'Answered at the last check, but slowly. Connecting may take a moment.',
  },
  offline: {
    label: ANSWER_LABEL.down,
    color: 'rose',
    hint: 'Its Argus agent has stopped checking in: the server may be off or cut off. You can still try; it may be back.',
  },
  unknown: {
    label: ANSWER_LABEL.down,
    color: 'rose',
    hint: 'No Argus agent has ever reported from it, so Argus cannot see it. Install the agent from its page. You can still try to connect.',
  },
  blocked: {
    label: 'Blocked',
    color: 'rose',
    hint: 'This server’s identity changed, so Argus will not connect to it until an admin checks it.',
  },
}

export function ReachBadge({ reach }: { reach: Reach }) {
  const r = REACH[reach]
  return (
    <Tooltip label={r.hint} multiline w={240}>
      <Badge color={r.color} variant="light" size="sm" leftSection={<Dot color={r.color} />}>
        {r.label}
      </Badge>
    </Tooltip>
  )
}

function Dot({ color }: { color: string }) {
  return (
    <Box
      w={6}
      h={6}
      style={{ borderRadius: '50%', background: `var(--mantine-color-${color}-5)` }}
    />
  )
}

/** One filter in the Assets bar: a word, a coloured dot and a count, on or off. */
function Pill({ pressed, onClick, label, count, dot }: {
  pressed: boolean; onClick: () => void; label: string; count?: number; dot?: string
}) {
  return (
    <button type="button" className="argus-pill" aria-pressed={pressed} onClick={onClick}>
      {dot && <span className="argus-pill-dot" style={{ background: `var(--mantine-color-${dot}-5)` }} />}
      {label}
      {count !== undefined && <>{' '}<span className="argus-pill-count">{count}</span></>}
    </button>
  )
}

/**
 * The card's usage row: processor, memory and the fullest disk from the
 * agent's last report, each a number and a bar coloured at the thresholds
 * the running-low badges use. A figure the agent did not send is left out
 * rather than drawn as zero.
 */
export function UsageRow({ usage }: { usage: AssetUsageRow }) {
  const cells: { key: string; label: string; aside?: string; value: number | null }[] = [
    { key: 'cpu', label: 'Processor', value: usage.cpu },
    { key: 'memory', label: 'Memory', value: usage.memory },
    { key: 'disk', label: 'Disk', aside: usage.disk?.mount, value: usage.disk?.percent ?? null },
  ]
  const shown = cells.filter((c) => c.value !== null)
  if (shown.length === 0) return null
  return (
    <Box
      pt={SP.snug}
      style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}
      data-testid="usage-row"
      title={`Reported ${relTime(usage.usageAt)}`}
    >
      <SimpleGrid cols={3} spacing="sm">
        {shown.map((c) => (
          <Box key={c.key} style={{ minWidth: 0 }} data-testid={`usage-${c.key}`}>
            <Text size={FS.micro} c="dimmed" truncate title={c.aside}>
              {c.label}
              {c.aside && <Text span ff="monospace" size={FS.micro}> {c.aside}</Text>}
            </Text>
            <Text size="sm" fw={600} c={usageTone(c.value!) === 'azure' ? undefined : `${usageTone(c.value!)}.7`}>
              {c.value}%
            </Text>
            <Progress value={c.value!} color={usageTone(c.value!)} size={5} radius="xl" mt={4}
              aria-label={`${c.label} ${c.value}%${c.key === 'disk' ? ' full' : c.key === 'cpu' ? ' busy' : ' used'}`} />
          </Box>
        ))}
      </SimpleGrid>
    </Box>
  )
}

/**
 * One server someone can use, and the shortest way onto it.
 *
 * Everything an administrator needs to judge a host -- pins, credential mode,
 * bypass posture -- is deliberately absent. The person reading this card has
 * one question, "can I get on it, and how", and the answer is the status, the
 * accounts they were given, and a button.
 */
export function ServerCard({ asset: a, admin = false, mine, waiting, usage, updates }: {
  asset: Asset; admin?: boolean; mine?: MyServer
  /** What is waiting to be updated on it, for those who see the fleet. */
  waiting?: UpdatesWaitingRow
  /**
   * Whether this person sees what is waiting across the fleet, and the agent
   * version offered: the card then always says its updates, "Up to date"
   * included. Undefined for someone who does not.
   */
  updates?: { latest: string }
  /**
   * Its processor, memory and fullest disk now, and what it is running low
   * on. Everyone gets it for the servers they see; absent when the agent
   * has not reported in the last few minutes.
   */
  usage?: AssetUsageRow
}) {
  const navigate = useNavigate()
  const { ask } = useSessions()
  const reach = reachOf(a)
  const rdp = a.protocol === 'rdp'
  const kind = kindOf(a)
  const type = assetTypeOf(a)
  // The type first: a laptop reads as a laptop whatever it runs.
  const KindIcon = kind === 'android' ? IconDeviceMobile : type === 'workstation' ? IconDeviceLaptop
    : kind === 'windows' ? IconBrandWindows : IconServer2
  const name = assetShortName(a)
  const aside = assetHostnameAside(a)
  const noAccount = a.principals.length === 0
  const disabled = reach === 'blocked' || noAccount
  // How this person may reach it from their own terminal, when they may.
  const direct = !rdp && mine?.personalAccount ? mine.directSsh : 'off'
  const tag = kind === 'android' ? { label: 'Android device', color: 'slate' }
    : rdp ? { label: 'Remote Desktop', color: 'slate' }
    : direct === 'allow' ? { label: 'SSH ready', color: 'teal' }
      : direct === 'approve' ? { label: 'SSH needs approval', color: 'amber' }
        : mine ? { label: 'Browser only', color: 'slate' } : null
  const openLabel = kind === 'android' ? 'Open device' : rdp ? 'Open desktop' : 'Open terminal'

  return (
    <Card padding="lg" className="argus-server" data-reach={reach} data-answer={answerOf(a)} h="100%">
      <Stack gap="md" h="100%">
        <Group justify="space-between" align="flex-start" wrap="nowrap" gap="sm">
          <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
            <ThemeIcon variant="light" color="azure" size={36} radius="md">
              <KindIcon size={20} stroke={1.6} />
            </ThemeIcon>
            <Box style={{ minWidth: 0 }}>
              <Text component="h3" fw={600} size="md" truncate style={{ margin: 0 }} title={aside}>
                {name}
              </Text>
              {aside && (
                <Text size={FS.meta} c="dimmed" ff="monospace" truncate data-testid="asset-hostname">{aside}</Text>
              )}
              <Text size={FS.meta} c="dimmed" truncate>
                {type === 'workstation' ? 'Workstation · ' : ''}{a.os} · {kind === 'android' ? 'Android device' : rdp ? 'Remote Desktop' : 'Terminal (SSH)'}
              </Text>
              <AssetAddresses asset={a} />
            </Box>
          </Group>
          <Stack gap={2} align="flex-end" style={{ flexShrink: 0 }}>
            <ReachBadge reach={reach} />
            {offlineReason(a, relTime) && (
              <Text size={FS.micro} c="dimmed" data-testid="offline-reason">{offlineReason(a, relTime)}</Text>
            )}
          </Stack>
        </Group>

        {updates && <UpdatesLine waiting={waiting} latest={updates.latest} checked={updatesChecked(a)} />}

        {((tag && reach !== 'blocked' && !noAccount) || !!usage?.warnings.length) && (
          <Group gap={SP.snug}>
            {tag && reach !== 'blocked' && !noAccount && (
              <Badge color={tag.color} variant="light" size="sm" radius="sm" tt="none" fw={600}>{tag.label}</Badge>
            )}
            {usage?.warnings.map((w) => (
              <Badge key={w.resource + (w.mount ?? '')} color={warningTone(w)} variant="light" size="sm" radius="sm" tt="none"
                title={warningText(w)} data-testid="low-badge">
                {warningBadge(w)}
              </Badge>
            ))}
          </Group>
        )}

        {reach === 'blocked' ? (
          <Group gap={SP.snug} wrap="nowrap" align="flex-start">
            <IconLock size={14} style={{ flexShrink: 0, marginTop: 2 }} opacity={0.7} />
            <Text size={FS.meta} c="dimmed" lh={1.5}>
              This server&rsquo;s identity changed. Nobody can connect until an admin checks it.
            </Text>
          </Group>
        ) : noAccount ? (
          <Text size={FS.meta} c="dimmed" lh={1.5}>
            {admin ? (
              <>
                Nobody can log in yet.{' '}
                <Link
                  to="/assets/$assetId"
                  params={{ assetId: a.id }}
                  className="argus-above"
                  style={{ color: INK.brand }}
                >
                  Add a login account
                </Link>
              </>
            ) : (
              'You have no account on this server yet. Ask an admin to add one for you.'
            )}
          </Text>
        ) : a.principals.length === 1 ? (
          <Text size={FS.meta} c="dimmed">
            Logs you in as <Text span ff="monospace" size={FS.meta}>{a.principals[0]}</Text>
          </Text>
        ) : (
          // More than one account: each is its own way in, and the card itself
          // opens the first.
          <Box className="argus-above">
            <Text size={FS.micro} c="dimmed" mb={SP.tight}>
              Log in as
            </Text>
            <Group gap={SP.snug}>
              {a.principals.map((p) => (
                <ButtonLink
                  key={p}
                  to="/connect"
                  search={{ host: a.hostname, as: p, open: true }}
                  size="compact-sm"
                  variant="default"
                  ff="monospace"
                  aria-label={`Connect to ${name} as ${p}`}
                >
                  {p}
                </ButtonLink>
              ))}
            </Group>
          </Box>
        )}

        {usage && <UsageRow usage={usage} />}

        <Group justify="space-between" align="center" mt="auto" wrap="nowrap" gap="sm">
          {admin ? (
            <Link
              to="/assets/$assetId"
              params={{ assetId: a.id }}
              className="argus-above"
              aria-label={`Manage ${name}`}
              style={{ color: INK.brand, fontSize: 13, fontWeight: 500, textDecoration: 'none', whiteSpace: 'nowrap' }}
            >
              Manage
            </Link>
          ) : (
            <Text size={FS.micro} c="dimmed">
              Checked {relTime(a.lastCheckedAt)}
            </Text>
          )}
          {disabled ? (
            <Button size="sm" disabled leftSection={<OpenIcon kind={kind} rdp={rdp} />}>
              {openLabel}
            </Button>
          ) : (
            <Group gap={0} wrap="nowrap" className="argus-splitbtn">
              <ButtonLink
                to="/connect"
                search={{ host: a.hostname, as: a.principals[0], open: true }}
                size="sm"
                className="argus-stretch"
                variant={reach === 'offline' ? 'default' : 'filled'}
                leftSection={<OpenIcon kind={kind} rdp={rdp} />}
                aria-label={`Connect to ${name}`}
                data-split={!rdp || undefined}
              >
                {openLabel}
              </ButtonLink>
              {!rdp && (
                <Menu position="bottom-end" width={280} shadow="md" withinPortal>
                  <Menu.Target>
                    <ActionIcon size={36} variant={reach === 'offline' ? 'default' : 'filled'}
                      className="argus-above argus-splitbtn-more" aria-label={`More ways onto ${name}`}>
                      <IconChevronDown size={16} />
                    </ActionIcon>
                  </Menu.Target>
                  <Menu.Dropdown>
                    <Menu.Item leftSection={<IconAppWindow size={18} color={INK.brand} />}
                      onClick={() => void navigate({ to: '/connect', search: { host: a.hostname, as: a.principals[0], open: true } })}>
                      <MenuText title="Open terminal in browser" sub={`As ${a.principals[0]}${a.principals.length > 1 ? ' · or pick another account' : ''}`} />
                    </Menu.Item>
                    {direct !== 'off' && (
                      <Menu.Item leftSection={<IconDeviceLaptop size={18} color="var(--mantine-color-violet-6)" />}
                        onClick={() => {
                          const cmd = shortCommand({ hostname: a.hostname, address: a.address })
                          void navigator.clipboard?.writeText(cmd).then(
                            () => notifyOk('Copied', `${cmd} works once your SSH config has the Argus entries.`),
                            () => undefined,
                          )
                        }}>
                        <MenuText title="Copy SSH command" sub={shortCommand({ hostname: a.hostname, address: a.address })} mono />
                      </Menu.Item>
                    )}
                    <Menu.Item leftSection={<IconFolder size={18} />}
                      onClick={() => void navigate({ to: '/connect', search: { host: a.hostname, as: a.principals[0], open: true, view: 'files' } })}>
                      <MenuText title="Open files" sub="Browse, upload and download, recorded" />
                    </Menu.Item>
                    <Menu.Divider />
                    <Menu.Item leftSection={<IconSettings size={18} />}
                      onClick={() => ask(a.hostname)}>
                      <MenuText title="All connect options…" sub="Ways in, accounts, what happens" />
                    </Menu.Item>
                  </Menu.Dropdown>
                </Menu>
              )}
            </Group>
          )}
        </Group>
      </Stack>
    </Card>
  )
}

/**
 * What is waiting to be updated on one asset, as short chips: the Argus agent
 * behind (from → to), a new OS release (a major upgrade), security fixes,
 * other patches, a restart. "Up to date" when nothing waits, so a card that
 * says nothing never leaves someone wondering whether it was checked.
 */
export function UpdatesLine({ waiting: w, latest, checked, compact = false }: {
  waiting?: UpdatesWaitingRow; latest: string
  /** Whether Argus reads this asset's updates at all: see updatesChecked. */
  checked: boolean
  compact?: boolean
}) {
  const chips = updateChips(w, latest)
  return (
    <Group gap={SP.tight} wrap="wrap" data-testid="updates-line">
      {!compact && <Text size={FS.micro} c="dimmed" mr={2}>Updates</Text>}
      {chips.length === 0 ? (
        checked
          ? <Text size={FS.micro} c="teal.7" fw={600}>Up to date</Text>
          : <Text size={FS.micro} c="dimmed" title="No agent has reported this asset's updates">Not checked</Text>
      ) : chips.map((c) => (
        <Badge key={c.key} color={c.color} variant="light" size="sm" radius="sm" tt="none" title={c.title}>{c.label}</Badge>
      ))}
    </Group>
  )
}

/**
 * Whether "Up to date" can be said of an asset: its agent has reported, and
 * it is a server whose packages Argus reads (over SSH). Anything else would be
 * claiming a check that never ran.
 */
export function updatesChecked(a: Asset): boolean {
  return a.agentState !== 'absent' && a.protocol !== 'rdp' && a.kind !== 'android'
}

/** The chips UpdatesLine draws, in the order they matter. */
export function updateChips(w: UpdatesWaitingRow | undefined, latest: string): { key: string; label: string; color: string; title: string }[] {
  if (!w) return []
  const out: { key: string; label: string; color: string; title: string }[] = []
  if (w.agent === 'behind') {
    out.push({ key: 'agent', color: 'azure', label: w.agentVersion && latest ? `Agent ${w.agentVersion} → ${latest}` : 'Agent behind',
      title: 'A newer Argus agent is out. It updates itself once nobody is connected.' })
  } else if (w.agent === 'updating') {
    out.push({ key: 'agent', color: 'azure', label: 'Agent updating', title: 'The Argus agent is updating now.' })
  } else if (w.agent === 'cannot' || w.agent === 'too_old') {
    out.push({ key: 'agent', color: 'amber', label: 'Agent needs a manual update', title: 'This agent cannot update itself. Open the asset for the command.' })
  }
  if (w.releaseNext) out.push({ key: 'release', color: 'violet', label: `${w.releaseNext} available`, title: 'A new operating system release (a major upgrade). Upgrade from the asset’s Updates tab.' })
  if (w.security > 0) out.push({ key: 'security', color: 'rose', label: `${w.security} security`, title: 'Security fixes waiting to be installed.' })
  const other = w.updates - w.security
  if (other > 0) out.push({ key: 'patches', color: 'slate', label: `${other} ${other === 1 ? 'patch' : 'patches'}`, title: 'Other package updates waiting.' })
  if (w.rebootRequired) out.push({ key: 'restart', color: 'amber', label: 'Restart needed', title: 'An installed update takes effect after a restart.' })
  return out
}

function MenuText({ title, sub, mono }: { title: string; sub: string; mono?: boolean }) {
  return (
    <Box>
      <Text size="sm" fw={500}>{title}</Text>
      <Text size={FS.micro} c="dimmed" ff={mono ? 'monospace' : undefined} lh={1.4}>{sub}</Text>
    </Box>
  )
}

export function ServerCardSkeleton() {
  return (
    <Card padding="lg" h={190}>
      <Group gap="sm" wrap="nowrap">
        <Skeleton height={36} width={36} radius="md" />
        <Stack gap={SP.snug} style={{ flex: 1 }}>
          <Skeleton height={12} width="50%" />
          <Skeleton height={10} width="70%" />
        </Stack>
      </Group>
      <Skeleton height={22} width="60%" mt="lg" />
      <Skeleton height={26} width={90} mt="lg" ml="auto" />
    </Card>
  )
}

/**
 * A server reached through an approved access request rather than an
 * assignment.
 *
 * The control plane's asset list is assignments only, so a host someone was
 * granted for an hour is often not in it at all. Without this, the approval
 * arrived and the server it was for appeared nowhere on the page meant to list
 * what you can reach.
 */
export function GrantCard({
  hostname, principal, expiresAt,
}: { hostname: string; principal: string; expiresAt: string | null }) {
  const name = shortName(hostname)
  return (
    <Card padding="lg" className="argus-server" data-reach="granted" h="100%">
      <Stack gap="md" h="100%">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <ThemeIcon variant="light" color="amber" size={36} radius="md">
            <IconClock size={20} stroke={1.6} />
          </ThemeIcon>
          <Box style={{ minWidth: 0 }}>
            <Text component="h3" fw={600} size="md" truncate style={{ margin: 0 }}>
              {name}
            </Text>
            <Text size={FS.meta} c="dimmed" truncate>
              Temporary access, as <Text span ff="monospace" size={FS.meta}>{principal}</Text>
            </Text>
          </Box>
        </Group>
        <Group justify="space-between" align="center" mt="auto" wrap="nowrap" gap="sm">
          <Text size={FS.micro} c={INK.pending}>
            {expiresAt ? `Access ends ${relTime(expiresAt)}` : 'Approved'}
          </Text>
          <ButtonLink
            to="/connect"
            search={{ host: hostname, as: principal, open: true }}
            size="sm"
            className="argus-stretch"
            leftSection={<IconTerminal2 size={16} />}
            aria-label={`Connect to ${name} as ${principal}`}
          >
            Connect
          </ButtonLink>
        </Group>
      </Stack>
    </Card>
  )
}

export interface AssetFilter {
  text: string
  show: Show
  kind: Kind
  type: TypeFilter
  needs: boolean
  lowOnly: boolean
  needsUpdate: (a: Asset) => boolean
  low: (a: Asset) => boolean
}

/** The words an asset is found by: its names, every address, OS, tags and accounts. */
function haystack(a: Asset): string {
  return [a.alias ?? '', assetShortName(a), a.hostname, a.agentHostname ?? '', a.address,
    ...(a.addresses ?? []).map((x) => x.address), a.os, ...a.tags, ...a.principals,
    ASSET_TYPE_LABEL[assetTypeOf(a)].one, ANSWER_LABEL[answerOf(a)]].join(' ').toLowerCase()
}

/**
 * The assets that fit what was picked. Every word typed must appear
 * somewhere (so "core prod" finds core-04 tagged production), and every
 * other filter narrows further. The one place both the cards and the table
 * get their rows, so the two always agree.
 */
export function filterAssets(assets: Asset[], f: AssetFilter): Asset[] {
  const words = f.text.trim().toLowerCase().split(/\s+/).filter(Boolean)
  return assets.filter((a) => {
    if (words.length > 0) {
      const hay = haystack(a)
      if (!words.every((w) => hay.includes(w))) return false
    }
    if (f.show !== 'all' && answerOf(a) !== f.show) return false
    if (f.kind !== 'all' && kindOf(a) !== f.kind) return false
    if (f.type !== 'all' && assetTypeOf(a) !== f.type) return false
    if (f.needs && !f.needsUpdate(a)) return false
    if (f.lowOnly && !f.low(a)) return false
    return true
  })
}

/** Blocked, then offline, then the rest; by name within each. */
export function sortAssets(assets: Asset[]): Asset[] {
  const rank: Record<Answer, number> = { blocked: 0, down: 1, online: 2 }
  return [...assets].sort((x, y) => rank[answerOf(x)] - rank[answerOf(y)] || assetShortName(x).localeCompare(assetShortName(y)))
}

/**
 * The servers the signed-in person can reach, as cards with a Connect button
 * on each, plus any temporary access from approved requests.
 *
 * Asked for directly: someone who has been given servers opened the console and
 * met fleet posture, host-key pins and a coverage report before finding the two
 * machines they were there to use. These cards answer "which servers can I get
 * on, and how", for people who do not need the vocabulary behind it.
 *
 * It shows only what the control plane already sent. For an operator that list
 * is narrowed on the server to their assignments (`AssetsForUser`), with each
 * host's accounts cut down to the ones they were given -- so nothing here is a
 * filter the console applies and could get wrong.
 */
export function ServerCards({ initialShow = 'all' }: { initialShow?: Show } = {}) {
  const { data: me } = useQuery(meQuery())
  const { data: assets } = useQuery(assetsQuery({}))
  const { data: approved } = useQuery(requestsQuery('approved'))
  const { data: mine } = useQuery({ ...myServersQuery(), retry: false })
  const mineBy = useMemo(() => new Map((mine ?? []).map((m) => [m.hostname, m])), [mine])
  const whole = seesWholeFleet(me?.role)
  const canAdmin = me?.role === 'admin' || me?.role === 'owner'

  const [text, setText] = useState('')
  const [show, setShow] = useState<Show>(initialShow)
  const [kind, setKind] = useState<Kind>('all')
  const [type, setType] = useState<TypeFilter>('all')
  const [needs, setNeeds] = useState(false)
  const { data: waiting } = useQuery({ ...updatesWaitingQuery(), enabled: whole })
  const [view, setView] = usePreference('assets.view')
  const [groupBy, setGroupBy] = useState<GroupBy>('needs')
  const waitingBy = useMemo(() => new Map((waiting?.assets ?? []).map((w) => [w.assetId, w])), [waiting])
  const needCount = (assets ?? []).filter((a) => needsUpdates(waitingBy.get(a.id))).length
  const [lowOnly, setLowOnly] = useState(false)
  // Everyone's own servers, or the whole fleet for those who see it: the
  // control plane decides which, by the same rule as the list above.
  const { data: usage } = useQuery(assetsUsageQuery())
  const usageBy = useMemo(() => new Map((usage?.assets ?? []).map((r) => [r.assetId, r])), [usage])
  const lowBy = useMemo(() => new Map((usage?.assets ?? []).filter((r) => r.warnings.length > 0).map((r) => [r.assetId, r])), [usage])
  const lowCount = (assets ?? []).filter((a) => lowBy.has(a.id)).length

  // Approved and not yet expired, for this person only: an approver is sent the
  // whole queue, and someone else's grant is not a server you can reach.
  // A grant for an account the person already holds on that server adds
  // nothing they can do, so it is left to the list below rather than shown
  // twice.
  const grants = useMemo(() => {
    if (!approved || !me) return []
    const t = now()
    const held = new Set((assets ?? []).flatMap((a) => a.principals.map((p) => `${a.hostname}\u0000${p}`)))
    return approved
      .filter((r) => r.requesterEmail.toLowerCase() === me.email.toLowerCase())
      .filter((r) => !r.expiresAt || Date.parse(r.expiresAt) > t)
      .flatMap((r) =>
        r.assetHostnames.map((h) => ({
          key: `${r.id}:${h}`,
          hostname: h,
          principal: r.principal,
          expiresAt: r.expiresAt,
        })),
      )
      .filter((g) => !held.has(`${g.hostname}\u0000${g.principal}`))
  }, [approved, me, assets])

  const kinds = useMemo(() => new Set((assets ?? []).map(kindOf)), [assets])
  const hasBothKinds = kinds.size > 1
  const typeCounts = useMemo(() => {
    const c: Record<AssetType, number> = { server: 0, workstation: 0, device: 0 }
    for (const a of assets ?? []) c[assetTypeOf(a)]++
    return c
  }, [assets])
  const hasTypes = ASSET_TYPES.filter((t) => typeCounts[t] > 0).length > 1

  const counts = useMemo(() => {
    if (!assets) return undefined
    const c: Record<Answer, number> = { online: 0, down: 0, blocked: 0 }
    for (const a of assets) c[answerOf(a)]++
    return { all: assets.length, ...c }
  }, [assets])

  const needle = text.trim().toLowerCase()
  const visible = useMemo(
    () => sortAssets(filterAssets(assets ?? [], {
      text, show, kind, type, needs, lowOnly,
      needsUpdate: (a) => needsUpdates(waitingBy.get(a.id)),
      low: (a) => lowBy.has(a.id),
    })),
    [assets, text, show, kind, type, needs, waitingBy, lowOnly, lowBy],
  )

  const filtering = needle !== '' || show !== 'all' || kind !== 'all' || type !== 'all' || needs || lowOnly
  const clear = () => {
    setNeeds(false)
    setLowOnly(false)
    setText('')
    setShow('all')
    setKind('all')
    setType('all')
  }

  return (
    <>
      {grants.length > 0 && (
        <Box>
          <Text component="h2" size="sm" fw={600} mb={SP.tight} style={{ margin: 0 }}>
            Temporary access
          </Text>
          <Text size={FS.meta} c="dimmed" mb="sm">
            Approved requests. These stop working when the time runs out.
          </Text>
          <CardGrid>
            {grants.map((g) => (
              <GrantCard key={g.key} hostname={g.hostname} principal={g.principal} expiresAt={g.expiresAt} />
            ))}
          </CardGrid>
        </Box>
      )}

      {assets && assets.length > 0 && (
        <Box className="argus-filterbar" data-testid="asset-filters">
          <Group gap="sm" wrap="wrap" align="center">
            <TextInput
              style={{ flex: '1 1 220px', minWidth: 180 }}
              size="sm"
              aria-label="Find a server"
              placeholder="Find by name, IP or tag"
              leftSection={<IconSearch size={15} />}
              value={text}
              onChange={(e) => setText(e.currentTarget.value)}
            />
            {hasTypes && (
              <Select
                size="sm"
                w={170}
                aria-label="Type of asset"
                value={type}
                allowDeselect={false}
                onChange={(v) => v && setType(v as TypeFilter)}
                data={[
                  { value: 'all', label: 'All types' },
                  ...ASSET_TYPES.filter((t) => typeCounts[t] > 0).map((t) => ({
                    value: t,
                    label: `${ASSET_TYPE_LABEL[t].many} · ${typeCounts[t]}`,
                  })),
                ]}
              />
            )}
            {hasBothKinds && (
              <Select
                size="sm"
                w={150}
                aria-label="Kind of server"
                value={kind}
                allowDeselect={false}
                onChange={(v) => v && setKind(v as Kind)}
                data={[
                  { value: 'all', label: 'Any system' },
                  { value: 'linux', label: 'Linux' },
                  { value: 'windows', label: 'Windows' },
                  { value: 'android', label: 'Android' },
                ].filter((k) => k.value === 'all' || kinds.has(k.value as 'linux' | 'windows' | 'android'))}
              />
            )}
            <Group gap="xs" wrap="nowrap" ml="auto">
              <SegmentedControl
                size="sm"
                aria-label="Show as"
                value={view}
                onChange={(v) => setView(v as 'cards' | 'table')}
                data={[
                  { value: 'cards', label: <Group gap={6} wrap="nowrap"><IconLayoutGrid size={15} />Cards</Group> },
                  { value: 'table', label: <Group gap={6} wrap="nowrap"><IconTable size={15} />Table</Group> },
                ]}
              />
              <DownloadSshConfigButton icon />
            </Group>
          </Group>

          <Group className="argus-filterbar-pills" gap="xs" wrap="wrap" align="center" justify="space-between">
            <Group gap={6} wrap="wrap" role="group" aria-label="Status">
              <Pill pressed={show === 'all'} onClick={() => setShow('all')} label="All" count={counts?.all} />
              {/* Blocked is rare, so it is offered only while a server is. */}
              {SHOWN.filter((k) => k !== 'blocked' || (counts?.blocked ?? 0) > 0 || show === 'blocked').map((k) => (
                <Pill key={k} pressed={show === k} onClick={() => setShow(show === k ? 'all' : k)}
                  label={ANSWER_LABEL[k]} count={counts?.[k]} dot={k === 'online' ? 'teal' : 'rose'} />
              ))}
              {(needCount > 0 || needs || lowCount > 0 || lowOnly) && (
                <Box aria-hidden w={1} h={18} mx={4} style={{ background: 'var(--color-line)' }} />
              )}
              {(needCount > 0 || needs) && (
                <Pill pressed={needs} onClick={() => setNeeds(!needs)} label="Needs updates" count={needCount} dot="azure" />
              )}
              {(lowCount > 0 || lowOnly) && (
                <Pill pressed={lowOnly} onClick={() => setLowOnly(!lowOnly)} label="Running low" count={lowCount}
                  dot={[...lowBy.values()].some((r) => r.warnings[0]?.level === 'critical') ? 'rose' : 'amber'} />
              )}
            </Group>
            <Group gap="sm" wrap="nowrap">
              {filtering && (
                <Button size="compact-xs" variant="subtle" color="gray" onClick={clear}>Clear</Button>
              )}
              <Text size={FS.meta} c="dimmed" data-testid="asset-count">
                {filtering ? `${visible.length} of ${assets.length}` : assets.length}{' '}
                {assets.length === 1 ? 'asset' : 'assets'}
              </Text>
              {view === 'table' && (
                <Select
                  size="xs"
                  w={190}
                  aria-label="Group by"
                  value={groupBy}
                  allowDeselect={false}
                  onChange={(v) => v && setGroupBy(v as GroupBy)}
                  data={[
                    { value: 'needs', label: 'Group: what needs you' },
                    { value: 'none', label: 'No groups' },
                  ]}
                />
              )}
            </Group>
          </Group>
        </Box>
      )}

      {assets === undefined ? (
        <CardGrid busy>
          {Array.from({ length: 6 }, (_, i) => <ServerCardSkeleton key={i} />)}
        </CardGrid>
      ) : assets.length === 0 ? (
        <EmptyState
          icon={IconServerOff}
          title={whole ? 'There are no servers yet.' : 'No servers have been given to you yet.'}
          description={
            whole
              ? 'Add your first server. It takes one line on the server and about a minute.'
              : // Not "request access": the request form can only offer the
                // servers the control plane sent, which for this person is
                // none, so pointing at it would be a next step that cannot be
                // taken. Assignment is an administrator's, so say so.
                'An admin decides who can reach each server. Ask your admin to give you the servers you need, and they will appear here.'
          }
          action={canAdmin && <ButtonLink to="/assets/add">Add server</ButtonLink>}
        />
      ) : visible.length === 0 ? (
        <EmptyState
          icon={IconSearch}
          title="No server matches."
          description={`None of your ${assets.length} servers fit what you picked above.`}
          action={
            <Button variant="default" onClick={clear}>
              Show all servers
            </Button>
          }
        />
      ) : view === 'table' ? (
        <Suspense fallback={<Skeleton h={320} />}>
          <AssetTable assets={visible} groupBy={groupBy} admin={canAdmin} mineBy={mineBy} waitingBy={waitingBy} usageBy={usageBy}
            updates={waiting ? { latest: waiting.latest } : undefined} />
        </Suspense>
      ) : (
        <CardGrid>
          {visible.map((a) => (
            <ServerCard key={a.id} asset={a} admin={canAdmin} mine={mineBy.get(a.hostname)}
              waiting={waitingBy.get(a.id)} usage={usageBy.get(a.id)}
              updates={waiting ? { latest: waiting.latest } : undefined} />
          ))}
        </CardGrid>
      )}
    </>
  )
}

/**
 * As many cards a row as fit at a readable width: one on a phone, five on a
 * 1920px screen. A plain CSS grid (`.argus-cardgrid`) rather than Mantine's,
 * because the column count follows the width rather than a breakpoint table.
 */
function CardGrid({ children, busy }: { children: ReactNode; busy?: boolean }) {
  return (
    <div className="argus-cardgrid" aria-busy={busy || undefined} aria-label={busy ? 'Loading servers' : undefined}>
      {children}
    </div>
  )
}
