import { useEffect, useState } from 'react'
import { Alert, Button, Group, Modal, Stack, Text, Textarea } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconDownload, IconInfoCircle, IconKey } from '@tabler/icons-react'
import { Mono } from '~/components/primitives'
import { notifyError, notifyOk } from '~/lib/notify'
import { myServerAccountQuery, useSetMySshKey } from '~/lib/queries'
import { canGenerateSshKey, generateSshKey } from '~/lib/sshKeygen'
import { FS } from '~/theme'

/**
 * The signed-in person's own SSH key, for SSH straight to their personal
 * accounts on servers where their team is allowed to.
 *
 * Only ever their own: nobody sets a key for someone else.
 *
 * They can paste a key they have, or have one made. A made key is generated
 * in this browser: only its public half is sent, and the private half is
 * offered once as a download and then forgotten, so Argus never holds it.
 */
export function MySshKeyModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { data: acct, error } = useQuery({ ...myServerAccountQuery(), enabled: opened })
  const setKey = useSetMySshKey()
  const [key, setKeyText] = useState('')
  const [made, setMade] = useState<{ privateKey: string; fingerprint: string } | null>(null)
  const [generating, setGenerating] = useState(false)
  useEffect(() => {
    if (opened) setKeyText(acct?.sshKey ?? '')
  }, [opened, acct?.sshKey])

  const generate = async () => {
    setGenerating(true)
    try {
      const k = await generateSshKey(`${acct?.loginName || 'me'}@argus`)
      const out = await setKey.mutateAsync(k.publicKey)
      setMade({ privateKey: k.privateKey, fingerprint: out.sshFingerprint })
      download(k.privateKey)
    } catch (err) {
      notifyError('No key made', err)
    } finally {
      setGenerating(false)
    }
  }

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

  if (made) {
    const login = acct?.loginName || 'you'
    return (
      <Modal opened={opened} onClose={onClose} title="Your new SSH key" size="lg">
        <Stack gap="md">
          <Alert color="amber" icon={<IconInfoCircle size={16} />}>
            Your browser has downloaded the private key as <Mono>{KEY_FILE}</Mono>. This is the only
            copy: Argus kept just the public half, so if you lose the file, make a new key.
          </Alert>
          <Text size="sm">Move it into place, then connect:</Text>
          <Textarea
            readOnly
            autosize
            value={[
              `mv ~/Downloads/${KEY_FILE} ~/.ssh/ && chmod 600 ~/.ssh/${KEY_FILE}`,
              `ssh -i ~/.ssh/${KEY_FILE} ${login}@server`,
            ].join('\n')}
            styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
          />
          <Text size={FS.meta} c="dimmed">
            Saved key: <Mono>{made.fingerprint}</Mono>. Servers where your team may SSH directly get
            it within a minute.
          </Text>
          <Group justify="space-between">
            <Button variant="subtle" leftSection={<IconDownload size={14} />}
              onClick={() => download(made.privateKey)}>
              Download again
            </Button>
            <Button onClick={onClose}>Done</Button>
          </Group>
        </Stack>
      </Modal>
    )
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
        {!error && canGenerateSshKey() && (
          <Group justify="space-between" wrap="nowrap" gap="md">
            <Text size="sm" c="dimmed">
              No key yet? Argus can make one. Your browser makes it and downloads the private key;
              Argus keeps only the public half.{acct?.sshKey ? ' This replaces your saved key.' : ''}
            </Text>
            <Button variant="light" leftSection={<IconKey size={14} />} loading={generating}
              style={{ flexShrink: 0 }} onClick={() => void generate()}>
              Generate a key for me
            </Button>
          </Group>
        )}
        <Textarea
          label="Public key"
          description="Or paste your own: the contents of ~/.ssh/id_ed25519.pub. Never paste a private key."
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

const KEY_FILE = 'argus_ed25519'

/** Hands the private key to the browser as a file. Nothing else keeps it. */
function download(privateKey: string) {
  const url = URL.createObjectURL(new Blob([privateKey], { type: 'application/octet-stream' }))
  const a = document.createElement('a')
  a.href = url
  a.download = KEY_FILE
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
