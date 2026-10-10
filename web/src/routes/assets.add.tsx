import {
  ActionIcon, Alert, Badge, Box, Button, Card, CopyButton, Grid, Group, Loader, Modal,
  SegmentedControl, Stack, Text, TextInput, Textarea, ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconCheck, IconCircleCheck, IconCopy, IconEye, IconInfoCircle, IconKey, IconServer2,
  IconUserCheck,
} from '@tabler/icons-react'
import { useMemo, useState, type ReactNode } from 'react'
import { PageBody, PageHeader } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { Mono, relTime } from '~/components/primitives'
import { controlPlaneHost, isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import type { EnrollToken } from '~/lib/live'
import {
  discoveredQuery, enrolledAgentsQuery, meQuery, useCreateEnrollToken, useEnrolHost,
  useIgnoreHost, useRevokeAgent,
} from '~/lib/queries'
import type { DiscoveredHost } from '~/types/domain'
import { FS, SP } from '~/theme'

/**
 * Adding a server, start to finish, on one page.
 *
 * Registration is automatic discovery with a manual approval, and this page is
 * built around saying that plainly. The agent finds Argus by itself: once it is
 * installed and pointed at the control plane it reports in, and the control
 * plane lists it as waiting. Argus never trusts a server by itself, though --
 * nobody can connect to it until an admin approves it here. Enrolling is a
 * change to what Argus manages, so it stays a decision a person makes.
 *
 * Before this page the flow existed but was spread across a README section, a
 * systemd unit with no control-plane flags in it, and a table at the bottom of
 * Coverage titled "hosts outside the inventory". Nothing in the console said
 * "this is how you add a server".
 *
 * Nothing here is invented. In fixture mode there is no control plane for an
 * agent to report to, so the waiting list says that instead of showing sample
 * servers as if they had checked in.
 */
export const Route = createFileRoute('/assets/add')({ component: AddServer })

type Distro = 'deb' | 'rpm' | 'apk'
type Arch = 'amd64' | 'arm64'

/**
 * Prefilled, and editable beside the commands: the agent should match the
 * control plane it reports to, which the console cannot ask about.
 */
const DEFAULT_VERSION = '0.1.0'

function AddServer() {
  const [distro, setDistro] = useState<Distro>('deb')
  const [arch, setArch] = useState<Arch>('amd64')
  const [version, setVersion] = useState(DEFAULT_VERSION)
  const [controlURL, setControlURL] = useState(
    isConfigured() ? `https://${controlPlaneHost()}` : '',
  )

  const install = useMemo(() => installCommand(distro, arch, version), [distro, arch, version])

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Servers', to: '/assets' }, { label: 'Add a server' }]}
        title="Add a server"
        description="Install the Argus agent on the server. It checks in by itself, you approve it here, and then people can connect to it."
      />

      <PageBody>
        <Grid gap="lg">
          <Grid.Col span={{ base: 12, lg: 8 }}>
            <Stack gap={0}>
              <Step n={1} title="Install the agent" detail="Run this on the server you are adding.">
                <Group gap="sm" mb="sm" wrap="wrap">
                  <SegmentedControl
                    size="xs"
                    value={distro}
                    onChange={(v) => setDistro(v as Distro)}
                    data={[
                      { value: 'deb', label: 'Debian / Ubuntu' },
                      { value: 'rpm', label: 'RHEL / Rocky / Alma' },
                      { value: 'apk', label: 'Alpine' },
                    ]}
                    aria-label="Linux distribution"
                  />
                  <SegmentedControl
                    size="xs"
                    value={arch}
                    onChange={(v) => setArch(v as Arch)}
                    data={[
                      { value: 'amd64', label: 'x86-64' },
                      { value: 'arm64', label: 'ARM64' },
                    ]}
                    aria-label="Processor"
                  />
                  <TextInput
                    size="xs"
                    w={120}
                    value={version}
                    onChange={(e) => setVersion(e.currentTarget.value.trim().replace(/^v/, ''))}
                    leftSection={<Text size={FS.meta} c="dimmed">v</Text>}
                    aria-label="Argus version"
                  />
                </Group>
                <Command text={install} />
                <Text size={FS.meta} c="dimmed" mt={SP.snug}>
                  Use the same version as your control plane. If the server cannot reach
                  GitHub, download the package elsewhere and copy it over. Nothing starts
                  on install.
                </Text>
              </Step>

              <Step
                n={2}
                title="Enroll it"
                detail="Gives this server its own credential, so it can report as itself and nothing else."
              >
                <EnrollStep controlURL={controlURL} setControlURL={setControlURL} />
              </Step>

              <Step
                n={3}
                title="Turn on recording and start it"
                detail="Makes sshd hand every login to the agent, including ones that skip Argus."
              >
                <Command text={START_COMMAND} />
                <Text size={FS.meta} c="dimmed" mt={SP.snug} lh={1.55}>
                  Put the gateway's public SSH key in <Mono>/etc/argus/gateway_keys</Mono> so the
                  agent can tell sessions that came through Argus from direct logins.{' '}
                  <Mono>argus-agent scan</Mono> then says whether anything can still get in around it.
                </Text>
              </Step>

              <Step
                n={4}
                title="Approve it when it checks in"
                detail="It appears below within a minute of starting."
                last
              >
                <Waiting />
              </Step>
            </Stack>
          </Grid.Col>

          <Grid.Col span={{ base: 12, lg: 4 }}>
            <Stack gap="lg">
              <HowItWorks />
              <EnrolledAgents />
            </Stack>
          </Grid.Col>
        </Grid>
      </PageBody>
    </Box>
  )
}

