import { Alert, Anchor, Box, Button, Card, Group, Progress, SimpleGrid, Skeleton, Stack, Text } from '@mantine/core'
import { queryOptions, useQuery } from '@tanstack/react-query'
import { Mono, absTime, now as clockNow, relTime } from '~/components/primitives'
import type { ServerTab } from '~/components/server/tabs'
import { live } from '~/lib/live'
import { bytes, trim, warningText, warningTone } from '~/lib/resourcesLow'
import { FS, INK } from '~/theme'
import type {
  Asset, ResourceWarning, ServerDisk, ServerInterface, ServerNetRate, ServerSystem, ServerSystemFacts, ServerUsage,
} from '~/types/domain'

export { bytes }

/**
 * Here rather than in lib/queries and lib/api, so the card's fixture and
 * fetch load with the server page and not on every first visit. Usage
 * arrives about once a minute, so the card refreshes on the same beat.
 */
export const systemQuery = (asset: Asset) =>
  queryOptions({
    queryKey: ['asset', asset.id, 'system'] as const,
    queryFn: () => live.orFallback(() => live.system(asset.id), async () => fixtureSystem(asset)),
    refetchInterval: 60_000,
    retry: false,
  })

/**
 * What the sample data shows: a busy KVM guest with one disk filling up, a
 * public and a private interface, and a container bridge.
 */
function fixtureSystem(a: Asset): ServerSystem {
  const none: ServerSystem = { reported: false, os: a.os ?? '', system: null, usage: null, usageAt: null }
  if (a.kind === 'android') {
    if (a.agentState === 'absent') return none
    return {
      reported: true,
      os: a.os || 'Android 14',
      system: {
        kernel: '', arch: '', cpuModel: '', cpus: 0, memoryBytes: 0, machine: '', hypervisor: '',
        bootedAt: null, timeZone: '', utcOffsetSeconds: 0,
        engine: a.controlMode === 'unattended' ? 'unattended' : 'attended',
        wirelessDebugging: a.controlMode === 'unattended',
        canInstallApps: true,
      },
      usage: null, usageAt: a.agentLastSeenAt ?? null, network: [],
    }
  }
  if (a.agentState === 'absent' || a.protocol === 'rdp') return none
  const GB = 1024 ** 3
  const at = a.agentLastSeenAt ?? new Date().toISOString()
  return {
    reported: true,
    os: a.os || 'Ubuntu 24.04.1 LTS',
    system: {
      kernel: '6.8.0-45-generic', arch: 'x86_64', cpuModel: 'Intel Xeon Gold 6248R', cpus: 8,
      memoryBytes: 32 * GB, machine: 'virtual', hypervisor: 'KVM',
      bootedAt: new Date(Date.parse(at) - 23 * 86_400_000).toISOString(), timeZone: 'Asia/Jakarta', utcOffsetSeconds: 25_200,
    },
    usage: {
      load: [1.42, 1.3, 1.18], cpuBusy: 18, memTotal: 32 * GB, memAvailable: 12.5 * GB, swapTotal: 4 * GB, swapFree: 3.8 * GB,
      disks: [
        { mount: '/', fs: 'ext4', total: 100 * GB, used: 47 * GB, avail: 48 * GB },
        { mount: '/var/lib/postgresql', fs: 'xfs', total: 500 * GB, used: 455 * GB, avail: 45 * GB },
      ],
      net: [
        { name: 'eth0', rxBps: 4_775_000, txBps: 14_000_000, speedMbps: 1000 },
        { name: 'eth1', rxBps: 512_500, txBps: 112_500, speedMbps: 10_000 },
        { name: 'docker0', rxBps: 0, txBps: 0, speedMbps: 0 },
      ],
    },
    usageAt: at,
    warnings: [{ resource: 'disk', level: 'low', mount: '/var/lib/postgresql', percent: 91, left: 45 * GB, total: 500 * GB }],
    network: [
      {
        name: 'eth0', mac: '52:54:00:3a:91:c4', addresses: [
          { address: '203.0.113.24', prefix: 24, family: 'ipv4', public: true },
          { address: '2001:db8:4f8::24', prefix: 64, family: 'ipv6', public: true },
        ],
      },
      { name: 'eth1', mac: '52:54:00:3a:91:c5', addresses: [{ address: '10.20.0.14', prefix: 16, family: 'ipv4', public: false }] },
      { name: 'docker0', mac: '02:42:7d:1e:0a:33', addresses: [{ address: '172.17.0.1', prefix: 16, family: 'ipv4', public: false }] },
    ],
  }
}

