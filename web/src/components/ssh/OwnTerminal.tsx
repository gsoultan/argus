import './ssh.css'
import { useEffect, useState, type ReactNode } from 'react'
import {
  Alert, Anchor, Badge, Box, Button, Code, CopyButton, Group, SegmentedControl, Stack, Text, Textarea, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconCheck, IconCopy, IconDeviceLaptop, IconDownload, IconHourglassHigh, IconKey, IconLock,
} from '@tabler/icons-react'
import { downloadText } from '~/lib/download'
import { notifyError, notifyOk } from '~/lib/notify'
import { assetsQuery, directLoginsQuery, myServerAccountQuery, myServersQuery, useSetMySshKey } from '~/lib/queries'
import { shortName } from '~/lib/reach'
import { canGenerateSshKey, generateSshKey } from '~/lib/sshKeygen'
import { directCommand, KEY_FILE, shortCommand, sshConfig, type SshTarget } from '~/lib/sshConfig'
import type { Asset, MyServer } from '~/types/domain'
import { MySshKeyModal } from '~/components/MySshKey'
import { FS, INK } from '~/theme'

/**
 * The second way onto a server: the person's own ssh client, as their own
 * account. What it shows depends on what is true for them here -- no account,
 * browser only, a key still to set up, held for approval each time, or
 * ready -- and every state says what to do next.
 */

export type OwnState = 'loading' | 'no-account' | 'off' | 'no-key' | 'approve' | 'ready' | 'unavailable'

export function useOwnTerminal(hostname: string | undefined) {
  const { data: account, error: accountError } = useQuery({ ...myServerAccountQuery(), retry: false })
  const { data: mine, error: mineError } = useQuery({ ...myServersQuery(), retry: false })
  const here: MyServer | undefined = mine?.find((m) => m.hostname === hostname)
  let state: OwnState
  if (accountError || mineError) state = 'unavailable'
  else if (!account || !mine) state = 'loading'
  else if (!here?.personalAccount || !account.loginName) state = 'no-account'
  else if (here.directSsh === 'off') state = 'off'
  else if (!account.sshKey) state = 'no-key'
  else state = here.directSsh === 'approve' ? 'approve' : 'ready'
  return { state, account, here, mine }
}

/** The servers this person may reach with their own client, with addresses. */
export function directTargets(mine: MyServer[] | undefined, assets: Asset[] | undefined): SshTarget[] {
  if (!mine || !assets) return []
  const ok = new Set(mine.filter((m) => m.personalAccount && m.directSsh !== 'off').map((m) => m.hostname))
  return assets.filter((a) => ok.has(a.hostname) && a.protocol !== 'rdp')
    .map((a) => ({ hostname: a.hostname, address: a.address }))
}

export function DownloadSshConfigButton({ variant = 'default', size }: { variant?: 'default' | 'light'; size?: 'xs' | 'sm' }) {
  const { data: account } = useQuery({ ...myServerAccountQuery(), retry: false })
  const { data: mine } = useQuery({ ...myServersQuery(), retry: false })
  const { data: assets } = useQuery(assetsQuery({}))
  const targets = directTargets(mine, assets)
  if (!account?.loginName || targets.length === 0) return null
  return (
    <Button variant={variant} size={size} leftSection={<IconDownload size={16} />}
      onClick={() => {
        downloadText(sshConfig(account.loginName, targets), 'argus-ssh-config.txt')
        notifyOk('SSH config downloaded', `Add it to ~/.ssh/config, then run "ssh <server>" for any of your ${targets.length} servers.`)
      }}>
      Download SSH config
    </Button>
  )
}

