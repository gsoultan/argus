import { useState } from 'react'
import { Alert, Box, Button, Group, Select, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconBrandWindows } from '@tabler/icons-react'
import { EmptyState } from '~/components/page'
import { isProductionAsset } from '~/components/server/ScriptInstall'
import { run as notifyRun } from '~/lib/notify'
import { signerShort, sizeOf, type SoftwareFile } from '~/lib/softwareApi'
import { softwareFilesQuery, useFileInstall } from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Installing an MSI or EXE from the library on a Windows server. The agent
 * fetches the exact file the library holds, checks its SHA-256, and runs it
 * quietly with the arguments the library keeps for it; what it changed comes
 * back in Recent changes.
 */
export function FileInstall({ asset, onClose }: { asset: Asset; onClose: () => void }) {
  const { data: files, error } = useQuery(softwareFilesQuery('windows'))
  const run = useFileInstall(asset.id)
  const [id, setId] = useState<string | null>(null)
  const file: SoftwareFile | undefined = files?.find((f) => f.id === id)
  const prod = isProductionAsset(asset)
  const send = () => file && void notifyRun(async () => {
    await run.mutateAsync(file.id)
    onClose()
  }, { failure: 'Not sent', success: ['Sent to the server', `It installs ${file.name} at its next check-in.`] })
  return (
    <Box p="md">
      <Stack gap={SP.cozy}>
        {error ? (
          <Alert color="rose"><Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text></Alert>
        ) : files && files.length === 0 ? (
          <EmptyState compact icon={IconBrandWindows} title="No Windows installers in the library."
            description="Add an MSI or EXE in Settings › Software, with the switches that make it install quietly." />
        ) : (
          <Select label="Installer" placeholder="Pick an MSI or EXE from the library" searchable value={id} onChange={setId}
            data={(files ?? []).map((f) => ({ value: f.id, label: `${f.name} (${f.kind.toUpperCase()})` }))} />
        )}
        {file && (
          <Alert color="slate" variant="light">
            <Text size={FS.body} fw={600}>{file.fileName}</Text>
            <Text size={FS.meta}>
              {sizeOf(file.size)} · {file.windows?.subject ? <>signed by {file.windows.subject} <Text span ff="monospace" title={file.windows.signer}>({signerShort(file.windows.signer!)})</Text></> : file.kind === 'msi' ? 'signature not read' : 'not signed'}
            </Text>
            <Text size={FS.meta} ff="monospace" mt={4}>
              {file.kind === 'msi' ? `msiexec /i ${file.fileName} /qn /norestart` : file.fileName} {file.windows?.args.join(' ')}
            </Text>
            <Text size={FS.micro} c="dimmed" mt={4}>SHA-256 {file.sha256}</Text>
          </Alert>
        )}
        {prod && (
          <Text size={FS.meta} c="dimmed">
            This server is tagged production: an installer runs here only once it has installed on a server outside production.
          </Text>
        )}
        <Text size={FS.meta} c="dimmed">
          Nobody is there to click Next: the installer runs as LocalSystem with the switches above, and a reboot it asks for is left to you.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!file} loading={run.isPending} onClick={send}>Install</Button>
        </Group>
      </Stack>
    </Box>
  )
}
