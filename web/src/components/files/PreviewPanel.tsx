import { useEffect, useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Box, Button, Checkbox, CopyButton, Group, Loader, SegmentedControl, Stack, Text,
  TextInput, Tooltip,
} from '@mantine/core'
import { IconCopy, IconDownload, IconSearch, IconX } from '@tabler/icons-react'
import { FileOpError, looksBinary, type FilesClient, type Preview } from '~/lib/files/client'
import { baseName, formatSize } from '~/lib/files/paths'
import { FS } from '~/theme'

/**
 * The start or the end of a file, read on the server and shown here, so the
 * last lines of an 18 MB log do not need an 18 MB download. Recorded in the
 * audit chain like a download of those bytes.
 */
export function PreviewPanel({ client, path, onClose, onDownload, onOpened }: {
  client: FilesClient
  path: string
  onClose: () => void
  onDownload: () => void
  onOpened: (path: string) => void
}) {
  const [from, setFrom] = useState<'start' | 'end'>(() => (/\.log(\.\d+)?$|log$/i.test(path) ? 'end' : 'start'))
  const [preview, setPreview] = useState<Preview | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [find, setFind] = useState('')
  const [wrap, setWrap] = useState(true)

  useEffect(() => {
    let live = true
    setPreview(null)
    setError(null)
    client.preview(path, from).then(
      (p) => {
        if (!live) return
        setPreview(p)
        onOpened(p.path)
      },
      (err) => {
        if (!live) return
        setError(err instanceof FileOpError && err.unsupported
          ? 'The agent on this server is older than the console and cannot preview files. Download the file instead, or update the agent.'
          : err instanceof Error ? err.message : String(err))
      },
    )
    return () => {
      live = false
    }
    // onOpened is a notification, not an input.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, path, from])

  const lines = useMemo(() => {
    if (!preview) return []
    let all = preview.text.split('\n')
    // A read that started mid-file starts mid-line: drop the fragment.
    if (preview.from === 'end' && preview.truncated && all.length > 1) all = all.slice(1)
    if (all.length > 1 && all[all.length - 1] === '') all = all.slice(0, -1)
    return all
  }, [preview])
  const needle = find.trim().toLowerCase()
  const matches = needle ? lines.filter((l) => l.toLowerCase().includes(needle)).length : 0
  const binary = preview ? looksBinary(preview.text) : false

  return (
    <Stack gap={0} h="100%" className="argus-files-preview">
      <Group gap="sm" wrap="nowrap" px="md" py="sm" className="argus-files-preview-head">
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Text fw={600} size="sm" truncate>{baseName(path)}</Text>
          <Text size={FS.micro} c="dimmed" truncate>
            {preview
              ? preview.truncated
                ? `${formatSize(preview.size)} · showing the ${preview.from === 'end' ? 'last' : 'first'} ${formatSize(preview.text.length)}`
                : formatSize(preview.size)
              : 'Reading…'}
          </Text>
        </Box>
        <CopyButton value={preview?.path ?? path}>
          {({ copied, copy }) => (
            <Button size="xs" variant="default" leftSection={<IconCopy size={14} />} onClick={copy}>
              {copied ? 'Copied' : 'Copy path'}
            </Button>
          )}
        </CopyButton>
        <Button size="xs" leftSection={<IconDownload size={14} />} onClick={onDownload}>Download</Button>
        <Tooltip label="Close preview">
          <ActionIcon variant="subtle" color="slate" onClick={onClose} aria-label="Close preview">
            <IconX size={16} />
          </ActionIcon>
        </Tooltip>
      </Group>
      <Group gap="sm" wrap="nowrap" px="md" py="xs" className="argus-files-preview-tools">
        <SegmentedControl
          size="xs"
          value={from}
          onChange={(v) => setFrom(v as 'start' | 'end')}
          data={[{ value: 'start', label: 'Start' }, { value: 'end', label: 'End' }]}
          aria-label="Which end of the file"
        />
        <TextInput
          size="xs"
          style={{ flex: 1 }}
          placeholder="Find in preview"
          leftSection={<IconSearch size={14} />}
          value={find}
          onChange={(e) => setFind(e.currentTarget.value)}
          rightSection={needle ? <Text size={FS.micro} c="dimmed" pr={6}>{matches}</Text> : null}
          rightSectionWidth={needle ? 36 : undefined}
          aria-label="Find in preview"
        />
        <Checkbox size="xs" label="Wrap" checked={wrap} onChange={(e) => setWrap(e.currentTarget.checked)} />
      </Group>
      <Box className="argus-files-code" style={{ flex: 1, minHeight: 0 }}>
        {error ? (
          <Alert color="rose" m="md" title="No preview">{error}</Alert>
        ) : !preview ? (
          <Group gap="xs" p="md"><Loader size="xs" /><Text size={FS.meta} c="dimmed">Reading the file on the server…</Text></Group>
        ) : binary ? (
          <Alert color="slate" m="md" title="This looks like a binary file">
            Its bytes are not text, so there is nothing readable to show. Download it instead.
          </Alert>
        ) : lines.length === 0 ? (
          <Text size={FS.meta} c="dimmed" p="md">The file is empty.</Text>
        ) : (
          <pre className={wrap ? 'argus-files-pre wrap' : 'argus-files-pre'}>
            {lines.map((l, i) => {
              const hit = needle && l.toLowerCase().includes(needle)
              return (
                <div key={i} className={hit ? 'hit' : /\b(ERROR|FATAL|PANIC|CRIT(ICAL)?)\b/.test(l) ? 'err' : undefined}>
                  <span className="ln" aria-hidden>{i + 1}</span>
                  {l || ' '}
                </div>
              )
            })}
          </pre>
        )}
      </Box>
    </Stack>
  )
}
