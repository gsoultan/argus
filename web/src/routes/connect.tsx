import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert, Badge, Box, Button, Center, Group, Loader, PasswordInput, Radio, SegmentedControl, Select,
  Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAppWindow, IconCircleCheck, IconDeviceDesktop, IconDeviceMobile, IconFolders, IconHourglassHigh, IconLayoutColumns, IconLock,
  IconPlugConnectedX, IconTerminal2, IconX,
} from '@tabler/icons-react'
import { OwnTerminalCard, PendingDirectLogin, useOwnTerminal } from '~/components/ssh/OwnTerminal'
import type { FilesState } from '~/components/FileBrowser'
import { ButtonLink } from '~/components/links'
import { SignedIn } from '~/components/connect/SignedIn'
import { ReachBadge, kindOf, reachOf } from '~/components/myservers'
import { relTime } from '~/components/primitives'
import { isElevatedAccount, principalLabel, seesWholeFleet } from '~/lib/roles'
import { PageBody, PageHeader } from '~/components/page'
import type { TerminalState } from '~/components/LiveTerminal'
import { assetsQuery, meQuery } from '~/lib/queries'
import {
  desktopEndpoint, filesEndpoint, terminalEndpoint, terminalTicket,
} from '~/lib/live'
import { FS } from '~/theme'

/**
 * Loaded on demand.
 *
 * Between them these pull in xterm.js — around 85 kB gzipped. Importing them at
 * module scope meant every visit to this page downloaded a terminal emulator
 * before the user had picked a host, and a visit to a *finished* session's
 * replay downloaded it for a live view that page can never show.
 */
const LiveTerminal = lazy(() =>
  import('~/components/LiveTerminal').then((m) => ({ default: m.LiveTerminal })),
)
const RDPScreen = lazy(() =>
  import('~/components/RDPScreen').then((m) => ({ default: m.RDPScreen })),
)
const FileBrowser = lazy(() =>
  import('~/components/FileBrowser').then((m) => ({ default: m.FileBrowser })),
)

/**
 * No loader, deliberately.
 *
 * A route loader is awaited before the router's first render, so it blanks the
 * whole console — shell included — until it resolves. This route's chunk also
 * carries a terminal emulator, so blocking everything on it to avoid a briefly
 * empty dropdown is the worst version of that trade in the application. The
 * host Select says it is loading instead.
 */
export const Route = createFileRoute('/connect')({
  component: Connect,
  // `?host=` preselects a server, so a "Connect" button anywhere else in the
  // console lands here one step from a terminal instead of at an empty form.
  // `?as=` preselects the account too, for the "log in as" buttons on My
  // servers. It is only a preselection: an account the server does not list
  // for this person is dropped below, and the control plane decides either way.
  // `?open=true` goes one step further and opens the session as soon as the
  // page knows the server, so clicking a server on Servers lands in a terminal
  // rather than on a form that asks again what was just clicked. It is still
  // only a request: the ticket is asked for exactly as the button asks.
  // `?view=files` (or `split`) says which view a session opens on, so "Open
  // files" on a server card lands in the file browser rather than a terminal.
  validateSearch: (
    s: Record<string, unknown>,
  ): { host?: string; as?: string; open?: boolean; view?: SessionView } => ({
    ...(typeof s.host === 'string' && s.host ? { host: s.host } : {}),
    ...(typeof s.as === 'string' && s.as ? { as: s.as } : {}),
    ...(s.open === true || s.open === 'true' || s.open === 1 ? { open: true } : {}),
    ...(s.view === 'files' || s.view === 'split' || s.view === 'terminal' ? { view: s.view } : {}),
  }),
})

/** What a Linux session shows: its terminal, its files, or both side by side. */
export type SessionView = 'terminal' | 'files' | 'split'

/** Stable identity, so the principal-reset effect is not re-armed every render. */
const DEFAULT_PRINCIPALS = ['ops', 'deploy']

