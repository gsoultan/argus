import { Suspense, lazy, useState } from 'react'
import {
  ActionIcon, Box, Button, Group, Select, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconPencil, IconPlus, IconSearch, IconServer2, IconShieldLock, IconUsers, IconX,
} from '@tabler/icons-react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import {
  AgentBadge, BypassBadge, CredentialBadge, HealthDot, HostKeyBadge, Mono,
  relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import { assetsQuery, groupsQuery, meQuery } from '~/lib/queries'
import type { Asset, CredentialMode } from '~/types/domain'
import { EPOCH } from '~/lib/seed'
import { isConfigured } from '~/lib/live'

/**
 * Loaded on demand.
 *
 * Both pull in Mantine's overlay and its focus trap, and both are opened only
 * by an administrator. An operator visiting their own list of hosts should not
 * download the dialogs for editing a fleet they cannot edit.
 */
const AssetForm = lazy(() =>
  import('~/components/AssetForm').then((m) => ({ default: m.AssetForm })),
)
const AssetAssignments = lazy(() =>
  import('~/components/AssetAssignments').then((m) => ({ default: m.AssetAssignments })),
)

/**
 * Filters live in the URL.
 *
 * Coverage counts hosts with no agent and hosts whose agent went quiet, and had
 * no way to answer "which ones?" — the two pages described the same fleet and
 * neither could hand a finding to the other. A filter that is addressable makes
 * that link possible, and makes a narrowed inventory something an operator can
 * paste into an incident channel.
 */
interface AssetSearch {
  q?: string
  group?: string
  /**
   * `unverified` is not a state an asset has; it means "any state but pinned".
   *
   * The Overview's "Unverified hosts" tile counts `hostKeyState !== 'pinned'`,
   * which is unpinned *and* changed, and no single-state filter could ever
   * select that set -- so the tile linked here unfiltered and landed on the
   * whole inventory.
   */
  hostKey?: Asset['hostKeyState'] | 'unverified'
  agent?: Asset['agentState']
  bypass?: Asset['bypassPosture']
  /**
   * `standing` is not a mode an asset has; it means "any mode but
   * ca-certificate" -- the set the Overview's zero-standing-privilege card
   * counts and then tells you to shrink.
   */
  credential?: CredentialMode | 'standing'
}

const HOST_KEY_STATES = ['pinned', 'unpinned', 'changed', 'unverified'] as const
const AGENT_STATES = ['healthy', 'stale', 'absent'] as const
const BYPASS_POSTURES = ['enforced', 'monitored', 'open'] as const
const CREDENTIAL_MODES = [
  'standing', 'ca-certificate', 'injected-key', 'injected-password',
] as const

const CREDENTIAL_LABEL: Record<(typeof CREDENTIAL_MODES)[number], string> = {
  standing: 'Standing credential (any)',
  'ca-certificate': 'Certificate, no standing secret',
  'injected-key': 'Injected key',
  'injected-password': 'Injected password',
}

const oneOf = <T extends string>(allowed: readonly T[], v: unknown): T | undefined =>
  typeof v === 'string' && (allowed as readonly string[]).includes(v) ? (v as T) : undefined

const str = (v: unknown): string | undefined =>
  typeof v === 'string' && v.length > 0 ? v : undefined

export const Route = createFileRoute('/assets/')({
  component: Assets,
  // Unrecognised values are dropped rather than passed through, so a
  // hand-edited URL cannot put the table into a state the controls cannot show.
  validateSearch: (search: Record<string, unknown>): AssetSearch => ({
    q: str(search.q),
    group: str(search.group),
    hostKey: oneOf(HOST_KEY_STATES, search.hostKey),
    agent: oneOf(AGENT_STATES, search.agent),
    bypass: oneOf(BYPASS_POSTURES, search.bypass),
    credential: oneOf(CREDENTIAL_MODES, search.credential),
  }),
  loader: ({ context }) => context.queryClient.ensureQueryData(assetsQuery({})),
})

function rotationOverdue(a: Asset): boolean {
  if (!a.credentialRotatedAt || !a.rotationIntervalDays) return false
  const ref = isConfigured() ? Date.now() : EPOCH
  return ref - Date.parse(a.credentialRotatedAt) > a.rotationIntervalDays * 86_400_000
}

const AGENT_LABEL: Record<Asset['agentState'], string> = {
  healthy: 'Agent reporting',
  stale: 'Agent gone quiet',
  absent: 'No agent installed',
}

/** The words the BypassBadge uses, so the filter reads like the column. */
const BYPASS_LABEL: Record<Asset['bypassPosture'], string> = {
  enforced: 'Closed — no way around',
  monitored: 'Monitored — a bypass is recorded',
  open: 'Unmonitored — a bypass is invisible',
}

