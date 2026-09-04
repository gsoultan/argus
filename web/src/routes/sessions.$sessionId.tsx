import { Suspense, lazy, useEffect, useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Grid, Group, Modal, ScrollArea, Stack, Text, Textarea, ThemeIcon, } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowLeft, IconDownload, IconEye, IconInfoCircle, IconLink,
  IconPlayerStop, IconShieldCheck,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { ButtonLink } from '~/components/links'
import { PlayerFallback } from '~/components/PlayerFallback'

/**
 * Split per branch, not merely per route.
 *
 * The terminal emulator and the desktop canvas are around 85 kB and 12 kB
 * gzipped respectively, and no session needs both: an SSH replay never draws a
 * desktop, an RDP replay never loads xterm, and the live-watch components are
 * reachable only from a modal on an active session. Importing all four at
 * module scope meant every visit paid for all of them.
 */
const Replay = lazy(() =>
  import('~/components/Replay').then((m) => ({ default: m.Replay })),
)
const RDPReplay = lazy(() =>
  import('~/components/RDPReplay').then((m) => ({ default: m.RDPReplay })),
)
const RDPScreen = lazy(() =>
  import('~/components/RDPScreen').then((m) => ({ default: m.RDPScreen })),
)
const ShadowTerminal = lazy(() =>
  import('~/components/ShadowTerminal').then((m) => ({ default: m.ShadowTerminal })),
)
import {
  Digest, Field, FidelityBadge, Mono, RiskFlags, SessionStateBadge, absTime, bytes,
  duration,
} from '~/components/primitives'
import { FS } from '~/theme'
import { buildCast } from '~/lib/cast'
import { downloadText, stamp } from '~/lib/download'
import { notifyOk } from '~/lib/notify'
import type { KernelExec } from '~/lib/castDecode'
import { sessionQuery, useTerminateSession } from '~/lib/queries'
import { GATEWAY_URL, live, rdpReplay, shadowTicket } from '~/lib/live'
import { useCastDecoder } from '~/lib/useWorkers'

export const Route = createFileRoute('/sessions/$sessionId')({
  component: SessionDetail,
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(sessionQuery(params.sessionId)),
})

