import { Alert, Badge, Box, Button, Group, Loader, Modal, Radio, Stack, Text } from '@mantine/core'
import { IconCheck } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import {
  checkCloudMove, prepareCloudAccount, useMoveCloudAccount, type CloudAccount, type CloudBase,
} from '~/lib/cloud'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'
import { RoleSetup } from './RoleSetup'

/** How often the dialog asks whether the roles trust the new identity, and for how long. */
const CHECK_EVERY = 5_000
const CHECK_FOR = 30 * 60_000

/**
 * Moving an account to another of argus-control's identities. The person
 * updates the argus-access stack so its roles trust the new identity as
 * well as the old one; Argus asks every few seconds whether it gets in with
 * the new one, and moves the account only then. Closing at any point keeps
 * the account where it is. Once moved, the stack can be updated again to
 * trust the new identity alone.
 */
export function ChangeIdentity({ a, bases, onClose }: { a: CloudAccount; bases: CloudBase[]; onClose: () => void }) {
  const others = bases.filter((b) => b.identity !== a.identity)
  const current = bases.find((b) => b.identity === a.identity)
  const [to, setTo] = useState(others.length === 1 ? others[0]?.identity ?? '' : '')
  const [moved, setMoved] = useState<CloudAccount | null>(null)
  const target = others.find((b) => b.identity === to)
  const manage = a.access === 'manage'
  // While moving, the roles trust both; a missing identity has nothing to keep.
  const keep = a.identityMissing ? '' : a.identity
  const prepared = useQuery({
    queryKey: ['cloud', 'prepare-move', a.id, to, keep],
    queryFn: () => prepareCloudAccount(a.accountId, manage, to, keep),
    enabled: !!to && !moved,
    gcTime: 0,
    retry: false,
  })
  const tidy = useQuery({
    queryKey: ['cloud', 'prepare-tidy', a.id, to],
    queryFn: () => prepareCloudAccount(a.accountId, manage, to),
    enabled: !!moved && !!prepared.data?.kept,
    gcTime: 0,
    retry: false,
  })

  return (
    <Modal opened onClose={onClose} size="xl" centered title={<Text fw={700}>Change the identity for {a.name}</Text>}>
      <Stack gap="md">
        {moved ? (
          <>
            <Alert color="teal" variant="light" icon={<IconCheck size={16} />} title={`${a.name} now uses ${target?.name}`}>
              Argus got in with it and reads the account as it from now on. The audit log has the move.
            </Alert>
            {prepared.data?.kept && (
              <Box>
                <Text fw={600} size={FS.body}>Then tidy the roles</Text>
                <Text size={FS.meta} c="dimmed" mb="xs" lh={1.55}>
                  They still trust <Text span ff="monospace" size={FS.meta}>{prepared.data.kept}</Text> too. Update the stack once more
                  so they trust {target?.name} alone. Nothing stops working while it updates.
                </Text>
                {tidy.data ? <RoleSetup prepared={tidy.data} update />
                  : tidy.error ? <Alert color="rose" variant="light">{(tidy.error as Error).message}</Alert>
                    : <Text size={FS.meta} c="dimmed">Making the template…</Text>}
              </Box>
            )}
            <Group justify="flex-end"><Button onClick={onClose}>Done</Button></Group>
          </>
        ) : (
          <>
            <Text size={FS.body} lh={1.55}>
              {a.identityMissing
                ? `The identity ${a.name} used (${a.identity}) is no longer available here, so Argus is not reading it. Pick the one to use now.`
                : `Argus keeps using ${current?.name ?? a.identity} until it can get into the account's roles with the new identity, so nothing stops working on the way.`}
            </Text>
            {others.length === 0 ? (
              <Alert color="amber" variant="light">
                No other identity is available to this organization. The platform's owner adds them under Settings › Cloud identities in the default organization.
              </Alert>
            ) : (
              <Radio.Group value={to} onChange={setTo} label="Move to">
                <Stack gap={SP.snug} mt={6}>
                  {others.map((b) => (
                    <Radio.Card key={b.identity} value={b.identity} radius="md" p="sm" disabled={!b.principal}>
                      <Group wrap="nowrap" align="flex-start" gap="sm">
                        <Radio.Indicator mt={2} disabled={!b.principal} />
                        <Box style={{ minWidth: 0 }}>
                          <Group gap={6}>
                            <Text fw={600} size={FS.body}>{b.name}</Text>
                            {b.principal ? <Badge size="sm" variant="light" color="teal" tt="none">Working</Badge>
                              : <Badge size="sm" variant="light" color="rose" tt="none">No answer</Badge>}
                          </Group>
                          <Text size={FS.micro} c="dimmed" ff="monospace" style={{ overflowWrap: 'anywhere' }}>{b.principal ?? b.error}</Text>
                        </Box>
                      </Group>
                    </Radio.Card>
                  ))}
                </Stack>
              </Radio.Group>
            )}
            {prepared.error && <Alert color="rose" variant="light">{(prepared.error as Error).message}</Alert>}
            {prepared.data && target && (
              <>
                <Box>
                  <Text size={FS.meta} fw={600} mb={4}>What changes in the roles' trust policy</Text>
                  <Box style={{ border: '1px solid var(--color-line)', borderRadius: 8, overflow: 'hidden', fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 }}>
                    {prepared.data.kept && (
                      <Box px="sm" py={6} style={{ overflowWrap: 'anywhere' }}>
                        {prepared.data.kept} <Text span c="dimmed" size={FS.micro}>(kept until you tidy)</Text>
                      </Box>
                    )}
                    <Box px="sm" py={6} style={{ background: 'var(--mantine-color-teal-light)', borderTop: prepared.data.kept ? '1px solid var(--color-line)' : undefined, overflowWrap: 'anywhere' }}>
                      + {prepared.data.principal}
                    </Box>
                    <Box px="sm" py={6} c="dimmed" style={{ borderTop: '1px solid var(--color-line)', overflowWrap: 'anywhere' }}>
                      sts:ExternalId {prepared.data.externalId} (unchanged)
                    </Box>
                  </Box>
                </Box>
                <RoleSetup prepared={prepared.data} update />
                <WatchMove a={a} to={target} onMoved={(c) => { setMoved(c); notifyOk('Identity changed', `${a.name} now uses ${target.name}.`) }} />
              </>
            )}
            <Group justify="flex-end">
              <Button variant="default" onClick={onClose}>{a.identityMissing ? 'Close' : `Close, keep ${current?.name ?? a.identity}`}</Button>
            </Group>
          </>
        )}
      </Stack>
    </Modal>
  )
}

