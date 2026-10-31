import { useState } from 'react'
import { Badge, Box, Button, Card, Group, Modal, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { TunnelBadge } from '~/components/TunnelStatus'
import { relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { enrolledAgentsQuery, meQuery, useRevokeAgent } from '~/lib/queries'
import { FS } from '~/theme'

/**
 * Every server able to write evidence, and the way to stop one.
 *
 * Admin-only on the control plane, so it is only drawn for someone who can
 * read it. Revoked ones stay listed: which credential vouched for a past
 * session is part of reading that session.
 */
export function EnrolledAgents() {
  const { data: me } = useQuery(meQuery())
  const isAdmin = me?.role === 'admin' || me?.role === 'owner'
  const { data: agents } = useQuery({ ...enrolledAgentsQuery(), enabled: isConfigured() && isAdmin })
  const revoke = useRevokeAgent()
  const [confirming, setConfirming] = useState<{ id: string; hostname: string } | null>(null)

  if (!isConfigured() || !isAdmin) return null

  const doRevoke = async () => {
    if (!confirming) return
    const ok = await run(() => revoke.mutateAsync(confirming.id), {
      success: ['Revoked', `${confirming.hostname} can no longer report`],
      failure: `${confirming.hostname} was not revoked`,
    })
    if (ok) setConfirming(null)
  }

  return (
    <Card padding="lg">
      <Text component="h2" fw={600} size="md" style={{ margin: 0 }} mb="sm">
        Server agents
      </Text>
      {!agents || agents.length === 0 ? (
        <Text size={FS.body} c="dimmed" lh={1.5}>
          No server has enrolled yet. Servers set up with the shared reporter token
          still report, but are not listed here; enroll them to give each its own credential.
        </Text>
      ) : (
        <Stack gap="xs">
          {agents.map((a) => (
            <Group key={a.id} justify="space-between" wrap="nowrap" gap="sm">
              <Box style={{ minWidth: 0 }}>
                <Text ff="monospace" size="sm" fw={500} truncate>{a.hostname}</Text>
                <Text size={FS.meta} c="dimmed">
                  {a.revokedAt
                    ? `Revoked ${relTime(a.revokedAt)} by ${a.revokedBy}`
                    : `Enrolled ${relTime(a.enrolledAt)}, token by ${a.enrolledBy}`}
                </Text>
              </Box>
              {a.revokedAt ? (
                <Badge color="slate" size="sm">Revoked</Badge>
              ) : (
                <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
                  <TunnelBadge tunnel={a.tunnel} />
                  <Button
                    size="xs"
                    variant="subtle"
                    color="rose"
                    style={{ flexShrink: 0 }}
                    onClick={() => setConfirming({ id: a.id, hostname: a.hostname })}
                  >
                    Revoke
                  </Button>
                </Group>
              )}
            </Group>
          ))}
        </Stack>
      )}

      <Modal
        opened={confirming !== null}
        onClose={() => setConfirming(null)}
        title={`Revoke ${confirming?.hostname ?? ''}?`}
      >
        <Text size="sm" c="dimmed" mb="md">
          Its agent keeps recording on the server, but the control plane refuses its reports
          from now on. What it reported before is unchanged. To bring it back, add it again
          from Add server.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" size="sm" onClick={() => setConfirming(null)}>
            Cancel
          </Button>
          <Button color="rose" size="sm" loading={revoke.isPending} onClick={() => void doRevoke()}>
            Revoke
          </Button>
        </Group>
      </Modal>
    </Card>
  )
}
