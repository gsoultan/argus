import '../ssh/ssh.css'
import './connect.css'
import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Badge, Box, Button, Center, Checkbox, Group, Loader, PasswordInput, Select, Stack, Text,
  ThemeIcon, UnstyledButton,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import {
  IconAppWindow, IconDeviceDesktop, IconDeviceMobile, IconEye, IconFolders, IconHourglassHigh,
  IconLayoutColumns, IconLock, IconPlugConnectedX, IconPointFilled, IconTerminal2,
} from '@tabler/icons-react'
import { OwnTerminalCard, PendingDirectLogin, useOwnTerminal } from '~/components/ssh/OwnTerminal'
import { SignedIn } from '~/components/connect/SignedIn'
import { ButtonLink } from '~/components/links'
import { ReachBadge, kindOf, reachOf } from '~/components/myservers'
import { relTime } from '~/components/primitives'
import { assetsQuery } from '~/lib/queries'
import { isElevatedAccount, principalLabel } from '~/lib/roles'
import {
  lastConnect, rememberConnect, useSessions, waysFor, type Way,
} from '~/lib/sessions'
import type { Asset } from '~/types/domain'
import { assetShortName } from '~/lib/assetType'
import { FS } from '~/theme'

/** Each way in, said the way a person would pick it. */
const WAYS: Record<Way | 'own', { label: string; hint: string; icon: typeof IconTerminal2; color: string; key?: string }> = {
  terminal: { label: 'Terminal', hint: 'A shell on this page', icon: IconTerminal2, color: 'teal', key: 't' },
  files: { label: 'Files', hint: 'Browse, upload, download', icon: IconFolders, color: 'violet', key: 'f' },
  split: { label: 'Both', hint: 'Terminal and files side by side', icon: IconLayoutColumns, color: 'azure', key: 'b' },
  desktop: { label: 'Remote desktop', hint: 'A Windows session as the account', icon: IconDeviceDesktop, color: 'azure', key: 'd' },
  screen: { label: 'Their screen', hint: 'See and use the screen as it is', icon: IconEye, color: 'amber', key: 's' },
  device: { label: 'Device screen', hint: 'Through the Argus app on it', icon: IconDeviceMobile, color: 'azure' },
  own: { label: 'My own app', hint: 'ssh from your laptop, VS Code, WinSCP', icon: IconAppWindow, color: 'slate', key: 'o' },
}

/** The main button says what will happen, not just "Connect". */
function actionLabel(way: Way, principal: string): string {
  switch (way) {
    case 'files': return 'Open files'
    case 'split': return 'Open terminal and files'
    case 'desktop': return 'Open desktop'
    case 'screen': return 'Open screen'
    case 'device': return principal === 'view' ? 'Watch' : 'Open'
    default: return 'Open terminal'
  }
}

/**
 * How to connect to one asset: which way, as whom, and what will happen.
 *
 * The same panel is the Connect drawer opened from anywhere in the console and
 * the /connect page a link lands on. It replaces the old form, which showed
 * one fixed way in per asset and named the outcome only in its button.
 */
export function ConnectPanel({ hostname, initialPrincipal, initialWay, autoOpen = false, onDone, inDrawer = false }: {
  /** Preselected asset; empty to pick one. */
  hostname?: string
  initialPrincipal?: string
  initialWay?: Way
  /**
   * Open straight away, as a link from a server card asks: only for a way
   * that needs nothing more typed (not a Remote Desktop, which needs the
   * password), as an account the asset lists for this person.
   */
  autoOpen?: boolean
  /** Called once a session opened (the drawer closes). */
  onDone?: () => void
  inDrawer?: boolean
}) {
  const { data: assets, refetch, isFetching } = useQuery(assetsQuery({}))
  const sessions = useSessions()
  const phone = useMediaQuery('(max-width: 36em)') ?? false
  const [target, setTarget] = useState<string | null>(hostname || null)
  useEffect(() => setTarget(hostname || null), [hostname])
  const asset = assets?.find((a) => a.hostname === target)

  if (assets && target && !asset) return <NoAccess hostname={target} />
  return (
    <Stack gap="lg">
      {!hostname && (
        <Select
          size="md"
          label="Connect to"
          placeholder={assets ? 'Type a server or device name' : 'Loading…'}
          disabled={!assets}
          searchable
          nothingFoundMessage="You have nothing by that name"
          value={target}
          onChange={setTarget}
          data={assets?.map((a) => ({ value: a.hostname, label: a.alias ? `${a.alias} (${a.hostname})` : a.hostname })) ?? []}
        />
      )}
      {target && !asset && <Text size={FS.meta} c="dimmed">Loading your accounts…</Text>}
      {asset && (asset.tunnelConnected !== true
        ? <Offline asset={asset} refetch={() => void refetch()} loading={isFetching} />
        : <Choose key={asset.id} asset={asset} initialPrincipal={initialPrincipal} initialWay={initialWay}
          autoOpen={autoOpen} phone={phone}
          inDrawer={inDrawer} open={sessions.open} onDone={onDone} />)}
    </Stack>
  )
}

