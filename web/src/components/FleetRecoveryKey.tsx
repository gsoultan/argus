import { useState } from 'react'
import { Alert, Badge, Button, Checkbox, Code, Group, Modal, Skeleton, Stack, Text } from '@mantine/core'
import { IconAlertTriangle, IconDownload, IconKey, IconShieldExclamation } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { notifyError } from '~/lib/notify'
import {
  holdingLabel, REKEY_COMMAND, REPLACE_COMMAND, saveTextFile, useMakeRecoveryKey, useRecoveryKey,
  type MadeRecoveryKey,
} from '~/lib/recoveryKey'
import { FS, SP } from '~/theme'

/** Settings › Account security, for platform owners. */
export function FleetRecoveryKey() {
  const { data: v, isLoading } = useRecoveryKey(true)
  const make = useMakeRecoveryKey()
  const [made, setMade] = useState<MadeRecoveryKey | null>(null)
  const [saved, setSaved] = useState(false)

  const create = async () => {
    try {
      const m = await make.mutateAsync()
      setMade(m)
      setSaved(false)
      saveTextFile(m.fileName, m.privateKey)
    } catch (e) {
      notifyError('Recovery key not made', e)
    }
  }

  return (
    <>
      <SectionCard
        title="Fleet recovery key"
        icon={IconKey}
        iconColor="amber"
        description="A second key, kept offline, that can do only one thing: tell every server to stop trusting Argus's current signing key and trust a new one. Use it if the Argus server may have been taken over. Argus never holds the private half."
      >
        {isLoading || !v ? <Skeleton h={120} /> : !v.set ? (
          <Stack gap="sm">
            <Text size={FS.meta} c="dimmed" lh={1.5}>
              No recovery key yet. Without one, moving the fleet to a new signing key means deleting
              grant-key.pub on every server by hand. Making one downloads its private half once; put it
              on a USB stick or print it and keep it away from this server.
            </Text>
            <Group>
              <Button size="xs" leftSection={<IconKey size={14} />} loading={make.isPending} onClick={() => void create()}>
                Make the recovery key
              </Button>
            </Group>
          </Stack>
        ) : (
          <Stack gap={SP.snug}>
            <Row label="Public half">
              <Mono>argus-recovery {v.fingerprint}</Mono>
              <Text span size={FS.meta} c="dimmed"> · made {v.createdAt ? relTime(v.createdAt) : ''} by {v.createdBy}</Text>
            </Row>
            <Row label="Servers holding it">
              <Text span size={FS.meta} fw={600}>{holdingLabel(v)}</Text>
              {v.holding < v.total && (
                <Text span size={FS.meta} c="dimmed"> · the rest pick it up at their next check-in; agents older than it never do</Text>
              )}
            </Row>
            <Row label="Private half">
              <Text span size={FS.meta}>Downloaded once when it was made. Not stored anywhere by Argus.</Text>
            </Row>
            {v.rekeyed && (
              <Row label="Last rekey">
                <Text span size={FS.meta}>{relTime(v.rekeyed)}</Text>
              </Row>
            )}
            <Text size={FS.micro} c="dimmed" lh={1.5} mt={SP.tight}>
              To replace it, bring the current private half to the control server:
            </Text>
            <Code block>{REPLACE_COMMAND}</Code>
          </Stack>
        )}
      </SectionCard>

      <SectionCard title="If you think Argus was taken over" icon={IconShieldExclamation} iconColor="rose">
        <Stack gap="sm">
          <Stack gap={4} style={{ background: '#0b0f19', borderRadius: 8, padding: '12px 16px' }}>
            <Text ff="monospace" size={FS.meta} c="#8b93a7"># on a new, clean control server, restored from a backup:</Text>
            <Text ff="monospace" size={FS.meta} c="#e6e9ef">{REKEY_COMMAND}</Text>
          </Stack>
          <Text size={FS.meta} c="dimmed" lh={1.5}>
            It makes a new signing secret and an order signed with the recovery key. Each server checks
            the order against the recovery key it already holds, then drops the old signing key: anything
            signed with it, by the old server or anyone holding its key, is refused from then on. Restart
            argus-control after it; everyone signs in again. The runbook has the rest.
          </Text>
        </Stack>
      </SectionCard>

      <Modal opened={made !== null} onClose={() => saved && setMade(null)} withCloseButton={saved}
        closeOnClickOutside={false} closeOnEscape={false} title="Keep the recovery key" size="lg">
        {made && (
          <Stack gap="sm">
            <Alert color="amber" icon={<IconAlertTriangle size={16} />} p="xs">
              <Text size={FS.meta} lh={1.5}>
                This is the only time Argus shows it. It was downloaded as <Mono>{made.fileName}</Mono>.
                Move that file off this computer to somewhere offline, or print this.
              </Text>
            </Alert>
            <Code block data-testid="recovery-private" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{made.privateKey}</Code>
            <Group justify="space-between">
              <Button size="xs" variant="default" leftSection={<IconDownload size={14} />}
                onClick={() => saveTextFile(made.fileName, made.privateKey)}>Download again</Button>
              <Badge variant="light" color="slate">{made.fingerprint}</Badge>
            </Group>
            <Checkbox size="xs" checked={saved} onChange={(e) => setSaved(e.currentTarget.checked)}
              label="I have kept it somewhere this server is not" />
            <Group justify="flex-end">
              <Button size="xs" disabled={!saved} onClick={() => setMade(null)}>Done</Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="md" wrap="nowrap" align="baseline">
      <Text size={FS.meta} c="dimmed" w={140} style={{ flexShrink: 0 }}>{label}</Text>
      <Text component="div" size={FS.meta}>{children}</Text>
    </Group>
  )
}
