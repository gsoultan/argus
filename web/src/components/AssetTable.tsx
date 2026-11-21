import { Fragment, useState, type ReactNode } from 'react'
import {
  Anchor, Box, Group, Progress, Select, Stack, Table, Text, UnstyledButton,
} from '@mantine/core'
import { IconChevronDown, IconChevronRight, IconDeviceDesktop, IconDeviceMobile, IconTerminal2 } from '@tabler/icons-react'
import { AssetAddresses } from '~/components/AssetAddresses'
import { addressesOf } from '~/lib/assetAddresses'
import { AnchorLink, ButtonLink } from '~/components/links'
import { relTime } from '~/components/primitives'
import { ReachBadge, UpdatesLine, updateChips, updatesChecked } from '~/components/myservers'
import { needsUpdates, type UpdatesWaitingRow } from '~/lib/updatesWaiting'
import { usageTone, type AssetUsageRow } from '~/lib/assetsUsage'
import { answerOf, kindOf, offlineReason, reachOf } from '~/lib/reach'
import { assetHostnameAside, assetShortName } from '~/lib/assetType'
import type { Asset, MyServer } from '~/types/domain'
import { FS, INK, SP } from '~/theme'

/*
 * Assets as a table: one row per asset, grouped by what needs someone, and a
 * row that opens in place to show the rest (accounts, every address, the
 * fullest disk, updates) without leaving the list.
 *
 * It draws the rows the cards would, in the same order, from the same
 * filtered list (filterAssets in myservers.tsx), so switching between the two
 * never changes what is shown.
 */

export type GroupBy = 'needs' | 'none'

interface Props {
  assets: Asset[]
  admin: boolean
  mineBy: Map<string, MyServer>
  waitingBy: Map<string, UpdatesWaitingRow>
  usageBy: Map<string, AssetUsageRow>
  /** Set for someone who sees what is waiting across the fleet. */
  updates?: { latest: string }
}

export type GroupKey = 'blocked' | 'offline' | 'updates' | 'low' | 'fine'

const GROUP_LABEL: Record<GroupKey, string> = {
  blocked: 'Blocked',
  offline: 'Offline',
  updates: 'Needs updates',
  low: 'Running low',
  fine: 'Online, nothing waiting',
}

/** The first group an asset belongs to, most urgent first. */
export function groupOf(a: Asset, waiting: UpdatesWaitingRow | undefined, usage: AssetUsageRow | undefined): GroupKey {
  const ans = answerOf(a)
  if (ans === 'blocked') return 'blocked'
  if (ans === 'down') return 'offline'
  if (needsUpdates(waiting)) return 'updates'
  if (usage && usage.warnings.length > 0) return 'low'
  return 'fine'
}

export function AssetTable({ assets, admin, mineBy, waitingBy, usageBy, updates }: Props) {
  const [groupBy, setGroupBy] = useState<GroupBy>('needs')
  const [open, setOpen] = useState<string | null>(null)
  const [showFine, setShowFine] = useState(false)
  const toggle = (id: string) => setOpen((o) => (o === id ? null : id))

  const groups: { key: GroupKey | 'all'; rows: Asset[] }[] = []
  if (groupBy === 'none') {
    groups.push({ key: 'all', rows: assets })
  } else {
    const by: Record<GroupKey, Asset[]> = { blocked: [], offline: [], updates: [], low: [], fine: [] }
    for (const a of assets) by[groupOf(a, waitingBy.get(a.id), usageBy.get(a.id))].push(a)
    for (const k of ['blocked', 'offline', 'updates', 'low', 'fine'] as GroupKey[]) {
      if (by[k].length > 0) groups.push({ key: k, rows: by[k] })
    }
  }
  // When everything is fine there is nothing to fold away from.
  const foldFine = groupBy === 'needs' && groups.length > 1
  const cols = updates ? 7 : 6

  return (
    <Stack gap="sm">
      <Group gap="sm">
        <Select
          size="xs"
          w={220}
          aria-label="Group by"
          value={groupBy}
          allowDeselect={false}
          onChange={(v) => v && setGroupBy(v as GroupBy)}
          data={[
            { value: 'needs', label: 'Group by: what needs you' },
            { value: 'none', label: 'No groups' },
          ]}
        />
      </Group>
      <Box className="argus-table-wrap" style={{ overflowX: 'auto', border: '1px solid var(--color-line)', borderRadius: 10, background: 'var(--color-surface)' }}>
        <Table verticalSpacing={8} horizontalSpacing="sm" className="argus-assettable" data-testid="asset-table" miw={900}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th style={{ width: 28 }} aria-label="Open the row" />
              <Table.Th>Asset</Table.Th>
              <Table.Th>IP address</Table.Th>
              <Table.Th>State</Table.Th>
              <Table.Th style={{ minWidth: 170 }}>Resources</Table.Th>
              {updates && <Table.Th>Updates</Table.Th>}
              <Table.Th style={{ textAlign: 'right' }} aria-label="Open" />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {groups.map((g) => {
              const folded = foldFine && g.key === 'fine' && !showFine
              return (
                <Fragment key={g.key}>
                  {g.key !== 'all' && (
                    <Table.Tr className="argus-assettable-group" data-group={g.key}>
                      <Table.Td colSpan={cols}>
                        <Group gap="xs">
                          <Text size="sm" fw={650}>{GROUP_LABEL[g.key]}</Text>
                          <Text size="sm" c="dimmed">{g.rows.length}</Text>
                          {g.key === 'fine' && foldFine && (
                            <Anchor component="button" size="sm" onClick={() => setShowFine(!showFine)}>
                              {showFine ? 'hide' : 'show'}
                            </Anchor>
                          )}
                        </Group>
                      </Table.Td>
                    </Table.Tr>
                  )}
                  {!folded && g.rows.map((a) => (
                    <AssetRow key={a.id} a={a} admin={admin} open={open === a.id} onToggle={() => toggle(a.id)}
                      mine={mineBy.get(a.hostname)} waiting={waitingBy.get(a.id)} usage={usageBy.get(a.id)}
                      updates={updates} cols={cols} />
                  ))}
                </Fragment>
              )
            })}
          </Table.Tbody>
        </Table>
      </Box>
    </Stack>
  )
}

