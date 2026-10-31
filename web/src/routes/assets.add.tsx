import {
  ActionIcon, Alert, Anchor, Badge, Box, Button, Checkbox, CopyButton, Group, Loader, Modal,
  SegmentedControl, Stack, Switch, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconBrandUbuntu, IconBrandWindows, IconCheck, IconCircleCheck, IconCopy, IconInfoCircle,
  IconPlus,
} from '@tabler/icons-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { PageBody, PageHeader } from '~/components/page'
import { controlPlaneVersion, fleetURL, installOneLiner, isConfigured, type EnrollToken } from '~/lib/live'
import { notifyError, notifyOk, run } from '~/lib/notify'
import {
  discoveredQuery, meQuery, teamsQuery, useCreateEnrollToken, useEnrolHost, useIgnoreHost, usersQuery,
} from '~/lib/queries'
import { guard } from '~/lib/guard'
import { isElevatedAccount } from '~/lib/roles'
import type { DiscoveredHost } from '~/types/domain'
import { FS, SP } from '~/theme'

/**
 * Adding a server, in three steps on one page: name it, install the agent
 * with one line, then say who can use it.
 *
 * Argus never trusts a server by itself. The agent reports in on its own, and
 * the server is usable only once an admin finishes step 3 here, which is the
 * approval. "Skip, only admins for now" approves it too, with nobody but
 * admins able to connect. A server whose page was closed before step 3 waits
 * at the top of Servers with Finish adding, which lands back on step 3.
 *
 * Nothing here is invented. With no control plane there is nothing for an
 * agent to report to, so step 2 says so instead of pretending one arrived.
 */
export const Route = createFileRoute('/assets/add')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: (s: Record<string, unknown>): { host?: string } =>
    typeof s.host === 'string' && s.host ? { host: s.host } : {},
  component: AddServer,
})

/**
 * The agent should match the control plane it reports to. A packaged control
 * plane says which release it is; a development one does not, and gets the
 * first release.
 */
const VERSION = controlPlaneVersion() ?? '0.1.0'

type Kind = 'linux' | 'windows'
type Step = 1 | 2 | 3

function AddServer() {
  const { host: finishing } = Route.useSearch()
  const [step, setStep] = useState<Step>(finishing ? 3 : 1)
  const [name, setName] = useState(finishing ?? '')
  const [kind, setKind] = useState<Kind>('linux')
  const [token, setToken] = useState<EnrollToken | null>(null)
  const [mintedAt, setMintedAt] = useState(0)
  const [found, setFound] = useState<string | null>(finishing ?? null)
  const mint = useCreateEnrollToken()

  const toInstall = async () => {
    if (!isConfigured()) {
      setStep(2)
      return
    }
    // Not bound to the name: the agent reports the hostname the server has,
    // and a token that only works for an exact match would fail on a typo.
    const ok = await run(async () => {
      setToken(await mint.mutateAsync(''))
      setMintedAt(Date.now())
    }, { failure: 'Could not start adding the server' })
    if (ok) setStep(2)
  }

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Servers', to: '/assets' }, { label: 'Add server' }]}
        title="Add server"
      />
      <PageBody>
        <Box maw={720} w="100%" mx="auto">
          <Stepper step={step} />
          <Box className="argus-panel" p={{ base: 'md', sm: 'xl' }} mt="lg">
            {step === 1 && (
              <NameStep
                name={name}
                kind={kind}
                onName={setName}
                onKind={setKind}
                busy={mint.isPending}
                onNext={() => void toInstall()}
              />
            )}
            {step === 2 && (
              <InstallStep
                name={name}
                kind={kind}
                token={token}
                since={mintedAt}
                onBack={() => setStep(1)}
                onFound={(h) => {
                  setFound(h)
                  setStep(3)
                }}
              />
            )}
            {step === 3 && found && <AccessStep hostname={found} />}
          </Box>
        </Box>
      </PageBody>
    </Box>
  )
}

const STEPS = ['Name', 'Install', 'Who can use it'] as const

