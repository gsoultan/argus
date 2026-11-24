import { useState } from 'react'
import { Alert, Badge, Button, Code, Group, List, Modal, Stack, Text, TextInput } from '@mantine/core'
import { IconArrowBigUpLines, IconDatabase, IconFolderOff, IconTrash } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { AssetDialogTitle } from '~/components/primitives'
import { AloneOr } from '~/components/ApproveOwn'
import type { AptSourcesFix, PackagesView, PostgresUpkeep } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { useStartPackageRun } from '~/lib/packageQueries'
import { FS, SP } from '~/theme'

/**
 * PostgreSQL around server updates, and the apt sources it comes from.
 *
 * What the agent found, each with what it means and, where Argus can do it,
 * a button: a newer psql than the server (harmless, said so), a cluster that
 * can move to a newer major version (a run that backs up first and keeps the
 * old cluster), the old cluster an upgrade left (removed only when asked,
 * once the applications look fine), and apt sources a release upgrade left
 * behind. Nothing runs without a confirmation that says what it does.
 */
export function PostgresCard({ view: v, assetId, name, canEdit, busy }: {
  view: PackagesView
  assetId: string
  name: string
  canEdit: boolean
  busy: boolean
}) {
  const [asking, setAsking] = useState<null | 'upgrade' | 'drop' | 'sources'>(null)
  const pg = v.report?.postgres
  const src = v.report?.apt_sources
  if (!src && (!pg || (!pg.newer_client && !pg.upgrade && !pg.old_cluster))) return null
  const why = !canEdit ? 'An admin or owner can make these changes from here.'
    : busy ? 'These can be done once the run going now has ended.' : null
  const running = (pg?.clusters ?? []).filter((c) => c.online)
  return (
    <SectionCard title="PostgreSQL" icon={IconDatabase}
      badge={running.length > 0 ? <Badge size="sm" variant="light" color="slate">{running.map((c) => `${c.version} ${c.name}`).join(', ')}</Badge> : undefined}
      description={clusterLine(pg)}>
      <Stack gap={SP.cozy}>
        {pg?.old_cluster && (
          <OldCluster pg={pg} canEdit={canEdit} busy={busy} onRemove={() => setAsking('drop')} />
        )}
        {pg?.newer_client && <NewerClient c={pg.newer_client} />}
        {pg?.upgrade && (
          <Upgrade u={pg.upgrade} canEdit={canEdit} busy={busy} onUpgrade={() => setAsking('upgrade')} />
        )}
        {src && <Sources src={src} canEdit={canEdit} busy={busy} onFix={() => setAsking('sources')} />}
        {why && <Text size={FS.meta} c="dimmed">{why}</Text>}
      </Stack>
      {asking === 'upgrade' && pg?.upgrade && <ConfirmUpgrade assetId={assetId} name={name} view={v} pg={pg} onClose={() => setAsking(null)} />}
      {asking === 'drop' && pg?.old_cluster && <ConfirmDrop assetId={assetId} name={name} view={v} pg={pg} onClose={() => setAsking(null)} />}
      {asking === 'sources' && src && <ConfirmSources assetId={assetId} name={name} view={v} src={src} onClose={() => setAsking(null)} />}
    </SectionCard>
  )
}

function clusterLine(pg: PostgresUpkeep | undefined): string {
  const cl = pg?.clusters ?? []
  if (cl.length === 0) return 'No PostgreSQL cluster is set up on this server.'
  return cl.map((c) => `${c.version} ${c.name} on port ${c.port}, ${c.online ? 'running' : 'stopped'}`).join(' · ')
}

export function gb(n: number): string {
  if (n >= 10 * 2 ** 30) return `${Math.round(n / 2 ** 30)} GB`
  if (n >= 2 ** 30 / 10) return `${(n / 2 ** 30).toFixed(1)} GB`
  return `${Math.max(1, Math.round(n / 2 ** 20))} MB`
}

function cap(s: string): string {
  return s ? s[0]!.toUpperCase() + s.slice(1) : s
}

function NewerClient({ c }: { c: NonNullable<PostgresUpkeep['newer_client']> }) {
  return (
    <Alert color="gray" p="xs" title={`psql ${c.client} next to PostgreSQL ${c.server} is fine`}>
      <Stack gap={SP.tight}>
        <Text size={FS.body} lh={1.5}>
          An update brought PostgreSQL {c.client}&apos;s client (psql, pg_dump) with the postgresql-client package. A newer
          psql talks to an older server, so <Code>psql ({c.client}.x, server {c.server}.x)</Code> is expected and nothing
          needs doing. The server still runs {c.server}.
        </Text>
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          To use {c.server}&apos;s own psql, run <Code>{c.server_psql ?? `/usr/lib/postgresql/${c.server}/bin/psql`}</Code>.
          To move the server itself to {c.client}, use the upgrade below.
        </Text>
      </Stack>
    </Alert>
  )
}

