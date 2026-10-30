import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert, Badge, Box, Button, Center, Group, Loader, PasswordInput, Radio, Select,
  Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconCircleCheck, IconDeviceDesktop, IconFolders, IconHourglassHigh, IconLock,
  IconPlugConnectedX, IconTerminal2, IconX,
} from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { ReachBadge, reachOf } from '~/components/myservers'
import { relTime } from '~/components/primitives'
import { isElevatedAccount, seesWholeFleet } from '~/lib/roles'
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
  validateSearch: (
    s: Record<string, unknown>,
  ): { host?: string; as?: string; open?: boolean } => ({
    ...(typeof s.host === 'string' && s.host ? { host: s.host } : {}),
    ...(typeof s.as === 'string' && s.as ? { as: s.as } : {}),
    ...(s.open === true || s.open === 'true' || s.open === 1 ? { open: true } : {}),
  }),
})

/** Stable identity, so the principal-reset effect is not re-armed every render. */
const DEFAULT_PRINCIPALS = ['ops', 'deploy']

function Connect() {
  const { data: assets, refetch: refetchAssets, isFetching: assetsFetching } = useQuery(assetsQuery({}))
  const { data: me } = useQuery(meQuery())
  const whole = seesWholeFleet(me?.role)

  const [requesting, setRequesting] = useState(false)
  const [denied, setDenied] = useState<string>()
  const { host, as, open } = Route.useSearch()
  const [target, setTarget] = useState<string | null>(host ?? null)
  const [principal, setPrincipal] = useState(as ?? 'ops')
  const [session, setSession] = useState<
    { target: string; principal: string; protocol: 'ssh' | 'rdp' | 'files' } | null
  >(null)
  /**
   * The Windows account's password, for a desktop's sign-in. Held only until
   * the desktop takes it -- it goes to the server's agent for this one logon
   * -- and cleared then, or when the server or account changes.
   */
  const [password, setPassword] = useState('')
  const heldPassword = useRef('')
  const [state, setState] = useState<TerminalState>('connecting')
  const [detail, setDetail] = useState<string>()

  /**
   * The ticket minted when the user pressed Connect, held for the terminal to
   * consume.
   *
   * Pressing Connect has to ask the control plane whether this is allowed, and
   * that answer *is* a ticket. Throwing it away and letting the terminal mint a
   * second one burned two single-use tickets per session and wrote an audit
   * entry for a session that never opened — in a product whose whole claim is
   * that the audit log means something.
   *
   * A ref rather than state: consuming it must not re-render, and the terminal
   * reads it exactly once. Any later call (a reconnect, or StrictMode's second
   * mount in development) finds it empty and mints a fresh one, which is the
   * behaviour the single-use property requires.
   */
  const heldTicket = useRef<string | null>(null)

  const selected = assets?.find((a) => a.hostname === target)
  const isRDPTarget = selected?.protocol === 'rdp'
  // Everything opens through the server's own agent, and only through it: a
  // server whose agent is not connected has no way in until it reconnects.
  const viaAgent = selected?.tunnelConnected === true
  const agentDown = !!selected && !viaAgent
  useEffect(() => setPassword(''), [target, principal])

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
  // desktop is left to the button because Windows needs the password first.
  const autoOpened = useRef(false)
  useEffect(() => {
    if (!open || autoOpened.current || !loaded || !selected || session) return
    autoOpened.current = true
    if (!viaAgent || isRDPTarget || !principals.includes(principal)) return
    void onConnect()
    // onConnect is recreated every render and reads the same state this
    // effect already gates on.
  }, [open, loaded, selected, viaAgent, isRDPTarget, principals, principal])

  if (session) {
    const isRDP = session.protocol === 'rdp'
    const isFiles = session.protocol === 'files'
    return (
      <Box h="calc(100vh - var(--app-shell-header-offset, 56px))" style={{ display: 'flex', flexDirection: 'column' }}>
        <PageHeader
          crumbs={[{ label: 'Servers', to: '/assets' }, { label: session.target.split('.')[0] ?? session.target }]}
          title={`${session.principal} on ${session.target.split('.')[0] ?? session.target}`}
          status={
            <Badge
              color={state === 'live' ? 'teal' : state === 'error' ? 'rose' : 'slate'}
              variant="light"
            >
              {{ connecting: 'Connecting', live: 'Recording', closed: 'Ended', error: 'Failed' }[state]}
            </Badge>
          }
          description={
            isRDP
              ? 'Windows desktop. Everything on screen is recorded.'
              : isFiles
                ? 'Files on the server. Every upload and download is recorded.'
                : 'Terminal. Everything you type and see is recorded.'
          }
          actions={
            <Button
              variant="light"
              color="rose"
              leftSection={<IconX size={14} />}
              onClick={() => {
                heldTicket.current = null
                setSession(null)
              }}
            >
              Disconnect
            </Button>
          }
        />
        {state === 'closed' && (
          <Alert
            color="teal"
            radius={0}
            icon={<IconCircleCheck size={18} />}
            title="Session ended"
          >
            <Group justify="space-between" wrap="wrap" gap="sm">
              <Text size={FS.body}>It was recorded. You can find it under {whole ? 'Activity' : 'My activity'}.</Text>
              <Group gap="xs">
                <Button
                  size="xs"
                  onClick={() => {
                    heldTicket.current = null
                    setState('connecting')
                    setSession(null)
                  }}
                >
                  Connect again
                </Button>
                <ButtonLink size="xs" variant="default" to="/assets">Back to servers</ButtonLink>
              </Group>
            </Group>
          </Alert>
        )}
        <Box style={{ flex: 1, minHeight: 0, overflow: 'auto' }} p={isRDP ? 'md' : undefined}>
          <Suspense fallback={<TerminalLoading />}>
            {isFiles ? (
              <FileBrowser
                endpoint={filesEndpoint()}
                getTicket={getTicket}
                target={session.target}
                principal={session.principal}
                onStateChange={(s, d) => {
                  setState(s)
                  setDetail(d)
                }}
              />
            ) : isRDP ? (
              <RDPScreen
                endpoint={desktopEndpoint()}
                target={session.target}
                principal={session.principal}
                getTicket={getTicket}
                takePassword={() => {
                  const p = heldPassword.current
                  heldPassword.current = ''
                  return p
                }}
                onStateChange={(s, d) => {
                  setState(s === 'live' ? 'live' : s === 'error' ? 'error' : 'closed')
                  setDetail(d)
                }}
              />
            ) : (
              <LiveTerminal
                endpoint={terminalEndpoint()}
                getTicket={getTicket}
                target={session.target}
                principal={session.principal}
                onStateChange={(s, d) => {
                  setState(s)
                  setDetail(d)
                }}
              />
            )}
          </Suspense>
        </Box>
      </Box>
    )
  }

  const onConnect = async (mode: 'session' | 'files' = 'session') => {
    if (!target) return
    setDenied(undefined)
    setRequesting(true)
    const res = await terminalTicket(target, principal)
    setRequesting(false)
    if ('error' in res) {
      // The control plane's refusal is the authoritative one, so show what it
      // actually said.
      setDenied(res.error)
      return
    }
    // Handed to the terminal rather than discarded — this ticket is the
    // authorisation the user just obtained.
    heldTicket.current = res.ticket
    heldPassword.current = password
    setPassword('')
    setSession({
      target, principal,
      protocol: mode === 'files' ? 'files' : (selected?.protocol ?? 'ssh'),
    })
  }

  const elevated = isElevatedAccount(principal)
  const name = target ? (target.split('.')[0] ?? target) : ''

  // Asked for by name and not in your list: you were not given it.
  if (loaded && target && !selected) {
    return (
      <Centered>
        <StateCard
          icon={<IconLock size={26} />}
          tone="slate"
          title={`You can't connect to ${name} yet`}
          body="Nobody has given you this server. Ask for access, and an approver decides. You get a note here when they do."
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
          body={`The Argus agent on it has not checked in${selected.agentLastSeenAt ? ` since ${relTime(selected.agentLastSeenAt)}` : ''}. It may be restarting. Try again in a minute, or tell an admin if it stays offline.`}
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
          body={`Checking you can log in as ${principal}, then opening it through the server’s agent. Everything you do is recorded.`}
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
          title={elevated ? `${principal} needs approval` : `You can't log in as ${principal} here`}
          body={elevated
            ? `${principal} has full control of ${name}, so someone else approves it first, for a set time. Ask, and you can connect as soon as they say yes.`
            : denied}
        >
          <ButtonLink size="md" to="/approvals" search={{ ask: selected.hostname, as: principal }}>
            {elevated ? `Ask for ${principal}` : 'Ask for access'}
          </ButtonLink>
          {principals.length > 1 && (
            <Button size="md" variant="default" onClick={() => setDenied(undefined)}>Pick another account</Button>
          )}
        </StateCard>
      </Centered>
    )
  }

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Servers', to: '/assets' }, { label: name || 'Connect' }]}
        title={selected ? `Connect to ${name}` : 'Connect'}
        status={selected && <ReachBadge reach={reachOf(selected)} />}
        description={selected
          ? `${selected.os || (isRDPTarget ? 'Windows' : 'Linux')} · opens here in your browser, recorded.`
          : 'Pick a server. It opens here in your browser, and the session is recorded.'}
      />

      <PageBody>
        <Box maw={560} w="100%">
          {state === 'error' && detail && (
            <Alert color="rose" icon={<IconX size={16} />} title="The last session could not open" mb="md">
              <Text size={FS.body} lh={1.5}>{detail}</Text>
            </Alert>
          )}

          <Box className="argus-panel" p={{ base: 'md', sm: 'xl' }}>
            <Stack gap="lg">
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

              {target && (
                <Box>
                  <Text size="sm" fw={600} mb="sm">Log in as</Text>
                  {principals.length === 0 ? (
                    <Text size="sm" c="dimmed">
                      You have no account on this server yet. Ask an admin to add one for you.
                    </Text>
                  ) : (
                    <Radio.Group value={principal} onChange={setPrincipal}>
                      <Stack gap="xs">
                        {principals.map((p) => (
                          <Radio.Card
                            key={p}
                            value={p}
                            className="argus-pick"
                            data-checked={p === principal || undefined}
                          >
                            <Group gap="sm" wrap="nowrap">
                              <Radio.Indicator />
                              <Box style={{ flex: 1, minWidth: 0 }}>
                                <Text size="sm" fw={600} ff="monospace">{p}</Text>
                                <Text size={FS.meta} c="dimmed">
                                  {isElevatedAccount(p) ? 'Full control. Needs approval first.' : 'Your everyday account'}
                                </Text>
                              </Box>
                              {isElevatedAccount(p) && <Badge color="amber" variant="light">approval</Badge>}
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
              {target && isRDPTarget && principals.length > 0 && (
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

              <Group justify="space-between" wrap="wrap" gap="sm">
                {viaAgent && !isRDPTarget ? (
                  <Button
                    variant="default"
                    leftSection={<IconFolders size={16} />}
                    disabled={!target || principals.length === 0}
                    onClick={() => void onConnect('files')}
                  >
                    Files only
                  </Button>
                ) : <span />}
                <Button
                  size="md"
                  leftSection={isRDPTarget ? <IconDeviceDesktop size={18} /> : <IconTerminal2 size={18} />}
                  disabled={!target || principals.length === 0}
                  onClick={() => void onConnect()}
                >
                  Connect
                </Button>
              </Group>
            </Stack>
          </Box>
          <Text size={FS.meta} c="dimmed" mt="sm" lh={1.5}>
            Argus checks you may use this account, then the server&rsquo;s own agent opens the
            session and records it. No password or key of the server is shown to you.
          </Text>
        </Box>
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