export function OwnTerminalCard({ asset, selected = false }: { asset: Asset; selected?: boolean }) {
  const { state, account, here } = useOwnTerminal(asset.hostname)
  const name = shortName(asset.hostname)
  const target: SshTarget = { hostname: asset.hostname, address: asset.address }
  const login = account?.loginName ?? 'you'
  const [keyOpen, setKeyOpen] = useState(false)

  const badge: Record<OwnState, ReactNode> = {
    loading: null,
    unavailable: null,
    'no-account': <Badge color="slate" variant="light">Not set up</Badge>,
    off: <Badge color="slate" variant="light">Browser only</Badge>,
    'no-key': <Badge color="slate" variant="light">Set up once</Badge>,
    approve: <Badge color="amber" variant="light">Approved each time</Badge>,
    ready: <Badge color="teal" variant="light" leftSection={<Dot />}>Ready</Badge>,
  }

  return (
    <Box className="argus-way" data-selected={selected || undefined}>
      <Group gap="md" align="flex-start" wrap="nowrap">
        <ThemeIcon size={44} radius="md" variant="light" color="violet"><IconDeviceLaptop size={22} /></ThemeIcon>
        <Box style={{ minWidth: 0 }}>
          <Group gap="xs"><Text component="h2" fw={650} size="lg" m={0}>From your own terminal</Text>{badge[state]}</Group>
          <Text size={FS.body} c="dimmed" mt={4} lh={1.5}>
            {state === 'no-key'
              ? `You may SSH to ${name} directly. Do this once and it works for every server that allows it.`
              : state === 'approve'
                ? `On ${name} an approver lets each direct login in. Your terminal waits up to two minutes, then refuses.`
                : 'Use ssh, VS Code Remote, scp or rsync from your computer, as your own account.'}
          </Text>
        </Box>
      </Group>

      {state === 'loading' && <Text size={FS.meta} c="dimmed">Checking what you may do here…</Text>}

      {state === 'unavailable' && (
        <Text size={FS.body} c="dimmed" lh={1.5}>
          This sign-in has no account of its own on servers, so only the browser can open {name}.
        </Text>
      )}

      {state === 'no-account' && (
        <Explain icon={<IconLock size={16} />}>
          You have no account of your own on {name}, only the shared ones in the browser. An admin can give your
          team personal accounts on it under the server&rsquo;s Access tab.
        </Explain>
      )}

      {state === 'off' && (
        <Explain icon={<IconLock size={16} />}>
          Your team reaches {name} through the browser only. You have an account of your own
          (<Text span ff="monospace" inherit>{login}</Text>) but direct SSH is off for it. An admin can turn it on
          under the server&rsquo;s Access tab.
        </Explain>
      )}

      {state === 'no-key' && account && <KeySteps login={login} target={target} />}

      {(state === 'ready' || state === 'approve') && account && (
        <Stack gap="md">
          <Box>
            <Text size="sm" fw={600} mb={8}>Run this on your computer</Text>
            <Command text={shortCommand(target)} />
            <Text size={FS.meta} c="dimmed" mt={8}>Works once your SSH config has the Argus entries. Without it:</Text>
            <Command text={directCommand(login, target)} small />
          </Box>
          <Stack gap={8}>
            <Fact ok>
              Logs you in as <Text span ff="monospace" fw={600} inherit>{login}</Text>, your own account
              {here?.sudo ? ', with sudo' : ''}
            </Fact>
            <Fact ok>
              Your key is saved <Text span ff="monospace" size={FS.micro} c="dimmed">{account.sshFingerprint}</Text>
              {' · '}<Anchor component="button" size={FS.body} c={INK.brand} onClick={() => setKeyOpen(true)}>Change key</Anchor>
            </Fact>
            {state === 'approve'
              ? <Fact warn>Waits at the terminal until someone approves it under Approvals</Fact>
              : <Fact ok>Opens straight away. Recorded like a browser session.</Fact>}
          </Stack>
          <Group gap="sm">
            <DownloadSshConfigButton />
            <Text size={FS.micro} c="dimmed">Every server you may SSH to, in one file</Text>
          </Group>
          <MySshKeyModal opened={keyOpen} onClose={() => setKeyOpen(false)} />
        </Stack>
      )}
    </Box>
  )
}

function Dot() {
  return <Box w={6} h={6} style={{ borderRadius: '50%', background: 'currentColor' }} />
}

