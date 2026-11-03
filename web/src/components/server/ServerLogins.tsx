import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Divider, Group, Modal, Stack, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconLock, IconLockOpen, IconPlus, IconTrash } from '@tabler/icons-react'
import { relTime } from '~/components/primitives'
import { run } from '~/lib/notify'
import { serverLoginsQuery, useServerLoginAction } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { Asset, ServerLogin } from '~/types/domain'

/**
 * Login accounts an admin has the server's agent make, lock or delete.
 *
 * Removing one locks it: nobody can log in, what runs as it is stopped, and
 * its home and files stay, so it can be unlocked later. Deleting it for good,
 * home folder included, is a second step on a locked account, behind typing
 * its name. Each step is in the audit log. What the agent last said it did is
 * shown beside what was asked, so "asked" never reads as "done".
 */

const NAME = /^[a-z_][a-z0-9._-]{0,31}$/

type Status = { label: string; color: string; detail?: string }

/** What Argus asked for, said with what the agent last reported doing. */
export function loginStatus(l: ServerLogin): Status {
  if (l.agent === 'taken') {
    return { label: 'Name taken', color: 'rose', detail: l.agentDetail ?? 'The server already has an account by this name that Argus did not make. Argus leaves it alone.' }
  }
  if (l.agent === 'failed') {
    return { label: 'Failed', color: 'rose', detail: l.agentDetail ?? 'The agent could not do it.' }
  }
  switch (l.state) {
    case 'active':
      return l.agent === 'ready'
        ? { label: 'Ready', color: 'teal' }
        : { label: 'Waiting for the agent', color: 'amber', detail: 'Made at the agent’s next check-in, within a minute.' }
    case 'locked':
      return l.agent === 'locked'
        ? { label: 'Locked', color: 'slate', detail: 'Nobody can log in. Its home and files are kept.' }
        : { label: 'Locking', color: 'amber', detail: 'Locked at the agent’s next check-in.' }
    case 'delete':
      return { label: 'Deleting', color: 'amber', detail: 'Deleted with its home folder at the agent’s next check-in.' }
    case 'deleted':
      return { label: 'Deleted', color: 'slate', detail: 'Deleted from the server with its home folder.' }
  }
}

export function ServerLogins({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data: logins, isLoading, isError } = useQuery(serverLoginsQuery(asset.id))
  const act = useServerLoginAction()
  const [adding, setAdding] = useState(false)
  const [locking, setLocking] = useState<string>()
  const [deleting, setDeleting] = useState<string>()
  const agentless = asset.agentState === 'absent'
  const short = asset.hostname.split('.')[0] ?? asset.hostname

  const unlock = (name: string) =>
    void run(() => act.mutateAsync({ id: asset.id, name, action: 'create' }), {
      failure: `Could not unlock ${name}`,
      success: ['Unlocking', `${name} on ${short} opens again at the agent’s next check-in.`],
    })

  return (
    <Card padding="md">
      <Group justify="space-between" align="flex-start" gap="sm" mb="sm">
        <Box style={{ flex: '1 1 220px', minWidth: 0 }}>
          <Text size="sm" fw={600}>Made by Argus</Text>
          <Text size={FS.meta} c="dimmed" lh={1.5}>
            Accounts the server&rsquo;s agent made from here. Only admins can add or remove them, and
            each change is in the audit log.
          </Text>
        </Box>
        {canEdit && (
          <Button
            size="sm"
            variant="default"
            leftSection={<IconPlus size={16} />}
            onClick={() => setAdding(true)}
            disabled={agentless}
          >
            Add login account
          </Button>
        )}
      </Group>

      {agentless && canEdit && (
        <Text size={FS.meta} c="dimmed" mb="sm">
          This server has no agent, so Argus cannot make accounts on it.
        </Text>
      )}
      {isError && <Text size={FS.meta} c="rose">Could not read the accounts Argus made here.</Text>}
      {!isLoading && !isError && logins?.length === 0 && (
        <Text size={FS.meta} c="dimmed">None yet.</Text>
      )}

      <Stack gap={0}>
        {logins?.map((l, i) => {
          const st = loginStatus(l)
          const gone = l.state === 'deleted'
          return (
            <Box key={l.name}>
              {i > 0 && <Divider my={SP.tight} />}
              <Group justify="space-between" align="center" gap="sm" py={SP.tight} wrap="wrap">
                <Box style={{ flex: '1 1 200px', minWidth: 0, opacity: gone ? 0.6 : 1 }}>
                  <Group gap="xs" wrap="nowrap">
                    <Text size="sm" fw={600} ff="monospace" truncate>{l.name}</Text>
                    <Badge size="sm" variant="light" color={st.color} style={{ flexShrink: 0 }}>{st.label}</Badge>
                  </Group>
                  <Text size={FS.meta} c="dimmed" lh={1.45}>
                    {st.detail ? `${st.detail} ` : ''}
                    {l.state === 'active'
                      ? `Added by ${l.createdBy} ${relTime(l.createdAt)}.`
                      : `By ${l.changedBy} ${relTime(l.changedAt)}.`}
                  </Text>
                </Box>
                {canEdit && !gone && (
                  <Group gap="xs" wrap="nowrap">
                    {l.state === 'active' && (
                      <Button size="xs" variant="default" leftSection={<IconLock size={14} />}
                        onClick={() => setLocking(l.name)} aria-label={`Remove ${l.name}`}>
                        Remove
                      </Button>
                    )}
                    {l.state === 'locked' && (
                      <>
                        <Button size="xs" variant="default" leftSection={<IconLockOpen size={14} />}
                          onClick={() => unlock(l.name)} loading={act.isPending && act.variables?.name === l.name}>
                          Unlock
                        </Button>
                        <Button size="xs" variant="light" color="rose" leftSection={<IconTrash size={14} />}
                          onClick={() => setDeleting(l.name)} aria-label={`Delete ${l.name} for good`}>
                          Delete for good
                        </Button>
                      </>
                    )}
                  </Group>
                )}
              </Group>
            </Box>
          )
        })}
      </Stack>

      <AddLoginModal asset={asset} opened={adding} onClose={() => setAdding(false)} />
      <LockModal asset={asset} name={locking} onClose={() => setLocking(undefined)} />
      <DeleteModal asset={asset} name={deleting} onClose={() => setDeleting(undefined)} />
    </Card>
  )
}

