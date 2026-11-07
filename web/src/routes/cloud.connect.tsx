import {
  Alert, Anchor, Badge, Box, Button, Card, Code, CopyButton, Group, List, SegmentedControl, SimpleGrid, Stack, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useMutation } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconBrandAws, IconCheck, IconCircleCheck, IconCopy, IconDownload, IconExternalLink, IconShieldLock,
} from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  ENV_LABEL, prepareCloudAccount, useConnectCloudAccount,
  type CloudAccount, type CloudEnvironment, type CloudPrepared,
} from '~/lib/cloud'
import { downloadText } from '~/lib/download'
import { guard } from '~/lib/guard'
import { FS, INK, SP } from '~/theme'

/**
 * Connect an AWS account in three steps: say which account, create a
 * read-only role in it, paste the role back. Argus never asks for access
 * keys; the role trusts only this control plane and requires an external id
 * only it can compute.
 */
export const Route = createFileRoute('/cloud/connect')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  component: Connect,
})

const ENVS: CloudEnvironment[] = ['production', 'staging', 'development']

function Connect() {
  const navigate = useNavigate()
  const [step, setStep] = useState(0)
  const [name, setName] = useState('')
  const [accountId, setAccountId] = useState('')
  const [env, setEnv] = useState<CloudEnvironment>('production')
  const [way, setWay] = useState<'console' | 'cli'>('console')
  const [roleArn, setRoleArn] = useState('')
  const [done, setDone] = useState<CloudAccount | null>(null)
  const prepare = useMutation({ mutationFn: (id: string) => prepareCloudAccount(id) })
  const connect = useConnectCloudAccount()
  const prepared: CloudPrepared | undefined = prepare.data

  const digits = accountId.replace(/[\s-]/g, '')
  const idOk = /^\d{12}$/.test(digits)
  const nameOk = name.trim().length > 0 && name.trim().length <= 64
  const arnHint = `arn:aws:iam::${digits || '123456789012'}:role/ArgusRead`

  const next = () => prepare.mutate(digits, { onSuccess: () => { setRoleArn(''); setStep(1) } })
  const submit = () => connect.mutate(
    { provider: 'aws', name: name.trim(), accountId: digits, roleArn: roleArn.trim(), environment: env },
    { onSuccess: (a) => { setDone(a); setStep(3) } },
  )

  return (
    <Box>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Connect an account' }]} title="Connect an AWS account"
        description="Argus reads the account through a role you create in it. No access keys are asked for or stored." />
      <PageBody>
        <Box maw={820} mx="auto">
          <Steps step={step} />

          {step === 0 && (
            <Card padding="lg">
              <Stack gap="md">
                <TextInput label="Name in Argus" description="How people here call it, for example payments-prod." placeholder="payments-prod"
                  value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={64} data-autofocus />
                <TextInput label="AWS account id" description="12 digits, shown under your name at the top right of the AWS console."
                  placeholder="4127 8893 1204" value={accountId} onChange={(e) => setAccountId(e.currentTarget.value)}
                  error={accountId && !idOk ? 'An AWS account id is 12 digits' : undefined} styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
                <Box>
                  <Text size="sm" fw={500}>Environment</Text>
                  <Text size={FS.meta} c="dimmed" mb={6}>Decides which approval rules apply once changes can be made from Argus.</Text>
                  <SegmentedControl value={env} onChange={(v) => setEnv(v as CloudEnvironment)}
                    data={ENVS.map((e) => ({ value: e, label: ENV_LABEL[e] }))} />
                </Box>
                {prepare.error && <Alert color="amber" variant="light" title="Argus cannot make the role yet">{(prepare.error as Error).message}</Alert>}
                <Group justify="space-between" mt="xs">
                  <ButtonLink to="/cloud" variant="default">Cancel</ButtonLink>
                  <Button disabled={!idOk || !nameOk} loading={prepare.isPending} onClick={next}>Next: create the role</Button>
                </Group>
              </Stack>
            </Card>
          )}

          {step === 1 && prepared && (
            <Card padding="lg">
              <Stack gap="md">
                <Group gap="sm" wrap="nowrap" align="flex-start">
                  <ThemeIcon variant="light" color="azure" size={34} radius="md"><IconShieldLock size={18} /></ThemeIcon>
                  <Box>
                    <Text fw={600}>A read-only role named <Text span ff="monospace">{prepared.roleName}</Text> in {digits.replace(/(\d{4})(\d{4})(\d{4})/, '$1 $2 $3')}</Text>
                    <Text size={FS.meta} c="dimmed" lh={1.55}>
                      It can describe and list, never read data or change anything. It trusts only this Argus
                      (<Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>), requires the external id
                      {' '}<Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, and requires the name of the person
                      using it, so CloudTrail shows who in Argus did what. CloudFormation does not charge for it.
                    </Text>
                  </Box>
                </Group>
                <SegmentedControl value={way} onChange={(v) => setWay(v as 'console' | 'cli')} w="fit-content"
                  data={[{ value: 'console', label: 'In the AWS console' }, { value: 'cli', label: 'With CloudShell or the AWS CLI' }]} />
                {way === 'console' ? (
                  <Stack gap="sm">
                    <List type="ordered" spacing="sm" size="sm">
                      <List.Item>
                        <Group gap="xs">
                          Download the template.
                          <Button size="xs" variant="light" leftSection={<IconDownload size={14} />}
                            onClick={() => downloadText(prepared.template, `argus-read-${digits}.yaml`, 'application/x-yaml')}>
                            argus-read-{digits}.yaml
                          </Button>
                        </Group>
                      </List.Item>
                      <List.Item>
                        Sign in to account {digits} and open{' '}
                        <Anchor href="https://console.aws.amazon.com/cloudformation/home#/stacks/create" target="_blank" rel="noreferrer">
                          CloudFormation › Create stack <IconExternalLink size={12} style={{ verticalAlign: 'middle' }} />
                        </Anchor>.
                      </List.Item>
                      <List.Item>Choose <b>Upload a template file</b>, pick the file, and name the stack <Code>argus-access</Code>.</List.Item>
                      <List.Item>Tick <b>I acknowledge that AWS CloudFormation might create IAM resources with custom names</b>, then <b>Submit</b>.</List.Item>
                      <List.Item>When it says <b>CREATE_COMPLETE</b>, copy <Code>ReadRoleArn</Code> from its <b>Outputs</b> tab.</List.Item>
                    </List>
                  </Stack>
                ) : (
                  <Stack gap="xs">
                    <Text size="sm">
                      Paste this into <Anchor href="https://console.aws.amazon.com/cloudshell/home" target="_blank" rel="noreferrer">AWS CloudShell</Anchor> while
                      signed in to account {digits}, or any shell with the AWS CLI. It carries the template with it and prints the role ARN at the end.
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
                <Group justify="space-between" mt="xs">
                  <Button variant="default" onClick={() => setStep(0)}>Back</Button>
                  <Button onClick={() => setStep(2)}>Next: paste the role</Button>
                </Group>
              </Stack>
            </Card>
          )}

          {step === 2 && (
            <Card padding="lg">
              <Stack gap="md">
                <TextInput label="Role ARN" description="The stack's ReadRoleArn output." placeholder={arnHint}
                  value={roleArn} onChange={(e) => setRoleArn(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} data-autofocus />
                {connect.error && (
                  <Alert color="rose" variant="light" title="Argus could not use that role">{(connect.error as Error).message}</Alert>
                )}
                <Text size={FS.meta} c="dimmed">
                  Argus now assumes the role as you, checks it is in account {digits}, and lists the regions it can read.
                  The connection is recorded in the audit log.
                </Text>
                <Group justify="space-between">
                  <Button variant="default" onClick={() => setStep(1)}>Back</Button>
                  <Button loading={connect.isPending} disabled={!roleArn.trim().startsWith('arn:')} onClick={submit}
                    leftSection={<IconBrandAws size={16} />}>
                    Check and connect
                  </Button>
                </Group>
              </Stack>
            </Card>
          )}

          {step === 3 && done && (
            <Card padding="xl">
              <Stack align="center" gap="sm" py="md">
                <ThemeIcon size={52} radius="xl" variant="light" color="teal"><IconCircleCheck size={30} /></ThemeIcon>
                <Text fw={700} size="lg">{done.name} is connected</Text>
                <Text size={FS.body} c="dimmed" ta="center" maw={520}>
                  Argus is reading its {done.regions.length} regions now. Instances, disks and firewalls appear in a minute or two,
                  and are read again every five minutes.
                </Text>
                <Group gap={SP.snug} mt={4}>
                  <Badge variant="light" color="slate" radius="sm" tt="none">{ENV_LABEL[done.environment]}</Badge>
                  <Badge variant="light" color="slate" radius="sm" tt="none">Read only</Badge>
                  <Text size={FS.micro} c={INK.brand} ff="monospace">{done.identityArn}</Text>
                </Group>
                <Group mt="md">
                  <Button variant="default" onClick={() => { setStep(0); setName(''); setAccountId(''); setDone(null); prepare.reset(); connect.reset() }}>
                    Connect another
                  </Button>
                  <Button onClick={() => void navigate({ to: '/cloud' })}>Go to Cloud</Button>
                </Group>
              </Stack>
            </Card>
          )}
        </Box>
      </PageBody>
    </Box>
  )
}

const STEPS = [
  ['The account', 'Name and id'],
  ['Create the role', 'One stack, free'],
  ['Paste the role', 'Argus checks it'],
] as const

/** Where the person is in the three steps: done in teal, current in azure. */
function Steps({ step }: { step: number }) {
  return (
    <SimpleGrid cols={3} spacing="sm" mb="xl" aria-label="Steps">
      {STEPS.map(([t, d], i) => {
        const state = i < step ? 'done' : i === step ? 'current' : 'next'
        const color = state === 'done' ? 'var(--mantine-color-teal-6)' : state === 'current' ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'
        return (
          <Box key={t} pt={SP.cozy} style={{ borderTop: `3px solid ${color}` }} aria-current={state === 'current' ? 'step' : undefined}>
            <Group gap={6} wrap="nowrap">
              {state === 'done' && <IconCheck size={14} color="var(--mantine-color-teal-6)" />}
              <Text size={FS.meta} fw={600} c={state === 'next' ? 'dimmed' : state === 'done' ? INK.verified : INK.brand}>{i + 1} · {t}</Text>
            </Group>
            <Text size={FS.micro} c="dimmed" visibleFrom="sm">{d}</Text>
          </Box>
        )
      })}
    </SimpleGrid>
  )
}