/** Usage older than this is shown faded, with the time it is from. */
const QUIET_AFTER = 3 * 60 * 1000

/**
 * What kind of machine a server is and how busy it is, as its agent reports.
 *
 * Every figure is the agent's, so the card always says how old they are, and
 * fades them once the agent goes quiet rather than present a stale sample as
 * the present. What is running low is decided by the control plane
 * (resources.go) and arrives as `warnings`: the card names each at the top
 * and colours its meter, amber when low and rose when critical.
 *
 * Fleet roles only, like what runs on a server; anyone else sees no card.
 */
export function SystemCard({ asset, onTab, canFreeSpace = false }: {
  asset: Asset
  onTab: (t: ServerTab) => void
  /** Whether a filling disk offers the Logs tab. */
  canFreeSpace?: boolean
}) {
  const { data, isError } = useQuery(systemQuery(asset))
  if (isError) return null
  if (!data) {
    return (
      <Card padding="md">
        <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>System</Text>
        <Skeleton h={120} mt="sm" />
      </Card>
    )
  }
  return <SystemCardBody asset={asset} data={data} onTab={onTab} now={clockNow()} canFreeSpace={canFreeSpace} />
}

export function SystemCardBody({ asset, data, onTab, now, canFreeSpace = false }: {
  asset: Asset
  data: ServerSystem
  onTab: (t: ServerTab) => void
  canFreeSpace?: boolean
  /** The moment "how old" is measured against; a prop so tests can fix it. */
  now: number
}) {
  const os = data.os || asset.os
  const at = data.usageAt ?? null
  const quiet = data.reported && (asset.agentState !== 'healthy' || !at || now - Date.parse(at) > QUIET_AFTER)

  if (!data.reported) {
    const noAgent = asset.agentState === 'absent'
    return (
      <Card padding={0}>
        <Header />
        {os && <Facts rows={[['Operating system', os]]} />}
        <Box px="md" py="md" ta="center" style={os ? { borderTop: '1px solid var(--color-line)' } : undefined}>
          {noAgent ? (
            <>
              <Text size="sm" fw={600}>No agent on this server</Text>
              <Text size={FS.meta} c="dimmed" mt={4} maw={440} mx="auto" lh={1.5}>
                The processor, memory and disks are reported by the Argus agent. Install it on this server to see them here.
              </Text>
            </>
          ) : (
            <>
              <Text size={FS.meta} c="dimmed" maw={440} mx="auto" lh={1.5}>
                This server&apos;s agent does not report its processor, memory or disks. Update it to see them here.
              </Text>
              <Button size="xs" variant="default" mt="sm" onClick={() => onTab('agent')}>Update agent</Button>
            </>
          )}
        </Box>
        <Network ifaces={data.network ?? []} net={[]} />
      </Card>
    )
  }

  return (
    <Card padding={0} data-quiet={quiet || undefined}>
      <Header
        when={at ? (
          <Text size={FS.meta} c={quiet ? INK.pending : 'dimmed'} title={absTime(at)}>
            {quiet ? relTime(at) : `Reported by the agent ${relTime(at)}`}
          </Text>
        ) : undefined}
      />
      {quiet && at && (
        <Box px="md" pt="md">
          <Alert color="amber" variant="light" p="xs">
            <Text size={FS.meta}>The agent has not reported since {absTime(at)}. These figures are from then.</Text>
          </Alert>
        </Box>
      )}
      {data.usage && <LowStrip warnings={data.warnings ?? []} faded={quiet} />}
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing={0} style={{ opacity: quiet ? 0.6 : 1 }}>
        <Facts rows={factRows(os, data.system, now)} />
        <Box className="system-usage">
          {data.usage
            ? <UsageMeters usage={data.usage} cpus={data.system?.cpus ?? 0} warnings={data.warnings}
                onFree={canFreeSpace ? () => onTab('logs') : undefined} />
            : <Text size={FS.meta} c="dimmed" p="md">Waiting for the agent&apos;s first report of how busy this server is.</Text>}
        </Box>
      </SimpleGrid>
      <Box style={{ opacity: quiet ? 0.6 : 1 }}>
        <Network ifaces={data.network ?? []} net={data.usage?.net ?? []} />
      </Box>
    </Card>
  )
}