function Explain({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <Group gap="sm" align="flex-start" wrap="nowrap" className="argus-way-note">
      <Box c="dimmed" mt={2}>{icon}</Box>
      <Text size={FS.body} lh={1.55}>{children}</Text>
    </Group>
  )
}

function Fact({ ok, warn, children }: { ok?: boolean; warn?: boolean; children: ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      {warn ? <IconAlertTriangle size={16} color={INK.pending} style={{ flexShrink: 0, marginTop: 1 }} />
        : ok ? <IconCheck size={16} color={INK.verified} style={{ flexShrink: 0, marginTop: 1 }} /> : null}
      <Text size={FS.body} lh={1.45}>{children}</Text>
    </Group>
  )
}

/** A command on a dark strip with a copy button: what people paste into a terminal. */
export function Command({ text, small = false }: { text: string; small?: boolean }) {
  return (
    <Group className="argus-cmd" data-small={small || undefined} gap="sm" wrap="nowrap" mt={small ? 6 : 0}>
      {!text.includes('\n') && <Text span className="argus-cmd-prompt" aria-hidden>$</Text>}
      <Code block={text.includes('\n')} className="argus-cmd-text">{text}</Code>
      <CopyButton value={text}>
        {({ copied, copy }) => (
          <Button size="compact-xs" variant="subtle" className="argus-cmd-copy" onClick={copy}
            leftSection={copied ? <IconCheck size={12} /> : <IconCopy size={12} />}>
            {copied ? 'Copied' : 'Copy'}
          </Button>
        )}
      </CopyButton>
    </Group>
  )
}

