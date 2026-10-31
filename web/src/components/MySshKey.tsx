import { useEffect, useState } from 'react'
import { Alert, Button, Group, Modal, Stack, Text, Textarea } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconInfoCircle } from '@tabler/icons-react'
import { Mono } from '~/components/primitives'
import { notifyError, notifyOk } from '~/lib/notify'
import { myServerAccountQuery, useSetMySshKey } from '~/lib/queries'
import { FS } from '~/theme'

/**
 * The signed-in person's own SSH key, for SSH straight to their personal
 * accounts on servers where their team is allowed to.
 *
 * Only ever their own: nobody sets a key for someone else.
 */
export function MySshKeyModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { data: acct, error } = useQuery({ ...myServerAccountQuery(), enabled: opened })
  const setKey = useSetMySshKey()
  const [key, setKeyText] = useState('')
  useEffect(() => {
    if (opened) setKeyText(acct?.sshKey ?? '')
  }, [opened, acct?.sshKey])

  const save = async (next: string) => {
    try {
      const out = await setKey.mutateAsync(next)
      notifyOk(next ? 'SSH key saved' : 'SSH key removed', next
        ? `Servers where your team may SSH directly get it within a minute (${out.sshFingerprint}).`
        : 'Servers take it off within a minute.')
      onClose()
    } catch (err) {
      notifyError('Not saved', err)
    }
  }

  return (
    <Modal opened={opened} onClose={onClose} title="My SSH key" size="lg">
      <Stack gap="md">
        {error ? (
          <Alert color="amber" icon={<IconInfoCircle size={16} />}>
            {error instanceof Error ? error.message : 'This sign-in cannot hold a key.'}
          </Alert>
        ) : (
          <Text size="sm">
            {acct?.loginName ? (
              <>Your account on servers is <Mono>{acct.loginName}</Mono>. </>
            ) : (
              <>You get an account on a server once a team of yours is given one. </>
            )}
            Where your team may SSH straight to the server, this key lets you in with{' '}
            <Mono>ssh {acct?.loginName || 'you'}@server</Mono>. Those sessions are recorded too.
          </Text>
        )}
        <Textarea
          label="Public key"
          description="The contents of ~/.ssh/id_ed25519.pub. Never paste a private key."
          placeholder="ssh-ed25519 AAAA… you@laptop"
          autosize
          minRows={3}
          value={key}
          onChange={(e) => setKeyText(e.currentTarget.value)}
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
        />
        {acct?.sshFingerprint && (
          <Text size={FS.meta} c="dimmed">Saved key: <Mono>{acct.sshFingerprint}</Mono></Text>
        )}
        <Group justify="space-between">
          {acct?.sshKey ? (
            <Button variant="subtle" color="rose" loading={setKey.isPending} onClick={() => void save('')}>
              Remove key
            </Button>
          ) : <span />}
          <Group gap="sm">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button
              loading={setKey.isPending}
              disabled={!key.trim() || key.trim() === acct?.sshKey}
              onClick={() => void save(key.trim())}
            >
              Save
            </Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}