function Stepper({ step }: { step: Step }) {
  return (
    <Group gap="xs" justify="center" wrap="nowrap" aria-label="Steps">
      {STEPS.map((label, i) => {
        const n = (i + 1) as Step
        const state = n < step ? 'done' : n === step ? 'current' : 'todo'
        return (
          <Group key={label} gap="xs" wrap="nowrap">
            {i > 0 && <Box w={{ base: 16, sm: 40 }} h={2} bg={n <= step ? 'azure.6' : 'slate.2'} />}
            <Box className="argus-stepper-dot" data-state={state} aria-current={state === 'current' ? 'step' : undefined}>
              {state === 'done' ? <IconCheck size={14} /> : n}
            </Box>
            <Text size="sm" fw={state === 'current' ? 600 : 500} c={state === 'todo' ? 'dimmed' : undefined} visibleFrom="xs">
              {label}
            </Text>
          </Group>
        )
      })}
    </Group>
  )
}

function StepTitle({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <Box mb="lg">
      <Text component="h2" fw={600} size="lg" style={{ margin: 0 }}>{title}</Text>
      {children && <Text size="sm" c="dimmed" mt={SP.tight} lh={1.5}>{children}</Text>}
    </Box>
  )
}

/* ── Step 1 ────────────────────────────────────────────────────────────── */

function NameStep({ name, kind, onName, onKind, busy, onNext }: {
  name: string
  kind: Kind
  onName: (v: string) => void
  onKind: (v: Kind) => void
  busy: boolean
  onNext: () => void
}) {
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        onNext()
      }}
    >
      <StepTitle title="Which server?" />
      <Stack gap="lg">
        <TextInput
          size="md"
          label="Name"
          description="So you can tell it apart, such as web-01. Argus shows the name the server reports for itself."
          placeholder="web-01"
          data-autofocus
          value={name}
          onChange={(e) => onName(e.currentTarget.value)}
        />
        <Box>
          <Text size="sm" fw={500} mb={SP.snug}>It runs</Text>
          <SegmentedControl
            size="md"
            fullWidth
            value={kind}
            onChange={(v) => onKind(v as Kind)}
            data={[
              { value: 'linux', label: <Group gap="xs" justify="center" wrap="nowrap"><IconBrandUbuntu size={18} />Linux</Group> },
              { value: 'windows', label: <Group gap="xs" justify="center" wrap="nowrap"><IconBrandWindows size={18} />Windows</Group> },
            ]}
            aria-label="Operating system"
          />
        </Box>
        <Group justify="flex-end">
          <Button size="md" type="submit" loading={busy}>Next: install</Button>
        </Group>
      </Stack>
    </form>
  )
}

/* ── Step 2 ────────────────────────────────────────────────────────────── */

function InstallStep({ name, kind, token, since, onBack, onFound }: {
  name: string
  kind: Kind
  token: EnrollToken | null
  since: number
  onBack: () => void
  onFound: (hostname: string) => void
}) {
  const command = token
    ? kind === 'linux'
      ? installOneLiner(token, VERSION)
      : windowsCommand(token)
    : kind === 'linux'
      ? `curl -fsSL '${fleetURL() || 'https://argus.example.com'}/api/v1/install/agent.sh?token=…' | sudo sh`
      : windowsCommand(null)

  return (
    <Box>
      <StepTitle title={`Install Argus on ${name.trim() || 'the server'}`}>
        {kind === 'linux'
          ? 'Copy this line, paste it into a terminal on the server, and press Enter. It installs the agent, links it to Argus and starts it.'
          : 'Copy this, paste it into PowerShell opened with Run as administrator on the server, and press Enter.'}
      </StepTitle>

      <Command text={command} />

      <Text size={FS.meta} c="dimmed" mt="sm" lh={1.5}>
        {token
          ? `It works once, until ${new Date(token.expiresAt).toLocaleTimeString()}. If it runs out, go back and press Next again.`
          : 'This console has no control plane behind it, so the line above is only an example.'}
        {kind === 'linux' && ' Your current ssh session stays open; if anything goes wrong it puts the ssh settings back as they were.'}
      </Text>

      <Box mt="xl">
        {isConfigured() ? (
          <WaitForServer name={name} since={since} onFound={onFound} />
        ) : (
          <Alert color="amber" icon={<IconInfoCircle size={16} />}>
            No agent can check in to sample data. On a real Argus, this page moves on by itself
            when the server reports in.
          </Alert>
        )}
      </Box>

      <Group justify="space-between" mt="xl">
        <Button variant="default" onClick={onBack}>Back</Button>
      </Group>
    </Box>
  )
}

