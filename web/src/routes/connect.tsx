import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Center, Grid, Group, Loader, Radio, Select, Stack,
  Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAdjustments, IconDeviceDesktop, IconInfoCircle, IconPlugConnected,
  IconServer2, IconTerminal2, IconUser, IconX,
} from '@tabler/icons-react'
import { PageBody, PageHeader } from '~/components/page'
import type { TerminalState } from '~/components/LiveTerminal'
import { assetsQuery } from '~/lib/queries'
import { GATEWAY_URL, isConfigured, terminalEndpoint, terminalTicket } from '~/lib/live'
import { HostKeyBadge } from '~/components/primitives'
import { FS, SP } from '~/theme'

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
  validateSearch: (s: Record<string, unknown>): { host?: string } =>
    typeof s.host === 'string' && s.host ? { host: s.host } : {},
})

const DEFAULT_GATEWAY = GATEWAY_URL

/** Stable identity, so the principal-reset effect is not re-armed every render. */
const DEFAULT_PRINCIPALS = ['ops', 'deploy']

function Connect() {
  const { data: assets } = useQuery(assetsQuery({}))

  const [gateway, setGateway] = useState(DEFAULT_GATEWAY)
  const [requesting, setRequesting] = useState(false)
  const [denied, setDenied] = useState<string>()
  const { host } = Route.useSearch()
  const [target, setTarget] = useState<string | null>(host ?? null)
  const [principal, setPrincipal] = useState('ops')
  // Shown by default only when the console has no control plane to get an
  // address from — that is the one case where the user has to supply it.
  const [showGateway, setShowGateway] = useState(!isConfigured())
  const [session, setSession] = useState<
    { target: string; principal: string; protocol: 'ssh' | 'rdp'; viaAgent: boolean } | null
  >(null)
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
  // SSH opens through the server's own agent whenever it is connected; the
  // gateway is only for servers whose agent is not. Remote Desktop is still
  // the gateway's.
  const viaAgent = !isRDPTarget && selected?.tunnelConnected === true

  // Memoised so the effect below depends on the contents rather than on a fresh
  // array identity, which the `??` default produced on every single render.
  const principals = useMemo(
    () => selected?.principals ?? DEFAULT_PRINCIPALS,
    [selected?.principals],
  )

  // A principal carried over from the previously selected host may not exist on
  // this one. Left alone, the radio group shows nothing selected and Connect
  // sends an account the target does not have.
  useEffect(() => {
    setPrincipal((p) => (principals.includes(p) ? p : (principals[0] ?? 'ops')))
  }, [principals])

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

  if (session) {
    const isRDP = session.protocol === 'rdp'
    return (
      <Box h="calc(100vh - var(--app-shell-header-offset, 56px))" style={{ display: 'flex', flexDirection: 'column' }}>
        <PageHeader
          crumbs={[{ label: 'Connect', to: '/connect' }, { label: session.target }]}
          title={isRDP ? 'Remote Desktop' : 'Terminal'}
          status={
            <Badge
              color={state === 'live' ? 'sky' : state === 'error' ? 'rose' : 'slate'}
              variant="light"
            >
              {state}
            </Badge>
          }
          description={
            isRDP
              ? 'Brokered through the gateway — same policy, certificate pinning and recording as a desktop client.'
              : session.viaAgent
                ? 'Opened on the server by its Argus agent, which records it there before anything reaches you.'
                : 'Brokered through the gateway — same policy, host-key verification and recording as ssh(1).'
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
        <Box style={{ flex: 1, minHeight: 0, overflow: 'auto' }} p={isRDP ? 'md' : undefined}>
          <Suspense fallback={<TerminalLoading />}>
            {isRDP ? (
              <RDPScreen
                gatewayUrl={gateway}
                target={session.target}
                principal={session.principal}
                getTicket={getTicket}
                onStateChange={(s, d) => {
                  setState(s === 'live' ? 'live' : s === 'error' ? 'error' : 'closed')
                  setDetail(d)
                }}
              />
            ) : (
              <LiveTerminal
                endpoint={terminalEndpoint(session.viaAgent, gateway)}
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

  const onConnect = async () => {
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
    setSession({ target, principal, protocol: selected?.protocol ?? 'ssh', viaAgent })
  }

  return (
    <Box>
      <PageHeader
        title="Connect"
        description="Pick a server and an account, and the session opens here in the browser: a terminal for SSH, a desktop for Remote Desktop."
      />

      <PageBody>
        {state === 'error' && detail && (
          <Alert color="rose" icon={<IconX size={16} />} title="Connection refused">
            <Text size={FS.body} lh={1.5}>{detail}</Text>
          </Alert>
        )}

        <Grid gap="sm">
          {/* Two decisions, numbered, then one button. The form previously led
              with a gateway address field — server plumbing the operator has no
              answer for and does not need, standing between them and the two
              choices that actually matter. */}
          <Grid.Col span={{ base: 12, lg: 7 }}>
            <Stack gap="sm">
              <Step n={1} icon={IconServer2} title="Choose a host">
                {/* An empty inventory and an empty *assignment* look identical
                    in a searchable Select — it answers "no host by that name"
                    to every query — and they need opposite responses. Saying
                    which one this is turns a dead form into a next step. */}
                {assets?.length === 0 && (
                  <Alert
                    variant="light"
                    color="amber"
                    icon={<IconInfoCircle size={16} />}
                    title="No hosts are assigned to you"
                    mb="xs"
                  >
                    <Text size={FS.meta} lh={1.5}>
                      An administrator assigns the hosts you may reach, and which account
                      you may assume on each. If you need one for a specific piece of work,
                      raise an access request and have an approver decide it.
                    </Text>
                  </Alert>
                )}
                <Select
                  size="sm"
                  placeholder={assets ? 'Search the inventory' : 'Loading hosts…'}
                  // An enabled, searchable, empty Select reports "No host by
                  // that name" for every query while the inventory is still on
                  // its way, which is a wrong answer rather than a slow one.
                  disabled={!assets}
                  searchable
                  nothingFoundMessage="No host by that name"
                  value={target}
                  onChange={setTarget}
                  data={assets?.map((a) => ({ value: a.hostname, label: a.hostname })) ?? []}
                />
                {selected && (
                  <Group gap="xs" mt="xs" wrap="wrap">
                    <Badge
                      variant="light"
                      color="slate"
                      leftSection={
                        isRDPTarget ? <IconDeviceDesktop size={11} /> : <IconTerminal2 size={11} />
                      }
                    >
                      {selected.protocol}
                    </Badge>
                    <Badge variant="outline" color="slate" className="argus-digest">
                      {selected.address}:{selected.port}
                    </Badge>
                    {/* The trust anchor, stated before connecting rather than
                        discovered afterwards: Argus terminates SSH, so an
                        unpinned target is one nobody has verified. */}
                    <HostKeyBadge state={selected.hostKeyState} />
                  </Group>
                )}
              </Step>

              <Step n={2} icon={IconUser} title="Choose an account" disabled={!target}>
                {!target ? (
                  <Text size={FS.meta} c="dimmed">
                    Pick a host first — the accounts you may assume depend on it.
                  </Text>
                ) : principals.length === 0 ? (
                  // An asset can list no principals at all — enrolled from
                  // Coverage and not finished, say. An empty radio group would
                  // look like a page that had not loaded.
                  <Text size={FS.meta} c="dimmed">
                    This host permits no accounts you may assume. An administrator lists the
                    accounts on the asset and assigns them.
                  </Text>
                ) : (
                  <Radio.Group value={principal} onChange={setPrincipal}>
                    <Group gap="lg">
                      {principals.map((p) => (
                        <Radio
                          key={p}
                          value={p}
                          label={
                            p === 'root' ? (
                              <Group gap={SP.snug}>
                                <Text size="sm">root</Text>
                                <Badge size="xs" color="rose">elevated</Badge>
                              </Group>
                            ) : p
                          }
                        />
                      ))}
                    </Group>
                  </Radio.Group>
                )}
              </Step>

              {denied && (
                <Alert color="rose" icon={<IconX size={16} />} title="Not permitted">
                  <Text size={FS.body} lh={1.5}>{denied}</Text>
                </Alert>
              )}

              <Group justify="space-between" align="center">
                <Text size={FS.micro} c="dimmed">
                  {target
                    ? `Opens ${principal}@${target.split('.')[0]} ${
                        viaAgent ? "through the server's Argus agent" : 'through the gateway'
                      }.`
                    : 'Nothing is sent until you choose a host.'}
                </Text>
                <Button
                  size="sm"
                  leftSection={<IconPlugConnected size={15} />}
                  disabled={!target || (!viaAgent && !gateway)}
                  loading={requesting}
                  onClick={onConnect}
                >
                  {isRDPTarget ? 'Open desktop' : 'Open terminal'}
                </Button>
              </Group>
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 5 }}>
            <Stack gap="sm">
              {/* Plain words first. The earlier panel was one paragraph of
                  gateway internals (principals, pins, hash-chained artefacts)
                  that answered a question a security reviewer asks, not the
                  one a person about to connect has. */}
              <Card padding="md">
                <Text fw={600} size="sm" mb={SP.snug}>What happens when you connect</Text>
                <Stack gap={SP.snug}>
                  {(viaAgent
                    ? [
                        'Argus checks you are allowed on that server as that account.',
                        "The server's own agent checks Argus's signature, then signs you in. No password or key is involved.",
                        'Nothing reaches the server except through the agent it enrolled, so nothing can pose as it.',
                        'The agent records the whole session on the server before you see any of it.',
                      ]
                    : [
                        'Argus checks you are allowed on that server as that account.',
                        'It signs in for you. You never see or type a password or key.',
                        'It checks the server is the one it was last time, so nothing can pose as it.',
                        'The whole session is recorded, the same as one opened with ssh.',
                      ]
                  ).map((line) => (
                    <Group key={line} gap={SP.cozy} wrap="nowrap" align="flex-start">
                      <IconInfoCircle size={15} style={{ marginTop: 2, flexShrink: 0, opacity: 0.55 }} />
                      <Text size={FS.body} c="dimmed" lh={1.5}>{line}</Text>
                    </Group>
                  ))}
                </Stack>
              </Card>

              <Card style={{ borderStyle: 'dashed' }}>
                <Group gap={SP.cozy} mb={SP.snug}>
                  <ThemeIcon variant="light" color="slate" size={22} radius="sm">
                    <IconTerminal2 size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Prefer your own client?</Text>
                </Group>
                <Text size={FS.meta} c="dimmed" lh={1.5}>
                  Nothing here is required. The target is encoded in the username, so ordinary
                  tooling works unchanged and gets the identical controls:
                </Text>
                <Text size={FS.digest} ff="monospace" mt={SP.cozy} c="slate.2">
                  ssh {principal}:{target?.split('.')[0] ?? 'HOST'}@argus.northwind.id
                </Text>
              </Card>

              {/* Advanced, and collapsed once a control plane supplies the
                  address. It only has to be typed when there is no control
                  plane to ask, which is also the only time it is editable
                  usefully. Not shown at all for a server reached through its
                  agent, which never touches a gateway. */}
              {!viaAgent && (
              <Card>
                <Group justify="space-between" wrap="nowrap">
                  <Group gap={SP.cozy}>
                    <ThemeIcon variant="light" color="slate" size={22} radius="sm">
                      <IconAdjustments size={13} />
                    </ThemeIcon>
                    <Text fw={600} size="sm">Gateway endpoint</Text>
                  </Group>
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    color="slate"
                    onClick={() => setShowGateway((v) => !v)}
                  >
                    {showGateway ? 'Hide' : 'Change'}
                  </Button>
                </Group>
                {showGateway ? (
                  <TextInput
                    mt="xs"
                    label="Address"
                    description="Where argus-gateway serves the browser terminal"
                    value={gateway}
                    onChange={(e) => setGateway(e.currentTarget.value)}
                  />
                ) : (
                  <Text size={FS.digest} ff="monospace" c="dimmed" mt={SP.snug}>
                    {gateway || 'not set'}
                  </Text>
                )}
              </Card>
              )}
            </Stack>
          </Grid.Col>
        </Grid>
      </PageBody>
    </Box>
  )
}

/** One numbered decision. Makes the page a sequence rather than a wall of cards. */
function Step({
  n, icon: Icon, title, disabled = false, children,
}: {
  n: number
  icon: typeof IconServer2
  title: string
  disabled?: boolean
  children: React.ReactNode
}) {
  return (
    <Card style={disabled ? { opacity: 0.65 } : undefined}>
      <Group gap={SP.cozy} mb="sm" wrap="nowrap">
        <Center
          w={22}
          h={22}
          style={{
            borderRadius: 999,
            border: '1px solid var(--color-line)',
            background: 'var(--color-raised)',
            flexShrink: 0,
          }}
        >
          <Text size={FS.micro} fw={700} c={disabled ? 'dimmed' : 'azure.3'}>{n}</Text>
        </Center>
        <Icon size={14} opacity={0.6} />
        <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>{title}</Text>
      </Group>
      {children}
    </Card>
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