function Connect() {
  const { data: assets, refetch: refetchAssets, isFetching: assetsFetching } = useQuery(assetsQuery({}))
  const { data: me } = useQuery(meQuery())
  const whole = seesWholeFleet(me?.role)
  const phone = useMediaQuery('(max-width: 36em)') ?? false

  const [requesting, setRequesting] = useState(false)
  const [denied, setDenied] = useState<string>()
  const { host, as, open, view: askedView } = Route.useSearch()
  const [target, setTarget] = useState<string | null>(host ?? null)
  const [principal, setPrincipal] = useState(as ?? 'ops')
  const [session, setSession] = useState<
    { target: string; principal: string; protocol: 'ssh' | 'rdp' } | null
  >(null)
  /** Which view is in front, and which have been opened (and so stay mounted). */
  const [view, setView] = useState<SessionView>('terminal')
  const [opened, setOpened] = useState<Set<'terminal' | 'files'>>(new Set())
  /** The folder the terminal is in, as far as its shell says; Split follows it. */
  const [cwd, setCwd] = useState<string | null>(null)
  /** Phone: which way onto the server is showing. */
  const [way, setWay] = useState<'browser' | 'own'>('browser')
  /**
   * The Windows account's password, for a desktop's sign-in. Held only until
   * the desktop takes it -- it goes to the server's agent for this one logon
   * -- and cleared then, or when the server or account changes.
   */
  const [password, setPassword] = useState('')
  const heldPassword = useRef('')
  const [termState, setTermState] = useState<TerminalState>('connecting')
  const [termDetail, setTermDetail] = useState<string>()
  const [filesState, setFilesState] = useState<FilesState>('connecting')
  const [filesDetail, setFilesDetail] = useState<string>()
  /** The last session's failure, kept to show on the form after it closes. */
  const [lastError, setLastError] = useState<string>()

  /**
   * The ticket minted when the user pressed Connect, held for the first view to
   * consume.
   *
   * Pressing Connect has to ask the control plane whether this is allowed, and
   * that answer *is* a ticket. Throwing it away and letting the view mint a
   * second one burned two single-use tickets per session and wrote an audit
   * entry for a session that never opened — in a product whose whole claim is
   * that the audit log means something.
   *
   * A ref rather than state: consuming it must not re-render, and it is read
   * exactly once. Any later call (a second view, a reconnect, or StrictMode's
   * second mount in development) finds it empty and mints a fresh one, which is
   * the behaviour the single-use property requires.
   */
  const heldTicket = useRef<string | null>(null)
  // Ends the terminal on purpose; set by LiveTerminal while one is open.
  const endTerminal = useRef<(() => void) | null>(null)

  const selected = assets?.find((a) => a.hostname === target)
  const isRDPTarget = selected?.protocol === 'rdp'
  // A Windows server set to screen sharing opens its console as it is: no
  // Windows sign-in here, so no password to ask for.
  const isScreenTarget = isRDPTarget && selected?.desktopMode === 'screen'
  // An Android device opens as a shared screen too, through the app on it:
  // no password, and the account is what you may do (watch or control).
  const isDeviceTarget = selected?.kind === 'android'
  // Everything opens through the server's own agent, and only through it: a
  // server whose agent is not connected has no way in until it reconnects.
  const viaAgent = selected?.tunnelConnected === true
  const agentDown = !!selected && !viaAgent
  useEffect(() => setPassword(''), [target, principal])
  const own = useOwnTerminal(selected && !isRDPTarget ? selected.hostname : undefined)

  // Memoised so the effect below depends on the contents rather than on a fresh
  // array identity, which the `??` default produced on every single render.
  const principals = useMemo(
    () => selected?.principals ?? DEFAULT_PRINCIPALS,
    [selected?.principals],
  )

  // A principal carried over from the previously selected host may not exist on
  // this one. Left alone, the radio group shows nothing selected and Connect
  // sends an account the target does not have.
  //
  // Not before the list has arrived, though: until then `principals` is the
  // placeholder, and a `?as=root` would be thrown away for not being in it.
  const loaded = assets !== undefined
  useEffect(() => {
    if (!loaded) return
    setPrincipal((p) => (principals.includes(p) ? p : (principals[0] ?? 'ops')))
  }, [principals, loaded])

  const getTicket = useCallback(async () => {
    if (!session) return null
    const held = heldTicket.current
    if (held) {
      heldTicket.current = null
      return held
    }
    const res = await terminalTicket(session.target, session.principal)
    if ('error' in res) {
      setDenied(res.error)
      return null
    }
    return res.ticket
  }, [session])

  // Opened once, and only for what the link named: a server that is listed,
  // whose agent is connected, as an account it lists for this person. A
  // Remote Desktop is left to the button because Windows needs the password
  // first; a shared screen needs none.
  const autoOpened = useRef(false)
  useEffect(() => {
    if (!open || autoOpened.current || !loaded || !selected || session) return
    autoOpened.current = true
    if (!viaAgent || (isRDPTarget && !isScreenTarget) || !principals.includes(principal)) return
    void onConnect(askedView ?? 'terminal')
    // onConnect is recreated every render and reads the same state this
    // effect already gates on.
  }, [open, loaded, selected, viaAgent, isRDPTarget, isScreenTarget, principals, principal])

  const show = (v: SessionView) => {
    setView(v)
    setOpened((o) => {
      const next = new Set(o)
      if (v !== 'files') next.add('terminal')
      if (v !== 'terminal') next.add('files')
      return next.size === o.size ? o : next
    })
  }
  const endSession = (err?: string) => {
    heldTicket.current = null
    setLastError(err)
    setSession(null)
    setOpened(new Set())
    setCwd(null)
    setTermState('connecting')
    setFilesState('connecting')
  }

  if (session) {
    const isRDP = session.protocol === 'rdp'
    const openedAsset = assets?.find((a) => a.hostname === session.target)
    const isDevice = isRDP && openedAsset?.kind === 'android'
    const isScreen = isRDP && !isDevice && openedAsset?.desktopMode === 'screen'
    const short = session.target.split('.')[0] ?? session.target
    // The banner and badge speak for the view in front: the terminal's when it
    // shows, the files' when only they do.
    const filesFront = !isRDP && view === 'files'
    const state = filesFront ? filesState : termState
    const detail = filesFront ? filesDetail : termDetail
    const recordedHere = isDevice
      ? session.principal === 'view'
        ? 'The device\u2019s screen, as whoever holds it sees it. Watching only. Everything on screen is recorded.'
        : 'The device\u2019s screen, through the Argus app on it: whoever holds it sees what you do. Everything on screen is recorded.'
      : isScreen
      ? 'The server’s own screen, shared: anyone in front of it sees what you do. Everything on screen is recorded.'
      : isRDP
        ? 'Windows desktop. Everything on screen is recorded.'
        : view === 'files'
          ? 'Files on the server. Every upload, download, change and preview is recorded.'
          : view === 'split'
            ? 'Terminal and files side by side. Everything is recorded.'
            : 'Terminal. Everything you type and see is recorded.'
    const tab = (v: SessionView, label: string, icon: React.ReactNode) => (
      <button type="button" role="tab" className="argus-session-tab" aria-selected={view === v}
        onClick={() => show(v)}>
        {icon}{label}
      </button>
    )
    return (
      <Box h="calc(100vh - var(--app-shell-header-offset, 56px))" style={{ display: 'flex', flexDirection: 'column' }}>
        <PageHeader
          crumbs={[{ label: 'Assets', to: '/assets' }, { label: short }]}
          title={`${principalLabel(session.principal, openedAsset)} on ${short}`}
          status={
            <Badge
              color={state === 'live' ? 'teal' : state === 'error' ? 'rose' : state === 'reconnecting' || state === 'lost' ? 'amber' : 'slate'}
              variant="light"
            >
              {{ connecting: 'Connecting', live: 'Recording', reconnecting: 'Reconnecting', lost: 'Connection lost', closed: 'Ended', error: 'Failed' }[state]}
            </Badge>
          }
          description={recordedHere}
          actions={
            <Button variant="light" color="rose" leftSection={<IconX size={14} />} onClick={() => {
              if (endTerminal.current && (termState === 'live' || termState === 'reconnecting') &&
                !window.confirm('End this session? Anything still running in it, such as an update, is stopped. To leave it running, go to another page instead: it is kept for you to come back to.')) return
              endTerminal.current?.()
              endSession()
            }}>
              Disconnect
            </Button>
          }
        />
        {!isRDP && (
          <div className="argus-session-tabs" role="tablist" aria-label="Session view">
            {tab('terminal', 'Terminal', <IconTerminal2 size={16} />)}
            {tab('files', 'Files', <IconFolders size={16} />)}
            {!phone && tab('split', 'Split', <IconLayoutColumns size={16} />)}
          </div>
        )}
        {state === 'lost' && (
          <Alert color="amber" radius={0} icon={<IconPlugConnectedX size={18} />} title="Connection lost">
            <Group justify="space-between" wrap="wrap" gap="sm">
              <Text size={FS.body}>
                {detail ?? 'The connection to the server dropped.'} What ran until then was recorded.
              </Text>
              <Group gap="xs">
                <Button size="xs" onClick={() => endSession()}>Connect again</Button>
                <ButtonLink size="xs" variant="default" to="/assets">Back to servers</ButtonLink>
              </Group>
            </Group>
          </Alert>
        )}
        {state === 'closed' && (
          <Alert color="teal" radius={0} icon={<IconCircleCheck size={18} />} title="Session ended">
            <Group justify="space-between" wrap="wrap" gap="sm">
              <Text size={FS.body}>It was recorded. You can find it under {whole ? 'Activity' : 'My activity'}.</Text>
              <Group gap="xs">
                <Button size="xs" onClick={() => endSession()}>Connect again</Button>
                <ButtonLink size="xs" variant="default" to="/assets">Back to servers</ButtonLink>
              </Group>
            </Group>
          </Alert>
        )}
        {state === 'error' && detail && (
          <Alert color="rose" radius={0} icon={<IconX size={18} />} title="It could not open">
            <Group justify="space-between" wrap="wrap" gap="sm">
              <Text size={FS.body}>{detail}</Text>
              <Button size="xs" onClick={() => endSession(detail)}>Back</Button>
            </Group>
          </Alert>
        )}
        <Box style={{ flex: 1, minHeight: 0, overflow: 'hidden' }} p={isRDP ? 'md' : undefined}>
          <Suspense fallback={<TerminalLoading />}>
            {isRDP ? (
              <RDPScreen
                endpoint={desktopEndpoint()}
                target={session.target}
                principal={session.principal}
                device={isDevice}
                readOnly={isDevice && session.principal === 'view'}
                getTicket={getTicket}
                takePassword={() => {
                  const p = heldPassword.current
                  heldPassword.current = ''
                  return p
                }}
                onStateChange={(s, d) => {
                  setTermState(s === 'live' ? 'live' : s === 'error' ? 'error' : 'closed')
                  setTermDetail(d)
                }}
              />
            ) : (
              // Each view keeps its own connection once opened, so moving
              // between them does not drop a shell or a folder.
              <div className={view === 'split' ? 'argus-session-split' : 'argus-session-pane'}>
                {opened.has('terminal') && (
                  <div className="argus-session-pane" hidden={view === 'files'}>
                    <LiveTerminal
                      endRef={endTerminal}
                      endpoint={terminalEndpoint()}
                      getTicket={getTicket}
                      target={session.target}
                      principal={session.principal}
                      onCwd={setCwd}
                      onStateChange={(s, d) => {
                        setTermState(s)
                        setTermDetail(d)
                      }}
                    />
                  </div>
                )}
                {opened.has('files') && (
                  <div className="argus-session-pane" hidden={view === 'terminal'}>
                    <FileBrowser
                      endpoint={filesEndpoint()}
                      getTicket={getTicket}
                      target={session.target}
                      principal={session.principal}
                      compact={view === 'split'}
                      follow={view === 'split' ? cwd : null}
                      onStateChange={(s, d) => {
                        setFilesState(s)
                        setFilesDetail(d)
                      }}
                    />
                  </div>
                )}
              </div>
            )}
          </Suspense>
        </Box>
      </Box>
    )
  }

  const onConnect = async (first: SessionView = 'terminal') => {
    if (!target) return
    setDenied(undefined)
    setLastError(undefined)
    setRequesting(true)
    const res = await terminalTicket(target, principal)
    setRequesting(false)
    if ('error' in res) {
      // The control plane's refusal is the authoritative one, so show what it
      // actually said.
      setDenied(res.error)
      return
    }
    // Handed to the first view rather than discarded — this ticket is the
    // authorisation the user just obtained.
    heldTicket.current = res.ticket
    heldPassword.current = password
    setPassword('')
    const protocol = selected?.protocol === 'rdp' ? 'rdp' : 'ssh'
    const v: SessionView = protocol === 'rdp' ? 'terminal' : phone && first === 'split' ? 'terminal' : first
    setView(v)
    setOpened(new Set(v === 'split' ? ['terminal', 'files'] : [v === 'files' ? 'files' : 'terminal']))
    setTermState('connecting')
    setFilesState('connecting')
    setSession({ target, principal, protocol })
  }

  const elevated = isElevatedAccount(principal, selected)
  const name = target ? (target.split('.')[0] ?? target) : ''

  // Asked for by name and not in your list: you were not given it.
  if (loaded && target && !selected) {
    return (
      <Centered>
        <StateCard
          icon={<IconLock size={26} />}
          tone="slate"
          title={`You can't connect to ${name} yet`}
          body="Nobody has given you this server or device. Ask for access, and an approver decides. You get a note here when they do."
        >
          <ButtonLink size="md" to="/approvals" search={{ ask: target }}>Ask for access</ButtonLink>
          <ButtonLink size="md" variant="default" to="/assets">Back to servers</ButtonLink>
        </StateCard>
      </Centered>
    )
  }

  // The agent is the only way in, so a server without one is not reachable.
  if (selected && agentDown) {
    return (
      <Centered>
        <StateCard
          icon={<IconPlugConnectedX size={26} />}
          tone="amber"
          title={`${name} is offline`}
          body={`The Argus ${isDeviceTarget ? 'app' : 'agent'} on it has not checked in${selected.agentLastSeenAt ? ` since ${relTime(selected.agentLastSeenAt)}` : ''}. It may be ${isDeviceTarget ? 'off or out of coverage' : 'restarting'}. Try again in a minute, or tell an admin if it stays offline.`}
        >
          <Button size="md" loading={assetsFetching} onClick={() => void refetchAssets()}>Try again</Button>
          <ButtonLink size="md" variant="default" to="/assets">Back to servers</ButtonLink>
        </StateCard>
      </Centered>
    )
  }

  if (requesting) {
    return (
      <Centered>
        <StateCard
          icon={<Loader size={26} />}
          tone="azure"
          title={`Connecting to ${name}`}
          body={isDeviceTarget
            ? `Checking you may ${principal === 'view' ? 'watch' : 'control'} it, then opening its screen through the Argus app on it. Everything you do is recorded.`
            : `Checking you can log in as ${principal}, then opening it through the server’s agent. Everything you do is recorded.`}
        />
      </Centered>
    )
  }

  if (denied && selected) {
    return (
      <Centered>
        <StateCard
          icon={<IconHourglassHigh size={26} />}
          tone="amber"
          title={elevated
            ? isDeviceTarget ? `Controlling ${name} needs approval` : `${principal} needs approval`
            : isDeviceTarget ? `You can't ${principal === 'view' ? 'watch' : 'control'} ${name}` : `You can't log in as ${principal} here`}
          body={elevated
            ? isDeviceTarget
              ? `Control of ${name} is approved by someone else first, for a set time. Ask, and you can connect as soon as they say yes.`
              : `${principal} has full control of ${name}, so someone else approves it first, for a set time. Ask, and you can connect as soon as they say yes.`
            : denied}
        >
          <ButtonLink size="md" to="/approvals" search={{ ask: selected.hostname, as: principal }}>
            {elevated ? `Ask for ${isDeviceTarget ? 'control' : principal}` : 'Ask for access'}
          </ButtonLink>
          {principals.length > 1 && (
            <Button size="md" variant="default" onClick={() => setDenied(undefined)}>Pick another account</Button>
          )}
        </StateCard>
      </Centered>
    )
  }

  const noAccounts = principals.length === 0
  const canFiles = !!selected && viaAgent && !isRDPTarget && !noAccounts
  const showOwn = !!selected && !isRDPTarget
  const browserCard = (
    <Box className="argus-way" data-selected={(showOwn && !phone) || undefined}>
      <Group gap="md" align="flex-start" wrap="nowrap">
        <ThemeIcon size={44} radius="md" variant="light" color="azure">
          {isDeviceTarget ? <IconDeviceMobile size={22} /> : isRDPTarget ? <IconDeviceDesktop size={22} /> : <IconAppWindow size={22} />}
        </ThemeIcon>
        <Box style={{ minWidth: 0 }}>
          <Group gap="xs">
            <Text component="h2" fw={650} size="lg" m={0}>In this browser</Text>
            {showOwn && <Badge color="azure" variant="light">Easiest</Badge>}
          </Group>
          <Text size={FS.body} c="dimmed" mt={4} lh={1.5}>
            {isDeviceTarget
              ? 'Nothing to install. The device\u2019s screen opens on this page, through the Argus app on it.'
              : isRDPTarget
              ? 'Nothing to install. The desktop opens on this page, through the server’s Argus agent.'
              : 'Nothing to install or set up. A terminal opens on this page, through the server’s Argus agent.'}
          </Text>
        </Box>
      </Group>

      {!host && (
        <Select
          size="md"
          label="Server"
          placeholder={assets ? 'Type a server name' : 'Loading servers…'}
          disabled={!assets}
          searchable
          nothingFoundMessage="You have no server by that name"
          value={target}
          onChange={setTarget}
          data={assets?.map((a) => ({ value: a.hostname, label: a.hostname })) ?? []}
        />
      )}

      {target && !selected && <Text size={FS.meta} c="dimmed">Loading your accounts…</Text>}

      {selected && (
        <Box>
          <Text size="sm" fw={600} mb="sm">{isDeviceTarget ? 'Open it to' : 'Log in as'}</Text>
          {noAccounts ? (
            <Text size="sm" c="dimmed">
              {isDeviceTarget
                ? 'You may neither watch nor control this device yet. Ask an admin.'
                : 'You have no account on this server yet. Ask an admin to add one for you.'}
            </Text>
          ) : (
            <Radio.Group value={principal} onChange={setPrincipal}>
              <Stack gap="xs">
                {principals.map((p) => (
                  <Radio.Card key={p} value={p} className="argus-pick" data-checked={p === principal || undefined}>
                    <Group gap="sm" wrap="nowrap">
                      <Radio.Indicator />
                      <Box style={{ flex: 1, minWidth: 0 }}>
                        <Text size="sm" fw={600} ff={isDeviceTarget ? undefined : 'monospace'}>{principalLabel(p, selected)}</Text>
                        <Text size={FS.meta} c="dimmed">
                          {isDeviceTarget
                            ? p === 'view'
                              ? 'See the screen. Nothing you do reaches the device.'
                              : isElevatedAccount(p, selected)
                                ? 'Touch, type and press its keys. Needs approval first.'
                                : 'Touch, type and press its keys, as whoever holds it.'
                            : isElevatedAccount(p) ? 'Full control. Someone approves it first, for a set time.' : 'Your everyday account'}
                        </Text>
                      </Box>
                      {isElevatedAccount(p, selected) && <Badge color="amber" variant="light">approval</Badge>}
                    </Group>
                  </Radio.Card>
                ))}
              </Stack>
            </Radio.Group>
          )}
        </Box>
      )}

      {/* Windows asks for the account's own password before it shows
          a desktop. It goes to the server's agent for this one sign-in
          and is kept nowhere. */}
      {target && isDeviceTarget && !noAccounts && (
        <Alert color="azure" variant="light" icon={<IconDeviceMobile size={16} />}
          title={selected?.controlMode === 'unattended' ? 'Opens without anyone on the device' : 'Whoever holds it accepts first'}>
          <Text size={FS.body} lh={1.5}>
            {selected?.controlMode === 'unattended'
              ? 'You will see the device\u2019s screen as it is, locked or not. The app shows that a session is live while you are on it.'
              : 'The app asks the person holding the device to accept. Once they do, you see the screen as they do, and they see a banner naming you while you are on it.'}
            {' '}The recording keeps everything on screen; secure apps, such as banking, stay black.
          </Text>
        </Alert>
      )}

      {target && isScreenTarget && !isDeviceTarget && !noAccounts && (
        <Alert color="amber" variant="light" icon={<IconDeviceDesktop size={16} />}
          title="Screen sharing, not Remote Desktop">
          <Text size={FS.body} lh={1.5}>
            You will see the server&rsquo;s screen as it is: whoever is signed in there, or
            the Windows sign-in screen. Anyone in front of the server sees what you do. The
            recording notes which Windows account was signed in.
          </Text>
        </Alert>
      )}

      {selected && viaAgent && !isDeviceTarget && !noAccounts && (
        <SignedIn assetId={selected.id} principal={principal} />
      )}

      {target && isRDPTarget && !isScreenTarget && !isDeviceTarget && !noAccounts && (
        <PasswordInput
          size="md"
          label={`Windows password for ${principal}`}
          description="Used once to sign in on the server. Argus does not keep it."
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void onConnect()
          }}
        />
      )}

      <Group gap="md" wrap="wrap" mt="auto">
        <Button
          size="md"
          fullWidth={phone}
          leftSection={isDeviceTarget ? <IconDeviceMobile size={18} /> : isRDPTarget ? <IconDeviceDesktop size={18} /> : <IconTerminal2 size={18} />}
          disabled={!target || noAccounts}
          onClick={() => void onConnect()}
        >
          {isDeviceTarget ? (principal === 'view' ? 'Watch' : 'Open') : isRDPTarget ? 'Open desktop' : 'Open terminal'}
        </Button>
        {phone && canFiles && (
          <Button size="md" fullWidth variant="default" leftSection={<IconFolders size={18} />}
            onClick={() => void onConnect('files')}>
            Open files
          </Button>
        )}
        <Group gap={8} wrap="nowrap">
          <Box w={8} h={8} style={{ borderRadius: '50%', background: 'var(--mantine-color-rose-6)', flexShrink: 0 }} />
          <Text size={FS.meta} c="dimmed">
            {isRDPTarget ? 'Everything on screen is recorded' : 'Everything you type and see is recorded'}
          </Text>
        </Group>
      </Group>
    </Box>
  )

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: name || 'Connect' }]}
        title={selected ? `Connect to ${name}` : 'Connect'}
        status={selected && <ReachBadge reach={reachOf(selected)} />}
        description={selected
          ? [selected.os || { windows: 'Windows', linux: 'Linux', android: 'Android' }[kindOf(selected)], selected.address]
            .filter(Boolean).join(' · ') + (showOwn ? '. Two ways in, both recorded.' : '. Opens here in your browser, recorded.')
          : 'Pick a server or device. It opens here in your browser, and the session is recorded.'}
        actions={canFiles && !phone ? (
          <Button variant="default" leftSection={<IconFolders size={16} />} onClick={() => void onConnect('files')}>
            Open files
          </Button>
        ) : undefined}
      />

      <PageBody>
        <Stack gap="lg" w="100%">
          {(lastError || (termState === 'error' && termDetail)) && (
            <Alert color="rose" icon={<IconX size={16} />} title="The last session could not open"
              withCloseButton onClose={() => {
                setLastError(undefined)
                setTermDetail(undefined)
              }}>
              <Text size={FS.body} lh={1.5}>{lastError ?? termDetail}</Text>
            </Alert>
          )}

          {showOwn && <PendingDirectLogin hostname={selected.hostname} login={own.account?.loginName} />}

          {showOwn && phone && (
            <SegmentedControl fullWidth value={way} onChange={(v) => setWay(v as 'browser' | 'own')}
              data={[{ value: 'browser', label: 'In this browser' }, { value: 'own', label: 'Your terminal' }]}
              aria-label="Way in" />
          )}

          {showOwn ? (
            phone ? (way === 'browser' ? browserCard : <OwnTerminalCard asset={selected} />) : (
              <div className="argus-ways">
                {browserCard}
                <OwnTerminalCard asset={selected} />
              </div>
            )
          ) : (
            <Box maw={640} w="100%">{browserCard}</Box>
          )}

          <Text size={FS.meta} c="dimmed" lh={1.5} maw={720}>
            {isDeviceTarget
              ? 'Argus checks you may open this device, then the Argus app on it shares the screen and records the session. Argus never holds the device\u2019s PIN.'
              : <>Argus checks you may use this account, then the server&rsquo;s own agent opens the
            session and records it. No password or key of the server is shown to you.</>}
          </Text>
        </Stack>
      </PageBody>
    </Box>
  )
}