const RESOURCE_NAME: Record<ResourceWarning['resource'], string> = { memory: 'memory', disk: 'disk', swap: 'swap', cpu: 'processor' }

/**
 * What is running low, said first: a card full of meters makes the reader
 * find the coloured one, and a phone shows only the top of it.
 */
function LowStrip({ warnings, faded }: { warnings: ResourceWarning[]; faded: boolean }) {
  if (warnings.length === 0) return null
  const tone = warnings.some((w) => w.level === 'critical') ? 'rose' : 'amber'
  const kinds = [...new Set(warnings.map((w) => w.resource))]
  const title = kinds.length === 1
    ? `Running low on ${RESOURCE_NAME[kinds[0]!]}`
    : `Running low on ${warnings.length} resources`
  return (
    <Box px="md" pt="md" style={{ opacity: faded ? 0.6 : 1 }}>
      <Alert color={tone} variant="light" p="xs" title={title} data-testid="system-low" data-tone={tone}>
        {warnings.length === 1
          ? <Text size={FS.meta}>{warningText(warnings[0]!)}.</Text>
          : (
            <Stack gap={2}>
              {warnings.map((w) => <Text key={w.resource + (w.mount ?? '')} size={FS.meta}>{warningText(w)}</Text>)}
            </Stack>
          )}
      </Alert>
    </Box>
  )
}

function Header({ when }: { when?: React.ReactNode }) {
  return (
    <Group justify="space-between" wrap="nowrap" px="md" py="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>System</Text>
      {when}
    </Group>
  )
}

type Row = [string, React.ReactNode]

function factRows(os: string, s: ServerSystemFacts | null, now: number): Row[] {
  const rows: Row[] = []
  if (os) rows.push(['Operating system', os])
  if (!s) return rows
  if (s.kernel) rows.push(['Kernel', <Text key="k" size={FS.meta} ff="monospace">{s.kernel}</Text>])
  if (s.arch) rows.push(['Architecture', <Text key="a" size={FS.meta} ff="monospace">{s.arch}</Text>])
  if (s.cpuModel || s.cpus) {
    rows.push(['Processor', <>
      {s.cpuModel && <Text size={FS.meta}>{s.cpuModel}</Text>}
      {s.cpus > 0 && <Text size={s.cpuModel ? FS.micro : FS.meta} c={s.cpuModel ? 'dimmed' : undefined}>{plural(s.cpus, 'CPU')}</Text>}
    </>])
  }
  if (s.memoryBytes) rows.push(['Memory', bytes(s.memoryBytes)])
  const machine = machineLabel(s)
  if (machine) rows.push(['Machine', machine])
  if (s.bootedAt) {
    rows.push(['Running since', <>
      <Text size={FS.meta} title={absTime(s.bootedAt)}>{shortDate(s.bootedAt)}</Text>
      <Text size={FS.micro} c="dimmed">{uptime(s.bootedAt, now)}</Text>
    </>])
  }
  if (s.timeZone) rows.push(['Time zone', `${s.timeZone} (${utcOffset(s.utcOffsetSeconds)})`])
  return rows
}

function Facts({ rows }: { rows: Row[] }) {
  return (
    <Stack gap="sm" p="md">
      {rows.map(([label, value]) => (
        <Group key={label} gap="sm" wrap="nowrap" align="flex-start">
          <Text size={FS.meta} c="dimmed" w={120} style={{ flexShrink: 0 }}>{label}</Text>
          <Box style={{ minWidth: 0, overflowWrap: 'anywhere' }}>
            {typeof value === 'string' ? <Text size={FS.meta}>{value}</Text> : value}
          </Box>
        </Group>
      ))}
    </Stack>
  )
}