function AssetRow({ a, admin, open, onToggle, mine, waiting, usage, updates, cols }: {
  a: Asset; admin: boolean; open: boolean; onToggle: () => void
  mine?: MyServer; waiting?: UpdatesWaitingRow; usage?: AssetUsageRow
  updates?: { latest: string }; cols: number
}) {
  const reach = reachOf(a)
  const name = assetShortName(a)
  const why = offlineReason(a, relTime)
  const kind = kindOf(a)
  const rdp = a.protocol === 'rdp'
  const canOpen = reach !== 'blocked' && a.principals.length > 0
  const openLabel = kind === 'android' ? 'Open device' : rdp ? 'Open desktop' : 'Open terminal'
  const OpenIcon = kind === 'android' ? IconDeviceMobile : rdp ? IconDeviceDesktop : IconTerminal2
  const direct = !rdp && mine?.personalAccount ? mine.directSsh : 'off'
  const access = kind === 'android' ? 'Android device' : rdp ? 'Remote Desktop'
    : direct === 'allow' ? 'SSH ready' : direct === 'approve' ? 'SSH needs approval' : 'Terminal (SSH)'

  return (
    <>
      <Table.Tr
        data-answer={answerOf(a)}
        data-testid="asset-row"
        className="argus-assettable-row"
        aria-expanded={open}
        onClick={(e) => {
          // A button or link in the row does its own thing.
          if ((e.target as HTMLElement).closest('a,button,input,[role="menu"]')) return
          onToggle()
        }}
        style={{ cursor: 'pointer' }}
      >
        <Table.Td>
          <UnstyledButton aria-label={open ? `Close ${name}` : `Show more about ${name}`} onClick={onToggle}>
            {open ? <IconChevronDown size={16} /> : <IconChevronRight size={16} />}
          </UnstyledButton>
        </Table.Td>
        <Table.Td style={{ maxWidth: 260 }}>
          <Text size="sm" fw={600} truncate title={assetHostnameAside(a)}>{name}</Text>
          <Text size={FS.micro} c="dimmed" truncate>
            {a.os}{a.tags.length > 0 ? ` · ${a.tags.join(', ')}` : ''} · {access}
          </Text>
        </Table.Td>
        <Table.Td><AssetAddresses asset={a} size="sm" /></Table.Td>
        <Table.Td>
          <ReachBadge reach={reach} />
          {why && <Text size={FS.micro} c="dimmed" mt={2}>{why}</Text>}
        </Table.Td>
        <Table.Td>{usage ? <MiniBars usage={usage} /> : <Text size={FS.micro} c="dimmed">No report</Text>}</Table.Td>
        {updates && (
          <Table.Td style={{ maxWidth: 280 }}>
            <UpdatesLine waiting={waiting} latest={updates.latest} checked={updatesChecked(a)} compact />
          </Table.Td>
        )}
        <Table.Td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
          <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
            {canOpen && (
              <ButtonLink to="/connect" search={{ host: a.hostname, as: a.principals[0], open: true }} size="compact-sm"
                variant={reach === 'online' || reach === 'slow' ? 'filled' : 'default'}
                leftSection={<OpenIcon size={14} />} aria-label={`${openLabel} on ${name}`}>
                Open
              </ButtonLink>
            )}
          </Group>
        </Table.Td>
      </Table.Tr>
      {open && (
        <Table.Tr className="argus-assettable-detail" data-testid="asset-row-detail">
          <Table.Td />
          <Table.Td colSpan={cols - 1}>
            <Box style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 18 }} py={4}>
              <Detail title="Log in as">
                {a.principals.length === 0 ? (
                  <Text size={FS.meta} c="dimmed">{admin ? 'Nobody can log in yet.' : 'You have no account on it yet.'}</Text>
                ) : (
                  <Group gap={SP.tight}>
                    {a.principals.map((p) => (
                      <ButtonLink key={p} to="/connect" search={{ host: a.hostname, as: p, open: true }} size="compact-xs"
                        variant="default" ff="monospace" disabled={!canOpen} aria-label={`Connect to ${name} as ${p}`}>
                        {p}
                      </ButtonLink>
                    ))}
                  </Group>
                )}
                {assetHostnameAside(a) && <Text size={FS.micro} c="dimmed" ff="monospace" mt={6}>{a.hostname}</Text>}
              </Detail>
              <Detail title="Addresses">
                {addressesOf(a, true).map((x) => (
                  <Text key={x.address} size={FS.meta}>
                    <Text span inherit ff="monospace">{x.address}</Text>
                    <Text span inherit c="dimmed"> {x.entered ? 'entered in Argus' : x.public ? 'public' : 'private'}</Text>
                  </Text>
                ))}
              </Detail>
              <Detail title="Resources">
                {usage ? (
                  <>
                    {usage.cpu !== null && <Text size={FS.meta}>Processor {usage.cpu}%</Text>}
                    {usage.memory !== null && <Text size={FS.meta}>Memory {usage.memory}%</Text>}
                    {usage.disk && (
                      <Text size={FS.meta} c={usage.disk.percent >= 90 ? INK.denied : undefined} fw={usage.disk.percent >= 90 ? 600 : undefined}>
                        Disk <Text span inherit ff="monospace">{usage.disk.mount}</Text> {usage.disk.percent}% full
                      </Text>
                    )}
                    <Text size={FS.micro} c="dimmed">Reported {relTime(usage.usageAt)}</Text>
                  </>
                ) : <Text size={FS.meta} c="dimmed">No recent report from its agent.</Text>}
              </Detail>
              <Detail title={updates ? 'Updates' : 'Checked'}>
                {updates ? (
                  updateChips(waiting, updates.latest).length === 0
                    ? (updatesChecked(a) ? <Text size={FS.meta} c="teal.7">Up to date</Text> : <Text size={FS.meta} c="dimmed">Not checked: no agent has reported</Text>)
                    : updateChips(waiting, updates.latest).map((c) => <Text key={c.key} size={FS.meta}>{c.label}</Text>)
                ) : <Text size={FS.meta}>{relTime(a.lastCheckedAt)}</Text>}
                {admin && (
                  <AnchorLink to="/assets/$assetId" params={{ assetId: a.id }} size={FS.meta} mt={6} display="block">
                    Open the asset →
                  </AnchorLink>
                )}
              </Detail>
            </Box>
          </Table.Td>
        </Table.Tr>
      )}
    </>
  )
}

