import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Center, Group, Loader, Radio, Select, Stack, Text,
  TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconInfoCircle, IconPlugConnected, IconTerminal2, IconX } from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import type { TerminalState } from '~/components/LiveTerminal'
import { assetsQuery } from '~/lib/queries'
import { GATEWAY_URL, terminalTicket } from '~/lib/live'
import { Mono } from '~/components/primitives'

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

export const Route = createFileRoute('/connect')({ component: Connect })

/**
 * Browser terminal.
 *
 * The gateway address and token are entered here rather than baked in, because
 * the control plane that would issue them does not exist yet. Once it does, the
 * console holds a session cookie and this form collapses to picking a host.
 */
const DEFAULT_GATEWAY = GATEWAY_URL

/** Stable identity, so the principal-reset effect is not re-armed every render. */
const DEFAULT_PRINCIPALS = ['ops', 'deploy']

function Connect() {
  const { data: assets } = useQuery(assetsQuery({}))

  const [gateway, setGateway] = useState(DEFAULT_GATEWAY)
  const [requesting, setRequesting] = useState(false)
  const [denied, setDenied] = useState<string>()
  const [target, setTarget] = useState<string | null>(null)
  const [principal, setPrincipal] = useState('ops')
  const [session, setSession] = useState<
    { target: string; principal: string; protocol: 'ssh' | 'rdp' } | null
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
      <Box h="calc(100vh - 52px)" style={{ display: 'flex', flexDirection: 'column' }}>
        <PageHeader
          title={isRDP ? 'Remote Desktop' : 'Terminal'}
          description={
            isRDP
              ? 'Brokered through the gateway — same policy, certificate pinning and recording as a desktop client.'
              : 'Brokered through the gateway — same policy, host-key verification and recording as ssh(1).'
          }
          actions={
            <Button
              size="xs"
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
                gatewayUrl={gateway}
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

  return (
    <Box>
      <PageHeader
        title="Connect"
        description="Open a session from the browser. For anyone who cannot use a native client — contractors on locked-down machines, auditors, on-call from a borrowed laptop."
      />

      <Box p="lg">
        <Stack gap="sm" maw={620}>
          {state === 'error' && detail && (
            <Alert color="rose" variant="light" icon={<IconX size={16} />} title="Connection refused">
              <Text size="xs">{detail}</Text>
            </Alert>
          )}

          <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />}>
            <Text size="xs">
              A browser session is not a lesser-controlled one. It goes through the same
              gateway code path as <Mono>ssh</Mono>: the principal is checked against the
              inventory, the target's host key is verified against its pin, the credential is
              injected so you never see it, and the session is recorded to the same
              hash-chained artefact.
            </Text>
          </Alert>

          <Card padding="md">
            <Text fw={600} size="sm" mb="sm">Gateway</Text>
            <TextInput
              size="xs"
              label="Address"
              description="Where argus-gateway serves the browser terminal"
              value={gateway}
              onChange={(e) => setGateway(e.currentTarget.value)}
            />
            <Text size="xs" c="dimmed" mt={8} lh={1.45}>
              No credential is entered here. When you open a terminal the console asks the
              control plane for a single-use ticket scoped to that host and account; it
              expires in a minute and cannot be reused.
            </Text>
          </Card>

          <Card padding="md">
            <Text fw={600} size="sm" mb="sm">Target</Text>
            <Stack gap="sm">
              <Select
                size="xs"
                label="Host"
                placeholder="Choose a host"
                searchable
                value={target}
                onChange={setTarget}
                data={assets?.map((a) => ({ value: a.hostname, label: a.hostname })) ?? []}
              />

              <Radio.Group
                label="Connect as"
                value={principal}
                onChange={setPrincipal}
              >
                <Group gap="lg" mt={6}>
                  {principals.map((p) => (
                    <Radio
                      key={p}
                      value={p}
                      size="xs"
                      label={
                        p === 'root' ? (
                          <Group gap={6}>
                            <Text size="sm">root</Text>
                            <Badge size="xs" color="rose">elevated</Badge>
                          </Group>
                        ) : p
                      }
                    />
                  ))}
                </Group>
              </Radio.Group>
            </Stack>
          </Card>

          {denied && (
            <Alert color="rose" variant="light" icon={<IconX size={16} />} title="Not permitted">
              <Text size="xs">{denied}</Text>
            </Alert>
          )}

          <Group justify="flex-end">
            <Button
              leftSection={<IconPlugConnected size={15} />}
              disabled={!target || !gateway}
              loading={requesting}
              onClick={async () => {
                if (!target) return
                setDenied(undefined)
                setRequesting(true)
                const res = await terminalTicket(target, principal)
                setRequesting(false)
                if ('error' in res) {
                  // The control plane's refusal is the authoritative one, so
                  // show what it actually said.
                  setDenied(res.error)
                  return
                }
                // Handed to the terminal rather than discarded — this ticket is
                // the authorisation the user just obtained.
                heldTicket.current = res.ticket
                setSession({ target, principal, protocol: selected?.protocol ?? 'ssh' })
              }}
            >
              {isRDPTarget ? 'Open desktop' : 'Open terminal'}
            </Button>
          </Group>

          <Card padding="md" style={{ borderStyle: 'dashed' }}>
            <Group gap={8} mb={6}>
              <IconTerminal2 size={15} className="text-slate-400" />
              <Text fw={600} size="sm">Prefer your own client?</Text>
            </Group>
            <Text size="xs" c="dimmed" lh={1.45}>
              Nothing here is required. The target is encoded in the username, so ordinary
              tooling works unchanged and gets the identical controls:
            </Text>
            <Text size="xs" ff="monospace" mt={8} c="slate.2">
              ssh {principal}:{target?.split('.')[0] ?? 'HOST'}@argus.northwind.id
            </Text>
          </Card>
        </Stack>
      </Box>
    </Box>
  )
}

function TerminalLoading() {
  return (
    <Center h="100%" mih={220}>
      <Stack align="center" gap="xs">
        <Loader size="sm" color="teal" />
        <Text size="xs" c="dimmed">Loading terminal…</Text>
      </Stack>
    </Center>
  )
}
