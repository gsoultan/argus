import { useState } from 'react'
import {
  ActionIcon, Alert, Avatar, Badge, Box, Button, Checkbox, Group, Modal, SegmentedControl, Select, Stack,
  Switch, Text, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconPencil, IconPlus, IconShieldCheck, IconUser, IconUsersGroup, IconX,
} from '@tabler/icons-react'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  assignmentsQuery, teamAssignmentsQuery, teamsQuery, useRemoveAssignment, useRemoveTeamAssignment,
  useSetAssignment, useSetTeamAssignment, usersQuery,
} from '~/lib/queries'
import { isElevatedAccount } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { Asset, TeamAssignment, TeamDirectSsh } from '~/types/domain'

/**
 * Who can connect to one server: admins always, then every team and person it
 * was given to, each with the accounts they log in as.
 *
 * This is the one place a server is given out after it was added, so the
 * button is on the list itself rather than behind an edit dialog.
 */
export function WhoCanConnect({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data: people } = useQuery(assignmentsQuery(asset.id))
  const { data: teams } = useQuery(teamAssignmentsQuery(asset.id))
  const { data: users } = useQuery({ ...usersQuery(), enabled: canEdit })
  const removePerson = useRemoveAssignment()
  const removeTeam = useRemoveTeamAssignment()
  const [adding, add] = useDisclosure(false)
  const [editing, setEditing] = useState<TeamAssignment | null>(null)
  const names = new Map((users ?? []).map((u) => [u.email, u.displayName]))

  const remove = async (kind: 'team' | 'person', id: string, label: string) => {
    try {
      if (kind === 'team') await removeTeam.mutateAsync({ assetId: asset.id, teamId: id })
      else await removePerson.mutateAsync({ assetId: asset.id, email: id })
      notifyOk('Removed', `${label} can no longer connect to ${asset.hostname}.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }

  const empty = (teams?.length ?? 0) === 0 && (people?.length ?? 0) === 0

  return (
    <Stack gap="xs">
      <Row
        icon={<IconShieldCheck size={16} />}
        name="Admins"
        sub="Always, as any account"
      />
      {teams?.map((g) => (
        <Row
          key={g.teamId}
          icon={<IconUsersGroup size={16} />}
          name={g.teamName}
          sub={`Team · ${describeTeamAccess(g)}`}
          onEdit={canEdit ? () => setEditing(g) : undefined}
          onRemove={canEdit ? () => void remove('team', g.teamId, g.teamName) : undefined}
        />
      ))}
      {people?.map((g) => (
        <Row
          key={g.userEmail}
          icon={<IconUser size={16} />}
          name={names.get(g.userEmail) ?? g.userEmail}
          sub={`as ${g.principals.join(', ')}`}
          onRemove={canEdit ? () => void remove('person', g.userEmail, g.userEmail) : undefined}
        />
      ))}
      {empty && (
        <Text size={FS.meta} c="dimmed">
          Only admins so far.
        </Text>
      )}
      {canEdit && (
        <Button
          variant="light"
          leftSection={<IconPlus size={16} />}
          onClick={add.open}
          style={{ alignSelf: 'flex-start' }}
        >
          Add team or person
        </Button>
      )}
      <AccountProblems asset={asset} />
      {canEdit && <AddAccessModal asset={asset} opened={adding} onClose={add.close} />}
      {canEdit && editing && (
        <AddAccessModal asset={asset} opened editing={editing} onClose={() => setEditing(null)} />
      )}
    </Stack>
  )
}

/** What a team was given, in a few words for its row. */
export function describeTeamAccess(g: Pick<TeamAssignment, 'principals' | 'personalAccounts' | 'sudo' | 'directSsh'>): string {
  const parts: string[] = []
  if (g.personalAccounts) {
    let own = 'own accounts'
    if (g.sudo) own += ' with sudo'
    parts.push(own)
  }
  if (g.principals.length > 0) parts.push(`as ${g.principals.join(', ')}`)
  if (g.personalAccounts && g.directSsh === 'allow') parts.push('direct SSH')
  if (g.personalAccounts && g.directSsh === 'approve') parts.push('direct SSH after approval')
  return parts.join(' · ')
}

/**
 * Personal accounts the agent could not make or keep, said where the access
 * is given, since that is where it is fixed.
 */
function AccountProblems({ asset }: { asset: Asset }) {
  const bad = (asset.agentPersonalAccounts ?? []).filter((s) => s.state === 'taken' || s.state === 'failed')
  if (bad.length === 0) return null
  return (
    <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="Some personal accounts are not ready">
      <Stack gap={SP.tight}>
        {bad.map((s) => (
          <Text key={s.name} size={FS.meta}>
            <Text span ff="monospace" fw={600}>{s.name}</Text>
            {s.owner ? ` (${s.owner})` : ''}: {s.state === 'taken'
              ? 'the server already has an account with this name that Argus did not make. Give the person another server account name on Teams, under People.'
              : s.detail ?? 'the agent could not set it up.'}
          </Text>
        ))}
      </Stack>
    </Alert>
  )
}

function Row({ icon, name, sub, onEdit, onRemove }: {
  icon: React.ReactNode
  name: string
  sub: string
  onEdit?: () => void
  onRemove?: () => void
}) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm" py={SP.tight}>
      <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
        <Avatar size={32} radius="xl" color="azure" variant="light">{icon}</Avatar>
        <Box style={{ minWidth: 0 }}>
          <Text size="sm" fw={500} truncate>{name}</Text>
          <Text size={FS.meta} c="dimmed" truncate>{sub}</Text>
        </Box>
      </Group>
      <Group gap={2} wrap="nowrap">
      {onEdit && (
        <Tooltip label={`Change what ${name} gets`}>
          <ActionIcon variant="subtle" color="slate" aria-label={`Change ${name}`} onClick={onEdit}>
            <IconPencil size={16} />
          </ActionIcon>
        </Tooltip>
      )}
      {onRemove && (
        <Tooltip label={`Remove ${name}`}>
          <ActionIcon variant="subtle" color="slate" aria-label={`Remove ${name}`} onClick={onRemove}>
            <IconX size={16} />
          </ActionIcon>
        </Tooltip>
      )}
      </Group>
    </Group>
  )
}

/**
 * Give this server to a team or one person. Starts on Team, with an account
 * of each person's own, because that is the way meant to be used most.
 *
 * With `editing`, changes what one team already has.
 */
function AddAccessModal({ asset, opened, onClose, editing }: {
  asset: Asset
  opened: boolean
  onClose: () => void
  editing?: TeamAssignment
}) {
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const setTeam = useSetTeamAssignment()
  const setPerson = useSetAssignment()
  const [kind, setKind] = useState<'team' | 'person'>('team')
  const [who, setWho] = useState<string | null>(editing?.teamId ?? null)
  const plain = asset.principals.filter((p) => !isElevatedAccount(p))
  const [personal, setPersonal] = useState(editing ? !!editing.personalAccounts : true)
  const [sudo, setSudo] = useState(!!editing?.sudo)
  const [direct, setDirect] = useState<TeamDirectSsh>(editing?.directSsh ?? 'off')
  const [accounts, setAccounts] = useState<string[]>(
    editing ? editing.principals : personal ? [] : plain.length > 0 ? [plain[0]!] : [],
  )
  const ownAccounts = kind === 'team' && personal

  const close = () => {
    if (!editing) setWho(null)
    onClose()
  }
  const save = async () => {
    if (!who) return
    try {
      if (kind === 'team') {
        await setTeam.mutateAsync({
          assetId: asset.id, teamId: who,
          access: {
            principals: accounts, personalAccounts: personal,
            sudo: personal && sudo, directSsh: personal ? direct : 'off',
          },
        })
      } else {
        await setPerson.mutateAsync({ assetId: asset.id, email: who, principals: accounts })
      }
      const label = kind === 'team' ? (teams?.find((t) => t.id === who)?.name ?? 'The team') : who
      const how = kind === 'team'
        ? describeTeamAccess({ principals: accounts, personalAccounts: personal, sudo, directSsh: direct })
        : `as ${accounts.join(', ')}`
      notifyOk(editing ? 'Access changed' : 'Access given', `${label} can connect to ${asset.hostname}: ${how}.`)
      close()
    } catch (err) {
      notifyError(editing ? 'Access not changed' : 'Access not given', err)
    }
  }

  const options =
    kind === 'team'
      ? (teams ?? []).map((t) => ({ value: t.id, label: `${t.name} (${t.members.length})` }))
      : (users ?? [])
          .filter((u) => !u.disabled && u.role !== 'admin' && u.role !== 'owner')
          .map((u) => ({ value: u.email, label: `${u.displayName} (${u.email})` }))

  const short = asset.hostname.split('.')[0]
  const canSave = !!who && (accounts.length > 0 || ownAccounts)

  return (
    <Modal
      opened={opened}
      onClose={close}
      title={editing ? `What ${editing.teamName} gets on ${short}` : `Who else can connect to ${short}`}
    >
      <Stack gap="md">
        {!editing && (
          <SegmentedControl
            fullWidth
            value={kind}
            onChange={(v) => {
              setKind(v as 'team' | 'person')
              setWho(null)
            }}
            data={[
              { value: 'team', label: 'A team' },
              { value: 'person', label: 'One person' },
            ]}
          />
        )}
        {!editing && (
          <Select
            label={kind === 'team' ? 'Team' : 'Person'}
            placeholder={kind === 'team' ? 'Pick a team' : 'Pick a person'}
            searchable
            data={options}
            value={who}
            onChange={setWho}
            nothingFoundMessage={kind === 'team' ? 'No teams yet. Make one on Teams.' : 'Nobody to add'}
          />
        )}
        {kind === 'team' && (
          <Switch
            checked={personal}
            onChange={(e) => setPersonal(e.currentTarget.checked)}
            label="Give each person their own account"
            description={`Argus makes a login on ${short} for everyone in the team, named after them, and locks it when they leave.`}
          />
        )}
        {ownAccounts && (
          <Stack gap="sm" pl="xl">
            <Switch
              checked={sudo}
              onChange={(e) => setSudo(e.currentTarget.checked)}
              label="Sudo without a password"
              description="Their accounts can run anything as root. They have no password to type."
            />
            <Box>
              <Text size="sm" fw={500}>SSH straight to the server</Text>
              <Text size={FS.meta} c="dimmed" mb={SP.snug}>
                With their own SSH key, without opening Argus. Every session is still recorded in their name.
              </Text>
              <SegmentedControl
                fullWidth
                value={direct}
                onChange={(v) => setDirect(v as TeamDirectSsh)}
                data={[
                  { value: 'off', label: 'No' },
                  { value: 'approve', label: 'Ask an admin' },
                  { value: 'allow', label: 'Yes' },
                ]}
              />
            </Box>
          </Stack>
        )}
        {asset.principals.length > 0 && (
          <Checkbox.Group
            label={ownAccounts ? 'Shared accounts too (optional)' : 'Log in as'}
            value={accounts}
            onChange={setAccounts}
          >
            <Stack gap={SP.snug} mt={SP.snug}>
              {asset.principals.map((p) => (
                <Checkbox
                  key={p}
                  value={p}
                  label={
                    <Group gap={SP.snug}>
                      <Text size="sm" ff="monospace">{p}</Text>
                      {isElevatedAccount(p) && <Badge size="xs" color="rose">full control</Badge>}
                    </Group>
                  }
                />
              ))}
            </Stack>
          </Checkbox.Group>
        )}
        {!ownAccounts && asset.principals.length === 0 && (
          <Text size={FS.meta} c="dimmed">
            {kind === 'team'
              ? 'This server has no shared accounts yet, so give the team their own accounts, or add one with Edit at the top.'
              : 'Add a login account first, with Edit at the top.'}
          </Text>
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button
            disabled={!canSave}
            loading={setTeam.isPending || setPerson.isPending}
            onClick={() => void save()}
          >
            {editing ? 'Save' : 'Give access'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
