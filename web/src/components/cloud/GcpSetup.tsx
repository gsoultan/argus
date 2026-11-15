import { Anchor, Box, Button, Code, CopyButton, Group, List, Stack, Text } from '@mantine/core'
import { IconCheck, IconChevronDown, IconChevronRight, IconCopy, IconExternalLink, IconTerminal2 } from '@tabler/icons-react'
import { useState } from 'react'
import type { CloudPrepared } from '~/lib/cloud'
import { FS } from '~/theme'

/** Opens the Google Cloud console on the project with Cloud Shell started. */
export const cloudShellURL = (project: string) =>
  `https://console.cloud.google.com/home/dashboard?project=${encodeURIComponent(project)}&cloudshell=true`

/**
 * How to make argus-read in a Google Cloud project: paste one command into
 * Cloud Shell. Its name is fixed, so nothing is copied back: Argus finds it
 * by itself. The same steps by hand in the console are one click away.
 */
export function GcpSetup({ prepared }: { prepared: CloudPrepared }) {
  const [byHand, setByHand] = useState(false)
  const id = prepared.accountId
  return (
    <Stack gap="md" style={{ minWidth: 0 }}>
      <List type="ordered" spacing="sm" size="sm">
        <List.Item>
          <Group gap="xs">
            Copy the command.
            <CopyButton value={prepared.command}>
              {({ copied, copy }) => (
                <Button size="xs" variant={copied ? 'light' : 'filled'} color={copied ? 'teal' : undefined}
                  leftSection={copied ? <IconCheck size={13} /> : <IconCopy size={13} />} onClick={copy}>
                  {copied ? 'Copied' : 'Copy the command'}
                </Button>
              )}
            </CopyButton>
          </Group>
        </List.Item>
        <List.Item>
          <Group gap="xs">
            <span>Open Cloud Shell on {id}, paste it, and press Enter.</span>
            <Button size="xs" variant="default" component="a" href={cloudShellURL(id)} target="_blank" rel="noreferrer"
              leftSection={<IconTerminal2 size={14} />} rightSection={<IconExternalLink size={12} />}>
              Open Cloud Shell
            </Button>
          </Group>
        </List.Item>
        <List.Item>That is all. It takes under a minute, and Argus connects as soon as argus-read works.</List.Item>
      </List>
      <Box style={{ minWidth: 0, maxWidth: '100%' }}>
        <Text size={FS.micro} c="dimmed" mb={4}>
          Run it as someone who owns the project. It works in any shell with gcloud, and running it again changes nothing.
        </Text>
        <Code block fz={FS.micro} mah={180} style={{ overflow: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{prepared.command}</Code>
      </Box>
      <Box>
        <Anchor component="button" type="button" size={FS.meta} onClick={() => setByHand((v) => !v)}>
          <Group gap={4} wrap="nowrap">
            {byHand ? <IconChevronDown size={13} /> : <IconChevronRight size={13} />}
            Rather click through the console?
          </Group>
        </Anchor>
        {byHand && <ByHand prepared={prepared} />}
      </Box>
    </Stack>
  )
}

/** The command's three steps, in the console. */
function ByHand({ prepared }: { prepared: CloudPrepared }) {
  const id = encodeURIComponent(prepared.accountId)
  const description = `Argus reads this project's Compute Engine inventory through this account. argus-external-id=${prepared.externalId}`
  return (
    <List type="ordered" spacing="sm" size="sm" mt="sm">
      <List.Item>
        In{' '}
        <Anchor href={`https://console.cloud.google.com/iam-admin/serviceaccounts/create?project=${id}`} target="_blank" rel="noreferrer">
          IAM › Service accounts › Create <IconExternalLink size={12} style={{ display: 'inline-block', verticalAlign: 'middle' }} />
        </Anchor>, name it <Code>argus-read</Code> and give it this description exactly:
        <Group gap="xs" mt={4} wrap="nowrap" align="flex-start">
          <Code block fz={FS.micro} style={{ flex: 1, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{description}</Code>
          <CopyButton value={description}>
            {({ copied, copy }) => (
              <Button size="xs" variant="default" onClick={copy} leftSection={copied ? <IconCheck size={13} /> : <IconCopy size={13} />}>
                {copied ? 'Copied' : 'Copy'}
              </Button>
            )}
          </CopyButton>
        </Group>
      </List.Item>
      <List.Item>Give it the roles <b>Compute Viewer</b> and <b>Service Account Viewer</b> on the project, and press <b>Done</b>.</List.Item>
      <List.Item>
        Open <Code>argus-read</Code> › <b>Principals with access</b> › <b>Grant access</b>, add{' '}
        <Code style={{ overflowWrap: 'anywhere' }}>{prepared.principal}</Code> and give it <b>Service Account Token Creator</b>.
      </List.Item>
    </List>
  )
}