function Assets() {
  const navigate = useNavigate()
  const search = Route.useSearch()

  // Mirrored locally so typing stays instant; the URL is the shareable record
  // rather than the source the input reads back from on every keystroke.
  const [text, setText] = useState(search.q ?? '')

  const setSearch = (patch: Partial<AssetSearch>) =>
    void navigate({ to: '/assets', search: (prev) => ({ ...prev, ...patch }), replace: true })

  const { data: me } = useQuery(meQuery())
  // Editing the fleet is the administrator's; an auditor may look at who is
  // assigned but change nothing. The control plane enforces both — this only
  // keeps the console from offering a button that would be refused.
  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const canSeeAssignments = canEdit || me?.role === 'auditor'

  const [editing, setEditing] = useState<Asset | undefined>()
  const [formOpen, setFormOpen] = useState(false)
  const [assigning, setAssigning] = useState<Asset | undefined>()

  const { data: groups } = useQuery(groupsQuery())
  const { data: assets } = useQuery(
    assetsQuery({
      search: text,
      groupId: search.group ?? null,
      hostKeyState: search.hostKey ?? null,
      agentState: search.agent ?? null,
      bypassPosture: search.bypass ?? null,
      credentialMode: search.credential ?? null,
    }),
  )

  const filtered = Boolean(
    text.trim() || search.group || search.hostKey || search.agent || search.bypass ||
      search.credential,
  )

  const clear = () => {
    setText('')
    void navigate({ to: '/assets', search: {}, replace: true })
  }

  return (
    <Box>
      <PageHeader
        title="Assets"
        description={
          canSeeAssignments
            ? 'Every host Argus can broker a session to. Host-key state is the trust anchor — an unpinned target is one nobody has verified.'
            : 'The hosts assigned to you, and the accounts you may assume on each. Anything else needs an approved access request.'
        }
        actions={
          <Group gap={SP.cozy}>
            {/* Says what this page cannot answer. The inventory lists the hosts
                Argus manages, so a machine nobody enrolled is invisible here by
                construction — which is the question Coverage exists for. */}
            <Tooltip label="This table only shows hosts Argus manages. Coverage also finds the ones it does not.">
              <ButtonLink variant="default" to="/coverage" leftSection={<IconShieldLock size={14} />}>
                Coverage
              </ButtonLink>
            </Tooltip>
            {canEdit && (
              <Button
                leftSection={<IconPlus size={14} />}
                onClick={() => {
                  setEditing(undefined)
                  setFormOpen(true)
                }}
              >
                Add asset
              </Button>
            )}
          </Group>
        }
      />

      <PageBody>
        <Toolbar
          right={
            assets && (
              <Text size={FS.micro} c="dimmed">
                {assets.length} {filtered ? 'matching' : 'assets'}
              </Text>
            )
          }
        >
          <TextInput
            w={260}
            placeholder="Filter by hostname, address, OS or tag"
            leftSection={<IconSearch size={14} />}
            value={text}
            onChange={(e) => {
              setText(e.currentTarget.value)
              setSearch({ q: e.currentTarget.value || undefined })
            }}
          />
          <Select
            w={180}
            placeholder="All groups"
            clearable
            value={search.group ?? null}
            onChange={(v) => setSearch({ group: v ?? undefined })}
            data={groups?.map((g) => ({ value: g.id, label: `${g.name} (${g.assetCount})` })) ?? []}
          />
          <Select
            w={170}
            placeholder="Any host key state"
            clearable
            value={search.hostKey ?? null}
            onChange={(v) => setSearch({ hostKey: (v as AssetSearch['hostKey']) ?? undefined })}
            data={[
              { value: 'unverified', label: 'Not pinned (any)' },
              { value: 'pinned', label: 'Pinned' },
              { value: 'unpinned', label: 'Unpinned' },
              { value: 'changed', label: 'Changed' },
            ]}
          />
          <Select
            w={180}
            placeholder="Any agent state"
            clearable
            value={search.agent ?? null}
            onChange={(v) => setSearch({ agent: (v as Asset['agentState']) ?? undefined })}
            data={AGENT_STATES.map((s) => ({ value: s, label: AGENT_LABEL[s] }))}
          />
          {/* The column exists, so the filter does too — Coverage links here
              with `?bypass=open`, and a filter with no visible control would
              leave the table narrowed for a reason nothing on screen gives. */}
          <Select
            w={230}
            placeholder="Any bypass posture"
            clearable
            value={search.bypass ?? null}
            onChange={(v) => setSearch({ bypass: (v as Asset['bypassPosture']) ?? undefined })}
            data={BYPASS_POSTURES.map((s) => ({ value: s, label: BYPASS_LABEL[s] }))}
          />
          {/* The Overview counts the assets still on a standing credential and
              says to move them; without this there was nowhere to go and see
              which ones. */}
          <Select
            w={240}
            placeholder="Any credential mode"
            clearable
            value={search.credential ?? null}
            onChange={(v) => setSearch({ credential: (v as AssetSearch['credential']) ?? undefined })}
            data={CREDENTIAL_MODES.map((m) => ({ value: m, label: CREDENTIAL_LABEL[m] }))}
          />
          {filtered && (
            <Button
              variant="subtle"
              color="slate"
              size="compact-xs"
              leftSection={<IconX size={12} />}
              onClick={clear}
            >
              Clear
            </Button>
          )}
        </Toolbar>

        <DataTable
          minWidth={980}
          loading={assets === undefined}
          isEmpty={assets?.length === 0}
          columns={
            canSeeAssignments
              ? ['Host', 'Address', 'OS', 'Auth', 'Host key', 'Agent', 'Bypass', 'Rotated', '']
              : ['Host', 'Address', 'OS', 'Auth', 'Host key', 'Agent', 'Bypass', 'Rotated']
          }
          empty={
            <EmptyState
              icon={IconServer2}
              title="No assets match."
              description={
                filtered
                  ? 'Nothing in the inventory fits these filters. Clear one to widen the search.'
                  : canEdit
                    ? 'The inventory is empty. Add a host here, or enrol one Coverage has already found running an agent.'
                    : 'No hosts are assigned to you. An administrator assigns one, or an approved access request covers it temporarily.'
              }
              action={
                filtered ? (
                  <Button variant="light" onClick={clear}>
                    Clear filters
                  </Button>
                ) : canEdit ? (
                  <Button
                    variant="light"
                    leftSection={<IconPlus size={14} />}
                    onClick={() => {
                      setEditing(undefined)
                      setFormOpen(true)
                    }}
                  >
                    Add asset
                  </Button>
                ) : (
                  <ButtonLink variant="light" to="/requests">
                    Request access
                  </ButtonLink>
                )
              }
            />
          }
        >
          {assets?.map((a) => (
            <Table.Tr
              key={a.id}
              {...rowNav(() => navigate({ to: '/assets/$assetId', params: { assetId: a.id } }))}
            >
              <Table.Td>
                <Group gap={SP.cozy} wrap="nowrap">
                  <HealthDot health={a.health} />
                  <Box>
                    <Mono>{a.hostname.split('.')[0]}</Mono>
                    <Text size={FS.micro} c="dimmed">
                      {a.hostname.split('.').slice(1).join('.')}
                    </Text>
                  </Box>
                </Group>
              </Table.Td>
              <Table.Td><Mono c="dimmed">{a.address}:{a.port}</Mono></Table.Td>
              <Table.Td><Text size="xs" c="dimmed">{a.os}</Text></Table.Td>
              <Table.Td><CredentialBadge mode={a.credentialMode} /></Table.Td>
              <Table.Td><HostKeyBadge state={a.hostKeyState} /></Table.Td>
              <Table.Td>
                <AgentBadge state={a.agentState} lastSeen={a.agentLastSeenAt} />
              </Table.Td>
              <Table.Td>
                <BypassBadge posture={a.bypassPosture} unmanagedKeys={a.unmanagedKeyCount} />
              </Table.Td>
              <Table.Td>
                {a.credentialMode === 'ca-certificate' ? (
                  <Tooltip label="Certificate auth — nothing to rotate">
                    <Text size="xs" c="dimmed">n/a</Text>
                  </Tooltip>
                ) : (
                  <Text size="xs" c={rotationOverdue(a) ? 'amber.4' : 'dimmed'}>
                    {relTime(a.credentialRotatedAt)}
                    {rotationOverdue(a) && ' ⚠'}
                  </Text>
                )}
              </Table.Td>
              {canSeeAssignments && (
                // Stops the row's own navigation: these open a panel about this
                // host rather than taking you away from it.
                <Table.Td align="right" onClick={(e) => e.stopPropagation()}>
                  <Group gap={SP.tight} justify="flex-end" wrap="nowrap">
                    <Tooltip label="Who can reach this host">
                      <ActionIcon
                        variant="subtle"
                        color="slate"
                        aria-label={`Who can reach ${a.hostname}`}
                        onClick={() => setAssigning(a)}
                      >
                        <IconUsers size={14} />
                      </ActionIcon>
                    </Tooltip>
                    {canEdit && (
                      <Tooltip label="Edit this asset">
                        <ActionIcon
                          variant="subtle"
                          color="slate"
                          aria-label={`Edit ${a.hostname}`}
                          onClick={() => {
                            setEditing(a)
                            setFormOpen(true)
                          }}
                        >
                          <IconPencil size={14} />
                        </ActionIcon>
                      </Tooltip>
                    )}
                  </Group>
                </Table.Td>
              )}
            </Table.Tr>
          ))}
        </DataTable>
      </PageBody>

      {/* Rendered only while open, so the chunk is fetched the first time an
          administrator actually opens one. */}
      <Suspense fallback={null}>
        {formOpen && canEdit && (
          <AssetForm opened={formOpen} onClose={() => setFormOpen(false)} asset={editing} />
        )}
        {assigning && canSeeAssignments && (
          <AssetAssignments
            asset={assigning}
            opened={Boolean(assigning)}
            onClose={() => setAssigning(undefined)}
            canEdit={Boolean(canEdit)}
          />
        )}
      </Suspense>
    </Box>
  )
}
