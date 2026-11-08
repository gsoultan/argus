import { Anchor, Box, Button, Code, CopyButton, Group, List, SegmentedControl, Stack, Text } from '@mantine/core'
import { IconCheck, IconCopy, IconDownload, IconExternalLink } from '@tabler/icons-react'
import { useState } from 'react'
import type { CloudPrepared } from '~/lib/cloud'
import { downloadText } from '~/lib/download'
import { FS } from '~/theme'

/**
 * How to make Argus's roles in an account: upload the template in the
 * CloudFormation console, or paste one command into CloudShell. Both make
 * or update the same `argus-access` stack, so allowing changes later is
 * the same steps with the other template.
 */
export function RoleSetup({ prepared, update = false }: { prepared: CloudPrepared; update?: boolean }) {
  const [way, setWay] = useState<'console' | 'cli'>('console')
  const id = prepared.accountId
  const file = `argus-access-${id}.yaml`
  const outputs = prepared.manageRoleName ? <><Code>ReadRoleArn</Code> and <Code>ManageRoleArn</Code></> : <Code>ReadRoleArn</Code>
  return (
    <Stack gap="md">
      <SegmentedControl value={way} onChange={(v) => setWay(v as 'console' | 'cli')} w="fit-content"
        data={[{ value: 'console', label: 'In the AWS console' }, { value: 'cli', label: 'With CloudShell or the AWS CLI' }]} />
      {way === 'console' ? (
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
          <List.Item>When it says <b>{update ? 'UPDATE_COMPLETE' : 'CREATE_COMPLETE'}</b>, copy {outputs} from its <b>Outputs</b> tab.</List.Item>
        </List>
      ) : (
        <Stack gap="xs">
          <Text size="sm">
            Paste this into <Anchor href="https://console.aws.amazon.com/cloudshell/home" target="_blank" rel="noreferrer">AWS CloudShell</Anchor> while
            signed in to account {id}, or any shell with the AWS CLI. It carries the template with it and prints {outputs} at the end.
          </Text>
          <Box pos="relative">
            <Code block fz={FS.micro} mah={220} style={{ overflow: 'auto' }}>{prepared.command}</Code>
            <CopyButton value={prepared.command}>
              {({ copied, copy }) => (
                <Button size="xs" variant={copied ? 'light' : 'default'} color={copied ? 'teal' : undefined} pos="absolute" top={8} right={8}
                  leftSection={copied ? <IconCheck size={13} /> : <IconCopy size={13} />} onClick={copy}>
                  {copied ? 'Copied' : 'Copy'}
                </Button>
              )}
            </CopyButton>
          </Box>
        </Stack>
      )}
    </Stack>
  )
}