function Choose({ asset, initialPrincipal, initialWay, autoOpen, phone, inDrawer, open, onDone }: {
  asset: Asset
  initialPrincipal?: string
  initialWay?: Way
  autoOpen: boolean
  phone: boolean
  inDrawer: boolean
  open: ReturnType<typeof useSessions>['open']
  onDone?: () => void
}) {
  const ways = useMemo(() => {
    const w = waysFor(asset).filter((x) => !(phone && x === 'split'))
    return asset.protocol === 'rdp' ? w : [...w, 'own' as const]
  }, [asset, phone])
  const last = lastConnect(asset.hostname)
  const principals = asset.principals
  const has = (w?: Way): w is Way => !!w && (ways as string[]).includes(w)
  const [way, setWay] = useState<Way | 'own'>(
    has(initialWay) ? initialWay : has(last?.way) ? last.way : ways[0]!,
  )
  const pick = (p?: string) => (p && principals.includes(p) ? p : undefined)
  const [principal, setPrincipal] = useState<string>(
    pick(initialPrincipal) ?? pick(last?.principal) ?? principals[0] ?? '',
  )
  const [password, setPassword] = useState('')
  const [oneClick, setOneClick] = useState(last?.oneClick ?? false)
  const [busy, setBusy] = useState(false)
  const [denied, setDenied] = useState<string>()

  const own = useOwnTerminal(asset.protocol !== 'rdp' ? asset.hostname : undefined)
  const device = asset.kind === 'android'
  const needsPassword = way === 'desktop'
  const elevated = isElevatedAccount(principal, asset)
  const name = assetShortName(asset)
  const noAccounts = principals.length === 0
  const askMode = asset.connectAsk ?? (asset.kind === 'workstation' ? 'ask' : 'never')

  // Letters pick a way, as the tiles show, unless typing in a field.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null
      if (e.metaKey || e.ctrlKey || e.altKey || t?.closest('input, textarea, [contenteditable]')) return
      const hit = ways.find((w) => WAYS[w].key === e.key.toLowerCase())
      if (hit) setWay(hit)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [ways])

  const connect = async () => {
    if (way === 'own' || noAccounts) return
    setBusy(true)
    setDenied(undefined)
    const res = await open({ target: asset.hostname, principal, way, password: needsPassword ? password : undefined })
    setPassword('')
    setBusy(false)
    if (res.error) {
      setDenied(res.error)
      return
    }
    rememberConnect(asset.hostname, { way, principal, oneClick })
    onDone?.()
  }

  // Once, for what the link named and nothing else.
  const autoDone = useRef(false)
  useEffect(() => {
    if (!autoOpen || autoDone.current) return
    autoDone.current = true
    if (way === 'own' || way === 'desktop' || noAccounts) return
    if (initialPrincipal && !principals.includes(initialPrincipal)) return
    void connect()
    // connect reads the same state this effect was gated on.
  }, [autoOpen])

  if (busy) {
    return (
      <StateCard icon={<Loader size={26} />} tone="azure" title={`Connecting to ${name}`}
        body={device
          ? `Checking you may ${principal === 'view' ? 'watch' : 'control'} it, then opening its screen through the Argus app on it. Everything you do is recorded.`
          : `Checking you can log in as ${principal}, then opening it through the server’s agent. Everything you do is recorded.`} />
    )
  }

  if (denied) {
    return (
      <StateCard icon={<IconHourglassHigh size={26} />} tone="amber"
        title={elevated
          ? device ? `Controlling ${name} needs approval` : `${principal} needs approval`
          : device ? `You can't ${principal === 'view' ? 'watch' : 'control'} ${name}` : `You can't log in as ${principal} here`}
        body={elevated
          ? device
            ? `Control of ${name} is approved by someone else first, for a set time. Ask, and you can connect as soon as they say yes.`
            : `${principal} has full control of ${name}, so someone else approves it first, for a set time. Ask, and you can connect as soon as they say yes.`
          : denied}>
        <ButtonLink size="md" to="/approvals" search={{ ask: asset.hostname, as: principal }}>
          {elevated ? `Ask for ${device ? 'control' : principal}` : 'Ask for access'}
        </ButtonLink>
        {principals.length > 1 && (
          <Button size="md" variant="default" onClick={() => setDenied(undefined)}>Pick another account</Button>
        )}
      </StateCard>
    )
  }

  return (
    <Stack gap="lg">
      <Group gap="sm">
        <ReachBadge reach={reachOf(asset)} />
        <Text size={FS.body} c="dimmed">
          {[asset.os || { windows: 'Windows', linux: 'Linux', android: 'Android' }[kindOf(asset)], asset.address].filter(Boolean).join(' · ')}
        </Text>
      </Group>


      {asset.protocol !== 'rdp' && <PendingDirectLogin hostname={asset.hostname} login={own.account?.loginName} />}

      <Box>
        <Group gap={8} mb={8}>
          <Text size="sm" fw={600}>How</Text>
          {ways.length > 1 && !phone && <Text size={FS.meta} c="dimmed">pick one, or press its letter</Text>}
        </Group>
        <div className="argus-ways-grid" role="radiogroup" aria-label="How to connect">
          {ways.map((w) => {
            const m = WAYS[w]
            const on = w === way
            return (
              <UnstyledButton key={w} role="radio" aria-checked={on} className="argus-way-tile"
                data-on={on || undefined} onClick={() => setWay(w)}>
                <Group gap={8} wrap="nowrap">
                  <m.icon size={18} color={`var(--mantine-color-${m.color}-6)`} />
                  <Text size="sm" fw={600}>{m.label}</Text>
                  {m.key && !phone && <Text className="argus-way-key" size={FS.micro} c="dimmed" ff="monospace">{m.key.toUpperCase()}</Text>}
                </Group>
                <Text size={FS.meta} c="dimmed" mt={4} lh={1.35}>
                  {asset.protocol === 'rdp' && w === 'terminal' ? 'PowerShell or Command Prompt' : m.hint}
                </Text>
              </UnstyledButton>
            )
          })}
        </div>
      </Box>

      {way === 'own' ? (
        <OwnTerminalCard asset={asset} />
      ) : (
        <>
          <Box>
            <Text size="sm" fw={600} mb={8}>{device ? 'Open it to' : 'As'}</Text>
            {noAccounts ? (
              <Text size="sm" c="dimmed">
                {device
                  ? 'You may neither watch nor control this device yet. Ask an admin.'
                  : 'You have no account on this server yet. Ask an admin to add one for you.'}
              </Text>
            ) : (
              <Group gap={6} role="radiogroup" aria-label={device ? 'Open it to' : 'Log in as'}>
                {principals.map((p) => (
                  <UnstyledButton key={p} role="radio" aria-checked={p === principal}
                    className="argus-acct" data-on={p === principal || undefined}
                    data-mono={!device || undefined} onClick={() => setPrincipal(p)}>
                    {principalLabel(p, asset)}
                    {isElevatedAccount(p, asset) && <Badge size="xs" color="amber" variant="light" ml={6}>approval</Badge>}
                  </UnstyledButton>
                ))}
              </Group>
            )}
            {principals.some((p) => isElevatedAccount(p, asset)) && (
              <Text size={FS.meta} c="dimmed" mt={8}>
                {device ? 'Control needs approval first, for a set time.' : 'An account marked approval needs someone to approve it first, for a set time.'}
              </Text>
            )}
          </Box>

          {!noAccounts && (
            <Box className="argus-what">
              <What icon={<IconPointFilled size={16} color="var(--mantine-color-rose-6)" />}>
                {way === 'files'
                  ? 'Recorded: every upload, download, change and preview.'
                  : way === 'terminal' || way === 'split'
                    ? 'Recorded: everything you type and see.'
                    : 'Recorded: everything on screen.'}
              </What>
              {elevated && (
                <What icon={<IconLock size={16} />}>
                  {device ? 'Control' : principal} has full control, so someone approves it first, for a set time.
                </What>
              )}
              {device && (
                <What icon={<IconDeviceMobile size={16} />}>
                  {asset.controlMode === 'unattended'
                    ? 'Opens without anyone on the device. The app shows that a session is live while you are on it.'
                    : 'The person holding it accepts first, and sees a banner naming you while you are on it.'}
                </What>
              )}
              {!device && askMode !== 'never' && (way === 'desktop' || way === 'screen') && (
                <What icon={<IconDeviceDesktop size={16} />}>
                  {askMode === 'ask'
                    ? 'If someone is using it, they are asked on their screen first, naming you. No answer in time is a no.'
                    : 'It opens straight away, and anyone signed in at its screen gets a notice naming you.'}
                </What>
              )}
              {way === 'screen' && (
                <What icon={<IconEye size={16} />}>
                  Screen sharing: you see the screen as it is, and anyone in front of it sees what you do.
                </What>
              )}
            </Box>
          )}

          {!device && !noAccounts && <SignedIn assetId={asset.id} principal={principal} />}

          {needsPassword && !noAccounts && (
            <PasswordInput
              size="md"
              label={`Windows password for ${principal}`}
              description="Used once to sign in on the server. Argus does not keep it."
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void connect()
              }}
            />
          )}

          {!noAccounts && (
            <Checkbox
              checked={oneClick}
              onChange={(e) => setOneClick(e.currentTarget.checked)}
              label={way === 'desktop'
                ? 'Next time, start here with this way and account'
                : 'Next time, open this way with one click'}
            />
          )}

          <Group gap="sm" justify={inDrawer ? 'flex-end' : 'flex-start'}>
            <Button size="md" fullWidth={phone} disabled={noAccounts} onClick={() => void connect()}
              leftSection={(() => { const I = WAYS[way].icon; return <I size={18} /> })()}>
              {actionLabel(way, principal)}
            </Button>
          </Group>
        </>
      )}
    </Stack>
  )
}

