import { Badge, Box, Card, CopyButton, Group, Text, ThemeIcon, Tooltip, UnstyledButton } from '@mantine/core'
import {
  IconAlertTriangle, IconCertificate, IconCheck, IconCopy, IconDoorExit, IconKey,
  IconLock, IconPlayerRecordFilled, IconPlugConnected, IconPlugConnectedX,
  IconRouteAltLeft, IconShieldCheck, IconShieldOff,
} from '@tabler/icons-react'
import type {
  AgentState, AssetHealth, BypassPosture, CredentialMode, HostKeyState,
  RecordingFidelity, RequestState, RiskFlag, SessionOrigin, SessionState,
} from '~/types/domain'
import { FS } from '~/theme'
import { EPOCH } from '~/lib/seed'
import { isConfigured } from '~/lib/live'

/* ── Time ────────────────────────────────────────────────────────────────── */

/**
 * "now" for relative timestamps.
 *
 * The fixture uses a frozen epoch so demo data reads consistently. Live data
 * carries real timestamps, and measuring those against a frozen epoch renders
 * a session that just happened as "4d from now". Real data wins: once a
 * control plane is configured, use the wall clock.
 */
function now(): number {
  return isConfigured() ? Date.now() : EPOCH
}

export function relTime(iso: string | null): string {
  if (!iso) return '—'
  const diff = now() - Date.parse(iso)
  const abs = Math.abs(diff)
  const suffix = diff >= 0 ? 'ago' : 'from now'
  const m = Math.round(abs / 60_000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ${suffix}`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h ${suffix}`
  const d = Math.round(h / 24)
  if (d < 30) return `${d}d ${suffix}`
  return `${Math.round(d / 30)}mo ${suffix}`
}

export function absTime(iso: string | null): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString('en-GB', {
    day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

export function duration(fromIso: string, toIso: string | null): string {
  const end = toIso ? Date.parse(toIso) : now()
  const secs = Math.max(0, Math.round((end - Date.parse(fromIso)) / 1000))
  const h = Math.floor(secs / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const s = secs % 60
  return h > 0 ? `${h}h ${m}m` : m > 0 ? `${m}m ${s}s` : `${s}s`
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 ** 2).toFixed(1)} MB`
}

/* ── Status badges ───────────────────────────────────────────────────────── */

export function SessionStateBadge({ state }: { state: SessionState }) {
  if (state === 'active') {
    return (
      <Badge
        color="sky"
        leftSection={<IconPlayerRecordFilled size={9} className="animate-pulse" />}
      >
        live
      </Badge>
    )
  }
  const map = { closed: 'slate', terminated: 'rose', rejected: 'rose' } as const
  return <Badge color={map[state]}>{state}</Badge>
}

export function RequestStateBadge({ state }: { state: RequestState }) {
  const map: Record<RequestState, string> = {
    pending: 'amber', approved: 'teal', denied: 'rose', expired: 'slate', revoked: 'rose',
  }
  return <Badge color={map[state]}>{state}</Badge>
}

export function HealthDot({ health }: { health: AssetHealth }) {
  const color =
    health === 'reachable' ? 'var(--color-verified)'
    : health === 'degraded' ? 'var(--color-pending)'
    : 'var(--color-denied)'
  return (
    <Tooltip label={health}>
      <Box
        w={7}
        h={7}
        style={{
          borderRadius: '50%',
          background: color,
          boxShadow: `0 0 7px ${color}`,
          flexShrink: 0,
        }}
      />
    </Tooltip>
  )
}

/**
 * Host key state is the single most important signal in the whole product:
 * Argus terminates SSH, so if it hasn't verified the target's identity, nobody
 * has. An unpinned or changed key gets loud treatment on purpose.
 */
export function HostKeyBadge({ state }: { state: HostKeyState }) {
  if (state === 'pinned') {
    return (
      <Tooltip label="Host key pinned and verified on every connect">
        <Badge color="teal" leftSection={<IconShieldCheck size={11} />}>pinned</Badge>
      </Tooltip>
    )
  }
  if (state === 'changed') {
    return (
      <Tooltip label="Presented key no longer matches the pin. Connections are refused.">
        <Badge color="rose" leftSection={<IconAlertTriangle size={11} />}>changed</Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip label="No key pinned yet — the target's identity is unverified">
      <Badge color="amber" leftSection={<IconShieldOff size={11} />}>unpinned</Badge>
    </Tooltip>
  )
}

export function CredentialBadge({ mode }: { mode: CredentialMode }) {
  if (mode === 'ca-certificate') {
    return (
      <Tooltip label="Short-lived certificate minted per session. No standing credential exists.">
        <Badge color="teal" leftSection={<IconCertificate size={11} />}>certificate</Badge>
      </Tooltip>
    )
  }
  const label = mode === 'injected-key' ? 'injected key' : 'injected password'
  return (
    <Tooltip label="Vaulted secret injected by the gateway. The user never sees it, but it is standing credential.">
      <Badge color="slate" leftSection={<IconKey size={11} />}>{label}</Badge>
    </Tooltip>
  )
}

/**
 * Recording fidelity, stated plainly. PTY capture can be defeated by base64 or
 * by running a script whose body never reaches the screen — so we say so rather
 * than letting a buyer assume otherwise.
 */
export function FidelityBadge({ fidelity }: { fidelity: RecordingFidelity }) {
  if (fidelity === 'ebpf') {
    return (
      <Tooltip label="PTY replay plus kernel-observed execve/connect events. Survives obfuscation.">
        <Badge color="teal" leftSection={<IconLock size={11} />}>eBPF</Badge>
      </Tooltip>
    )
  }
  if (fidelity === 'pty') {
    return (
      <Tooltip label="Terminal replay only. An audit aid, not a security boundary — commands can be obscured.">
        <Badge color="amber">PTY only</Badge>
      </Tooltip>
    )
  }
  return <Badge color="rose">not recorded</Badge>
}

const RISK_LABEL: Record<RiskFlag, string> = {
  'bypassed-gateway':
    'Connected straight to sshd — no policy was evaluated. Captured by the host agent after the fact.',
  'off-hours': 'Outside the configured working window',
  'new-asset-for-user': 'First time this user has reached this host',
  'root-principal': 'Session opened as root',
  'unpinned-host-key': 'Target host key was not pinned',
  'break-glass': 'Emergency access path used',
  'bulk-file-transfer': 'Unusually large file transfer volume',
  'kernel-evidence-waived':
    'Policy required kernel-observed execution evidence and there was none. Allowed by role — this recording cannot evidence what ran.',
  terminated: 'Ended by an administrator, not by the person using it',
  'recording-incomplete':
    'The replay is short of what happened — frames the session saw never reached the file',
  'no-credential-injection':
    'The user supplied their own password, so a standing credential still exists on the target',
  'legacy-credssp-binding':
    'Pre-version-5 CredSSP binding — not bound to a nonce, so a captured exchange can be replayed against another channel',
  'no-network-level-auth':
    'TLS without CredSSP — the user met the host’s own logon screen through the tunnel, unauthenticated until they typed something',
}

export function RiskFlags({ flags }: { flags: RiskFlag[] }) {
  if (flags.length === 0) return <Text size="xs" c="dimmed">—</Text>
  return (
    <Group gap={4} wrap="wrap">
      {flags.map((f) => (
        <Tooltip key={f} label={RISK_LABEL[f]}>
          <Badge
            size="xs"
            color={
              f === 'bypassed-gateway' ||
              f === 'root-principal' ||
              f === 'break-glass' ||
              f === 'kernel-evidence-waived' ||
              f === 'recording-incomplete'
                ? 'rose'
                : 'amber'
            }
            variant={f === 'bypassed-gateway' ? 'filled' : 'light'}
          >
            {f}
          </Badge>
        </Tooltip>
      ))}
    </Group>
  )
}

/**
 * Where the session came from.
 *
 * A direct session means someone reached sshd without passing through the
 * gateway: no approval, no time window, no principal restriction. The recording
 * exists — the host agent saw it — but every *preventive* control was skipped.
 * That is a control failure, so it is styled like one.
 */
export function OriginBadge({ origin }: { origin: SessionOrigin }) {
  if (origin === 'brokered') {
    return (
      <Tooltip label="Went through the gateway — policy evaluated before the connection existed.">
        <Badge color="teal" leftSection={<IconRouteAltLeft size={11} />}>brokered</Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip
      label="Connected straight to sshd on the host. Recorded by the agent, but no policy ran — no approval, no window, no principal check."
      multiline
      maw={300}
    >
      <Badge color="rose" variant="filled" leftSection={<IconDoorExit size={11} />}>
        direct
      </Badge>
    </Tooltip>
  )
}

/** Agent liveness. Silence is the interesting state — see BypassBadge. */
export function AgentBadge({ state, lastSeen }: { state: AgentState; lastSeen: string | null }) {
  if (state === 'healthy') {
    return (
      <Tooltip label={`Reporting — last seen ${relTime(lastSeen)}`}>
        <Badge color="teal" leftSection={<IconPlugConnected size={11} />}>agent</Badge>
      </Tooltip>
    )
  }
  if (state === 'stale') {
    return (
      <Tooltip
        label={`Stopped reporting ${relTime(lastSeen)}. A user with root can kill the agent — treat silence as hostile until proven otherwise.`}
        multiline
        maw={300}
      >
        <Badge color="rose" leftSection={<IconAlertTriangle size={11} />}>silent</Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip label="No agent installed — a direct connection to port 22 would leave no trace.">
      <Badge color="amber" leftSection={<IconPlugConnectedX size={11} />}>no agent</Badge>
    </Tooltip>
  )
}

/**
 * Can this host still be reached without Argus, and would anyone know?
 *
 * `enforced` is the goal state: no standing keys, CA-only trust, port 22
 * reachable from the gateway alone. A bypass you prevented beats one you
 * merely recorded.
 */
export function BypassBadge({ posture, unmanagedKeys }: {
  posture: BypassPosture
  unmanagedKeys: number
}) {
  if (posture === 'enforced') {
    return (
      <Tooltip label="No standing keys, certificate-only trust, port 22 firewalled to the gateway. Direct access is not possible.">
        <Badge color="teal" leftSection={<IconShieldCheck size={11} />}>closed</Badge>
      </Tooltip>
    )
  }
  if (posture === 'monitored') {
    return (
      <Tooltip
        label={
          unmanagedKeys > 0
            ? `${unmanagedKeys} key(s) Argus did not issue. A bypass is possible, but the agent would record it.`
            : 'A bypass is possible, but the agent would record it.'
        }
        multiline
        maw={300}
      >
        <Badge color="amber" leftSection={<IconShieldOff size={11} />}>
          {unmanagedKeys > 0 ? `${unmanagedKeys} unmanaged` : 'monitored'}
        </Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip label="No agent and no lockdown — a direct connection here would go completely unrecorded.">
      <Badge color="rose" variant="filled" leftSection={<IconAlertTriangle size={11} />}>
        unmonitored
      </Badge>
    </Tooltip>
  )
}

/* ── Digest ──────────────────────────────────────────────────────────────── */

/** Truncated hash with click-to-copy. Auditors compare these by eye. */
export function Digest({ value, chars = 12 }: { value: string; chars?: number }) {
  const short = value.length > chars ? `${value.slice(0, chars)}…` : value
  return (
    <CopyButton value={value} timeout={1200}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copied' : value} multiline maw={420}>
          <UnstyledButton onClick={copy} className="argus-digest" c={copied ? 'teal' : 'dimmed'}>
            <Group gap={4} wrap="nowrap">
              <span>{short}</span>
              {copied ? <IconCheck size={11} /> : <IconCopy size={11} opacity={0.5} />}
            </Group>
          </UnstyledButton>
        </Tooltip>
      )}
    </CopyButton>
  )
}

/**
 * Inline monospace. Renders as a span so it can sit inside a sentence — as a
 * block-level Text it broke the line and orphaned the following punctuation.
 */
export function Mono({ children, c }: { children: React.ReactNode; c?: string }) {
  return (
    <Text component="span" display="inline" ff="monospace" size="xs" c={c}>
      {children}
    </Text>
  )
}

/* ── Layout primitives ───────────────────────────────────────────────────── */

/**
 * Labelled value in a detail panel.
 *
 * Previously defined twice — once in the session detail and once in the asset
 * detail — which had already drifted by a pixel of top margin, so the same
 * construct sat differently on two pages a user moves between constantly.
 */
export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Box>
      <Text size={FS.micro} c="dimmed" fw={600} style={{ letterSpacing: '0.05em' }}>
        {label.toUpperCase()}
      </Text>
      <Box mt={2}>{children}</Box>
    </Box>
  )
}

/**
 * Headline number on a dashboard card.
 *
 * Also previously duplicated, in two visibly different forms: the overview's
 * had an icon, uppercase label and a 27px figure; coverage's had none of those
 * and a 28px one. Two cards that mean the same thing now look the same, and
 * `tone` decides emphasis rather than each caller inventing it.
 */
export function Stat({
  label, value, sub, icon, color = 'slate', tone = 'ok', onClick,
}: {
  label: string
  value: string | number
  sub?: string
  icon?: typeof IconShieldCheck
  color?: string
  tone?: 'ok' | 'warn'
  onClick?: () => void
}) {
  const Icon = icon
  const interactive = Boolean(onClick)
  return (
    <Card
      padding="md"
      h="100%"
      onClick={onClick}
      {...(interactive
        ? {
            role: 'button',
            tabIndex: 0,
            onKeyDown: (e: React.KeyboardEvent) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                onClick?.()
              }
            },
          }
        : {})}
      className={`transition-colors hover:border-slate-600 ${interactive ? 'cursor-pointer' : ''}`}
      style={tone === 'warn' ? { borderColor: 'var(--color-pending)' } : undefined}
    >
      <Group justify="space-between" wrap="nowrap" align="flex-start">
        <Box>
          <Text size={FS.micro} c="dimmed" fw={600} style={{ letterSpacing: '0.06em' }}>
            {label.toUpperCase()}
          </Text>
          <Text size={FS.figure} fw={600} lh={1.2} mt={4} c={tone === 'warn' ? 'amber.4' : undefined}>
            {value}
          </Text>
          {sub && <Text size={FS.micro} c="dimmed" mt={2}>{sub}</Text>}
        </Box>
        {Icon && (
          <ThemeIcon variant="light" color={color} size={32} radius="md">
            <Icon size={17} stroke={1.7} />
          </ThemeIcon>
        )}
      </Group>
    </Card>
  )
}

/**
 * Props that make a table row behave like the link it already looks like.
 *
 * Three tables navigated on click while being unreachable by keyboard and
 * announcing nothing to a screen reader; a fourth told the user to click rows
 * that had no handler at all. Spreading this onto a `Table.Tr` fixes both, and
 * makes it obvious when a row is *not* navigable.
 */
export function rowNav(onActivate: () => void) {
  return {
    onClick: onActivate,
    onKeyDown: (e: React.KeyboardEvent) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        onActivate()
      }
    },
    tabIndex: 0,
    role: 'link',
    className: 'cursor-pointer argus-row',
  } as const
}
