import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Group, Loader, Progress, Select, Table, Text,
  TextInput, ThemeIcon, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconDownload, IconFileDescription, IconSearch, IconShieldCheck,
  IconShieldX,
} from '@tabler/icons-react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { Digest, Mono, absTime, relTime } from '~/components/primitives'
import { FS, SP } from '~/theme'
import { downloadJSON, stamp } from '~/lib/download'
import { notifyOk, notifyWarn } from '~/lib/notify'
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

const LIMIT = 200

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
    return out.slice(0, LIMIT)
  }, [chain.links, search, severity])

  const verified = chain.verified
  const filtered = Boolean(search.trim() || severity)

  /**
   * Writes the whole chain out, not the filtered view.
   *
   * An evidence pack containing only the rows someone had searched for would be
   * unverifiable by definition — the chain is only checkable end to end. The
   * filter is a reading aid; the export is the record.
   */
  const onExport = () => {
    if (chain.links.length === 0) return
    if (!verified) {
      notifyWarn(
        'Exporting an unverified chain',
        'Run "Verify chain" first so the pack records a verdict rather than an absence of one.',
      )
    }
    downloadJSON(
      {
        exportedAt: new Date().toISOString(),
        // Named so a reader knows this was recomputed here rather than asserted
        // by the server that served the log.
        verification: verified
          ? {
              performedBy: 'argus-console (browser, Web Worker)',
              ok: verified.ok,
              linksChecked: verified.checked,
              brokenAtSequence: verified.brokenAt,
              durationMs: verified.ms,
            }
          : { performedBy: null, ok: null, note: 'chain was not verified before export' },
        algorithm: 'SHA-256(prevHash || canonicalJSON(event))',
        chainHead: chain.head,
        eventCount: chain.links.length,
        events: chain.links,
      },
      `argus-evidence-${stamp()}.json`,
    )
    notifyOk(
      'Evidence pack exported',
      `${chain.links.length.toLocaleString()} events with their hashes and the chain head.`,
    )
  }

  return (
    <Box>
      <PageHeader
        title="Audit log"
        description="Every privileged action, hash-chained so tampering is detectable rather than merely discouraged."
        actions={
          <>
            <Button
              variant="default"
              leftSection={<IconDownload size={14} />}
              disabled={chain.links.length === 0}
              onClick={onExport}
            >
              Export evidence pack
            </Button>
            <Button
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

      <PageBody>
        {/* The integrity panel is the product's core compliance claim, so it
            gets top billing rather than being buried in a settings page. */}
        <Card
          style={verified && !verified.ok ? { borderColor: 'var(--color-denied)' } : undefined}
        >
          <Group justify="space-between" wrap="nowrap" align="flex-start" gap="md">
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
                <Text size={FS.meta} c="dimmed" mt={SP.hair} maw={620} lh={1.5}>
                  Each entry is <Mono>SHA-256(prevHash ‖ canonical(event))</Mono>, over the
                  same six fields the control plane hashes. Verification runs in your browser,
                  in a Web Worker.{' '}
                  {chain.againstServer
                    ? 'It checks the hashes the control plane stored against the contents it served, so an intact result does not depend on trusting it.'
                    : 'These events carry no server hashes, so the console is confirming its own arithmetic — there is no served record to check against.'}
                </Text>
                {verified && (
                  <Group gap="xs" mt={SP.cozy}>
                    <Badge size="xs" color={verified.ok ? 'teal' : 'rose'} variant="light">
                      {verified.checked.toLocaleString()} links checked
                    </Badge>
                    <Badge size="xs" variant="outline" color="slate">
                      {verified.ms}ms off main thread
                    </Badge>
                    {/* Which claim the verdict supports. An operator must not
                        read "intact" as stronger than it is. */}
                    <Badge
                      size="xs"
                      variant="outline"
                      color={chain.againstServer ? 'teal' : 'amber'}
                    >
                      {chain.againstServer ? 'against the served record' : 'self-computed'}
                    </Badge>
                  </Group>
                )}
              </Box>
            </Group>

            <Box ta="right" style={{ flexShrink: 0 }}>
              <Text size={FS.micro} c="dimmed" fw={700} style={{ letterSpacing: '0.06em' }}>
                CHAIN HEAD
              </Text>
              <Box mt={SP.tight}>
                {chain.head ? <Digest value={chain.head} chars={24} /> : <Loader size="xs" />}
              </Box>
              {chain.ms !== null && (
                <Text size={FS.micro} c="dimmed" mt={SP.tight}>built in {chain.ms}ms</Text>
              )}
            </Box>
          </Group>

          {chain.status === 'working' && (
            <Progress value={chain.progress * 100} size="xs" mt="sm" color="azure" animated />
          )}
        </Card>

        {verified && !verified.ok && (
          <Alert
            color="rose"
            icon={<IconAlertTriangle size={17} />}
            title="Integrity failure"
          >
            <Text size={FS.body} lh={1.5}>
              The recomputed hash diverges from the stored value at sequence {verified.brokenAt}.
              Everything from that point forward is unverifiable. Preserve the current store,
              pull the offline replica, and treat this as an incident.
            </Text>
          </Alert>
        )}

        <Toolbar
          right={
            <Text size={FS.micro} c="dimmed">
              {rows.length === LIMIT
                ? `First ${LIMIT} of ${chain.links.length.toLocaleString()}`
                : `${rows.length} of ${chain.links.length.toLocaleString()} events`}
            </Text>
          }
        >
          <TextInput
            w={300}
            placeholder="Filter by actor, action, target or detail"
            leftSection={<IconSearch size={14} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
          />
          <Select
            w={150}
            placeholder="Any severity"
            clearable
            value={severity}
            onChange={setSeverity}
            data={['info', 'notice', 'warning', 'critical']}
          />
        </Toolbar>

        <DataTable
          minWidth={900}
          loading={chain.status === 'working' && rows.length === 0}
          isEmpty={rows.length === 0}
          columns={[
            { label: 'Seq', width: 60 },
            { label: 'When', width: 150 },
            'Action', 'Actor', 'Target', 'Detail',
            { label: 'Hash', width: 130 },
          ]}
          empty={
            <EmptyState
              icon={IconFileDescription}
              title="No events match."
              description={
                filtered
                  ? 'Nothing in the chain fits this filter. Note that the export always writes the whole chain regardless.'
                  : 'The audit log is empty. Every privileged action lands here as it happens.'
              }
            />
          }
        >
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
        </DataTable>
      </PageBody>
    </Box>
  )
}
