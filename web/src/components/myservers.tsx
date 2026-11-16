import './ssh/ssh.css'
import { useMemo, useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Button, Card, Group, Menu, Progress, SegmentedControl, SimpleGrid, Skeleton, Stack, Text, TextInput,
  ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import {
  IconAppWindow, IconBrandWindows, IconChevronDown, IconClock, IconDeviceDesktop, IconDeviceLaptop, IconDeviceMobile, IconFolder,
  IconLock, IconSearch, IconServer2, IconServerOff, IconSettings, IconTerminal2,
} from '@tabler/icons-react'
import { AssetAddresses } from '~/components/AssetAddresses'
import { DownloadSshConfigButton } from '~/components/ssh/OwnTerminal'
import { shortCommand } from '~/lib/sshConfig'
import { notifyOk } from '~/lib/notify'
import { EmptyState, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { now, relTime } from '~/components/primitives'
import { assetsQuery, meQuery, myServersQuery, requestsQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { useSessions } from '~/lib/sessions'
import type { Asset, MyServer } from '~/types/domain'
import { ANSWER_LABEL, answerOf, isUp, kindOf, reachOf, shortName, type Answer, type Reach } from '~/lib/reach'
import { ASSET_TYPES, ASSET_TYPE_LABEL, assetHostnameAside, assetShortName, assetTypeOf, type AssetType } from '~/lib/assetType'
import { needsUpdates, updatesWaitingQuery, type UpdatesWaitingRow } from '~/lib/updatesWaiting'
import { warningBadge, warningText, warningTone } from '~/lib/resourcesLow'
import { assetsUsageQuery, usageTone, type AssetUsageRow } from '~/lib/assetsUsage'
import { isConfigured } from '~/lib/live'

// Re-exported: every page that shows a server's status imports them from here.
export { ANSWER_LABEL, answerOf, isUp, kindOf, reachOf, shortName, type Answer, type Reach }
import { FS, SP, INK } from '~/theme'

type Show = 'all' | Answer
type Kind = 'all' | 'linux' | 'windows' | 'android'
type TypeFilter = 'all' | AssetType

const SHOWN: Answer[] = ['online', 'down', 'noagent', 'blocked']

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
    hint: 'Its Argus agent has stopped checking in. You can still try; it may be back.',
  },
  unknown: {
    label: ANSWER_LABEL.noagent,
    color: 'slate',
    hint: 'No Argus agent reports from it, so Argus cannot tell whether it is up. You can still try.',
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
export function ServerCard({ asset: a, admin = false, mine, waiting, usage }: {
  asset: Asset; admin?: boolean; mine?: MyServer
  /** What is waiting to be updated on it, for those who see the fleet. */
  waiting?: UpdatesWaitingRow
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
    <Card padding="lg" className="argus-server" data-reach={reach} h="100%">
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
          <ReachBadge reach={reach} />
        </Group>

        {((tag && reach !== 'blocked' && !noAccount) || needsUpdates(waiting) || !!usage?.warnings.length) && (
          <Group gap={SP.snug}>
            {tag && reach !== 'blocked' && !noAccount && (
              <Badge color={tag.color} variant="light" size="sm" radius="sm" tt="none" fw={600}>{tag.label}</Badge>
            )}
            {!!waiting?.security && <Badge color="rose" variant="light" size="sm" radius="sm" tt="none">{waiting.security} security</Badge>}
            {!waiting?.security && !!waiting?.updates && <Badge color="slate" variant="light" size="sm" radius="sm" tt="none">{waiting.updates} updates</Badge>}
            {waiting?.agent === 'behind' && <Badge color="azure" variant="light" size="sm" radius="sm" tt="none">agent behind</Badge>}
            {waiting?.rebootRequired && <Badge color="amber" variant="light" size="sm" radius="sm" tt="none">restart</Badge>}
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
  const { data: waiting } = useQuery({ ...updatesWaitingQuery(), enabled: whole && isConfigured() })
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
    const c: Record<Answer, number> = { online: 0, down: 0, noagent: 0, blocked: 0 }
    for (const a of assets) c[answerOf(a)]++
    return { all: assets.length, ...c }
  }, [assets])

  const needle = text.trim().toLowerCase()
  const visible = useMemo(
    () =>
      (assets ?? [])
        .filter((a) => {
          if (needle) {
            const hay = `${a.alias ?? ''} ${a.hostname} ${a.address} ${(a.addresses ?? []).map((x) => x.address).join(' ')} ${a.os} ${a.tags.join(' ')} ${a.principals.join(' ')}`
            if (!hay.toLowerCase().includes(needle)) return false
          }
          if (show !== 'all' && answerOf(a) !== show) return false
          if (kind !== 'all' && kindOf(a) !== kind) return false
          if (type !== 'all' && assetTypeOf(a) !== type) return false
          if (needs && !needsUpdates(waitingBy.get(a.id))) return false
          if (lowOnly && !lowBy.has(a.id)) return false
          return true
        })
        .sort((x, y) => assetShortName(x).localeCompare(assetShortName(y))),
    [assets, needle, show, kind, type, needs, waitingBy, lowOnly, lowBy],
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

      <Toolbar
        right={
          assets && (
            <Group gap="md" wrap="nowrap">
              <Text size={FS.micro} c="dimmed">
                {filtering ? `${visible.length} of ${assets.length}` : assets.length}{' '}
                {assets.length === 1 ? 'asset' : 'assets'}
              </Text>
              <DownloadSshConfigButton size="sm" />
            </Group>
          )
        }
      >
        {hasTypes && (
          <SegmentedControl
            aria-label="Type of asset"
            value={type}
            onChange={(v) => setType(v as TypeFilter)}
            data={[
              { value: 'all', label: 'All' },
              ...ASSET_TYPES.filter((t) => typeCounts[t] > 0).map((t) => ({
                value: t,
                label: `${ASSET_TYPE_LABEL[t].many} (${typeCounts[t]})`,
              })),
            ]}
          />
        )}
        <TextInput
          w={260}
          aria-label="Find a server"
          placeholder="Find by name, IP or tag"
          leftSection={<IconSearch size={14} />}
          value={text}
          onChange={(e) => setText(e.currentTarget.value)}
        />
        <SegmentedControl
          aria-label="Status"
          value={show}
          onChange={(v) => setShow(v as Show)}
          data={[
            { value: 'all', label: counts ? `All (${counts.all})` : 'All' },
            // Blocked is rare, so it is offered only while a server is.
            ...SHOWN.filter((k) => k !== 'blocked' || (counts?.blocked ?? 0) > 0 || show === 'blocked').map((k) => ({
              value: k, label: counts ? `${ANSWER_LABEL[k]} (${counts[k]})` : ANSWER_LABEL[k],
            })),
          ]}
        />
        {(needCount > 0 || needs) && (
          <Button size="xs" variant={needs ? 'filled' : 'default'} aria-pressed={needs} onClick={() => setNeeds(!needs)}>
            Needs updates ({needCount})
          </Button>
        )}
        {(lowCount > 0 || lowOnly) && (
          <Button size="xs" variant={lowOnly ? 'filled' : 'default'} aria-pressed={lowOnly} onClick={() => setLowOnly(!lowOnly)}
            color={[...lowBy.values()].some((r) => r.warnings[0]?.level === 'critical') ? 'rose' : 'amber'}>
            Running low ({lowCount})
          </Button>
        )}
        {hasBothKinds && (
          <SegmentedControl
            aria-label="Kind of server"
            value={kind}
            onChange={(v) => setKind(v as Kind)}
            data={[
              { value: 'all', label: 'Any' },
              { value: 'linux', label: 'Linux' },
              { value: 'windows', label: 'Windows' },
              { value: 'android', label: 'Android' },
            ].filter((k) => k.value === 'all' || kinds.has(k.value as 'linux' | 'windows' | 'android'))}
          />
        )}
      </Toolbar>

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
      ) : (
        <CardGrid>
          {visible.map((a) => (
            <ServerCard key={a.id} asset={a} admin={canAdmin} mine={mineBy.get(a.hostname)}
              waiting={waitingBy.get(a.id)} usage={usageBy.get(a.id)} />
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
