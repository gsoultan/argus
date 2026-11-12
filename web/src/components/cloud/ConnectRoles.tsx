import { Alert, Box, Button, Card, CopyButton, Group, Loader, Stack, Text, ThemeIcon } from '@mantine/core'
import { useMutation, useQuery } from '@tanstack/react-query'
import { IconCheck, IconCopy, IconWand } from '@tabler/icons-react'
import { useEffect, useRef, useState } from 'react'
import { checkCloudAccount, deployCloudAccess, type CloudPrepared } from '~/lib/cloud'
import { FS, INK } from '~/theme'

/** How often the connect page asks whether the roles work, and for how long. */
const CHECK_EVERY = 5_000
const CHECK_FOR = 30 * 60_000

/**
 * Making the stack with argus-control's own identity, offered only in the
 * account that identity is in. Whether it may is up to its IAM policy; when
 * it may not, the reason is shown and the steps below still work.
 */
export function MakeItForMe({ prepared, name, manage }: { prepared: CloudPrepared; name: string; manage: boolean }) {
  const deploy = useMutation({ mutationFn: () => deployCloudAccess(prepared.accountId, manage, name, prepared.identity) })
  return (
    <Card withBorder padding="md" radius="md">
      <Stack gap="sm">
        <Group gap="sm" wrap="nowrap" align="flex-start">
          <ThemeIcon variant="light" color="teal" size={30} radius="md"><IconWand size={16} /></ThemeIcon>
          <Box style={{ flex: 1 }}>
            <Text fw={600} size={FS.body}>Argus can make it for you</Text>
            <Text size={FS.meta} c="dimmed" lh={1.55}>
              argus-control signs in to AWS as <Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>, which is in this account.
              If that identity is allowed to create IAM roles, Argus checks it has every right first, then creates the stack itself. The audit log and the stack's tags say you asked.
            </Text>
          </Box>
        </Group>
        {deploy.error && (
          <Alert color="amber" variant="light" title="Argus made nothing">
            <Stack gap="xs">
              <Text size={FS.meta}>{(deploy.error as Error).message}</Text>
              <Text size={FS.meta}>
                Use the CloudShell command below while signed in as an administrator. Or attach this policy to
                {' '}<Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text> and try again, then remove it once the
                roles are made: an identity that may make roles can give itself any permission.
              </Text>
              {prepared.deployRights && (
                <Group>
                  <CopyButton value={prepared.deployRights}>
                    {({ copied, copy }) => (
                      <Button size="xs" variant="default" leftSection={copied ? <IconCheck size={13} /> : <IconCopy size={13} />} onClick={copy}>
                        {copied ? 'Copied' : 'Copy the policy it needs'}
                      </Button>
                    )}
                  </CopyButton>
                </Group>
              )}
            </Stack>
          </Alert>
        )}
        {deploy.isSuccess ? (
          <Text size={FS.meta} c={INK.verified}>CloudFormation is making the roles. Argus connects as soon as they work.</Text>
        ) : (
          <Group>
            <Button leftSection={<IconWand size={16} />} loading={deploy.isPending} onClick={() => deploy.mutate()}>
              Make the roles for me
            </Button>
          </Group>
        )}
      </Stack>
    </Card>
  )
}

/**
 * Asks every few seconds whether the roles let Argus in, and connects once
 * they do. It stops after half an hour, on a failed stack, and when a
 * connect fails, with a button to look again.
 */
export function WatchForRoles({ prepared, manage, connecting, failed, onReady }: {
  prepared: CloudPrepared; manage: boolean; connecting: boolean; failed: Error | null; onReady: () => void
}) {
  const [since, setSince] = useState(() => Date.now())
  const fired = useRef(false)
  const [stale, setStale] = useState(false)
  const check = useQuery({
    queryKey: ['cloud', 'connect-check', prepared.accountId, manage, since],
    queryFn: () => checkCloudAccount(prepared.accountId, manage, prepared.identity),
    enabled: !stale && !connecting && !failed,
    // A failed stack keeps being watched: the fix is to run the command
    // below, and the page should notice when that works.
    refetchInterval: (q) => (q.state.data?.ready ? false : CHECK_EVERY),
    refetchIntervalInBackground: true,
    retry: false,
    gcTime: 0,
  })
  useEffect(() => {
    const t = window.setTimeout(() => setStale(true), Math.max(0, since + CHECK_FOR - Date.now()))
    return () => window.clearTimeout(t)
  }, [since])
  useEffect(() => {
    if (check.data?.ready && !fired.current) {
      fired.current = true
      onReady()
    }
  }, [check.data?.ready, onReady])
  const again = () => { fired.current = false; setStale(false); setSince(Date.now()) }
  const stack = check.data?.stack

  if (failed) {
    return (
      <Alert color="rose" variant="light" title="The roles are there, but Argus could not connect">
        <Stack gap="xs">
          <Text size={FS.meta}>{failed.message}</Text>
          <Group><Button size="xs" variant="default" onClick={onReady}>Try again</Button></Group>
        </Stack>
      </Alert>
    )
  }
  if (stack?.failed) {
    return (
      <Alert color="rose" variant="light" title={`The argus-access stack stopped at ${stack.status}`}>
        <Stack gap="xs">
          <Text size={FS.meta}>{stack.reason || 'CloudFormation did not say why. Its Events tab does.'}</Text>
          <Text size={FS.meta} fw={500}>
            To fix it, run the CloudShell command below while signed in as an administrator. It deletes this stack first and
            makes the roles again; Argus keeps watching and connects when they work.
          </Text>
        </Stack>
      </Alert>
    )
  }
  const waiting = !stale && !check.error
  return (
    <Box p="sm" style={{ borderRadius: 8, background: 'var(--color-raised)', border: '1px solid var(--color-line)' }} aria-live="polite">
      <Group gap="sm" wrap="nowrap" align="flex-start">
        {connecting || check.data?.ready ? <IconCheck size={18} color="var(--mantine-color-teal-6)" />
          : waiting ? <Loader size={16} mt={2} /> : null}
        <Stack gap={2} style={{ flex: 1 }}>
          <Text size={FS.body} fw={500}>
            {connecting || check.data?.ready ? `The ${manage ? 'roles work' : 'role works'}. Connecting…`
              : waiting ? `Waiting for the ${manage ? 'roles' : 'role'} in ${prepared.accountId}`
                : 'Argus stopped checking'}
          </Text>
          <Text size={FS.meta} c="dimmed">
            {check.error ? (check.error as Error).message
              : stale ? 'It checked for half an hour. Look again once the stack is made.'
                : stack?.status ? `CloudFormation: ${stack.status}. Argus connects as soon as it is done.`
                  : 'Argus checks every few seconds and connects by itself as soon as the stack is made. Leave this page open.'}
          </Text>
        </Stack>
        {(!waiting) && <Button size="xs" variant="default" onClick={again}>Check again</Button>}
      </Group>
    </Box>
  )
}