/**
 * The addresses each interface holds, in the order the agent lists them. Says out loud when none is public, because a machine
 * behind NAT has a public address no interface holds, and the card must not
 * read as "this server has no public address".
 */
function Network({ ifaces, net }: { ifaces: ServerInterface[]; net: ServerNetRate[] }) {
  if (ifaces.length === 0 && net.length === 0) return null
  const anyPublic = ifaces.some((i) => i.addresses.some((a) => a.public))
  const byName = new Map(net.map((n) => [n.name, n]))
  const named = new Set(ifaces.map((i) => i.name))
  // Traffic on an interface that holds no address (a bridge member, a
  // tunnel) still counts; it is listed after the ones that do.
  const trafficOnly = net.filter((n) => !named.has(n.name))
  return (
    <Stack gap="sm" p="md" style={{ borderTop: '1px solid var(--color-line)' }} data-testid="system-network">
      <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" style={{ letterSpacing: '0.05em' }}>Network</Text>
      {ifaces.map((i, n) => (
        <Group key={i.name || n} gap="sm" wrap="nowrap" align="flex-start" data-testid={`iface-${i.name || 'addresses'}`}>
          <Box w={120} style={{ flexShrink: 0, minWidth: 0, overflowWrap: 'anywhere' }}>
            {i.name ? <Text size={FS.meta} ff="monospace">{i.name}</Text> : <Text size={FS.meta} c="dimmed">Addresses</Text>}
            {i.mac && <Text size={FS.micro} c="dimmed" ff="monospace">{i.mac}</Text>}
          </Box>
          <Stack gap={2} style={{ minWidth: 0 }}>
            {i.addresses.map((a) => (
              <Group key={a.address} gap={6} wrap="nowrap" align="baseline">
                <Text size={FS.meta} ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                  {a.address}
                  {a.prefix >= 0 && <Text span inherit c="dimmed">/{a.prefix}</Text>}
                </Text>
                <Text size={FS.micro} c="dimmed" style={{ flexShrink: 0 }}>{a.public ? 'public' : 'private'}</Text>
              </Group>
            ))}
            {i.name && byName.has(i.name) && <Traffic rate={byName.get(i.name)!} />}
          </Stack>
        </Group>
      ))}
      {trafficOnly.map((n) => (
        <Group key={n.name} gap="sm" wrap="nowrap" align="flex-start" data-testid={`iface-${n.name}`}>
          <Text size={FS.meta} ff="monospace" w={120} style={{ flexShrink: 0, overflowWrap: 'anywhere' }}>{n.name}</Text>
          <Traffic rate={n} />
        </Group>
      ))}
      {net.length > 0 && (
        <Text size={FS.micro} c="dimmed" lh={1.5}>
          Traffic is the average over the agent&apos;s last minute, from each interface&apos;s own counters.
        </Text>
      )}
      {ifaces.length > 0 && !anyPublic && (
        <Text size={FS.micro} c="dimmed" lh={1.5}>
          No interface on this server holds a public address. If it reaches the internet through NAT or a cloud
          provider&apos;s public IP, that address is not on the machine and the agent does not report it.
        </Text>
      )}
    </Stack>
  )
}

/** One interface's traffic: in, out, and the link it is on. */
function Traffic({ rate: n }: { rate: ServerNetRate }) {
  return (
    <Group gap={4} style={{ columnGap: 12 }} wrap="wrap" data-testid={`traffic-${n.name}`}>
      <Text size={FS.meta}><Text span inherit c="dimmed">↓ In</Text> <b>{n.rxBps === null ? '—' : bitRate(n.rxBps)}</b></Text>
      <Text size={FS.meta}><Text span inherit c="dimmed">↑ Out</Text> <b>{n.txBps === null ? '—' : bitRate(n.txBps)}</b></Text>
      {n.speedMbps > 0 && <Text size={FS.meta} c="dimmed">of {linkSpeed(n.speedMbps)} link</Text>}
    </Group>
  )
}