/** Asks every few seconds whether the new identity gets in, and moves the account once it does. */
function WatchMove({ a, to, onMoved }: { a: CloudAccount; to: CloudBase; onMoved: (c: CloudAccount) => void }) {
  const move = useMoveCloudAccount()
  const [since, setSince] = useState(() => Date.now())
  const [stale, setStale] = useState(false)
  const fired = useRef(false)
  const check = useQuery({
    queryKey: ['cloud', 'move-check', a.id, to.identity, since],
    queryFn: () => checkCloudMove(a.id, to.identity),
    enabled: !stale && !move.isPending && !move.isError,
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
      move.mutate({ id: a.id, identity: to.identity }, {
        onSuccess: onMoved,
        onError: (err) => notifyError('The identity was not changed', err),
      })
    }
  }, [check.data?.ready, move, a.id, to.identity, onMoved])
  const again = () => { fired.current = false; move.reset(); setStale(false); setSince(Date.now()) }
  const waiting = !stale && !check.error && !move.isError
  return (
    <Box p="sm" style={{ borderRadius: 8, background: 'var(--color-raised)', border: '1px solid var(--color-line)' }} aria-live="polite">
      <Group gap="sm" wrap="nowrap" align="flex-start">
        {check.data?.ready ? <IconCheck size={18} color="var(--mantine-color-teal-6)" /> : waiting ? <Loader size={16} mt={2} /> : null}
        <Stack gap={2} style={{ flex: 1 }}>
          <Text size={FS.body} fw={500}>
            {check.data?.ready ? `${to.name} gets in. Moving…`
              : waiting ? `Waiting for the roles to trust ${to.name}` : 'Argus stopped checking'}
          </Text>
          <Text size={FS.meta} c={move.error ? INK.denied : 'dimmed'}>
            {move.error ? (move.error as Error).message
              : check.error ? (check.error as Error).message
                : stale ? 'It checked for half an hour. Look again once the stack is updated.'
                  : check.data?.detail ?? 'Argus checks every few seconds and moves the account as soon as the new identity gets in.'}
          </Text>
        </Stack>
        {!waiting && <Button size="xs" variant="default" onClick={again}>Check again</Button>}
      </Group>
    </Box>
  )
}