/**
 * Step 2: a single-use token, and the command that spends it.
 *
 * This step used to tell the operator to copy the gateways' reporter token onto
 * every server -- one secret, identical everywhere, so root on any server could
 * report as any other. Now each server gets a token that works once, for an
 * hour, and buys a credential that speaks only for that server.
 */
function EnrollStep({
  controlURL, setControlURL,
}: {
  controlURL: string
  setControlURL: (v: string) => void
}) {
  const { data: me } = useQuery(meQuery())
  const canMint = me?.role === 'admin' || me?.role === 'owner'
  const mint = useCreateEnrollToken()
  const [hostname, setHostname] = useState('')
  const [tok, setTok] = useState<EnrollToken | null>(null)

  const make = async () => {
    await run(async () => setTok(await mint.mutateAsync(hostname.trim())), {
      failure: 'No enrollment token was made',
    })
  }

  return (
    <Stack gap="sm">
      <Group gap="md" align="flex-end" wrap="wrap">
        <TextInput
          size="xs"
          label="Control plane address, as the server reaches it"
          placeholder="https://argus-control.example.com:8080"
          value={controlURL}
          onChange={(e) => setControlURL(e.currentTarget.value.trim())}
          style={{ flex: '1 1 280px' }}
        />
        <TextInput
          size="xs"
          label="Only for this hostname (optional)"
          placeholder="Any hostname"
          value={hostname}
          onChange={(e) => setHostname(e.currentTarget.value)}
          style={{ flex: '1 1 180px' }}
        />
        {isConfigured() ? (
          <Tooltip label="Making a token needs the admin or owner role" disabled={canMint}>
            <Button
              size="xs"
              leftSection={<IconKey size={14} />}
              disabled={!canMint}
              loading={mint.isPending}
              onClick={() => void make()}
            >
              {tok ? 'Make another token' : 'Make a token'}
            </Button>
          </Tooltip>
        ) : null}
      </Group>

      {!isConfigured() && (
        <Alert color="amber" icon={<IconInfoCircle size={16} />}>
          <Text size={FS.body} lh={1.5}>
            This console is running on sample data with no control plane, so it cannot make
            an enrollment token. The command below shows its shape.
          </Text>
        </Alert>
      )}

      <Command text={enrollCommand(controlURL, tok)} />

      <Text size={FS.meta} c="dimmed" lh={1.55}>
        {tok ? (
          <>
            The token works once, until {new Date(tok.expiresAt).toLocaleTimeString()}
            {tok.hostname ? `, and only for ${tok.hostname}` : ''}. It is not shown again.
            {!tok.pin &&
              ' This control plane sits behind a TLS proxy, so the server checks its certificate the usual way; add --control-ca if that certificate is from an internal CA.'}
          </>
        ) : (
          <>
            Make a token, then run this on the server. It works once and expires in an hour,
            so it is safe to paste into a terminal.
          </>
        )}{' '}
        To keep it out of shell history, set <Mono>ARGUS_ENROLL_TOKEN</Mono> instead of
        passing <Mono>--token</Mono>.
      </Text>
    </Stack>
  )
}

/**
 * Every server able to write evidence, and the way to stop one.
 *
 * Admin-only on the control plane, so it is only drawn for someone who can
 * read it. Revoked ones stay listed: which credential vouched for a past
 * session is part of reading that session.
 */