/** The full-height middle of the page, for the one-card states. */
function Centered({ children }: { children: React.ReactNode }) {
  return (
    <Center mih="calc(100dvh - var(--app-shell-header-offset, 60px))" p="md">
      {children}
    </Center>
  )
}

/** One state of connecting, said in a sentence, with what to do next. */
function StateCard({ icon, tone, title, body, children }: {
  icon: React.ReactNode
  tone: 'azure' | 'amber' | 'slate' | 'teal'
  title: string
  body: string
  children?: React.ReactNode
}) {
  return (
    <Box className="argus-panel" p="xl" maw={480} w="100%" ta="center">
      <Center mb="md">
        <ThemeIcon size={56} radius="xl" color={tone} variant="light">{icon}</ThemeIcon>
      </Center>
      <Text component="h1" fw={600} size="xl" style={{ margin: 0 }}>{title}</Text>
      <Text size="sm" c="dimmed" mt="xs" lh={1.6}>{body}</Text>
      {children && <Group justify="center" mt="lg" gap="sm">{children}</Group>}
    </Box>
  )
}

function TerminalLoading() {
  return (
    <Center h="100%" mih={220}>
      <Stack align="center" gap="xs">
        <Loader size="sm" color="azure" />
        <Text size={FS.meta} c="dimmed">Loading terminal…</Text>
      </Stack>
    </Center>
  )
}
