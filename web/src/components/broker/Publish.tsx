import {
  ActionIcon, Alert, Box, Button, Drawer, Group, SegmentedControl, Select, SimpleGrid, Stack, Text, TextInput, Textarea,
} from '@mantine/core'
import { IconCheck, IconPlus, IconX } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { notifyError, notifyOk } from '~/lib/notify'
import { routeKey } from '~/lib/rabbit'
import { FS } from '~/theme'
import { MONO, useBroker } from './BrokerWorkspace'

/*
Publish one message, with the properties consumers actually branch on. The
routing key is matched against the bindings as it is typed, so a message
that would be dropped says so before it is sent.
*/

export interface PublishPreset { exchange?: string; routingKey?: string; payload?: string }

export function PublishDrawer({ preset, onClose }: { preset: PublishPreset; onClose: () => void }) {
  const { exchanges, bindings, queues, ask, reload, narrow } = useBroker()
  const [exchange, setExchange] = useState(preset.exchange ?? '')
  const [key, setKey] = useState(preset.routingKey ?? '')
  const [payload, setPayload] = useState(preset.payload ?? '')
  const [contentType, setContentType] = useState('application/json')
  const [mode, setMode] = useState('2')
  const [messageId, setMessageId] = useState('')
  const [correlationId, setCorrelationId] = useState('')
  const [headers, setHeaders] = useState<{ k: string; v: string }[]>([])
  const [sending, setSending] = useState(false)

  const hdrs = useMemo(() => Object.fromEntries(headers.filter((h) => h.k.trim()).map((h) => [h.k.trim(), h.v])), [headers])
  const routes = useMemo(() => routeKey(exchange, key, hdrs, exchanges, bindings, queues), [exchange, key, hdrs, exchanges, bindings, queues])
  const json = useMemo(() => {
    if (!payload.trim() || !contentType.includes('json')) return null
    try { JSON.parse(payload); return true } catch { return false }
  }, [payload, contentType])

  const format = () => { try { setPayload(JSON.stringify(JSON.parse(payload), null, 2)) } catch { /* not JSON */ } }

  const send = async () => {
    setSending(true)
    const properties: Record<string, string> = { delivery_mode: mode }
    if (contentType) properties.content_type = contentType
    if (messageId) properties.message_id = messageId
    if (correlationId) properties.correlation_id = correlationId
    const a = await ask({ op: 'publish', exchange, routingKey: key, payload, headers: hdrs, properties })
    setSending(false)
    if (a.type !== 'result') { notifyError('Not published', new Error(a.error ?? 'failed')); return }
    const routed = (a.data as { routed?: boolean } | undefined)?.routed
    if (routed) notifyOk('Published', 'RabbitMQ routed it to at least one queue. It is in the session’s recording.')
    else notifyError('Published, but not routed', new Error('No binding matched, so RabbitMQ dropped it.'))
    void reload()
    if (routed) onClose()
  }

  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 520} title={<Text fw={650}>Publish a message</Text>}>
      <Stack gap="sm">
        <Group grow align="flex-start" wrap={narrow ? 'wrap' : 'nowrap'}>
          <Select label="Exchange" value={exchange} onChange={(v) => setExchange(v ?? '')} allowDeselect={false} searchable
            data={[{ value: '', label: '(default) by queue name' },
              ...exchanges.filter((x) => x.name && !x.internal).map((x) => ({ value: x.name, label: `${x.name} (${x.type})` }))]} />
          <TextInput label="Routing key" styles={MONO} value={key} onChange={(e) => setKey(e.currentTarget.value)}
            placeholder={exchange ? 'orders.created' : queues[0]?.name ?? 'queue name'} />
        </Group>
        {(key || exchange) && (
          <Alert variant="light" color={routes.length ? 'teal' : 'amber'} icon={routes.length ? <IconCheck size={16} /> : <IconX size={16} />} py={8}>
            {routes.length
              ? <>Will reach {routes.length} queue{routes.length === 1 ? '' : 's'}: <Text span ff="monospace" size={FS.meta}>{routes.map((r) => r.queue).join(', ')}</Text>.</>
              : <>Reaches no queue: RabbitMQ would drop it.</>}
          </Alert>
        )}
        <Box>
          <Group gap="xs" mb={4}>
            <Text size={FS.meta} fw={500}>Payload</Text>
            {json !== null && <Text size={FS.micro} c={json ? 'teal' : 'rose'} ml="auto">{json ? 'valid JSON' : 'not valid JSON'}</Text>}
          </Group>
          <Textarea value={payload} onChange={(e) => setPayload(e.currentTarget.value)} autosize minRows={6} maxRows={16}
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} placeholder='{"id": 1}' aria-label="Payload" />
          {json && <Button size="compact-xs" variant="subtle" mt={4} onClick={format}>Format</Button>}
        </Box>
        <Text size={FS.meta} fw={600} mt="xs">Properties</Text>
        <SimpleGrid cols={2} spacing="sm">
          <TextInput label="Content type" styles={MONO} value={contentType} onChange={(e) => setContentType(e.currentTarget.value)} />
          <Box>
            <Text size={FS.meta} fw={500} mb={4}>Delivery</Text>
            <SegmentedControl size="xs" value={mode} onChange={setMode} data={[{ label: 'Persistent', value: '2' }, { label: 'Transient', value: '1' }]} />
          </Box>
          <TextInput label="Message id" styles={MONO} value={messageId} onChange={(e) => setMessageId(e.currentTarget.value)} placeholder="optional" />
          <TextInput label="Correlation id" styles={MONO} value={correlationId} onChange={(e) => setCorrelationId(e.currentTarget.value)} placeholder="optional" />
        </SimpleGrid>
        <Text size={FS.meta} fw={500} mt="xs">Headers</Text>
        {headers.map((h, i) => (
          <Group key={i} gap="xs" wrap="nowrap">
            <TextInput size="xs" styles={MONO} placeholder="name" value={h.k} style={{ flex: 1 }} aria-label="Header name"
              onChange={(e) => { const v = e.currentTarget.value; setHeaders((hs) => hs.map((x, j) => (j === i ? { ...x, k: v } : x))) }} />
            <TextInput size="xs" styles={MONO} placeholder="value" value={h.v} style={{ flex: 1 }} aria-label="Header value"
              onChange={(e) => { const v = e.currentTarget.value; setHeaders((hs) => hs.map((x, j) => (j === i ? { ...x, v } : x))) }} />
            <ActionIcon variant="subtle" color="slate" aria-label="Remove header" onClick={() => setHeaders((hs) => hs.filter((_, j) => j !== i))}><IconX size={14} /></ActionIcon>
          </Group>
        ))}
        <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }}
          onClick={() => setHeaders((hs) => [...hs, { k: '', v: '' }])} disabled={headers.length >= 64}>Add header</Button>
        <Group mt="md" gap="xs">
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>The payload, headers and properties are kept in the recording.</Text>
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void send()} loading={sending} disabled={!payload && !key}>Publish</Button>
        </Group>
      </Stack>
    </Drawer>
  )
}
