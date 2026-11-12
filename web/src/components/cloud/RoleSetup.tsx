import { Anchor, Box, Button, Code, CopyButton, Group, List, SegmentedControl, Stack, Text } from '@mantine/core'
import { IconCheck, IconCopy, IconDownload, IconExternalLink, IconTerminal2 } from '@tabler/icons-react'
import { useState } from 'react'
import type { CloudPrepared } from '~/lib/cloud'
import { downloadText } from '~/lib/download'
import { FS } from '~/theme'

const CLOUDSHELL = 'https://console.aws.amazon.com/cloudshell/home'

/**
 * How to make Argus's roles in an account yourself: paste one command into
 * CloudShell, or upload the template in the CloudFormation console. Both
 * make or update the same `argus-access` stack, so allowing changes later is
 * the same steps with the other template. The roles have fixed names, so
 * nothing has to be copied back: Argus finds them by itself.
 */
export function RoleSetup({ prepared, update = false }: { prepared: CloudPrepared; update?: boolean }) {
  const [way, setWay] = useState<'cli' | 'console'>('cli')
  const id = prepared.accountId
  const file = `argus-access-${id}.yaml`
  const done = update ? 'UPDATE_COMPLETE' : 'CREATE_COMPLETE'
  return (
    <Stack gap="md" style={{ minWidth: 0 }}>
      <SegmentedControl value={way} onChange={(v) => setWay(v as 'cli' | 'console')} w="fit-content"
        data={[{ value: 'cli', label: 'One command' }, { value: 'console', label: 'Upload a file' }]} />
      {way === 'cli' ? (
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
              <span>Open CloudShell signed in to account {id}, paste it, and press Enter.</span>
              <Button size="xs" variant="default" component="a" href={CLOUDSHELL} target="_blank" rel="noreferrer"
                leftSection={<IconTerminal2 size={14} />} rightSection={<IconExternalLink size={12} />}>
                Open CloudShell
              </Button>
            </Group>
          </List.Item>
          <List.Item>That is all. It takes about a minute{update ? '' : ', and Argus connects as soon as the roles work'}.</List.Item>
        </List>
      ) : (
        <List type="ordered" spacing="sm" size="sm">
          <List.Item>
            <Group gap="xs">
              Download the template.
              <Button size="xs" variant="light" leftSection={<IconDownload size={14} />}
                onClick={() => downloadText(prepared.template, file, 'application/x-yaml')}>
                {file}
              </Button>
            </Group>
          </List.Item>
          {update ? (
            <>
              <List.Item>
                <span>
                  Sign in to account {id} and open{' '}
                  <Anchor href="https://console.aws.amazon.com/cloudformation/home#/stacks" target="_blank" rel="noreferrer" inherit display="inline">
                    CloudFormation › Stacks <IconExternalLink size={12} style={{ display: 'inline-block', verticalAlign: 'middle' }} />
                  </Anchor>.
                </span>
              </List.Item>
              <List.Item>Select <Code>argus-access</Code>, choose <b>Update</b> › <b>Replace current template</b>, and upload the file.</List.Item>
            </>
          ) : (
            <>
              <List.Item>
                Sign in to account {id} and open{' '}
                <Anchor href="https://console.aws.amazon.com/cloudformation/home#/stacks/create" target="_blank" rel="noreferrer">
                  CloudFormation › Create stack <IconExternalLink size={12} style={{ display: 'inline-block', verticalAlign: 'middle' }} />
                </Anchor>.
              </List.Item>
              <List.Item>Choose <b>Upload a template file</b>, pick the file, and name the stack <Code>argus-access</Code>.</List.Item>
            </>
          )}
          <List.Item>Tick <b>I acknowledge that AWS CloudFormation might create IAM resources with custom names</b>, then <b>Submit</b>.</List.Item>
          <List.Item>Wait for <b>{done}</b>. There is nothing to copy back{update ? '' : ': Argus connects by itself'}.</List.Item>
        </List>
      )}
      {way === 'cli' && (
        <Box style={{ minWidth: 0, maxWidth: '100%' }}>
          <Text size={FS.micro} c="dimmed" mb={4}>
            The command carries the template with it, so CloudShell does not need to reach Argus. It also works in any shell with the AWS CLI.
          </Text>
          <Code block fz={FS.micro} mah={160} style={{ overflow: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{prepared.command}</Code>
        </Box>
      )}
    </Stack>
  )
}