function UsageMeters({ usage: u, cpus, warnings, onFree }: {
  usage: ServerUsage
  cpus: number
  /** Undefined from a control plane older than warnings; disks then keep their own colours. */
  warnings?: ResourceWarning[]
  onFree?: () => void
}) {
  const warn = (r: ResourceWarning['resource']) => warnings?.find((w) => w.resource === r)
  const memUsed = u.memTotal - u.memAvailable
  const load = u.load
  const loadLine = load
    ? `Load ${load[0].toFixed(2)} now · ${load[1].toFixed(2)} over 5 min · ${load[2].toFixed(2)} over 15 min${cpus ? `, on ${plural(cpus, 'CPU')}` : ''}`
    : `This system keeps no load average, so this is how busy the processor was over the last minute${cpus ? `, on ${plural(cpus, 'CPU')}` : ''}.`
  const cpuPct = u.cpuBusy ?? (cpus && load ? Math.min(100, (load[0] / cpus) * 100) : null)
  const cpuTone = warningTone(warn('cpu'))
  const memTone = warningTone(warn('memory'))
  const swapTone = warningTone(warn('swap'))
  return (
    <Stack gap="md" p="md">
      {cpuPct !== null && (
        <Meter
          label="Processor"
          value={u.cpuBusy !== null
            ? cpuTone
              ? <Text span inherit c={`${cpuTone}.6`} fw={600}>{Math.round(u.cpuBusy)}% busy</Text>
              : <><b>{Math.round(u.cpuBusy)}%</b> busy</>
            : <>load <b>{load![0].toFixed(2)}</b></>}
          pct={cpuPct}
          tone={cpuTone}
          sub={loadLine}
          testId="meter-cpu"
        />
      )}
      {u.memTotal > 0 && (
        <Meter
          label="Memory"
          value={memTone
            ? <><Text span inherit c={`${memTone}.6`} fw={600}>{Math.round((memUsed / u.memTotal) * 100)}% used</Text> · {gb(memUsed)} of {bytes(u.memTotal)}</>
            : <><b>{gb(memUsed)}</b> of {bytes(u.memTotal)} used</>}
          pct={(memUsed / u.memTotal) * 100}
          tone={memTone}
          sub={<Text span inherit c={memTone ? `${memTone}.6` : undefined}>{bytes(u.memAvailable)} available</Text>}
          testId="meter-memory"
        />
      )}
      {u.swapTotal > 0 && (
        <Meter
          label="Swap"
          value={<><b>{gb(u.swapTotal - u.swapFree)}</b> of {bytes(u.swapTotal)} used</>}
          pct={((u.swapTotal - u.swapFree) / u.swapTotal) * 100}
          tone={swapTone}
          testId="meter-swap"
        />
      )}
      {u.disks.length > 0 && (
        <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" style={{ letterSpacing: '0.05em' }}>Disks</Text>
      )}
      {u.disks.map((d) => (
        <DiskMeter key={d.mount} disk={d} onFree={onFree} warnings={warnings} />
      ))}
    </Stack>
  )
}

/** How full a disk is, the way df counts it: used over what could be used. */
export function diskFull(d: ServerDisk): number {
  const usable = d.used + d.avail
  return usable > 0 ? Math.ceil((d.used / usable) * 100) : 0
}

export function diskTone(pct: number): 'rose' | 'amber' | undefined {
  return pct >= 95 ? 'rose' : pct >= 90 ? 'amber' : undefined
}

