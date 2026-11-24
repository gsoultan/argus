import { useEffect, useState } from 'react'
import { ActionIcon, Box, Button, CopyButton, Group, Stack, Text, ThemeIcon, UnstyledButton } from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconChevronDown, IconChevronRight, IconCopy, IconQuestionMark, IconX } from '@tabler/icons-react'
import { absTime, now, relTime } from '~/components/primitives'
import type { PackageRun, RunOutcome } from '~/lib/live'
import { FS, SP } from '~/theme'

/**
 * The parts every long run on a server shares: how long it has been going,
 * when the server was last heard from, and, once it ends, a result that
 * says plainly what happened, whether anything changed on the server and
 * what to do next. Used by OS updates, release upgrades and reboots.
 */

/** Re-renders every `ms` while mounted, so elapsed times count up. */
export function useTicker(ms = 1000): number {
  const [t, setT] = useState(() => now())
  useEffect(() => {
    const id = window.setInterval(() => setT(now()), ms)
    return () => window.clearInterval(id)
  }, [ms])
  return t
}

/** A length of time as a clock: 0:41, 8:50, 1:12:05. */
export function clock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`
}

/** A length of time in words: 40 s, 3 min, 1 h 12 min. */
export function span(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min${s % 60 && m < 5 ? ` ${s % 60} s` : ''}`
  return `${Math.floor(m / 60)} h ${m % 60} min`
}

/**
 * How long each step took, from when the agent reached it to when it
 * reached the next (or now, for the step going). Keys the agent did not
 * report are absent; a step reached twice keeps its last stretch.
 */
export function stepTimes(r: PackageRun, at: number): Record<string, number> {
  const steps = r.steps ?? []
  const end = r.finishedAt ? Date.parse(r.finishedAt) : at
  const out: Record<string, number> = {}
  steps.forEach((s, i) => {
    const next = steps[i + 1]
    out[s.step] = (next ? Date.parse(next.at) : end) - Date.parse(s.at)
  })
  return out
}

/** After this long with nothing from the server, say so. */
export const QUIET_MS = 60_000

/**
 * "Server last heard from 3 s ago", amber once the server has been quiet
 * for a minute, with what that means while it reboots.
 */
export function LastHeard({ heardAt, rebooting }: { heardAt: string | null | undefined; rebooting: boolean }) {
  const t = useTicker()
  if (!heardAt) return null
  const quiet = t - Date.parse(heardAt)
  const stale = quiet > QUIET_MS
  const c = stale ? 'var(--mantine-color-amber-7)' : 'var(--mantine-color-teal-7)'
  return (
    <Group gap={6} wrap="nowrap" data-testid="last-heard" title={absTime(heardAt)}>
        <Box w={8} h={8} style={{ borderRadius: '50%', background: c, flex: 'none' }} />
        <Text size={FS.meta} fw={600} c={c}>
          {stale && rebooting ? `Server silent for ${span(quiet)} while it restarts` : `Server last heard from ${span(quiet)} ago`}
        </Text>
    </Group>
  )
}