/**
 * Watches for the server. The one wanted is the one with the name typed in
 * step 1; failing that, the first server that checked in after the command
 * was made, which is almost always the one just installed.
 */
function WaitForServer({ name, since, onFound }: {
  name: string
  since: number
  onFound: (hostname: string) => void
}) {
  const { data: hosts } = useQuery({ ...discoveredQuery('unreviewed'), refetchInterval: 3_000 })
  const want = name.trim().toLowerCase()
  const match = useMemo(() => {
    const list = hosts ?? []
    const byName = want
      ? list.find((h) => h.hostname.toLowerCase() === want || h.hostname.toLowerCase().split('.')[0] === want)
      : undefined
    // A minute of slack for a server whose clock is behind ours.
    return byName ?? list.find((h) => Date.parse(h.firstSeenAt) >= since - 60_000)
  }, [hosts, want, since])

  return match ? (
    <Alert color="teal" icon={<IconCircleCheck size={18} />} title={`${match.hostname} is connected`}>
      <Text size="sm" mb="md">The agent is running. Last step: say who can use it.</Text>
      <Button size="md" onClick={() => onFound(match.hostname)}>Next: who can use it</Button>
    </Alert>
  ) : (
    <Group gap="sm" wrap="nowrap" className="argus-listening">
      <Box className="argus-pulse" aria-hidden />
      <Text size="sm" c="dimmed">
        Waiting for the server. This moves on by itself, usually within a minute of running the line.
      </Text>
    </Group>
  )
}