function EnrolledAgents() {
  const { data: me } = useQuery(meQuery())
  const isAdmin = me?.role === 'admin' || me?.role === 'owner'
  const { data: agents } = useQuery({ ...enrolledAgentsQuery(), enabled: isConfigured() && isAdmin })
  const revoke = useRevokeAgent()
  const [confirming, setConfirming] = useState<{ id: string; hostname: string } | null>(null)

  if (!isConfigured() || !isAdmin) return null

  const doRevoke = async () => {
    if (!confirming) return
    const ok = await run(() => revoke.mutateAsync(confirming.id), {
      success: ['Revoked', `${confirming.hostname} can no longer report`],
      failure: `${confirming.hostname} was not revoked`,
    })
    if (ok) setConfirming(null)
  }

  return (
    <Card padding="lg">
      <Text component="h2" fw={600} size="md" style={{ margin: 0 }} mb="sm">
        Enrolled servers
      </Text>
      {!agents || agents.length === 0 ? (
        <Text size={FS.body} c="dimmed" lh={1.5}>
          No server has enrolled yet. Servers set up with the gateways' shared reporter token
          still report, but are not listed here; enroll them to give each its own credential.
        </Text>
      ) : (
        <Stack gap="xs">
          {agents.map((a) => (
            <Group key={a.id} justify="space-between" wrap="nowrap" gap="sm">
              <Box style={{ minWidth: 0 }}>
                <Text ff="monospace" size="sm" fw={500} truncate>{a.hostname}</Text>
                <Text size={FS.meta} c="dimmed">
                  {a.revokedAt
                    ? `Revoked ${relTime(a.revokedAt)} by ${a.revokedBy}`
                    : `Enrolled ${relTime(a.enrolledAt)}, token by ${a.enrolledBy}`}
                </Text>
              </Box>
              {a.revokedAt ? (
                <Badge color="slate" size="sm">Revoked</Badge>
              ) : (
                <Button
                  size="xs"
                  variant="subtle"
                  color="red"
                  onClick={() => setConfirming({ id: a.id, hostname: a.hostname })}
                >
                  Revoke
                </Button>
              )}
            </Group>
          ))}
        </Stack>
      )}

      <Modal
        opened={confirming !== null}
        onClose={() => setConfirming(null)}
        title={`Revoke ${confirming?.hostname ?? ''}?`}
      >
        <Text size="sm" c="dimmed" mb="md">
          Its agent keeps recording on the server, but the control plane refuses its reports
          from now on. What it reported before is unchanged. To bring it back, make a new token
          and run <Mono>argus-agent enroll --force</Mono> on it.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" size="sm" onClick={() => setConfirming(null)}>
            Cancel
          </Button>
          <Button color="red" size="sm" loading={revoke.isPending} onClick={() => void doRevoke()}>
            Revoke
          </Button>
        </Group>
      </Modal>
    </Card>
  )
}

/**
 * The answer to "is it automatic or manual?", kept beside the steps.
 */
function HowItWorks() {
  return (
    <Card padding="lg">
      <Text component="h2" fw={600} size="md" style={{ margin: 0 }} mb="sm">
        How adding a server works
      </Text>
      <Stack gap="md">
        <Point icon={IconEye} title="The server finds Argus by itself">
          Once the agent is running it reports to the control plane every 30 seconds, with
          its name, addresses, SSH ports and local accounts. Nobody types those in.
        </Point>
        <Point icon={IconUserCheck} title="A person decides whether to trust it">
          A new server waits here until an admin approves it. Argus never adds one to the
          list on its own, because anything that can install an agent could otherwise add
          itself.
        </Point>
        <Point icon={IconServer2} title="Then you choose who can connect">
          Approving grants nobody access. On the server's page, list the accounts people
          may use; after that it is in Connect for everyone allowed to reach it.
        </Point>
      </Stack>
      <Text size={FS.meta} c="dimmed" mt="md" lh={1.55}>
        A server already in the list attaches to its existing entry when its agent reports,
        and is not shown here.
      </Text>
    </Card>
  )
}

function Point({ icon: Icon, title, children }: { icon: typeof IconEye; title: string; children: ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <ThemeIcon variant="light" color="azure" size={30} radius="md">
        <Icon size={16} />
      </ThemeIcon>
      <Box>
        <Text size="sm" fw={500}>{title}</Text>
        <Text size={FS.body} c="dimmed" lh={1.5}>{children}</Text>
      </Box>
    </Group>
  )
}