/** Text with `commands` in backticks, each shown as code with a copy button. */
export function WithCommands({ text }: { text: string }) {
  const parts = text.split(/`([^`]+)`/)
  return (
    <>
      {parts.map((p, i) => i % 2 === 0 ? <span key={i}>{p}</span> : (
        <Group key={i} component="span" gap={4} wrap="nowrap" display="inline-flex"
          style={{ verticalAlign: 'middle', background: 'var(--mantine-color-default-hover)', borderRadius: 6, padding: '1px 4px 1px 8px', margin: '0 2px' }}>
          <Text span ff="monospace" fz={FS.meta}>{p}</Text>
          <CopyButton value={p} timeout={1500}>
            {({ copied, copy }) => (
              <ActionIcon size="xs" variant="subtle" color={copied ? 'teal' : 'gray'} onClick={copy}
                aria-label={copied ? 'Copied' : `Copy ${p}`}>
                {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
              </ActionIcon>
            )}
          </CopyButton>
        </Group>
      ))}
    </>
  )
}

/* ── How a run ended ─────────────────────────────────────────────────────── */

const ENDED = new Set<PackageRun['state']>(['done', 'failed', 'interrupted'])

/**
 * Whether the run changed the server: the agent's answer when it gave one,
 * otherwise what the run's own record shows for certain, otherwise unknown.
 */
export function changedOf(r: PackageRun): RunOutcome['changed'] {
  if (r.outcome) return r.outcome.changed
  if (r.state === 'done') {
    if (r.mode === 'release_upgrade' || r.mode === 'reboot') return 'yes'
    if (r.mode === 'prepare_rabbitmq') return r.message?.includes('nothing was changed') ? 'no' : 'yes'
    if (r.mode === 'postgres_upgrade' || r.mode === 'postgres_drop_old' || r.mode === 'apt_sources_fix') {
      return /nothing was changed|need nothing any more/.test(r.message ?? '') ? 'no' : 'yes'
    }
    if (r.upgraded.length > 0 || (r.mode === 'install' || r.mode === 'remove')) return 'yes'
    if (r.mode === 'all' || r.mode === 'security' || r.mode === 'selected') return 'no'
  }
  return 'unknown'
}

/** What happened, in one sentence a person reads first. */
export function headline(r: PackageRun): string {
  const ok = r.state === 'done'
  const stopped = r.state === 'interrupted' ? 'was interrupted' : 'failed'
  switch (r.mode) {
    case 'release_upgrade': {
      const to = r.release ?? 'the next release'
      if (ok) return r.rebooted ? `The server is on ${to}` : `Upgraded to ${to}; a reboot is still needed`
      return changedOf(r) === 'no' ? `The upgrade to ${to} did not start` : `The upgrade to ${to} ${stopped}`
    }
    case 'reboot':
      return ok ? 'The server rebooted and is back' : `The reboot ${stopped}`
    case 'prepare_rabbitmq':
      if (ok) return 'RabbitMQ is ready for the next release'
      return changedOf(r) === 'no' ? 'RabbitMQ was not prepared' : `Preparing RabbitMQ ${stopped}`
    case 'postgres_upgrade': {
      const to = r.target?.split(':')[1]
      if (ok) return `PostgreSQL is on ${to ?? 'its new version'}`
      return changedOf(r) === 'no' ? 'PostgreSQL was not upgraded' : `The PostgreSQL upgrade ${stopped}`
    }
    case 'postgres_drop_old':
      if (ok) return 'The old PostgreSQL cluster is removed'
      return changedOf(r) === 'no' ? 'The old PostgreSQL cluster was not removed' : `Removing the old PostgreSQL cluster ${stopped}`
    case 'apt_sources_fix':
      if (ok) return changedOf(r) === 'no' ? "apt's sources needed nothing" : "apt's sources are fixed"
      return changedOf(r) === 'no' ? "apt's sources were not changed" : `Fixing apt's sources ${stopped}`
    default: {
      if (ok) {
        const n = r.upgraded.length
        return n > 0 ? `${n} package${n === 1 ? '' : 's'} updated${r.rebooted ? ', then rebooted' : ''}` : 'Nothing needed installing'
      }
      return changedOf(r) === 'no' ? 'The updates did not start' : `The update run ${stopped}`
    }
  }
}

const CHANGED: Record<RunOutcome['changed'], { label: string; color: string; icon: typeof IconCheck }> = {
  no: { label: 'No, nothing on the server changed', color: 'teal', icon: IconCheck },
  yes: { label: 'Yes', color: 'amber', icon: IconAlertTriangle },
  unknown: { label: 'Not sure: the agent could not tell', color: 'slate', icon: IconQuestionMark },
}

function changedDetail(r: PackageRun, c: RunOutcome['changed']): string | null {
  if (r.state === 'done') {
    if (r.mode === 'release_upgrade') return r.rebooted ? `Every package moved to ${r.release ?? 'the new release'}, and the server rebooted into it.` : null
    if (r.mode === 'reboot') return 'Nothing was installed; the server restarted.'
    if (r.mode === 'prepare_rabbitmq') return c === 'no' ? 'RabbitMQ was ready already.' : 'RabbitMQ was upgraded and its feature flags enabled. The release upgrade can go ahead.'
    if (r.mode === 'postgres_upgrade' || r.mode === 'postgres_drop_old' || r.mode === 'apt_sources_fix') return r.message || null
    if (r.upgraded.length > 0) return `${r.upgraded.length} package${r.upgraded.length === 1 ? '' : 's'} replaced${r.restarted.length ? `, ${r.restarted.join(', ')} restarted` : ''}.`
    return null
  }
  if (c === 'yes' && r.mode === 'prepare_rabbitmq') return 'RabbitMQ may have been upgraded before it stopped. Follow the steps below.'
  if (c === 'yes' && r.mode === 'postgres_upgrade') return 'Part of the upgrade ran before it stopped. Follow the steps below; they name the backup.'
  if (c === 'yes' && r.mode === 'apt_sources_fix') return "Some of apt's sources changed before it stopped; copies are in /var/backups/argus-apt-sources."
  if (c === 'yes') return 'Some packages were replaced before it stopped, so the server may be part way. Follow the steps below before rebooting.'
  if (c === 'no' && r.mode === 'release_upgrade') return 'No package was installed or removed, so the server is still on the release it was on.'
  return null
}

/** Whether a result has been put away on this browser. */
function dismissedKey(id: string) {
  return `argus.run.dismissed.${id}`
}
function isDismissed(id: string): boolean {
  try { return window.localStorage.getItem(dismissedKey(id)) === '1' } catch { return false }
}

/**
 * The run whose result belongs at the top of the tab: the newest that
 * ended, a failure for a day and a success for an hour, unless put away.
 */
export function resultToShow(runs: PackageRun[], at: number): PackageRun | undefined {
  const ended = runs.filter((r) => ENDED.has(r.state) && r.finishedAt)
    .sort((a, b) => Date.parse(b.finishedAt!) - Date.parse(a.finishedAt!))[0]
  if (!ended || isDismissed(ended.id)) return undefined
  const age = at - Date.parse(ended.finishedAt!)
  return age < (ended.state === 'done' ? 3_600_000 : 86_400_000) ? ended : undefined
}