function SessionDetail() {
  const { sessionId } = Route.useParams()
  const { data: session } = useQuery(sessionQuery(sessionId))
  const terminate = useTerminateSession()
  const [confirmOpen, confirm] = useDisclosure(false)
  const [shadowOpen, shadow] = useDisclosure(false)
  const [shadowError, setShadowError] = useState<string>()
  const [note, setNote] = useState('')
  const [cursor, setCursor] = useState(0)

  // Recording source. In production this is a signed URL to object storage.
  //
  // ?cast=<url> loads a real recording instead of the generated fixture, which
  // is how gateway output is checked against the production decoder.
  const [rdpFrames, setRdpFrames] = useState<{
    buffer: ArrayBuffer
    width: number
    height: number
    verified: string
  } | null>(null)
  const [rdpError, setRdpError] = useState<string>()
  const [realCast, setRealCast] = useState<string | undefined>()
  const [verified, setVerified] = useState<string | null>(null)
  const [fetched, setFetched] = useState(false)
  const castUrl = new URLSearchParams(window.location.search).get('cast')

  useEffect(() => {
    if (session?.protocol !== 'rdp' || rdpFrames) return
    let cancelled = false
    void (async () => {
      try {
        const got = await rdpReplay(session.id)
        if (cancelled) return
        if (!got) {
          setRdpError('This recording is not available for replay.')
          return
        }
        setRdpFrames(got)
      } catch (err) {
        if (!cancelled) {
          setRdpError(err instanceof Error ? err.message : 'Replay failed.')
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [session?.id, session?.protocol, rdpFrames])

  useEffect(() => {
    let cancelled = false
    if (castUrl) {
      void fetch(castUrl)
        .then((r) => r.text())
        .then((t) => { if (!cancelled) { setRealCast(t); setFetched(true) } })
        .catch(() => { if (!cancelled) setFetched(true) })
      return () => { cancelled = true }
    }
    if (!session) return
    void live
      .recording(session.id)
      .then((r) => {
        if (cancelled) return
        if (r) { setRealCast(r.cast); setVerified(r.verified) }
        setFetched(true)
      })
      .catch(() => { if (!cancelled) setFetched(true) })
    return () => { cancelled = true }
  }, [castUrl, session])

  const cast = useMemo(() => {
    if (realCast) return realCast
    // Only fall back to a generated cast once the real fetch has been tried and
    // failed, or the console would briefly replay a fixture and then swap it
    // for the real thing — which looks like the recording changed.
    if (!fetched) return undefined
    return session ? buildCast(session.assetHostname, session.principal, session.startedAt) : undefined
  }, [realCast, fetched, session])

  const decoded = useCastDecoder(cast)

  /**
   * Command timeline.
   *
   * For eBPF-fidelity sessions the control plane supplies kernel-observed
   * execve events. Without an agent we reconstruct the timeline from the
   * recording itself — stdin frames when present, prompt scraping otherwise.
   * The UI labels which one it is rather than presenting both as equivalent
   * evidence.
   */
  // Kernel evidence wins when it exists. extractCommands infers commands from
  // what the terminal echoed, which is a reasonable guess and nothing more; the
  // kernel saw what actually ran. Showing the guess alongside real evidence
  // would invite reading them as equally reliable.
  const commands = useMemo(() => {
    if (decoded.execs.length > 0) {
      return decoded.execs.map((e) => ({
        t: e.t,
        cmd: renderExec(e),
      }))
    }
    // Computed in the worker, which is where the frames now live.
    return decoded.heuristicCommands
  }, [decoded.execs, decoded.heuristicCommands])

  const kernelObserved = decoded.execs.length > 0

  if (!session) {
    return (
      <Box p="lg">
        <Text size="sm" c="dimmed">Session not found.</Text>
      </Box>
    )
  }

  /**
   * Exports the recording exactly as decoded — the asciicast bytes themselves,
   * not a re-serialisation of the player's state. `asciinema play` and any
   * other v2 reader consume it unchanged, and its hash still matches the chain.
   */
  const onExportCast = () => {
    if (!cast) return
    downloadText(
      cast,
      `argus-${session.assetHostname.split('.')[0]}-${session.principal}-${stamp(new Date(session.startedAt))}.cast`,
      'application/x-asciicast',
    )
    notifyOk(
      'Recording exported',
      'asciicast v2. Play it with `asciinema play`, or re-hash it to check it against the chain head.',
    )
  }

  const onTerminate = async () => {
    try {
      await terminate.mutateAsync({ id: session.id, reason: note })
    } catch (err) {
      // Never close the dialog on failure. Dismissing it would leave the
      // operator believing they stopped a session that is still running.
      notifications.show({
        color: 'rose',
        title: 'Session not terminated',
        message: err instanceof Error ? err.message : 'the gateway refused',
      })
      return
    }
    confirm.close()
    setNote('')
    notifications.show({
      color: 'rose',
      title: 'Session terminated',
      message: `Connection to ${session.assetHostname} was closed and the reason recorded in the audit log.`,
    })
  }

  // Watching is offered only while there is something to watch. A shadow button
  // on a finished session would open a stream that immediately ends, which
  // reads as a fault rather than as "this session is over".
  const canShadow = session.state === 'active'
  const isRDP = session.protocol === 'rdp'

  return (
    <Box>
      <PageHeader
        title={`${session.principal}@${session.assetHostname.split('.')[0]}`}
        description={`Opened by ${session.userEmail} from ${session.clientIp} · ${absTime(session.startedAt)}`}
        actions={
          <>
            <ButtonLink
              size="xs"
              variant="subtle"
              color="slate"
              to="/sessions"
              leftSection={<IconArrowLeft size={14} />}
            >
              Back
            </ButtonLink>
            <Button
              size="xs"
              variant="default"
              leftSection={<IconDownload size={14} />}
              disabled={!cast}
              onClick={onExportCast}
            >
              Export .cast
            </Button>
            {canShadow && (
              <Button
                size="xs"
                color="amber"
                variant="light"
                leftSection={<IconEye size={14} />}
                onClick={shadow.open}
              >
                Watch live
              </Button>
            )}
            {session.state === 'active' && (
              <Button
                size="xs"
                color="rose"
                leftSection={<IconPlayerStop size={14} />}
                onClick={confirm.open}
              >
                Terminate
              </Button>
            )}
          </>
        }
      />

      <Box p="lg">
        {session.fidelity === 'pty' && !kernelObserved && (
          <Alert
            color="amber"
            variant="light"
            icon={<IconInfoCircle size={16} />}
            mb="md"
            title="PTY-only recording"
          >
            <Text size="xs">
              This session was captured at the gateway as a terminal stream. It faithfully shows
              what crossed the wire, but a user can obscure intent — base64-encoded commands, or
              a script whose body never appears on screen. Treat the command timeline below as an
              audit aid, not proof. Enable the eBPF agent on this host for kernel-observed
              execve evidence.
            </Text>
          </Alert>
        )}

        <Grid gap="sm">
          <Grid.Col span={{ base: 12, xl: 8 }}>
            <Card padding={0} style={{ overflow: 'hidden' }}>
              {isRDP ? (
                rdpFrames ? (
                  <Box p="sm">
                    <Suspense fallback={<PlayerFallback label="Loading desktop replay…" />}>
                    <RDPReplay
                      buffer={rdpFrames.buffer}
                      width={rdpFrames.width}
                      height={rdpFrames.height}
                      verified={rdpFrames.verified}
                    />
                    </Suspense>
                  </Box>
                ) : (
                  <Box p="lg">
                    <Text size="xs" c="dimmed">
                      {rdpError ?? 'Decoding the recording…'}
                    </Text>
                  </Box>
                )
              ) : (
                <Suspense fallback={<PlayerFallback label="Loading player…" />}>
                  <Replay decoded={decoded} onTimeChange={setCursor} />
                </Suspense>
              )}
            </Card>
          </Grid.Col>

          <Grid.Col span={{ base: 12, xl: 4 }}>
            <Stack gap="sm">
              <Card padding="md">
                <Group justify="space-between" mb="sm">
                  <Text fw={600} size="sm">Session</Text>
                  <SessionStateBadge state={session.state} />
                </Group>
                <Stack gap={10}>
                  <Field label="Target">
                    <Group gap={6}>
                      <Mono>{session.assetHostname}</Mono>
                      <ButtonLink
                        size="compact-xs"
                        variant="subtle"
                        color="slate"
                        to="/assets/$assetId"
                        params={{ assetId: session.assetId }}
                        leftSection={<IconLink size={11} />}
                      >
                        asset
                      </ButtonLink>
                    </Group>
                  </Field>
                  <Group grow>
                    <Field label="Principal"><Mono c="teal.4">{session.principal}</Mono></Field>
                    <Field label="Protocol"><Mono>{session.protocol}</Mono></Field>
                  </Group>
                  <Group grow>
                    <Field label="Duration">
                      <Text size="xs">{duration(session.startedAt, session.endedAt)}</Text>
                    </Field>
                    <Field label="Size">
                      <Text size="xs">{bytes(session.recordingBytes)}</Text>
                    </Field>
                  </Group>
                  <Field label="Recording fidelity">
                    <FidelityBadge fidelity={session.fidelity} />
                  </Field>
                  <Field label="Risk flags"><RiskFlags flags={session.riskFlags} /></Field>
                </Stack>
              </Card>

              <Card padding="md">
                <Group gap={8} mb={6}>
                  <ThemeIcon variant="light" color="teal" size={22} radius="sm">
                    <IconShieldCheck size={13} />
                  </ThemeIcon>
                  <Text fw={600} size="sm">Recording integrity</Text>
                </Group>
                <Text size={FS.micro} c="dimmed" mb="sm" lh={1.45}>
                  Each recording chunk is hashed into a chain rooted at the session start, so any
                  edit to the stored artefact invalidates every subsequent link.
                </Text>
                <Field label="Chain head">
                  {session.chainHead ? <Digest value={session.chainHead} chars={32} /> : '—'}
                </Field>
                {verified && (
                  <Field label="Server verification">
                    <Badge
                      size="sm"
                      color={verified === 'intact' ? 'teal' : verified === 'tampered' ? 'rose' : 'slate'}
                      variant={verified === 'tampered' ? 'filled' : 'light'}
                    >
                      {verified === 'intact' ? 'chain intact' : verified}
                    </Badge>
                  </Field>
                )}
                <ButtonLink
                  size="compact-xs"
                  variant="light"
                  color="teal"
                  mt="sm"
                  fullWidth
                  to="/audit"
                >
                  Verify in audit log
                </ButtonLink>
              </Card>

              <Card padding={0}>
                <Group justify="space-between" p="md" pb="xs">
                  <Text fw={600} size="sm">Command timeline</Text>
                  <Badge
                    size="xs"
                    color={kernelObserved ? 'teal' : 'amber'}
                    variant="light"
                  >
                    {kernelObserved ? 'kernel-observed' : 'heuristic'}
                  </Badge>
                </Group>
                <ScrollArea.Autosize mah={300}>
                  <Stack gap={0}>
                    {commands.map((c, i) => {
                      const active =
                        cursor >= c.t && (i === commands.length - 1 || cursor < commands[i + 1]!.t)
                      return (
                        <Box
                          key={`${c.t}-${i}`}
                          px="md"
                          py={6}
                          style={{
                            borderTop: '1px solid var(--color-line)',
                            background: active ? 'rgba(45,212,167,0.07)' : undefined,
                            borderLeft: active
                              ? '2px solid var(--color-verified)'
                              : '2px solid transparent',
                          }}
                        >
                          <Group gap={8} wrap="nowrap" align="flex-start">
                            <Text size={FS.micro} c="dimmed" ff="monospace" w={38} style={{ flexShrink: 0 }}>
                              {`${String(Math.floor(c.t / 60)).padStart(2, '0')}:${String(Math.floor(c.t % 60)).padStart(2, '0')}`}
                            </Text>
                            <Text size="xs" ff="monospace" c={active ? 'teal.3' : 'slate.2'} style={{ wordBreak: 'break-all' }}>
                              {c.cmd}
                            </Text>
                          </Group>
                        </Box>
                      )
                    })}
                    {commands.length === 0 && (
                      <Text size="xs" c="dimmed" ta="center" py="lg">
                        No commands detected.
                      </Text>
                    )}
                  </Stack>
                </ScrollArea.Autosize>
              </Card>
            </Stack>
          </Grid.Col>
        </Grid>
      </Box>

      <Modal
        opened={shadowOpen}
        onClose={shadow.close}
        title={`Watching ${session.userEmail} · ${session.principal}@${session.assetHostname}`}
        size="90%"
      >
        <Alert color="amber" variant="light" icon={<IconEye size={16} />} mb="md">
          <Text size="xs">
            Read-only. Keystrokes are not shown — the host echoes what the user types, so the
            output below already contains it. Your attaching to this session has been written to
            the audit log against your account.
          </Text>
        </Alert>
        {shadowError && (
          <Alert color="rose" variant="light" mb="md">
            <Text size="xs">{shadowError}</Text>
          </Alert>
        )}
        {/* Keyed on open state so closing the modal disposes the terminal and
            drops the subscription, rather than leaving a socket streaming a
            privileged session into a hidden component. */}
        {shadowOpen && isRDP && (
          <Suspense fallback={<PlayerFallback label="Loading desktop view…" />}>
          <RDPScreen
            gatewayUrl={GATEWAY_URL}
            target={session.assetHostname}
            principal={session.principal}
            readOnly
            shadowSessionId={session.id}
            getTicket={async () => {
              const res = await shadowTicket(session.id)
              if ('error' in res) {
                setShadowError(res.error)
                return null
              }
              setShadowError(undefined)
              return res.ticket
            }}
          />
          </Suspense>
        )}
        {shadowOpen && !isRDP && (
          <Suspense fallback={<PlayerFallback label="Loading terminal…" />}>
          <ShadowTerminal
            gatewayUrl={GATEWAY_URL}
            sessionId={session.id}
            getTicket={async () => {
              const res = await shadowTicket(session.id)
              if ('error' in res) {
                setShadowError(res.error)
                return null
              }
              setShadowError(undefined)
              return res.ticket
            }}
          />
          </Suspense>
        )}
      </Modal>

      <Modal opened={confirmOpen} onClose={confirm.close} title="Terminate session" size="md">
        <Alert color="rose" variant="light" icon={<IconAlertTriangle size={16} />} mb="md">
          <Text size="xs">
            The connection to <Mono>{session.assetHostname}</Mono> will be closed immediately.
            Any in-flight command keeps running on the host — terminating the session stops
            further input, it does not roll anything back.
          </Text>
        </Alert>
        <Textarea
          label="Reason"
          description="Recorded in the audit log against your account."
          placeholder="e.g. Session opened outside the approved window for INC-4471."
          minRows={3}
          autosize
          value={note}
          onChange={(e) => setNote(e.currentTarget.value)}
        />
        <Group justify="flex-end" mt="md">
          <Button variant="subtle" color="slate" size="xs" onClick={confirm.close}>
            Cancel
          </Button>
          <Button
            color="rose"
            size="xs"
            loading={terminate.isPending}
            disabled={note.trim().length < 8}
            onClick={onTerminate}
          >
            Terminate session
          </Button>
        </Group>
      </Modal>
    </Box>
  )
}

/**
 * Renders one kernel-observed execution.
 *
 * Quoting matters: unquoted, `sh -c "rm -rf /"` reads as three harmless words,
 * which misrepresents what ran — and being able to trust this line is the whole
 * reason the kernel tier exists.
 */
function renderExec(e: KernelExec): string {
  const args = e.args.length > 0 ? e.args : [e.filename]
  const rendered = args
    .map((a) => (a === '' ? "''" : /[\s'"\\$`]/.test(a) ? `'${a.replaceAll("'", `'\\''`)}'` : a))
    .join(' ')
  return e.truncated ? `${rendered} …[truncated]` : rendered
}
