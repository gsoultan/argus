import { ActionIcon, Alert, Button, Group, Modal, Select, Skeleton, Stack, Switch, Text, TextInput, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconUser, IconUsersGroup, IconX } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import { run } from '~/lib/notify'
import { teamsQuery, usersQuery } from '~/lib/queries'
import {
  checkViewersQuery, useAddCheckGroup, useRenameCheckGroup, useSetCheckViewers, type CheckGroup, type CheckViewer, type ViewerTarget,
} from '~/lib/websites'
import { FS } from '~/theme'

/*
 * Who sees a check or a group, beyond admins, owners and auditors, who see
 * every check. The control plane decides (internal/control/checks_access.go):
 * a person or team given a group sees every check in it, and "can check now
 * and pause" adds those two buttons. Adding, changing and removing checks
 * stays with admins and owners.
 */

const keyOf = (v: CheckViewer) => (v.email ? `u:${v.email}` : `t:${v.teamId}`)

/** "Who can see this" for one check or one group, edited and saved as one list. */
export function WhoCanSee({ target, id, name, onClose }: { target: ViewerTarget; id: string; name: string; onClose: () => void }) {
  const { data: saved, isLoading, error } = useQuery(checkViewersQuery(target, id))
  const { data: people } = useQuery(usersQuery())
  const { data: teams } = useQuery(teamsQuery())
  const save = useSetCheckViewers(target, id)
  const [list, setList] = useState<CheckViewer[] | null>(null)
  useEffect(() => { if (saved && list === null) setList(saved) }, [saved, list])

  const rows = list ?? []
  const taken = new Set(rows.map(keyOf))
  const options = [
    {
      group: 'Teams',
      items: (teams ?? []).filter((t) => !taken.has(`t:${t.id}`)).map((t) => ({ value: `t:${t.id}`, label: t.name })),
    },
    {
      group: 'People',
      items: (people ?? [])
        .filter((p) => !taken.has(`u:${p.email.toLowerCase()}`))
        .map((p) => ({ value: `u:${p.email.toLowerCase()}`, label: p.displayName && p.displayName !== p.email ? `${p.displayName} · ${p.email}` : p.email })),
    },
  ].filter((g) => g.items.length > 0)

  const add = (v: string | null) => {
    if (!v) return
    if (v.startsWith('t:')) {
      const t = teams?.find((x) => `t:${x.id}` === v)
      if (t) setList([...rows, { teamId: t.id, teamName: t.name, canOperate: false }])
    } else {
      setList([...rows, { email: v.slice(2), canOperate: false }])
    }
  }

  return (
    <Modal opened onClose={onClose} title={<Text fw={700}>Who can see {name}</Text>} size="lg">
      <Stack gap="md">
        <Text size={FS.body} c="dimmed">
          Admins, owners and auditors see every check. Add the people and teams who should see
          {target === 'group' ? ' every check in this group' : ' this check'} too. They see whether it is up, its uptime and its outages,
          and are told in their bell when it goes down. What it sends and where its alerts go stay hidden from them.
        </Text>
        {isLoading ? <Skeleton h={120} /> : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read who sees it: {error.message}</Text></Alert>
        ) : (
          <>
            <Select
              aria-label="Add a person or team"
              placeholder="Add a person or team"
              searchable
              value={null}
              onChange={add}
              data={options}
              nothingFoundMessage="Nobody else to add"
              comboboxProps={{ withinPortal: true }}
            />
            {rows.length === 0 ? (
              <Text size={FS.body} c="dimmed" data-testid="viewers-empty">Only admins, owners and auditors see it.</Text>
            ) : (
              <Stack gap={6} data-testid="viewers">
                {rows.map((v) => (
                  <Group key={keyOf(v)} justify="space-between" wrap="nowrap" gap="sm"
                    style={{ border: '1px solid var(--color-line)', borderRadius: 8, padding: '8px 10px' }}>
                    <Group gap={8} wrap="nowrap" style={{ minWidth: 0 }}>
                      {v.teamId ? <IconUsersGroup size={16} /> : <IconUser size={16} />}
                      <Text size="sm" truncate>{v.teamId ? `Team ${v.teamName ?? ''}` : v.email}</Text>
                    </Group>
                    <Group gap="sm" wrap="nowrap">
                      <Switch size="xs" label="Can check now and pause" checked={v.canOperate}
                        onChange={(e) => {
                          const on = e.currentTarget.checked
                          setList(rows.map((x) => (keyOf(x) === keyOf(v) ? { ...x, canOperate: on } : x)))
                        }} />
                      <Tooltip label="Remove">
                        <ActionIcon variant="subtle" color="gray" aria-label={`Remove ${v.email ?? v.teamName}`}
                          onClick={() => setList(rows.filter((x) => keyOf(x) !== keyOf(v)))}>
                          <IconX size={14} />
                        </ActionIcon>
                      </Tooltip>
                    </Group>
                  </Group>
                ))}
              </Stack>
            )}
          </>
        )}
        <Group justify="flex-end" gap="xs">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={save.isPending} disabled={list === null}
            onClick={() => void run(() => save.mutateAsync(rows.map((v) => (v.teamId
              ? { teamId: v.teamId, canOperate: v.canOperate }
              : { email: v.email, canOperate: v.canOperate }))), {
              failure: 'Not saved', success: ['Saved', `Who can see ${name} is updated.`],
            }).then((ok) => { if (ok) onClose() })}>
            Save
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** Add a group, or rename one. */
export function GroupForm({ group, onClose }: { group?: CheckGroup; onClose: () => void }) {
  const [name, setName] = useState(group?.name ?? '')
  const addGroup = useAddCheckGroup()
  const rename = useRenameCheckGroup()
  const busy = addGroup.isPending || rename.isPending
  const submit = () => void run(
    () => (group ? rename.mutateAsync({ id: group.id, name }) : addGroup.mutateAsync(name)),
    { failure: group ? 'Not renamed' : 'Not added', success: group ? ['Renamed', `The group is now ${name.trim()}.`] : ['Added', `Move checks into ${name.trim()} from each check.`] },
  ).then((ok) => { if (ok) onClose() })
  return (
    <Modal opened onClose={onClose} title={<Text fw={700}>{group ? 'Rename group' : 'New group'}</Text>}>
      <form onSubmit={(e) => { e.preventDefault(); submit() }}>
        <Stack gap="md">
          <TextInput label="Name" placeholder="Public APIs" maxLength={60} value={name} data-autofocus
            description="Checks in a group show together, and whoever is given the group sees all of them."
            onChange={(e) => setName(e.currentTarget.value)} />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button type="submit" loading={busy} disabled={name.trim() === ''}>{group ? 'Rename' : 'Add group'}</Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