/** A command with a copy button, on the dark surface the server's own screen uses. */
function Command({ text }: { text: string }) {
  return (
    <Box pos="relative">
      <pre className="argus-cmd" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{text}</pre>
      <CopyButton value={text} timeout={1600}>
        {({ copied, copy }) => (
          <Tooltip label={copied ? 'Copied' : 'Copy'} position="left">
            <ActionIcon
              variant="filled"
              color={copied ? 'teal' : 'azure'}
              size="lg"
              onClick={copy}
              aria-label="Copy command"
              pos="absolute"
              top={8}
              right={8}
            >
              {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
            </ActionIcon>
          </Tooltip>
        )}
      </CopyButton>
    </Box>
  )
}

/**
 * Windows has no package manager to lean on, so the block downloads the
 * release archive, links the agent and installs its service. Pinning the
 * control plane's key happens in the agent, from --pin.
 */
function windowsCommand(t: EnrollToken | null): string {
  const url = fleetURL() || 'https://argus.example.com'
  const pin = t?.pin ? ` --pin ${t.pin}` : ''
  return [
    `$v='${VERSION}'; $d='C:\\Program Files\\Argus'; $t=Join-Path $env:TEMP 'argus'`,
    `New-Item -ItemType Directory -Force $d, $t | Out-Null`,
    `Invoke-WebRequest "https://github.com/gsoultan/argus/releases/download/v$v/argus_$($v)_windows_amd64.zip" -OutFile "$t\\argus.zip"`,
    `Expand-Archive "$t\\argus.zip" $t -Force; Copy-Item "$t\\argus-agent.exe" $d -Force`,
    `& "$d\\argus-agent.exe" enroll --control-url ${url}${pin} --token ${t?.token ?? '…'}`,
    `& "$d\\argus-agent.exe" service install`,
  ].join('\n')
}

/* ── Step 3 ────────────────────────────────────────────────────────────── */

function AccessStep({ hostname }: { hostname: string }) {
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  const { data: hosts, isPending } = useQuery(discoveredQuery('unreviewed'))
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const enrol = useEnrolHost()
  const host: DiscoveredHost | undefined = hosts?.find((h) => h.hostname === hostname)

  const [accounts, setAccounts] = useState<string[]>([])
  const [picked, setPicked] = useState<string[]>([])
  const [extra, setExtra] = useState('')
  const [pickedTeams, setPickedTeams] = useState<string[]>([])
  const [pickedPeople, setPickedPeople] = useState<string[]>([])
  const [personal, setPersonal] = useState(true)
  const [ignoring, setIgnoring] = useState(false)

  // Offer what the server reported, with the first ordinary account ticked:
  // on most servers that is the one the person who set it up logs in as.
  useEffect(() => {
    const reported = host?.accounts ?? []
    setAccounts(reported)
    const first = reported.find((a) => !isElevatedAccount(a))
    setPicked(first ? [first] : [])
  }, [host])

  const people = (users ?? []).filter(
    (u) => !u.disabled && u.role !== 'admin' && u.role !== 'owner' && u.email !== me?.email,
  )

  const addExtra = () => {
    const n = extra.trim()
    if (!n) return
    if (!accounts.includes(n)) setAccounts((a) => [...a, n])
    if (!picked.includes(n)) setPicked((p) => [...p, n])
    setExtra('')
  }

  const finish = async (giveAccess: boolean) => {
    try {
      const res = await enrol.mutateAsync({
        hostname,
        grant: {
          principals: picked,
          people: giveAccess ? pickedPeople : [],
          teams: giveAccess ? pickedTeams : [],
          personalAccounts: giveAccess && pickedTeams.length > 0 && personal,
        },
      })
      notifyOk('Server added', giveAccess && (pickedTeams.length || pickedPeople.length)
        ? `${res.assetHostname} is ready, and the people you picked can connect.`
        : `${res.assetHostname} is ready. Only admins can connect for now.`)
      void navigate({ to: '/assets/$assetId', params: { assetId: res.assetId } })
    } catch (err) {
      notifyError('Server not added', err)
    }
  }

  if (isPending) {
    return <Group gap="sm"><Loader size="sm" /><Text size="sm" c="dimmed">Loading…</Text></Group>
  }
  if (!host) {
    return (
      <Alert color="amber" icon={<IconInfoCircle size={16} />} title={`${hostname} is not waiting any more`}>
        It may already be added, or someone ignored it. <Anchor href="/assets">Back to Servers</Anchor>
      </Alert>
    )
  }

  const choosing = pickedTeams.length + pickedPeople.length
  // Teams with their own accounts need no shared one; people picked one by
  // one still do.
  const ownOnly = personal && pickedTeams.length > 0 && pickedPeople.length === 0
  const needShared = picked.length === 0 && !ownOnly

  return (
    <Box>
      <StepTitle title={`Who can use ${hostname}?`}>
        Admins can always connect. Pick the teams or people who should too. You can change this
        later on the server&rsquo;s page.
      </StepTitle>

      <Stack gap="xl">
        <Box>
          <Text size="sm" fw={600} mb={SP.tight}>Log in as</Text>
          <Text size={FS.meta} c="dimmed" mb="sm">The accounts on the server people may use.</Text>
          <Group gap="xs">
            {accounts.map((a) => (
              <Checkbox.Card
                key={a}
                className="argus-pick"
                data-checked={picked.includes(a) || undefined}
                checked={picked.includes(a)}
                onClick={() => setPicked((p) => (p.includes(a) ? p.filter((x) => x !== a) : [...p, a]))}
                w="auto"
                style={{ minHeight: 44, padding: '8px 12px' }}
              >
                <Group gap="xs" wrap="nowrap">
                  <Checkbox.Indicator />
                  <Text size="sm" ff="monospace">{a}</Text>
                  {isElevatedAccount(a) && <Badge size="xs" color="rose">full control</Badge>}
                </Group>
              </Checkbox.Card>
            ))}
            <TextInput
              w={180}
              placeholder="Another account"
              value={extra}
              onChange={(e) => setExtra(e.currentTarget.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  addExtra()
                }
              }}
              rightSection={
                <ActionIcon variant="subtle" onClick={addExtra} aria-label="Add account" disabled={!extra.trim()}>
                  <IconPlus size={16} />
                </ActionIcon>
              }
            />
          </Group>
          {needShared && (
            <Text size={FS.meta} c="rose.7" mt="xs">Pick at least one, or nobody can log in.</Text>
          )}
        </Box>

        <PickList
          title="Teams"
          empty="No teams yet. You can make them on Teams and give them this server later."
          items={(teams ?? []).map((t) => ({
            value: t.id,
            label: t.name,
            sub: `${t.members.length} ${t.members.length === 1 ? 'person' : 'people'}`,
          }))}
          value={pickedTeams}
          onChange={setPickedTeams}
        />
        {pickedTeams.length > 0 && (
          <Switch
            checked={personal}
            onChange={(e) => setPersonal(e.currentTarget.checked)}
            label="Give each person in these teams their own account"
            description={`Argus makes a login on ${hostname} for each of them, named after them, and locks it when they leave the team. Sudo and SSH straight to the server can be turned on later on the server's page.`}
          />
        )}

        <PickList
          title="People"
          empty="Nobody else yet. Add people on Teams, under People."
          items={people.map((u) => ({ value: u.email, label: u.displayName, sub: u.email }))}
          value={pickedPeople}
          onChange={setPickedPeople}
        />

        <Group justify="space-between" wrap="wrap" gap="sm">
          <Button variant="subtle" color="slate" onClick={() => setIgnoring(true)}>
            Not mine, ignore it
          </Button>
          <Group gap="sm">
            <Button
              size="md"
              variant="default"
              disabled={picked.length === 0}
              loading={enrol.isPending && choosing === 0}
              onClick={() => void finish(false)}
            >
              Skip, only admins for now
            </Button>
            <Button
              size="md"
              disabled={needShared || choosing === 0}
              loading={enrol.isPending && choosing > 0}
              onClick={() => void finish(true)}
            >
              Give access
            </Button>
          </Group>
        </Group>
      </Stack>

      <IgnoreModal hostname={hostname} opened={ignoring} onClose={() => setIgnoring(false)} />
    </Box>
  )
}