function What({ icon, children }: { icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <Group gap={10} wrap="nowrap" align="flex-start">
      <Box mt={2} c="dimmed" style={{ flexShrink: 0, display: 'flex' }}>{icon}</Box>
      <Text size={FS.body} lh={1.45}>{children}</Text>
    </Group>
  )
}

function NoAccess({ hostname }: { hostname: string }) {
  const name = hostname.split('.')[0] ?? hostname
  return (
    <StateCard icon={<IconLock size={26} />} tone="slate" title={`You can't connect to ${name} yet`}
      body="Nobody has given you this server or device. Ask for access, and an approver decides. You get a note here when they do.">
      <ButtonLink size="md" to="/approvals" search={{ ask: hostname }}>Ask for access</ButtonLink>
      <ButtonLink size="md" variant="default" to="/assets">Back to assets</ButtonLink>
    </StateCard>
  )
}

function Offline({ asset, refetch, loading }: { asset: Asset; refetch: () => void; loading: boolean }) {
  const name = assetShortName(asset)
  const device = asset.kind === 'android'
  return (
    <StateCard icon={<IconPlugConnectedX size={26} />} tone="amber" title={`${name} is offline`}
      body={`The Argus ${device ? 'app' : 'agent'} on it has not checked in${asset.agentLastSeenAt ? ` since ${relTime(asset.agentLastSeenAt)}` : ''}. It may be ${device ? 'off or out of coverage' : 'restarting'}. Try again in a minute, or tell an admin if it stays offline.`}>
      <Button size="md" loading={loading} onClick={refetch}>Try again</Button>
      <ButtonLink size="md" variant="default" to="/assets">Back to assets</ButtonLink>
    </StateCard>
  )
}

/** One state of connecting, said in a sentence, with what to do next. */
export function StateCard({ icon, tone, title, body, children }: {
  icon: React.ReactNode
  tone: 'azure' | 'amber' | 'slate' | 'teal' | 'rose'
  title: string
  body: string
  children?: React.ReactNode
}) {
  return (
    <Box className="argus-panel" p="xl" maw={480} w="100%" mx="auto" ta="center">
      <Center mb="md">
        <ThemeIcon size={56} radius="xl" color={tone} variant="light">{icon}</ThemeIcon>
      </Center>
      <Text component="h1" fw={600} size="xl" style={{ margin: 0 }}>{title}</Text>
      <Text size="sm" c="dimmed" mt="xs" lh={1.6}>{body}</Text>
      {children && <Group justify="center" mt="lg" gap="sm">{children}</Group>}
    </Box>
  )
}
