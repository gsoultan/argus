import {
  Alert, Badge, Box, Button, Card, Group, Radio, SegmentedControl, SimpleGrid, Stack, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useMutation } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconBrandAws, IconCheck, IconCircleCheck, IconEye, IconPencil, IconShieldLock,
} from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  ENV_LABEL, prepareCloudAccount, useConnectCloudAccount,
  type CloudAccount, type CloudEnvironment, type CloudPrepared,
} from '~/lib/cloud'
import { RoleSetup } from '~/components/cloud/RoleSetup'
import { guard } from '~/lib/guard'
import { FS, INK, SP } from '~/theme'

/**
 * Connect an AWS account in three steps: say which account and whether Argus
 * may change it, create the role (or roles) in it, paste them back. Argus never asks for access
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
  const [manage, setManage] = useState(false)
  const [manageArn, setManageArn] = useState('')
  const [roleArn, setRoleArn] = useState('')
  const [done, setDone] = useState<CloudAccount | null>(null)
  const prepare = useMutation({ mutationFn: (id: string) => prepareCloudAccount(id, manage) })
  const connect = useConnectCloudAccount()
  const prepared: CloudPrepared | undefined = prepare.data

  const digits = accountId.replace(/[\s-]/g, '')
  const idOk = /^\d{12}$/.test(digits)
  const nameOk = name.trim().length > 0 && name.trim().length <= 64
  const arnHint = `arn:aws:iam::${digits || '123456789012'}:role/ArgusRead`

  const next = () => prepare.mutate(digits, { onSuccess: () => { setRoleArn(''); setManageArn(''); setStep(1) } })
  const submit = () => connect.mutate(
    { provider: 'aws', name: name.trim(), accountId: digits, roleArn: roleArn.trim(), environment: env,
      manageRoleArn: manage ? manageArn.trim() : undefined },
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
                  error={accountId && !idOk ? 'An AWS account id is 12 digits' : undefined} styles={MONO} />
                <Box>
                  <Text size="sm" fw={500}>Environment</Text>
                  <Text size={FS.meta} c="dimmed" mb={6}>Decides which approval rules apply once changes can be made from Argus.</Text>
                  <SegmentedControl value={env} onChange={(v) => setEnv(v as CloudEnvironment)}
                    data={ENVS.map((e) => ({ value: e, label: ENV_LABEL[e] }))} />
                </Box>
                <Box>
                  <Text size="sm" fw={500}>What Argus may do</Text>
                  <Text size={FS.meta} c="dimmed" mb={6}>You can allow changes later, and take them back.</Text>
                  <Radio.Group value={manage ? 'manage' : 'read'} onChange={(v) => setManage(v === 'manage')}>
                    <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                      <Choice value="read" icon={IconEye} title="Read only"
                        text="See instances, disks and firewalls. Nothing in the account can be changed from Argus." />
                      <Choice value="manage" icon={IconPencil} title="Read and change"
                        text="Also launch, stop, resize and remove instances, with approvals and the audit log." />
                    </SimpleGrid>
                  </Radio.Group>
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
                    <Text fw={600}>
                      {prepared.manageRoleName
                        ? <>Two roles, <Text span ff="monospace">{prepared.roleName}</Text> and <Text span ff="monospace">{prepared.manageRoleName}</Text>,</>
                        : <>A read-only role named <Text span ff="monospace">{prepared.roleName}</Text></>}
                      {' '}in {digits.replace(/(\d{4})(\d{4})(\d{4})/, '$1 $2 $3')}
                    </Text>
                    <Text size={FS.meta} c="dimmed" lh={1.55}>
                      {prepared.manageRoleName
                        ? 'The first can describe and list, never read data. The second can launch, start, stop, resize, tag and remove instances, and nothing in IAM. '
                        : 'It can describe and list, never read data or change anything. '}
                      {prepared.manageRoleName ? 'Both trust' : 'It trusts'} only this Argus
                      (<Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>), requires the external id
                      {' '}<Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, and requires the name of the person
                      using it, so CloudTrail shows who in Argus did what. CloudFormation does not charge for it.
                    </Text>
                  </Box>
                </Group>
                <RoleSetup prepared={prepared} />
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
                <TextInput label={manage ? 'Read role ARN' : 'Role ARN'} description="The stack's ReadRoleArn output." placeholder={arnHint}
                  value={roleArn} onChange={(e) => setRoleArn(e.currentTarget.value)} styles={MONO} data-autofocus />
                {manage && (
                  <TextInput label="Manage role ARN" description="The stack's ManageRoleArn output." placeholder={arnHint.replace('ArgusRead', 'ArgusManage')}
                    value={manageArn} onChange={(e) => setManageArn(e.currentTarget.value)} styles={MONO} />
                )}
                {connect.error && (
                  <Alert color="rose" variant="light" title="Argus could not use that role">{(connect.error as Error).message}</Alert>
                )}
                <Text size={FS.meta} c="dimmed">
                  Argus now assumes {manage ? 'both roles' : 'the role'} as you, checks {manage ? 'they are' : 'it is'} in account {digits}, and lists the regions it can read.
                  The connection is recorded in the audit log.
                </Text>
                <Group justify="space-between">
                  <Button variant="default" onClick={() => setStep(1)}>Back</Button>
                  <Button loading={connect.isPending} disabled={!roleArn.trim().startsWith('arn:') || (manage && !manageArn.trim().startsWith('arn:'))} onClick={submit}
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
                  <Badge variant="light" color={done.access === 'manage' ? 'azure' : 'slate'} radius="sm" tt="none">
                    {done.access === 'manage' ? 'Read and change' : 'Read only'}
                  </Badge>
                  <Text size={FS.micro} c={INK.brand} ff="monospace">{done.identityArn}</Text>
                </Group>
                <Group mt="md">
                  <Button variant="default" onClick={() => { setStep(0); setName(''); setAccountId(''); setManage(false); setDone(null); prepare.reset(); connect.reset() }}>
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

const MONO = { input: { fontFamily: 'var(--mantine-font-family-monospace)' } }

function Choice({ value, icon: Icon, title, text }: { value: string; icon: typeof IconEye; title: string; text: string }) {
  return (
    <Radio.Card value={value} radius="md" p="sm" style={{ height: '100%' }}>
      <Group wrap="nowrap" align="flex-start" gap="sm">
        <Radio.Indicator mt={2} />
        <Box>
          <Group gap={6}><Icon size={15} /><Text fw={600} size={FS.body}>{title}</Text></Group>
          <Text size={FS.meta} c="dimmed" lh={1.5} mt={2}>{text}</Text>
        </Box>
      </Group>
    </Radio.Card>
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