/** One step of a real sequence: numbered, joined to the next by a rule. */
function Step({
  n, title, detail, last = false, children,
}: {
  n: number
  title: string
  detail: string
  last?: boolean
  children: ReactNode
}) {
  return (
    <Group gap="md" wrap="nowrap" align="stretch" className="argus-step">
      <Stack gap={0} align="center" style={{ flexShrink: 0 }}>
        <Box className="argus-step-n" aria-hidden>{n}</Box>
        {!last && <Box className="argus-step-rule" />}
      </Stack>
      <Box pb={last ? 0 : 'xl'} style={{ flex: 1, minWidth: 0 }}>
        <Text component="h2" fw={600} size="md" style={{ margin: 0 }} lh="28px">
          <span className="sr-only">Step {n}: </span>
          {title}
        </Text>
        <Text size="sm" c="dimmed" mb="sm">{detail}</Text>
        {children}
      </Box>
    </Group>
  )
}

/** A shell command with a copy button. */
function Command({ text }: { text: string }) {
  return (
    <Box pos="relative">
      <pre className="argus-cmd">{text}</pre>
      <CopyButton value={text} timeout={1600}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? 'Copied' : 'Copy'} position="left">
            <ActionIcon
              variant="default"
              size="md"
              onClick={copy}
              aria-label="Copy command"
              pos="absolute"
              top={8}
              right={8}
            >
              {copied ? <IconCheck size={15} /> : <IconCopy size={15} />}
            </ActionIcon>
          </Tooltip>
        )}
      </CopyButton>
    </Box>
  )
}

/**
 * Servers whose agent has reported and that nobody has decided on yet.
 *
 * Polled faster than the Coverage page polls the same list: someone on this
 * page has usually just started an agent and is watching for it.
 */
function Waiting() {
  const { data: me } = useQuery(meQuery())
  const canApprove = me?.role === 'admin' || me?.role === 'owner'
  const { data: hosts, isPending } = useQuery({
    ...discoveredQuery('unreviewed'),
    refetchInterval: 5_000,
  })
  const [approved, setApproved] = useState<{ hostname: string; assetId: string }[]>([])
  const [ignoring, setIgnoring] = useState<DiscoveredHost | null>(null)
  const [note, setNote] = useState('')
  const enrol = useEnrolHost()
  const ignore = useIgnoreHost()

  if (!isConfigured()) {
    return (
      <Alert color="amber" icon={<IconInfoCircle size={16} />}>
        <Text size={FS.body} lh={1.5}>
          This console is running on sample data with no control plane, so no agent can
          check in to it. Servers appear here only when a real agent reports.
        </Text>
      </Alert>
    )
  }

  const approve = async (h: DiscoveredHost) => {
    let assetId = ''
    const ok = await run(
      async () => {
        assetId = (await enrol.mutateAsync(h.hostname)).assetId
      },
      { failure: `${h.hostname} was not approved` },
    )
    if (ok) setApproved((a) => [...a, { hostname: h.hostname, assetId }])
  }

  const dismiss = async () => {
    if (!ignoring) return
    const ok = await run(() => ignore.mutateAsync({ hostname: ignoring.hostname, note }), {
      failure: `${ignoring.hostname} was not ignored`,
    })
    if (ok) {
      setIgnoring(null)
      setNote('')
    }
  }

  return (
    <Stack gap="sm">
      {approved.map((a) => (
        <Alert
          key={a.hostname}
          color="teal"
          icon={<IconCircleCheck size={16} />}
          title={`${a.hostname} is approved`}
        >
          <Group justify="space-between" gap="sm" wrap="wrap">
            <Text size={FS.body} lh={1.5}>
              Nobody can connect yet. Choose which accounts people may use on it.
            </Text>
            <ButtonLink
              size="xs"
              variant="light"
              color="teal"
              to="/assets/$assetId"
              params={{ assetId: a.assetId }}
            >
              Set up access
            </ButtonLink>
          </Group>
        </Alert>
      ))}

      {isPending ? (
        <Group gap="sm" py="sm">
          <Loader size="xs" />
          <Text size="sm" c="dimmed">Checking for new servers…</Text>
        </Group>
      ) : hosts && hosts.length > 0 ? (
        hosts.map((h) => (
          <Card key={h.hostname} padding="md" withBorder>
            <Group justify="space-between" wrap="wrap" gap="md" align="flex-start">
              <Box style={{ minWidth: 0 }}>
                <Group gap={SP.cozy} wrap="nowrap">
                  <Text ff="monospace" fw={600} size="sm">{h.hostname}</Text>
                  <Badge color="amber" size="sm">Waiting for approval</Badge>
                </Group>
                <Text size={FS.meta} c="dimmed" mt={SP.tight}>
                  {[h.os, h.addresses.join(', '), h.sshPorts.length ? `SSH on ${h.sshPorts.join(', ')}` : '']
                    .filter(Boolean)
                    .join('. ')}
                </Text>
                <Text size={FS.meta} c="dimmed">
                  First checked in {relTime(h.firstSeenAt)}, last {relTime(h.lastSeenAt)}
                  {h.accounts.length > 0 && `. Accounts: ${h.accounts.slice(0, 6).join(', ')}`}
                  {h.accounts.length > 6 && ` and ${h.accounts.length - 6} more`}
                </Text>
              </Box>
              <Group gap="xs" wrap="nowrap">
                <Button
                  size="xs"
                  variant="subtle"
                  color="slate"
                  disabled={!canApprove}
                  onClick={() => setIgnoring(h)}
                >
                  Ignore
                </Button>
                <Tooltip
                  label="Approving a server needs the admin or owner role"
                  disabled={canApprove}
                >
                  <Button
                    size="xs"
                    disabled={!canApprove}
                    loading={enrol.isPending && enrol.variables === h.hostname}
                    onClick={() => void approve(h)}
                  >
                    Approve
                  </Button>
                </Tooltip>
              </Group>
            </Group>
          </Card>
        ))
      ) : (
        <Group gap="sm" py="sm" wrap="nowrap" className="argus-listening">
          <Box className="argus-pulse" aria-hidden />
          <Text size="sm" c="dimmed">
            Listening for new servers. Once the agent starts, it shows up here by itself;
            there is no need to reload.
          </Text>
        </Group>
      )}

      <Modal
        opened={ignoring !== null}
        onClose={() => setIgnoring(null)}
        title={`Ignore ${ignoring?.hostname ?? ''}?`}
      >
        <Text size="sm" c="dimmed" mb="sm">
          It stops being listed as waiting, and nobody can connect to it through Argus.
          Say why, so the next person does not have to find out again. A reason is required.
        </Text>
        <Textarea
          size="sm"
          autosize
          minRows={2}
          placeholder="For example: lab machine, decommissioned next week"
          value={note}
          onChange={(e) => setNote(e.currentTarget.value)}
        />
        <Group justify="flex-end" mt="md">
          <Button variant="default" size="sm" onClick={() => setIgnoring(null)}>
            Cancel
          </Button>
          <Button
            size="sm"
            loading={ignore.isPending}
            disabled={!note.trim()}
            onClick={() => void dismiss()}
          >
            Ignore server
          </Button>
        </Group>
      </Modal>
    </Stack>
  )
}