function Upgrade({ u, canEdit, busy, onUpgrade }: {
  u: NonNullable<PostgresUpkeep['upgrade']>
  canEdit: boolean
  busy: boolean
  onUpgrade: () => void
}) {
  const blockers = u.blockers ?? []
  return (
    <Alert color={blockers.length ? 'amber' : 'azure'} p="xs"
      title={`PostgreSQL ${u.from} ${u.cluster} can move to ${u.to}`}>
      <Stack gap={SP.cozy}>
        <Text size={FS.body} lh={1.5}>
          Optional. {u.from} keeps getting fixes until its end of life, so this is when it suits you. The cluster holds
          {' '}{gb(u.size_bytes)}; it is down while it is upgraded, {u.method === 'upgrade' ? 'usually minutes, since its files are copied' : 'for as long as a dump and restore takes'}.
        </Text>
        {blockers.length > 0 && (
          <List size="sm" spacing={4}>
            {blockers.map((b) => <List.Item key={b}>{cap(b)}.</List.Item>)}
          </List>
        )}
        {canEdit && (
          <Group gap={SP.cozy} wrap="wrap">
            <Button size="compact-sm" variant="white" color={blockers.length ? 'amber' : 'azure'}
              leftSection={<IconArrowBigUpLines size={14} />} disabled={busy || blockers.length > 0} onClick={onUpgrade}>
              Upgrade to PostgreSQL {u.to}…
            </Button>
            <Text size={FS.meta} c="dimmed">
              {blockers.length ? 'The button turns on when nothing above is in the way.' : 'You see every step before anything changes.'}
            </Text>
          </Group>
        )}
      </Stack>
    </Alert>
  )
}

function OldCluster({ pg, canEdit, busy, onRemove }: {
  pg: PostgresUpkeep
  canEdit: boolean
  busy: boolean
  onRemove: () => void
}) {
  const o = pg.old_cluster!
  return (
    <Alert color="amber" p="xs" title={`PostgreSQL ${o.version} ${o.cluster} is kept, stopped, on port ${o.port}`}>
      <Stack gap={SP.cozy}>
        <Text size={FS.body} lh={1.5}>
          {o.current} {o.cluster} replaced it. It is kept so you can go back. Once your applications work on {o.current},
          remove it to free its disk space.
        </Text>
        <details>
          <summary><Text span size={FS.meta} c="dimmed">How to go back to {o.version}</Text></summary>
          <List size="sm" spacing={2} mt={4} type="ordered">
            <List.Item><Code>sudo pg_ctlcluster {o.current} {o.cluster} stop</Code></List.Item>
            <List.Item>Swap the two <Code>port</Code> settings in <Code>/etc/postgresql/{o.version}/{o.cluster}/postgresql.conf</Code> and <Code>/etc/postgresql/{o.current}/{o.cluster}/postgresql.conf</Code>.</List.Item>
            <List.Item><Code>sudo pg_ctlcluster {o.version} {o.cluster} start</Code></List.Item>
          </List>
          <Text size={FS.meta} c="dimmed" mt={4}>Writes made on {o.current} since the upgrade are not in {o.version}.</Text>
        </details>
        {canEdit && (
          <Group gap={SP.cozy} wrap="wrap">
            <Button size="compact-sm" variant="white" color="rose" leftSection={<IconTrash size={14} />}
              disabled={busy} onClick={onRemove}>
              Remove PostgreSQL {o.version} {o.cluster}…
            </Button>
            <Text size={FS.meta} c="dimmed">Its data is deleted; this cannot be undone.</Text>
          </Group>
        )}
      </Stack>
    </Alert>
  )
}