/**
 * How a run ended: what happened, whether anything on the server changed,
 * why it stopped and what to do now, with the details folded away. It
 * stays at the top of the tab until put away.
 */
export function RunResult({ run: r, onDismiss, log }: { run: PackageRun; onDismiss: () => void; log: React.ReactNode }) {
  const [open, setOpen] = useState(false)
  const ok = r.state === 'done'
  const c = changedOf(r)
  const ch = CHANGED[c]
  const why = r.outcome?.why || (ok ? '' : r.message)
  const next = r.outcome?.next ?? []
  const took = r.startedAt && r.finishedAt ? span(Date.parse(r.finishedAt) - Date.parse(r.startedAt)) : null
  const dismiss = () => {
    try { window.localStorage.setItem(dismissedKey(r.id), '1') } catch { /* private window: it comes back on reload */ }
    onDismiss()
  }
  const tone = ok ? 'teal' : 'rose'
  return (
    <Box role="region" aria-label="How the last run ended"
      style={{ border: `1px solid var(--mantine-color-${tone}-3)`, borderRadius: 10, overflow: 'hidden', background: 'var(--mantine-color-body)' }}>
      <Group p="md" gap="sm" wrap="nowrap" align="flex-start" style={{ background: `var(--mantine-color-${tone}-light)` }}>
        <ThemeIcon size={28} radius="xl" color={tone} variant="light">{ok ? <IconCheck size={16} /> : <IconX size={16} />}</ThemeIcon>
        <Box style={{ flex: 1 }}>
          <Text fw={650} size="md">{headline(r)}</Text>
          <Text size={FS.meta} c="dimmed">
            {[took, r.finishedAt ? `ended ${relTime(r.finishedAt)}` : null, `asked by ${r.requestedBy}`].filter(Boolean).join(' · ')}
          </Text>
        </Box>
        <Button size="compact-sm" variant="default" onClick={dismiss}>Dismiss</Button>
      </Group>

      <Stack gap={0}>
        <Box px="md" py="sm" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
          <Text size={FS.micro} tt="uppercase" fw={700} c="dimmed" mb={6}>Did anything change on the server?</Text>
          <Group gap={8} wrap="nowrap">
            <ThemeIcon size={20} radius="xl" variant="light" color={ok && c === 'yes' ? 'teal' : ch.color}>
              {ok && c === 'yes' ? <IconCheck size={12} /> : <ch.icon size={12} />}
            </ThemeIcon>
            <Text fw={600} size={FS.body} data-testid="run-changed">{ok && c === 'yes' ? 'Yes, as asked' : ch.label}</Text>
          </Group>
          {changedDetail(r, c) && <Text size={FS.meta} c="dimmed" mt={4}>{changedDetail(r, c)}</Text>}
        </Box>

        {!ok && why && (
          <Box px="md" py="sm" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
            <Text size={FS.micro} tt="uppercase" fw={700} c="dimmed" mb={6}>Why it stopped</Text>
            <Text size={FS.body} lh={1.5} style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}><WithCommands text={why} /></Text>
          </Box>
        )}

        {next.length > 0 && (
          <Box px="md" py="sm" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
            <Text size={FS.micro} tt="uppercase" fw={700} c="dimmed" mb={6}>What to do next</Text>
            <Stack gap={SP.cozy} component="ol" style={{ listStyle: 'none', padding: 0, margin: 0 }}>
              {next.map((n, i) => (
                <Group key={i} component="li" gap={10} wrap="nowrap" align="flex-start">
                  <ThemeIcon size={20} radius="xl" variant="light" color="slate"><Text fz={11} fw={700}>{i + 1}</Text></ThemeIcon>
                  <Text size={FS.body} lh={1.6} style={{ overflowWrap: 'anywhere' }}><WithCommands text={n} /></Text>
                </Group>
              ))}
            </Stack>
          </Box>
        )}

        <Box style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
          <UnstyledButton px="md" py="sm" w="100%" onClick={() => setOpen((o) => !o)} aria-expanded={open}>
            <Group gap={6}>
              {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
              <Text size={FS.body} fw={600}>Details</Text>
              <Text size={FS.meta} c="dimmed">the full message, what changed, and what the server printed</Text>
            </Group>
          </UnstyledButton>
          {/* Plain show and hide, a <pre> and a title, rather than Collapse,
              Code and Tooltip: using those here made the bundler re-split
              shared code and put 2.9 kB more on the first screen
              (e2e/weight.spec.ts). */}
          {open && (
            <Stack gap={SP.cozy} px="md" pb="md">
              {r.message && (ok || r.outcome?.why) && <Text size={FS.meta} style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{r.message}</Text>}
              {r.upgraded.length > 0 && (
                <Box component="pre" fz={FS.micro} ff="monospace" m={0} p="xs" style={{ background: 'var(--mantine-color-default-hover)', borderRadius: 4, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{r.upgraded.map((u) => `${u.name} ${u.from ?? ''} → ${u.to}`).join('\n')}</Box>
              )}
              {log}
            </Stack>
          )}
        </Box>
      </Stack>
    </Box>
  )
}