function Detail({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Box style={{ minWidth: 0 }}>
      <Text size={FS.micro} c="dimmed" tt="uppercase" fw={650} mb={4} style={{ letterSpacing: '.05em' }}>{title}</Text>
      {children}
    </Box>
  )
}

/** Processor, memory and the fullest disk as three small bars. */
function MiniBars({ usage }: { usage: AssetUsageRow }) {
  const cells = [
    { k: 'CPU', v: usage.cpu },
    { k: 'Mem', v: usage.memory },
    { k: 'Disk', v: usage.disk?.percent ?? null },
  ].filter((c) => c.v !== null) as { k: string; v: number }[]
  if (cells.length === 0) return <Text size={FS.micro} c="dimmed">No report</Text>
  return (
    <Box style={{ display: 'grid', gridTemplateColumns: '30px 1fr 34px', gap: '2px 6px', alignItems: 'center' }} data-testid="mini-bars">
      {cells.map((c) => (
        <Fragment key={c.k}>
          <Text size={FS.micro} c="dimmed">{c.k}</Text>
          <Progress value={c.v} size={5} radius="xl" color={usageTone(c.v)} aria-label={`${c.k} ${c.v}%`} />
          <Text size={FS.micro} fw={600} ta="right" c={usageTone(c.v) === 'azure' ? undefined : `${usageTone(c.v)}.7`}>{c.v}%</Text>
        </Fragment>
      ))}
    </Box>
  )
}