function Sources({ src, canEdit, busy, onFix }: {
  src: AptSourcesFix
  canEdit: boolean
  busy: boolean
  onFix: () => void
}) {
  const left = src.leftovers ?? []
  const movable = left.filter((l) => l.movable)
  const kept = left.filter((l) => !l.movable)
  const can = movable.length > 0 || Boolean(src.pgdg)
  return (
    <Alert color="gray" p="xs" title="apt's sources after the release upgrade">
      <Stack gap={SP.cozy}>
        {src.pgdg && (
          <Text size={FS.body} lh={1.5}>
            PostgreSQL&apos;s own repository (apt.postgresql.org) in <Code>/etc/apt/{src.pgdg.file}</Code> is
            {src.pgdg.disabled ? ' turned off' : ''}{src.pgdg.disabled && !src.pgdg.suites.includes(src.pgdg.want) ? ' and' : ''}
            {!src.pgdg.suites.includes(src.pgdg.want) ? <> on <Code>{src.pgdg.suites.join(' ')}</Code>, not <Code>{src.pgdg.want}</Code></> : null},
            so PostgreSQL gets no updates from it.
          </Text>
        )}
        {left.length > 0 && (
          <Text size={FS.body} lh={1.5}>
            The upgrade left {left.length === 1 ? 'a file' : `${left.length} files`} apt does not read, which is why apt
            says &ldquo;Ignoring file &hellip; invalid filename extension&rdquo;. Harmless, but noisy:{' '}
            {left.map((l, i) => <span key={l.name}>{i > 0 ? ', ' : ''}<Code>{l.name}</Code></span>)}.
          </Text>
        )}
        {kept.length > 0 && (
          <Text size={FS.meta} c="dimmed" lh={1.5}>
            Left where {kept.length === 1 ? 'it is' : 'they are'}: {kept.map((l) => `${l.name} (${l.why})`).join('; ')}.
          </Text>
        )}
        {canEdit && can && (
          <Group gap={SP.cozy} wrap="wrap">
            <Button size="compact-sm" variant="white" color="azure" leftSection={<IconFolderOff size={14} />}
              disabled={busy} onClick={onFix}>
              Fix apt&apos;s sources…
            </Button>
            <Text size={FS.meta} c="dimmed">Everything it changes is kept in a copy, so it can be undone.</Text>
          </Group>
        )}
      </Stack>
    </Alert>
  )
}

/* ── Confirmations ──────────────────────────────────────────────────────── */

function useTypedName(hostname: string) {
  const [typed, setTyped] = useState('')
  const short = hostname.split('.')[0] ?? hostname
  return { typed, setTyped, short, confirmed: typed.trim() === short || typed.trim() === hostname }
}

function Approval({ v, what }: { v: PackagesView; what: string }) {
  if (!v.rule.needsApproval) return null
  return (
    <Alert color="azure" p="xs">
      <Text size={FS.body}><AloneOr kind="runs" what={what}>This server&apos;s rule asks for a second admin: it waits until another admin approves it.</AloneOr></Text>
    </Alert>
  )
}

function useAsk(assetId: string, v: PackagesView, onClose: () => void) {
  const start = useStartPackageRun(assetId)
  const ask = (body: Parameters<typeof start.mutateAsync>[0], failure: string, going: [string, string]) => void notifyRun(
    () => start.mutateAsync(body),
    {
      failure,
      success: v.rule.needsApproval ? ['Waiting for approval', 'Another admin approves it before it starts.'] : going,
    },
  ).then((ok) => ok && onClose())
  return { ask, pending: start.isPending }
}

