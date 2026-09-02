import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Group, Loader, Progress, Select, Stack, Table,
  Text, TextInput, ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconDownload, IconSearch, IconShieldCheck, IconShieldX,
} from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Digest, Mono, absTime, relTime } from '~/components/primitives'
import { auditQuery } from '~/lib/queries'
import { useAuditChain } from '~/lib/useWorkers'
import type { AuditSeverity } from '~/types/domain'

export const Route = createFileRoute('/audit')({
  component: Audit,
  loader: ({ context }) => context.queryClient.ensureQueryData(auditQuery()),
})

const SEVERITY_COLOR: Record<AuditSeverity, string> = {
  info: 'slate',
  notice: 'sky',
  warning: 'amber',
  critical: 'rose',
}

function Audit() {
  const { data: skeleton } = useQuery(auditQuery())
  const chain = useAuditChain(skeleton)
  const [search, setSearch] = useState('')
  const [severity, setSeverity] = useState<string | null>(null)

  const rows = useMemo(() => {
    let out = chain.links
    if (severity) out = out.filter((l) => l.severity === severity)
    const needle = search.trim().toLowerCase()
    if (needle) {
      out = out.filter((l) =>
        `${l.actorEmail} ${l.action} ${l.target} ${l.detail}`.toLowerCase().includes(needle),
      )
    }
    return out.slice(0, 200)
  }, [chain.links, search, severity])

  const verified = chain.verified

  return (
    <Box>
      <PageHeader
        title="Audit log"
        description="Every privileged action, hash-chained so tampering is detectable rather than merely discouraged."
        actions={
          <>
            <Button size="xs" variant="default" leftSection={<IconDownload size={14} />}>
              Export evidence pack
            </Button>
            <Button
              size="xs"
              color="teal"
              leftSection={<IconShieldCheck size={14} />}
              loading={chain.status === 'working'}
              disabled={chain.links.length === 0}
              onClick={chain.verify}
            >
              Verify chain
            </Button>
          </>
        }
      />

      <Box p="lg">
        {/* The integrity panel is the product's core compliance claim, so it
            gets top billing rather than being buried in a settings page. */}
        <Card
          padding="md"
          mb="sm"
          style={
            verified && !verified.ok ? { borderColor: 'var(--color-denied)' } : undefined
          }
        >
          <Group justify="space-between" wrap="nowrap" align="flex-start">
            <Group gap="sm" wrap="nowrap" align="flex-start">
              <ThemeIcon
                variant="light"
                size={36}
                radius="md"
                color={verified ? (verified.ok ? 'teal' : 'rose') : 'slate'}
              >
                {verified && !verified.ok
                  ? <IconShieldX size={19} />
                  : <IconShieldCheck size={19} />}
              </ThemeIcon>
              <Box>
                <Text fw={600} size="sm">
                  {chain.status === 'working'
                    ? 'Computing chain…'
                    : verified
                      ? verified.ok
                        ? 'Chain intact'
                        : `Chain broken at sequence ${verified.brokenAt}`
                      : 'Chain built — not yet verified'}
                </Text>
                <Text size="xs" c="dimmed" mt={2} maw={620}>
                  Each entry is <Mono>SHA-256(prevHash ‖ canonical(event))</Mono>. Verification
                  recomputes the whole chain in your browser, in a Web Worker — so an intact
                  result does not depend on trusting the server that served the log.
                </Text>
                {verified && (
                  <Group gap="xs" mt={7}>
                    <Badge size="xs" color={verified.ok ? 'teal' : 'rose'} variant="light">
                      {verified.checked.toLocaleString()} links checked
                    </Badge>
                    <Badge size="xs" variant="outline" color="slate">
                      {verified.ms}ms off main thread
                    </Badge>
                  </Group>
                )}
              </Box>
            </Group>

            <Box ta="right" style={{ flexShrink: 0 }}>
              <Text size="10px" c="dimmed" fw={600} style={{ letterSpacing: '0.05em' }}>
                CHAIN HEAD
              </Text>
              <Box mt={3}>
                {chain.head ? <Digest value={chain.head} chars={24} /> : <Loader size="xs" />}
              </Box>
              {chain.ms !== null && (
                <Text size="10px" c="dimmed" mt={3}>built in {chain.ms}ms</Text>
              )}
            </Box>
          </Group>

          {chain.status === 'working' && (
            <Progress value={chain.progress * 100} size="xs" mt="sm" color="teal" animated />
          )}
        </Card>

        {verified && !verified.ok && (
          <Alert
            color="rose"
            variant="light"
            icon={<IconAlertTriangle size={17} />}
            mb="sm"
            title="Integrity failure"
          >
            <Text size="xs">
              The recomputed hash diverges from the stored value at sequence {verified.brokenAt}.
              Everything from that point forward is unverifiable. Preserve the current store,
              pull the offline replica, and treat this as an incident.
            </Text>
          </Alert>
        )}

        <Group gap="xs" mb="sm">
          <TextInput
            size="xs"
            w={300}
            placeholder="Filter by actor, action, target or detail"
            leftSection={<IconSearch size={14} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
          />
          <Select
            size="xs"
            w={150}
            placeholder="Any severity"
            clearable
            value={severity}
            onChange={setSeverity}
            data={['info', 'notice', 'warning', 'critical']}
          />
        </Group>

        <Card padding={0}>
          <Table verticalSpacing={6} horizontalSpacing="md" highlightOnHover striped="even">
            <Table.Thead>
              <Table.Tr>
                <Table.Th w={60}>Seq</Table.Th>
                <Table.Th w={150}>When</Table.Th>
                <Table.Th>Action</Table.Th>
                <Table.Th>Actor</Table.Th>
                <Table.Th>Target</Table.Th>
                <Table.Th>Detail</Table.Th>
                <Table.Th w={130}>Hash</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((l) => (
                <Table.Tr key={l.id}>
                  <Table.Td><Mono c="dimmed">{l.seq}</Mono></Table.Td>
                  <Table.Td>
                    <Tooltip label={absTime(l.at)}>
                      <Text size="xs" c="dimmed">{relTime(l.at)}</Text>
                    </Tooltip>
                  </Table.Td>
                  <Table.Td>
                    <Badge
                      size="xs"
                      color={SEVERITY_COLOR[l.severity as AuditSeverity]}
                      variant="light"
                      className="argus-digest"
                    >
                      {l.action}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs">{l.actorEmail.split('@')[0]}</Text>
                  </Table.Td>
                  <Table.Td><Mono c="dimmed">{l.target.split('.')[0]}</Mono></Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed" lineClamp={1} maw={400}>{l.detail}</Text>
                  </Table.Td>
                  <Table.Td><Digest value={l.hash} chars={10} /></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
          {chain.status === 'working' && rows.length === 0 && (
            <Stack align="center" py="xl" gap="xs">
              <Loader size="sm" color="teal" />
              <Text size="xs" c="dimmed">Hashing {skeleton?.length ?? 0} events…</Text>
            </Stack>
          )}
          {chain.status === 'ready' && rows.length === 0 && (
            <Text size="xs" c="dimmed" ta="center" py="xl">No events match.</Text>
          )}
        </Card>

        <Text size="10px" c="dimmed" mt="xs">
          Showing {rows.length} of {chain.links.length.toLocaleString()} events.
        </Text>
      </Box>
    </Box>
  )
}
