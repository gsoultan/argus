import { useState } from 'react'
import { Box, Button, Group, Text, ThemeIcon } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconShieldCheck } from '@tabler/icons-react'
import { ButtonLink } from '~/components/links'
import { relTime } from '~/components/primitives'
import { impactOf, impactWords, shortName } from '~/lib/changeApproval'
import { meQuery, requestsQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { AccessRequest, Database } from '~/types/domain'

/*
Changes waiting on the person looking: other people's change requests they
may decide, as an editor of the database (where its editors approve each
other's) or by their role. Never their own.
*/

const DECIDES = new Set(['approver', 'admin', 'owner'])

/** The pending change requests the viewer may decide, newest first. */
export function useChangesToReview(): { reqs: AccessRequest[]; me?: string } {
  const { data: me } = useQuery(meQuery())
  const { data } = useQuery({ ...requestsQuery('pending'), refetchInterval: 30_000 })
  const mine = me?.email?.toLowerCase()
  const reqs = (data ?? []).filter((r) => r.statement && r.state === 'pending' &&
    r.requesterEmail.toLowerCase() !== mine && (r.asEditor || DECIDES.has(me?.role ?? '')))
  return { reqs, me: me?.email }
}

/** Inside a database: who is waiting on you there, with a way to review it. */
export function ChangesWaitingBanner({ database }: { database: Database; me?: string }) {
  const { reqs } = useChangesToReview()
  const [later, setLater] = useState<Set<string>>(new Set())
  const here = reqs.filter((r) => r.statement?.databaseId === database.id && !later.has(r.id))
  if (!here.length) return null
  const first = here[0]!
  const impact = impactWords(impactOf(first.statement?.summary))
  const who = shortName(first.requesterEmail)
  return (
    <Box px="md" py={SP.cozy} style={{ background: 'var(--mantine-color-amber-light)', borderBottom: '1px solid var(--color-line)' }}
      role="status" aria-label="Changes waiting on you">
      <Group gap="sm" wrap="nowrap" align="flex-start">
        <ThemeIcon variant="transparent" color="amber.8" size={22}><IconShieldCheck size={18} /></ThemeIcon>
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Text size={FS.body} fw={650} c="amber.9">
            {here.length === 1 ? `${who} is waiting on you` : `${who} and ${here.length - 1} more are waiting on you`}
          </Text>
          <Text size={FS.meta}>
            A change{impact ? ` of ${impact.split(' · ')[0]}` : ''} on {database.dbname}, asked {relTime(first.createdAt)}.
            {first.asEditor ? ' You can approve it as an editor here.' : ' You can approve it.'}
          </Text>
          <Group gap="xs" mt={6}>
            <ButtonLink to="/approvals" search={{ req: first.id }} size="compact-sm">Review</ButtonLink>
            <Button size="compact-sm" variant="subtle" onClick={() => setLater(new Set([...later, ...here.map((r) => r.id)]))}>Later</Button>
          </Group>
        </Box>
      </Group>
    </Box>
  )
}
