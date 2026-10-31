import { useState } from 'react'
import {
  ActionIcon, Avatar, Badge, Box, Button, Checkbox, Group, Modal, SegmentedControl, Select, Stack,
  Text, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconPlus, IconShieldCheck, IconUser, IconUsersGroup, IconX } from '@tabler/icons-react'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  assignmentsQuery, teamAssignmentsQuery, teamsQuery, useRemoveAssignment, useRemoveTeamAssignment,
  useSetAssignment, useSetTeamAssignment, usersQuery,
} from '~/lib/queries'
import { isElevatedAccount } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

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
          sub={`Team · as ${g.principals.join(', ')}`}
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
          disabled={asset.principals.length === 0}
          style={{ alignSelf: 'flex-start' }}
        >
          Add team or person
        </Button>
      )}
      {canEdit && asset.principals.length === 0 && (
        <Text size={FS.meta} c="dimmed">Add a login account first, with Edit at the top.</Text>
      )}
      {canEdit && <AddAccessModal asset={asset} opened={adding} onClose={add.close} />}
    </Stack>
  )
}

function Row({ icon, name, sub, onRemove }: {
  icon: React.ReactNode
  name: string
  sub: string
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
      {onRemove && (
        <Tooltip label={`Remove ${name}`}>
          <ActionIcon variant="subtle" color="slate" aria-label={`Remove ${name}`} onClick={onRemove}>
            <IconX size={16} />
          </ActionIcon>
        </Tooltip>
      )}
    </Group>
  )
}

/**
 * Give this server to a team or one person, as one or more of its accounts.
 * Starts on Team, because that is the way meant to be used most.
 */
function AddAccessModal({ asset, opened, onClose }: { asset: Asset; opened: boolean; onClose: () => void }) {
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const setTeam = useSetTeamAssignment()
  const setPerson = useSetAssignment()
  const [kind, setKind] = useState<'team' | 'person'>('team')
  const [who, setWho] = useState<string | null>(null)
  const plain = asset.principals.filter((p) => !isElevatedAccount(p))
  const [accounts, setAccounts] = useState<string[]>(plain.length > 0 ? [plain[0]!] : [])

  const close = () => {
    setWho(null)
    onClose()
  }
  const save = async () => {
    if (!who) return
    try {
      if (kind === 'team') {
        await setTeam.mutateAsync({ assetId: asset.id, teamId: who, principals: accounts })
      } else {
        await setPerson.mutateAsync({ assetId: asset.id, email: who, principals: accounts })
      }
      const label = kind === 'team' ? (teams?.find((t) => t.id === who)?.name ?? 'The team') : who
      notifyOk('Access given', `${label} can connect to ${asset.hostname} as ${accounts.join(', ')}.`)
      close()
    } catch (err) {
      notifyError('Access not given', err)
    }
  }

  const options =
    kind === 'team'
      ? (teams ?? []).map((t) => ({ value: t.id, label: `${t.name} (${t.members.length})` }))
      : (users ?? [])
          .filter((u) => !u.disabled && u.role !== 'admin' && u.role !== 'owner')
          .map((u) => ({ value: u.email, label: `${u.displayName} (${u.email})` }))

  return (
    <Modal opened={opened} onClose={close} title={`Who else can connect to ${asset.hostname.split('.')[0]}`}>
      <Stack gap="md">
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
        <Select
          label={kind === 'team' ? 'Team' : 'Person'}
          placeholder={kind === 'team' ? 'Pick a team' : 'Pick a person'}
          searchable
          data={options}
          value={who}
          onChange={setWho}
          nothingFoundMessage={kind === 'team' ? 'No teams yet. Make one on Teams.' : 'Nobody to add'}
        />
        <Checkbox.Group label="Log in as" value={accounts} onChange={setAccounts}>
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
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button
            disabled={!who || accounts.length === 0}
            loading={setTeam.isPending || setPerson.isPending}
            onClick={() => void save()}
          >
            Give access
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