/** First time: get a key, put it in place, connect. */
function KeySteps({ login, target }: { login: string; target: SshTarget }) {
  const setKey = useSetMySshKey()
  const [made, setMade] = useState<{ privateKey: string; fingerprint: string } | null>(null)
  const [pasting, setPasting] = useState(!canGenerateSshKey())
  const [pasted, setPasted] = useState('')
  const [busy, setBusy] = useState(false)
  const [os, setOs] = useState(() => (/Windows/i.test(navigator.userAgent) ? 'windows' : 'unix'))

  const generate = async () => {
    setBusy(true)
    try {
      const k = await generateSshKey(`${login}@argus`)
      const out = await setKey.mutateAsync(k.publicKey)
      setMade({ privateKey: k.privateKey, fingerprint: out.sshFingerprint })
      saveKey(k.privateKey)
    } catch (err) {
      notifyError('No key made', err)
    } finally {
      setBusy(false)
    }
  }
  const savePasted = async () => {
    try {
      await setKey.mutateAsync(pasted.trim())
      notifyOk('SSH key saved', 'Servers you may SSH to get it within a minute.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }

  const moveCmd = os === 'windows'
    ? `Move-Item $HOME\\Downloads\\${KEY_FILE} $HOME\\.ssh\\\nicacls $HOME\\.ssh\\${KEY_FILE} /inheritance:r /grant:r "$($env:USERNAME):R"`
    : `mv ~/Downloads/${KEY_FILE} ~/.ssh/\nchmod 600 ~/.ssh/${KEY_FILE}`

  return (
    <Stack gap="lg">
      <Step n={1} done={!!made} title="Get a key"
        sub={made ? `Saved. Your browser downloaded the private key as ${KEY_FILE}; it is the only copy.`
          : 'Your browser makes it. Argus keeps only the public half; the private key downloads once.'}>
        {made ? (
          <Group gap="sm">
            <Text size={FS.micro} ff="monospace" c="dimmed">{made.fingerprint}</Text>
            <Anchor size={FS.meta} component="button" onClick={() => saveKey(made.privateKey)}>Download again</Anchor>
          </Group>
        ) : pasting ? (
          <Stack gap="sm">
            <Textarea autosize minRows={3} placeholder="ssh-ed25519 AAAA… you@laptop" value={pasted}
              onChange={(e) => setPasted(e.currentTarget.value)}
              description="The contents of ~/.ssh/id_ed25519.pub. Never paste a private key."
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }} />
            <Group gap="sm">
              <Button loading={setKey.isPending} disabled={!pasted.trim()} onClick={() => void savePasted()}>Save key</Button>
              {canGenerateSshKey() && <Button variant="subtle" onClick={() => setPasting(false)}>Make one for me instead</Button>}
            </Group>
          </Stack>
        ) : (
          <Group gap="sm">
            <Button leftSection={<IconKey size={16} />} loading={busy} onClick={() => void generate()}>Make a key for me</Button>
            <Button variant="subtle" onClick={() => setPasting(true)}>I have a key, paste it</Button>
          </Group>
        )}
      </Step>
      {!pasting && (
        <Step n={2} title="Put it in place" muted={!made}>
          <SegmentedControl size="xs" mb={8} value={os} onChange={setOs}
            data={[{ value: 'unix', label: 'macOS / Linux' }, { value: 'windows', label: 'Windows' }]} />
          <Command text={moveCmd} small />
        </Step>
      )}
      <Step n={pasting ? 2 : 3} title="Connect" muted={!made && !pasting}
        sub="Servers pick up a new key within a minute.">
        <Command text={pasting ? `ssh ${login}@${target.address || target.hostname}` : directCommand(login, target)} small />
      </Step>
      {made && (
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
          Lose the private key file and you make a new one here. Argus cannot give it back.
        </Alert>
      )}
    </Stack>
  )
}

function Step({ n, title, sub, done, muted, children }: {
  n: number
  title: string
  sub?: string
  done?: boolean
  muted?: boolean
  children?: ReactNode
}) {
  return (
    <Group gap="md" align="flex-start" wrap="nowrap">
      <ThemeIcon size={24} radius="xl" color={done ? 'teal' : 'azure'} variant={muted ? 'outline' : 'filled'}>
        {done ? <IconCheck size={14} /> : <Text size="xs" fw={700}>{n}</Text>}
      </ThemeIcon>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size="sm" fw={600}>{title}</Text>
        {sub && <Text size={FS.meta} c="dimmed" mt={2} lh={1.45}>{sub}</Text>}
        {children && <Box mt={8}>{children}</Box>}
      </Box>
    </Group>
  )
}

function saveKey(privateKey: string) {
  const url = URL.createObjectURL(new Blob([privateKey], { type: 'application/octet-stream' }))
  const a = document.createElement('a')
  a.href = url
  a.download = KEY_FILE
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

/**
 * Your own terminal is sitting at "Waiting for approval" on this server: say
 * so here, with how long it has left, so the person is not left guessing.
 */
export function PendingDirectLogin({ hostname, login }: { hostname: string; login: string | undefined }) {
  const { data } = useQuery({ ...directLoginsQuery(), retry: false, enabled: !!login })
  const [now, setNow] = useState(() => Date.now())
  const pending = (data ?? []).filter((d) => d.state === 'pending' && d.principal === login
    && shortName(d.hostname) === shortName(hostname) && Date.parse(d.expiresAt) > now)
  const any = pending.length > 0
  useEffect(() => {
    if (!any) return
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [any])
  const d = pending[0]
  if (!d) return null
  const left = Math.max(0, Math.round((Date.parse(d.expiresAt) - now) / 1000))
  const asked = Math.max(0, Math.round((now - Date.parse(d.createdAt)) / 1000))
  const span = (s: number) => (s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`)
  return (
    <Group className="argus-way-pending" gap="md" wrap="nowrap" role="status">
      <ThemeIcon size={36} radius="md" color="amber" variant="light"><IconHourglassHigh size={20} /></ThemeIcon>
      <Box style={{ minWidth: 0 }}>
        <Text fw={600} size="sm">Your login to {shortName(hostname)} is waiting for approval</Text>
        <Text size={FS.meta} c={INK.pending} mt={2}>
          {d.principal}{d.clientAddr ? ` from ${d.clientAddr}` : ''} · asked {span(asked)} ago · refused in {span(left)} if nobody answers
        </Text>
      </Box>
    </Group>
  )
}
