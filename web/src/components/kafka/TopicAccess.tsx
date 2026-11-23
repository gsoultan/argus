import { Badge, Box, Card, Group, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconSettings } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import type { StoreAnswer } from '~/components/useStoreSession'
import { levelCaps, topicAccess, type KafkaCap, type KafkaUsersData, type TopicReach } from '~/lib/kafkaUsers'
import { dbAssignmentsQuery } from '~/lib/queries'
import { FS } from '~/theme'

/*
Who can reach a topic, in one place: people who can open the entry in
Argus, and apps signing in to Kafka with their own users, with the ACL that
lets them. It answers "who could have written this message?". The ACLs are
read back; Kafka's authorizer still decides.
*/

const CAP_COLOR: Record<KafkaCap, string> = { Produce: 'azure', Consume: 'teal', 'Alter topics': 'amber', Deny: 'rose' }

function Caps({ caps }: { caps: KafkaCap[] }) {
  return (
    <Group gap={4} wrap="wrap">
      {caps.map((c) => <Badge key={c} size="xs" variant="light" color={CAP_COLOR[c]} radius="sm" style={{ textTransform: 'none' }}>{c === 'Alter topics' ? 'Alter' : c}</Badge>)}
    </Group>
  )
}

const LEVEL_WORD: Record<string, string> = { read: 'Read', write: 'Write', owner: 'Manage', dba: 'Admin' }

export function TopicAccess({ topic, databaseId, ask }: {
  topic: string; databaseId: string; ask: (r: { op: string } & Record<string, unknown>) => Promise<StoreAnswer>
}) {
  const [apps, setApps] = useState<TopicReach[] | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const people = useQuery({ ...dbAssignmentsQuery(databaseId), retry: false })
  useEffect(() => {
    let live = true
    void ask({ op: 'users' }).then((a) => {
      if (!live) return
      if (a.type !== 'result') { setErr(a.error ?? 'could not read the users'); return }
      setApps(topicAccess(a.data as KafkaUsersData, topic))
    })
    return () => { live = false }
  }, [ask, topic])

  const rows = people.data ?? []
  return (
    <Card withBorder radius="md" padding={0}>
      <Box p="sm" pb={4}>
        <Text fw={600} size={FS.body}>Access</Text>
        <Text size={FS.micro} c="dimmed">
          Everyone who can reach {topic}: people through Argus, and apps signing in to Kafka with their own user. Kafka decides with its ACLs; this is them read back.
        </Text>
      </Box>
      <Table.ScrollContainer minWidth={560}>
        <Table verticalSpacing={6} fz="sm">
          <Table.Thead><Table.Tr><Table.Th>Who</Table.Th><Table.Th>Can</Table.Th><Table.Th>Because of</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {rows.map((p) => (
              <Table.Tr key={p.id}>
                <Table.Td>
                  <Text size={FS.meta} fw={600}>{p.teamName ?? p.userEmail ?? 'someone'}</Text>
                  <Text size={FS.micro} c="dimmed">Argus · {p.teamName ? 'team' : 'person'} · can open</Text>
                </Table.Td>
                <Table.Td><Caps caps={levelCaps(p.level)} /></Table.Td>
                <Table.Td ff="monospace" fz={FS.micro}>{LEVEL_WORD[p.level] ?? p.level} in Argus{p.needsApproval ? ', once approved' : ''}</Table.Td>
              </Table.Tr>
            ))}
            {apps?.map((u) => (
              <Table.Tr key={u.user}>
                <Table.Td>
                  <Group gap={6} wrap="nowrap">
                    <IconSettings size={14} color="var(--mantine-color-dimmed)" />
                    <Box>
                      <Text size={FS.meta} fw={600} ff="monospace">{u.user}</Text>
                      <Text size={FS.micro} c="dimmed">App user{u.mechanisms.length ? ` · ${u.mechanisms.map((m) => m.replace('SCRAM-', 'SCRAM ')).join(', ')}` : ''}</Text>
                    </Box>
                  </Group>
                </Table.Td>
                <Table.Td><Caps caps={u.caps} /></Table.Td>
                <Table.Td ff="monospace" fz={FS.micro}>{u.because.join(', ')}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
      {err && <Text size={FS.micro} c="rose" p="sm">{err}</Text>}
      {apps && apps.length === 0 && <Text size={FS.micro} c="dimmed" px="sm" pt={4}>No app user's ACLs reach this topic.</Text>}
      {people.isError && (
        <Text size={FS.micro} c="dimmed" p="sm">Only an admin sees who can open this entry in Argus.</Text>
      )}
    </Card>
  )
}
