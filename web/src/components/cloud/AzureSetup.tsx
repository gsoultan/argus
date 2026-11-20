import { Anchor, Box, Button, Code, CopyButton, Group, List, Stack, Text } from '@mantine/core'
import { IconCheck, IconChevronDown, IconChevronRight, IconCopy, IconExternalLink, IconTerminal2 } from '@tabler/icons-react'
import { useState } from 'react'
import type { CloudPrepared } from '~/lib/cloud'
import { FS } from '~/theme'

/** Opens Azure Cloud Shell, signed in as whoever is signed in to the portal. */
export const AZURE_CLOUD_SHELL = 'https://shell.azure.com/bash'

/** The subscription's Access control (IAM) page in the Azure portal. */
export const azureAccessURL = (subscription: string) =>
  `https://portal.azure.com/#@/resource/subscriptions/${encodeURIComponent(subscription)}/users`

/** What the Reader assignment's description must say, exactly as the command writes it. */
export const azureDescription = (externalId: string) =>
  `Argus reads this subscription's inventory through this assignment. argus-external-id=${externalId}`

/**
 * How to let Argus read an Azure subscription: paste one command into Cloud
 * Shell. It gives argus-control the Reader role at the subscription, and
 * the Argus manage role too when changes are allowed, so nothing is copied
 * back: Argus finds the assignments by itself. The same
 * steps by hand in the portal are one click away.
 */
export function AzureSetup({ prepared }: { prepared: CloudPrepared }) {
  const [byHand, setByHand] = useState(false)
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
            <span>Open Azure Cloud Shell (Bash), paste it, and press Enter.</span>
            <Button size="xs" variant="default" component="a" href={AZURE_CLOUD_SHELL} target="_blank" rel="noreferrer"
              leftSection={<IconTerminal2 size={14} />} rightSection={<IconExternalLink size={12} />}>
              Open Cloud Shell
            </Button>
          </Group>
        </List.Item>
        <List.Item>
          That is all. Azure can take a few minutes to apply a new role, and Argus connects as soon as it can read the subscription.
        </List.Item>
      </List>
      <Box style={{ minWidth: 0, maxWidth: '100%' }}>
        <Text size={FS.micro} c="dimmed" mb={4}>
          Run it as someone who is Owner or User Access Administrator of the subscription. It works in any Bash shell with the
          Azure CLI, and running it again changes nothing.
        </Text>
        <Code block fz={FS.micro} mah={180} style={{ overflow: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{prepared.command}</Code>
      </Box>
      <Box>
        <Anchor component="button" type="button" size={FS.meta} onClick={() => setByHand((v) => !v)}>
          <Group gap={4} wrap="nowrap">
            {byHand ? <IconChevronDown size={13} /> : <IconChevronRight size={13} />}
            Rather click through the portal?
          </Group>
        </Anchor>
        {byHand && <ByHand prepared={prepared} />}
      </Box>
    </Stack>
  )
}

/** The command's steps, in the portal. */
function ByHand({ prepared }: { prepared: CloudPrepared }) {
  const description = azureDescription(prepared.externalId)
  return (
    <List type="ordered" spacing="sm" size="sm" mt="sm">
      <List.Item>
        Open the subscription's{' '}
        <Anchor href={azureAccessURL(prepared.accountId)} target="_blank" rel="noreferrer">
          Access control (IAM) <IconExternalLink size={12} style={{ display: 'inline-block', verticalAlign: 'middle' }} />
        </Anchor>{' '}
        and choose <b>Add</b> › <b>Add role assignment</b>.
      </List.Item>
      <List.Item>Pick the role <b>Reader</b>, under Job function roles.</List.Item>
      <List.Item>
        Under <b>Members</b>, select argus-control's application by its id{' '}
        <Code style={{ overflowWrap: 'anywhere' }}>{prepared.principal}</Code>. If the search does not find it, the
        application is not in this directory yet: run{' '}
        <Code style={{ overflowWrap: 'anywhere' }}>az ad sp create --id {prepared.principal}</Code> in Cloud Shell first.
      </List.Item>
      <List.Item>
        Give the assignment this description exactly, then <b>Review + assign</b>:
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
      {prepared.manageRoleName && (
        <List.Item>
          That lets Argus read. The role for changes, <b>{prepared.manageRoleName}</b>, cannot be made in the portal: Argus looks for it
          under an id it works out from the subscription, and the portal picks its own. Run the command above for that part.
        </List.Item>
      )}
    </List>
  )
}