function DiskMeter({ disk: d, onFree, warnings }: {
  disk: ServerDisk
  onFree?: () => void
  /** The control plane's warnings; undefined from one older than them. */
  warnings?: ResourceWarning[]
}) {
  const pct = diskFull(d)
  const tone = warnings
    ? warningTone(warnings.find((w) => w.resource === 'disk' && w.mount === d.mount))
    : diskTone(pct)
  return (
    <Meter
      label={<Mono>{d.mount}</Mono>}
      value={tone
        ? <><Text span inherit c={`${tone}.6`} fw={600}>{pct}% full</Text> · {gb(d.used)} of {bytes(d.total)}</>
        : <><b>{gb(d.used)}</b> of {bytes(d.total)} used</>}
      pct={pct}
      tone={tone}
      sub={<>
        <Text span inherit c={tone ? `${tone}.6` : undefined}>{bytes(d.avail)} free</Text>
        {tone && onFree && (
          <> · <Anchor component="button" type="button" inherit onClick={onFree}>Free space…</Anchor></>
        )}
      </>}
      testId={`disk-${d.mount}`}
    />
  )
}

export function Meter({ label, value, pct, sub, tone, testId }: {
  label: React.ReactNode
  value: React.ReactNode
  pct: number
  sub?: React.ReactNode
  tone?: 'amber' | 'rose'
  testId?: string
}) {
  return (
    <Box data-testid={testId} data-tone={tone}>
      <Group justify="space-between" gap="xs" wrap="nowrap" align="baseline">
        <Text size={FS.meta} fw={500} style={{ minWidth: 0, overflowWrap: 'anywhere' }}>{label}</Text>
        <Text size={FS.meta} c="dimmed" ta="right" style={{ flexShrink: 0 }}>{value}</Text>
      </Group>
      <Progress
        value={Math.max(0, Math.min(100, pct))}
        size="sm"
        mt={6}
        color={tone ? `${tone}.5` : 'slate.4'}
        aria-label={typeof label === 'string' ? label : undefined}
      />
      {sub && <Text size={FS.micro} c="dimmed" mt={4}>{sub}</Text>}
    </Box>
  )
}

/* ── Formatting ─────────────────────────────────────────────────────────── */

const GiB = 1024 ** 3

/** A number of gigabytes without the unit, for "19.5 of 32 GB". */
function gb(n: number): string {
  const v = n / GiB
  if (v >= 1024) return trim((v / 1024).toFixed(1)) + ' TB'
  return v >= 100 ? String(Math.round(v)) : trim(v.toFixed(1))
}

/** Bytes per second as a network speed: bits, the way links are sold. */
export function bitRate(bytesPerSec: number): string {
  const b = bytesPerSec * 8
  const fmt = (v: number) => (v >= 100 ? String(Math.round(v)) : trim(v.toFixed(1)))
  if (b >= 1e9) return `${fmt(b / 1e9)} Gbit/s`
  if (b >= 1e6) return `${fmt(b / 1e6)} Mbit/s`
  if (b >= 1e3) return `${fmt(b / 1e3)} kbit/s`
  return `${Math.round(b)} bit/s`
}

/** A link speed in megabits as the card writes it: "1 Gbit/s", "866 Mbit/s". */
export function linkSpeed(mbps: number): string {
  return mbps >= 1000 ? `${trim((mbps / 1000).toFixed(1))} Gbit/s` : `${mbps} Mbit/s`
}


function plural(n: number, one: string): string {
  return `${n} ${one}${n === 1 ? '' : 's'}`
}

function machineLabel(s: ServerSystemFacts): string {
  switch (s.machine) {
    case 'virtual': return s.hypervisor ? `Virtual machine (${s.hypervisor})` : 'Virtual machine'
    case 'physical': return 'Physical machine'
    case 'container': return 'Container'
    default: return ''
  }
}

export function utcOffset(seconds: number): string {
  if (!seconds) return 'UTC'
  const sign = seconds < 0 ? '−' : '+'
  const m = Math.abs(seconds) / 60
  const h = Math.floor(m / 60)
  const mm = m % 60
  return `UTC${sign}${h}${mm ? `:${String(mm).padStart(2, '0')}` : ''}`
}

function shortDate(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

/** How long since boot, in the largest unit that reads naturally. */
export function uptime(iso: string, now = Date.now()): string {
  const mins = Math.max(0, Math.floor((now - Date.parse(iso)) / 60_000))
  if (mins < 60) return plural(mins, 'minute')
  const hours = Math.floor(mins / 60)
  if (hours < 48) return plural(hours, 'hour')
  return plural(Math.floor(hours / 24), 'day')
}