function PickList({ title, empty, items, value, onChange }: {
  title: string
  empty: string
  items: { value: string; label: string; sub: string }[]
  value: string[]
  onChange: (v: string[]) => void
}) {
  return (
    <Box>
      <Text size="sm" fw={600} mb="sm">{title}</Text>
      {items.length === 0 ? (
        <Text size={FS.meta} c="dimmed">{empty}</Text>
      ) : (
        <Stack gap="xs">
          {items.map((i) => {
            const on = value.includes(i.value)
            return (
              <Checkbox.Card
                key={i.value}
                className="argus-pick"
                data-checked={on || undefined}
                checked={on}
                onClick={() => onChange(on ? value.filter((x) => x !== i.value) : [...value, i.value])}
              >
                <Group gap="sm" wrap="nowrap">
                  <Checkbox.Indicator />
                  <Box style={{ minWidth: 0 }}>
                    <Text size="sm" fw={500} truncate>{i.label}</Text>
                    <Text size={FS.meta} c="dimmed" truncate>{i.sub}</Text>
                  </Box>
                </Group>
              </Checkbox.Card>
            )
          })}
        </Stack>
      )}
    </Box>
  )
}

function IgnoreModal({ hostname, opened, onClose }: { hostname: string; opened: boolean; onClose: () => void }) {
  const navigate = useNavigate()
  const ignore = useIgnoreHost()
  const [note, setNote] = useState('')
  return (
    <Modal opened={opened} onClose={onClose} title={`Ignore ${hostname}?`}>
      <Text size="sm" c="dimmed" mb="sm">
        It stops waiting here and nobody can connect to it through Argus. Say why, so the next
        person does not have to find out again.
      </Text>
      <Textarea
        autosize
        minRows={2}
        placeholder="For example: lab machine, going away next week"
        value={note}
        onChange={(e) => setNote(e.currentTarget.value)}
      />
      <Group justify="flex-end" mt="md">
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button
          loading={ignore.isPending}
          disabled={!note.trim()}
          onClick={() =>
            void run(() => ignore.mutateAsync({ hostname, note }), {
              failure: `${hostname} was not ignored`,
              success: ['Ignored', `${hostname} is no longer waiting.`],
            }).then((ok) => {
              if (ok) void navigate({ to: '/assets' })
            })
          }
        >
          Ignore server
        </Button>
      </Group>
    </Modal>
  )
}