function AddLoginModal({ asset, opened, onClose }: { asset: Asset; opened: boolean; onClose: () => void }) {
  const act = useServerLoginAction()
  const [name, setName] = useState('')
  const [touched, setTouched] = useState(false)
  const short = asset.hostname.split('.')[0] ?? asset.hostname
  const error = !NAME.test(name)
    ? 'Lowercase letters, digits, dot, underscore and hyphen, starting with a letter or underscore. Up to 32.'
    : asset.principals.includes(name)
      ? `${short} already lists ${name}. Argus only makes new accounts.`
      : undefined
  const close = () => { setName(''); setTouched(false); onClose() }
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setTouched(true)
    if (error) return
    const ok = await run(() => act.mutateAsync({ id: asset.id, name, action: 'create' }), {
      failure: 'Login account not added',
      success: ['Login account added', `The agent on ${short} makes ${name} at its next check-in. It is on the server’s login accounts now, so you can give it to people.`],
    })
    if (ok) close()
  }
  return (
    <Modal opened={opened} onClose={close} title="Add a login account" size="md">
      <form onSubmit={submit}>
        <Stack gap="md">
          <TextInput
            label="Account name"
            description={`The server's agent makes this account on ${short}, with a home folder and no password. People reach it through Argus once you give it to them under Who can connect.`}
            placeholder="deploy"
            value={name}
            onChange={(e) => setName(e.currentTarget.value.trim())}
            onBlur={() => setTouched(true)}
            error={touched && name !== '' ? error : undefined}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            data-autofocus
          />
          <Group justify="flex-end">
            <Button variant="subtle" color="slate" onClick={close}>Cancel</Button>
            <Button type="submit" disabled={!!error} loading={act.isPending}>Add account</Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

function LockModal({ asset, name, onClose }: { asset: Asset; name?: string; onClose: () => void }) {
  const act = useServerLoginAction()
  const short = asset.hostname.split('.')[0] ?? asset.hostname
  const go = async () => {
    if (!name) return
    const ok = await run(() => act.mutateAsync({ id: asset.id, name, action: 'lock' }), {
      failure: `Could not remove ${name}`,
      success: ['Login account removed', `${name} on ${short} is locked at the agent’s next check-in. Its files stay.`],
    })
    if (ok) onClose()
  }
  return (
    <Modal opened={!!name} onClose={onClose} title={`Remove ${name ?? ''}?`} size="md">
      <Stack gap="md">
        <Text size="sm" lh={1.55}>
          The account is locked: nobody can log in as <Text span ff="monospace" fw={600}>{name}</Text> on {short},
          anything still running as it is stopped, and it comes off the server&rsquo;s login accounts. Its home
          folder and files stay, and you can unlock it later.
        </Text>
        <Group justify="flex-end">
          <Button variant="subtle" color="slate" onClick={onClose}>Cancel</Button>
          <Button color="amber" onClick={() => void go()} loading={act.isPending}>Lock account</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function DeleteModal({ asset, name, onClose }: { asset: Asset; name?: string; onClose: () => void }) {
  const act = useServerLoginAction()
  const [typed, setTyped] = useState('')
  const short = asset.hostname.split('.')[0] ?? asset.hostname
  const close = () => { setTyped(''); onClose() }
  const go = async () => {
    if (!name || typed !== name) return
    const ok = await run(() => act.mutateAsync({ id: asset.id, name, action: 'delete' }), {
      failure: `Could not delete ${name}`,
      success: ['Deleting for good', `The agent on ${short} deletes ${name} and its home folder at its next check-in.`],
    })
    if (ok) close()
  }
  return (
    <Modal opened={!!name} onClose={close} title={`Delete ${name ?? ''} for good?`} size="md">
      <Stack gap="md">
        <Alert color="rose" icon={<IconAlertTriangle size={16} />}>
          <Text size={FS.body} lh={1.5}>
            This deletes the account and its home folder from {short}. Anything kept there is gone and
            cannot be brought back. Recorded sessions and the audit log are not affected.
          </Text>
        </Alert>
        <TextInput
          label={`Type ${name ?? ''} to confirm`}
          value={typed}
          onChange={(e) => setTyped(e.currentTarget.value.trim())}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          data-autofocus
        />
        <Group justify="flex-end">
          <Button variant="subtle" color="slate" onClick={close}>Cancel</Button>
          <Button color="rose" disabled={typed !== name} onClick={() => void go()} loading={act.isPending}>
            Delete for good
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
