import { useState } from 'react'
import { Alert, Badge, Box, Button, Group, Progress, Stack, Text } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconInfoCircle, IconLogout } from '@tabler/icons-react'
import { relTime } from '~/components/primitives'
import { assetLogons, connectOutcome, logonsKey, remoteInUse, signOutLogon, slotsFull, type Logon, type LogonsView } from '~/lib/logonsApi'
import { isConfigured } from '~/lib/live'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS } from '~/theme'

/**
 * Who is signed in on the host, shown before Connect: so the person
 * connecting knows whether they will land in a fresh session, take one over,
 * or (on a full Windows server) make Windows ask someone to give theirs up.
 * An admin can sign out a disconnected session to make room instead.
 *
 * Quiet when it has nothing to say: an old agent, an offline one, or an
 * empty host shows a line at most, never an error in the way of Connect.
 */
export function SignedIn({ assetId, principal }: { assetId: string; principal: string }) {
  const { data, isLoading } = useQuery({
    queryKey: logonsKey(assetId),
    queryFn: () => assetLogons(assetId),
    enabled: isConfigured(),
    // Who is signed in changes while the page is open.
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
  if (!isConfigured()) return null
  if (isLoading) return <Text size={FS.meta} c="dimmed">Checking who is signed in…</Text>
  if (!data || data.state !== 'ok') {
    return data?.state === 'unsupported'
      ? <Text size={FS.meta} c="dimmed">The agent is too old to say who is signed in. Updating it shows that here.</Text>
      : null
  }
  return <SignedInList assetId={assetId} principal={principal} view={data} />
}

export function SignedInList({ assetId, principal, view }: { assetId: string; principal: string; view: LogonsView }) {
  const qc = useQueryClient()
  const [asking, setAsking] = useState<number>()
  const signOut = useMutation({
    mutationFn: (l: Logon) => signOutLogon(assetId, l.session ?? 0, l.account),
    onSuccess: (res, l) => {
      if (res.state === 'ok') notifyOk('Signed out', `${l.account}'s session has ended. Anything left open in it was closed.`)
      else notifyError('Not signed out', new Error(res.error))
      void qc.invalidateQueries({ queryKey: logonsKey(assetId) })
    },
    onError: (e) => notifyError('Not signed out', e),
    onSettled: () => setAsking(undefined),
  })

  const windows = view.platform === 'windows'
  const limit = view.remoteLimit ?? 0
  const used = remoteInUse(view)
  const full = slotsFull(view)
  const outcome = connectOutcome(view, principal)

  return (
    <Box aria-label="Signed in now" role="region">
      <Group justify="space-between" mb={6}>
        <Text size="sm" fw={600}>Signed in now</Text>
        {windows && limit > 0 && (
          <Text size={FS.meta} c={full ? 'rose' : 'dimmed'}>
            {used} of {limit} Remote Desktop {limit === 1 ? 'session' : 'sessions'} in use
          </Text>
        )}
      </Group>
      {windows && limit > 0 && (
        <Progress value={Math.min(100, (used / limit) * 100)} color={full ? 'rose' : 'azure'} size="sm" mb="xs"
          aria-label="Remote Desktop sessions in use" />
      )}
      {view.logons.length === 0 ? (
        <Text size={FS.meta} c="dimmed">
          Nobody. {windows ? 'You get a session of your own.' : 'Every login is its own.'}
        </Text>
      ) : (
        <Stack gap={6}>
          {view.logons.map((l) => {
            const idle = l.state === 'disconnected' && !l.console
            return (
              <Group key={`${l.session}-${l.account}-${l.line}`} gap="sm" wrap="nowrap" justify="space-between">
                <Box style={{ minWidth: 0 }}>
                  <Group gap={6} wrap="nowrap">
                    <Text size="sm" ff="monospace" truncate>{l.account}</Text>
                    {l.console && <Badge size="xs" variant="light" color="slate">on its screen</Badge>}
                    {l.state === 'disconnected'
                      ? <Badge size="xs" variant="light" color="amber">disconnected</Badge>
                      : <Badge size="xs" variant="light" color="teal">active</Badge>}
                  </Group>
                  <Text size={FS.meta} c="dimmed">
                    {[
                      l.from && `from ${l.from}`,
                      l.line,
                      l.logonAt && `signed in ${relTime(l.logonAt)}`,
                      l.idleSeconds && l.idleSeconds >= 60 ? `idle ${idleFor(l.idleSeconds)}` : undefined,
                    ].filter(Boolean).join(' · ')}
                  </Text>
                </Box>
                {view.canSignOut && idle && (
                  asking === l.session ? (
                    <Group gap={6} wrap="nowrap">
                      <Button size="xs" color="rose" loading={signOut.isPending}
                        onClick={() => signOut.mutate(l)}>
                        Sign out {l.account.split('\\').pop()}
                      </Button>
                      <Button size="xs" variant="default" disabled={signOut.isPending}
                        onClick={() => setAsking(undefined)}>
                        Keep
                      </Button>
                    </Group>
                  ) : (
                    <Button size="xs" variant="default" leftSection={<IconLogout size={14} />}
                      onClick={() => setAsking(l.session)}>
                      Sign out
                    </Button>
                  )
                )}
              </Group>
            )
          })}
        </Stack>
      )}
      {asking !== undefined && (
        <Text size={FS.meta} c="dimmed" mt={6}>
          Signing out closes everything left open in that session without saving. It is recorded in the audit log.
        </Text>
      )}
      {outcome && (
        <Alert mt="xs" color={full ? 'amber' : 'azure'} variant="light" icon={<IconInfoCircle size={16} />}>
          <Text size={FS.body} lh={1.5}>{outcome}</Text>
        </Alert>
      )}
    </Box>
  )
}

function idleFor(s: number): string {
  if (s < 3600) return `${Math.round(s / 60)} min`
  if (s < 86400) return `${Math.round(s / 3600)} h`
  return `${Math.round(s / 86400)} d`
}
