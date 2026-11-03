import { useMemo, useState } from 'react'
import { ActionIcon, Box, Button, Group, Loader, Menu, Modal, Radio, Stack, Text, TextInput, UnstyledButton } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconDots, IconSearch, IconUsers } from '@tabler/icons-react'
import { FS } from '~/theme'
import { savedQueriesQuery, useDeleteSavedQuery, useSaveQuery } from '~/lib/queries'
import { notifyError, notifyOk } from '~/lib/notify'
import type { SavedQuery } from '~/lib/live'

/*
Saved queries, beside Objects and History: the person's own, and the ones
others shared on this database. They are kept in Argus, so they follow the
person to any browser; opening one puts its text in a tab, and running it is
the person's own statement with their own access.
*/

export function SavedQueriesList({ databaseId, onOpen, admin }: {
  databaseId: string
  onOpen: (q: SavedQuery) => void
  admin: boolean
}) {
  const { data, isPending, error } = useQuery(savedQueriesQuery(databaseId))
  const del = useDeleteSavedQuery(databaseId)
  const save = useSaveQuery(databaseId)
  const [find, setFind] = useState('')
  const list = useMemo(() => {
    const n = find.trim().toLowerCase()
    return (data ?? []).filter((q) => !n || q.name.toLowerCase().includes(n) || q.sql.toLowerCase().includes(n))
  }, [data, find])
  const mine = list.filter((q) => q.mine)
  const shared = list.filter((q) => !q.mine)

  const remove = async (q: SavedQuery) => {
    try {
      await del.mutateAsync(q.id)
      notifyOk('Removed', `${q.name} is no longer saved.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  const share = async (q: SavedQuery, on: boolean) => {
    try {
      await save.mutateAsync({ id: q.id, name: q.name, sql: q.sql, shared: on })
      notifyOk(on ? 'Shared' : 'Not shared', on ? `Everyone who can open this database sees ${q.name}.` : `Only you see ${q.name} now.`)
    } catch (err) {
      notifyError('Not changed', err)
    }
  }

  const row = (q: SavedQuery) => (
    <Group key={q.id} gap={2} wrap="nowrap" style={{ borderRadius: 6 }}>
      <UnstyledButton onClick={() => onOpen(q)} px="xs" py={6} style={{ flex: 1, minWidth: 0 }} title={q.sql}>
        <Group gap={6} wrap="nowrap">
          <Text size={FS.meta} fw={600} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{q.name}</Text>
          {q.mine && q.shared && <IconUsers size={12} color="var(--mantine-color-dimmed)" aria-label="shared" />}
          {!q.mine && <Text size={FS.micro} c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap' }}>{q.owner.split('@')[0]}</Text>}
        </Group>
        <Text size={FS.micro} c="dimmed" ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {q.sql.replace(/\s+/g, ' ').trim()}
        </Text>
      </UnstyledButton>
      {(q.mine || admin) && (
        <Menu position="bottom-end" withinPortal>
          <Menu.Target>
            <ActionIcon size="sm" variant="subtle" aria-label={`More for ${q.name}`}><IconDots size={14} /></ActionIcon>
          </Menu.Target>
          <Menu.Dropdown>
            {q.mine && <Menu.Item onClick={() => void share(q, !q.shared)}>{q.shared ? 'Stop sharing' : 'Share with everyone who can open this database'}</Menu.Item>}
            <Menu.Item color="rose" onClick={() => void remove(q)}>Remove</Menu.Item>
          </Menu.Dropdown>
        </Menu>
      )}
    </Group>
  )

  const head = (s: string) => <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} px={4} pt="xs" pb={2} style={{ letterSpacing: '.06em' }}>{s}</Text>
  return (
    <Stack gap={2}>
      <TextInput size="xs" placeholder="Find a saved query" leftSection={<IconSearch size={12} />} value={find}
        onChange={(e) => setFind(e.currentTarget.value)} aria-label="Find a saved query" mb={4} />
      {isPending && <Loader size={14} ml="sm" />}
      {error && <Text size={FS.meta} c="rose" px="xs">{(error as Error).message}</Text>}
      {data && (
        <>
          {head('Mine')}
          {mine.length === 0 && <Text size={FS.micro} c="dimmed" px="xs">{find ? 'Nothing matches.' : 'Save a tab with ☆ Save or Ctrl+S.'}</Text>}
          {mine.map(row)}
          {head('Shared on this database')}
          {shared.length === 0 && <Text size={FS.micro} c="dimmed" px="xs">{find ? 'Nothing matches.' : 'Nothing shared yet.'}</Text>}
          {shared.map(row)}
          <Text size={FS.micro} c="dimmed" px="xs" pt="sm">
            Opening one puts it in a new tab. Running a saved query uses your own access, never the author’s.
          </Text>
        </>
      )}
    </Stack>
  )
}

/** The Save dialog: a name and who sees it. */
export function SaveQueryDialog({ opened, onClose, databaseId, databaseName, initial, onSaved }: {
  opened: boolean
  onClose: () => void
  databaseId: string
  databaseName: string
  initial: { id?: string; name: string; sql: string; shared: boolean }
  onSaved: (q: SavedQuery) => void
}) {
  const save = useSaveQuery(databaseId)
  const [name, setName] = useState(initial.name)
  const [shared, setShared] = useState(initial.shared)
  const [key, setKey] = useState(initial)
  if (key !== initial) {
    setKey(initial)
    setName(initial.name)
    setShared(initial.shared)
  }
  const submit = async () => {
    try {
      const q = await save.mutateAsync({ id: initial.id, name: name.trim(), sql: initial.sql, shared })
      notifyOk('Saved', shared ? `${q.name} is saved and shared on ${databaseName}.` : `${q.name} is saved. Only you see it.`)
      onSaved(q)
      onClose()
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={initial.id ? 'Save changes' : 'Save query'} centered size="sm">
      <Box component="form" onSubmit={(e) => { e.preventDefault(); void submit() }}>
        <Stack gap="sm">
          <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={120} data-autofocus required />
          <Radio.Group label="Who can see it" value={shared ? 'shared' : 'me'} onChange={(v) => setShared(v === 'shared')}>
            <Stack gap={8} mt={6}>
              <Radio value="me" label="Only me" />
              <Radio value="shared" label={`Everyone who can open ${databaseName}`}
                description="They run it with their own access. You can stop sharing later." />
            </Stack>
          </Radio.Group>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button type="submit" loading={save.isPending} disabled={!name.trim()}>Save</Button>
          </Group>
        </Stack>
      </Box>
    </Modal>
  )
}