/**
 * Package file names follow GoReleaser's conventional naming for each format,
 * which is what .goreleaser.yaml produces when it sets no file_name_template.
 */
function installCommand(distro: Distro, arch: Arch, version: string): string {
  const v = version || DEFAULT_VERSION
  const base = `https://github.com/gsoultan/argus/releases/download/v${v}`
  if (distro === 'deb') {
    const f = `argus-agent_${v}_${arch}.deb`
    return `curl -fsSLO ${base}/${f}\nsudo dpkg -i ${f}`
  }
  if (distro === 'rpm') {
    const f = `argus-agent-${v}-1.${arch === 'amd64' ? 'x86_64' : 'aarch64'}.rpm`
    return `curl -fsSLO ${base}/${f}\nsudo rpm -i ${f}`
  }
  const f = `argus-agent_${v}_${arch === 'amd64' ? 'x86_64' : 'aarch64'}.apk`
  return `curl -fsSLO ${base}/${f}\nsudo apk add --allow-untrusted ${f}`
}

/**
 * The one command that enrolls a server. The pin lets the agent check it is
 * talking to this control plane before it has a CA to check with.
 */
function enrollCommand(controlURL: string, tok: EnrollToken | null): string {
  const url = controlURL || 'https://argus-control.example.com:8080'
  const pin = tok?.pin ? ` \\\n    --pin ${tok.pin}` : ''
  return `sudo argus-agent enroll \\
    --control-url ${url}${pin} \\
    --token ${tok?.token ?? 'MAKE-A-TOKEN-ABOVE'}`
}

/**
 * The agent first, then sshd. Once sshd_config carries the ForceCommand, every
 * login goes through the agent's shim, and the shim refuses a session when the
 * agent is not running. Reversing these would lock out the person typing them.
 */
const START_COMMAND = `sudo systemctl daemon-reload
sudo systemctl enable --now argus-agent
sudo argus-agent install --shim /usr/bin/argus-agent | sudo tee -a /etc/ssh/sshd_config >/dev/null
sudo sshd -t && sudo systemctl reload sshd`