function ConfirmUpgrade({ assetId, name, view: v, pg, onClose }: {
  assetId: string
  name: string
  view: PackagesView
  pg: PostgresUpkeep
  onClose: () => void
}) {
  const u = pg.upgrade!
  const { typed, setTyped, short, confirmed } = useTypedName(v.hostname)
  const { ask, pending } = useAsk(assetId, v, onClose)
  return (
    <Modal opened onClose={onClose} size="lg"
      title={<AssetDialogTitle text={`Upgrade PostgreSQL ${u.from} ${u.cluster} to ${u.to} on ${name}?`} name={name} hostname={v.hostname} />}>
      <Stack gap="sm">
        <Text size={FS.body} lh={1.5}>
          The agent does what PostgreSQL&apos;s Debian tools document, on this server only:
        </Text>
        <List size="sm" spacing={4} type="ordered">
          <List.Item>Backs up the whole cluster, roles included, with pg_dumpall to <Code>/var/backups/postgresql</Code>.</List.Item>
          <List.Item>Installs {u.packages.join(', ')}.</List.Item>
          {(u.drop_empty || u.packages.includes(`postgresql-${u.to}`)) && (
            <List.Item>Drops the empty {u.to} {u.cluster} cluster the package makes, only if it is empty.</List.Item>
          )}
          <List.Item>
            Runs <Code>pg_upgradecluster -m {u.method} {u.from} {u.cluster}</Code>. Settings carry over, and {u.to} takes
            port {u.port}. Applications cannot connect until it is done.
          </List.Item>
          <List.Item>Checks {u.to} answers on port {u.port}, then refreshes its statistics (vacuumdb --analyze-in-stages).</List.Item>
        </List>
        <Alert color="amber" p="xs">
          <Text size={FS.body} lh={1.5}>
            PostgreSQL {u.from} {u.cluster} is kept, stopped, on another port, so you can go back until you remove it.
            Test your applications on {u.to} first. Take a snapshot of the server if you can.
          </Text>
        </Alert>
        <Approval v={v} what="this upgrade" />
        <TextInput label="Type the server name to confirm" placeholder={short} value={typed}
          onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" leftSection={<IconArrowBigUpLines size={16} />} disabled={!confirmed} loading={pending}
            onClick={() => ask({ mode: 'postgres_upgrade', reboot: 'never', expect: `${u.from}/${u.cluster}:${u.to}` },
              'PostgreSQL upgrade not started',
              ['Upgrading PostgreSQL', 'The server starts at its next check-in, within 30 seconds.'])}>
            Upgrade to {u.to}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ConfirmDrop({ assetId, name, view: v, pg, onClose }: {
  assetId: string
  name: string
  view: PackagesView
  pg: PostgresUpkeep
  onClose: () => void
}) {
  const o = pg.old_cluster!
  const { typed, setTyped, short, confirmed } = useTypedName(v.hostname)
  const { ask, pending } = useAsk(assetId, v, onClose)
  return (
    <Modal opened onClose={onClose} size="lg"
      title={<AssetDialogTitle text={`Remove PostgreSQL ${o.version} ${o.cluster} from ${name}?`} name={name} hostname={v.hostname} />}>
      <Stack gap="sm">
        <List size="sm" spacing={4} type="ordered">
          <List.Item>Drops the stopped cluster <Code>{o.version} {o.cluster}</Code> (pg_dropcluster) and deletes its data.</List.Item>
          {(o.packages ?? []).length > 0 ? (
            <List.Item>
              Removes {o.packages!.join(', ')}, unless apt would remove anything else with them. Configuration stays.
            </List.Item>
          ) : (
            <List.Item>Leaves PostgreSQL {o.version}&apos;s packages: another cluster still uses them.</List.Item>
          )}
        </List>
        <Text size={FS.body} lh={1.5}>PostgreSQL {o.current} {o.cluster} keeps running; nothing about it changes.</Text>
        <Alert color="rose" p="xs">
          <Text size={FS.body} lh={1.5}>
            After this, going back to {o.version} means restoring a backup.
            {(o.backups ?? []).length > 0 ? <> The backup taken before the upgrade stays in <Code>{o.backups![o.backups!.length - 1]}</Code>.</> : null}
          </Text>
        </Alert>
        <Approval v={v} what="this" />
        <TextInput label="Type the server name to confirm" placeholder={short} value={typed}
          onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" leftSection={<IconTrash size={16} />} disabled={!confirmed} loading={pending}
            onClick={() => ask({ mode: 'postgres_drop_old', reboot: 'never', expect: `${o.version}/${o.cluster}` },
              'Old PostgreSQL not removed',
              ['Removing old PostgreSQL', 'The server starts at its next check-in, within 30 seconds.'])}>
            Remove {o.version} {o.cluster}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ConfirmSources({ assetId, name, view: v, src, onClose }: {
  assetId: string
  name: string
  view: PackagesView
  src: AptSourcesFix
  onClose: () => void
}) {
  const { ask, pending } = useAsk(assetId, v, onClose)
  const movable = (src.leftovers ?? []).filter((l) => l.movable)
  return (
    <Modal opened onClose={onClose} size="md"
      title={<AssetDialogTitle text={`Fix apt's sources on ${name}?`} name={name} hostname={v.hostname} />}>
      <Stack gap="sm">
        <List size="sm" spacing={4}>
          {movable.length > 0 && (
            <List.Item>
              Moves {movable.map((l, i) => <span key={l.name}>{i > 0 ? ', ' : ''}<Code>{l.name}</Code></span>)} out
              of <Code>/etc/apt/sources.list.d</Code>. The real files they copy stay.
            </List.Item>
          )}
          {src.pgdg && (
            <List.Item>
              Points apt.postgresql.org at <Code>{src.pgdg.want}</Code>{src.pgdg.disabled ? ' and turns it on' : ''}
              {' '}in <Code>/etc/apt/{src.pgdg.file}</Code>, then runs apt update. If apt.postgresql.org does not serve
              {' '}{src.pgdg.want} yet, the file is put back as it was.
            </List.Item>
          )}
        </List>
        <Text size={FS.body} lh={1.5}>
          Nothing is installed. Copies of every file it moves or changes are kept in
          {' '}<Code>/var/backups/argus-apt-sources</Code>.
        </Text>
        <Approval v={v} what="this" />
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button leftSection={<IconFolderOff size={16} />} loading={pending}
            onClick={() => ask({ mode: 'apt_sources_fix', reboot: 'never', expect: src.fingerprint },
              "apt's sources not fixed", ["Fixing apt's sources", 'The server starts at its next check-in, within 30 seconds.'])}>
            Fix apt&apos;s sources
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
