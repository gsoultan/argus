import {
  Alert, Badge, Box, Button, Card, Collapse, Group, Radio, SegmentedControl, SimpleGrid, Stack, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useMutation } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconCheck, IconCircleCheck, IconEye, IconPencil, IconShieldLock,
} from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  ENV_LABEL, prepareCloudAccount, useConnectCloudAccount,
  type CloudAccount, type CloudEnvironment, type CloudPrepared,
} from '~/lib/cloud'
import { MakeItForMe, WatchForRoles } from '~/components/cloud/ConnectRoles'
import { RoleSetup } from '~/components/cloud/RoleSetup'
import { guard } from '~/lib/guard'
import { FS, INK, SP } from '~/theme'

/**
 * Connect an AWS account in two steps: say which account and whether Argus
 * may change it, then make the roles in it. The roles have fixed names, so
 * Argus watches for them and connects by itself; nothing is copied back.
 * Where argus-control's own identity is in that account, Argus can make
 * them itself. Argus never asks for access keys; the roles trust only this
 * control plane and require an external id only it can compute.
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
  const [done, setDone] = useState<CloudAccount | null>(null)
  const prepare = useMutation({ mutationFn: (id: string) => prepareCloudAccount(id, manage) })
  const connect = useConnectCloudAccount()
  const prepared: CloudPrepared | undefined = prepare.data

  const digits = accountId.replace(/[\s-]/g, '')
  const idOk = /^\d{12}$/.test(digits)
  const nameOk = name.trim().length > 0 && name.trim().length <= 64

  const next = () => prepare.mutate(digits, { onSuccess: () => { connect.reset(); setStep(1) } })
  const submit = () => connect.mutate(
    { provider: 'aws', name: name.trim(), accountId: digits, environment: env, manage },
    { onSuccess: (a) => { setDone(a); setStep(2) } },
  )
  const reset = () => { setStep(0); setName(''); setAccountId(''); setManage(false); setDone(null); prepare.reset(); connect.reset() }

  return (
    <Box>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Connect an account' }]} title="Connect an AWS account"
        description="Argus reads the account through a role in it. No access keys are asked for or stored." />
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
              <Stack gap="lg">
                <RoleSummary prepared={prepared} digits={digits} />
                {prepared.canDeploy && <MakeItForMe prepared={prepared} name={name.trim()} manage={manage} />}
                <Box style={{ minWidth: 0 }}>
                  {prepared.canDeploy && <Text fw={600} size={FS.body} mb="xs">Or make it yourself</Text>}
                  <RoleSetup prepared={prepared} />
                </Box>
                <WatchForRoles prepared={prepared} manage={manage} connecting={connect.isPending} failed={connect.error as Error | null}
                  onReady={submit} />
                <Group justify="space-between">
                  <Button variant="default" onClick={() => setStep(0)}>Back</Button>
                </Group>
              </Stack>
            </Card>
          )}

          {step === 2 && done && (
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
                  <Button variant="default" onClick={reset}>Connect another</Button>
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

/** What the role is and what it may do, with the details one click away. */
function RoleSummary({ prepared, digits }: { prepared: CloudPrepared; digits: string }) {
  const [more, setMore] = useState(false)
  const two = !!prepared.manageRoleName
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <ThemeIcon variant="light" color="azure" size={34} radius="md"><IconShieldLock size={18} /></ThemeIcon>
      <Box>
        <Text fw={600}>
          Argus needs {two ? 'two roles' : 'a read-only role'} in {digits.replace(/(\d{4})(\d{4})(\d{4})/, '$1 $2 $3')}
        </Text>
        <Text size={FS.meta} c="dimmed" lh={1.55}>
          {two
            ? 'One to describe and list, never read data; one to launch, start, stop, resize, tag and remove instances, and nothing in IAM. '
            : 'It can describe and list, never read data or change anything. '}
          They trust only this Argus and cost nothing.{' '}
          <Text span size={FS.meta} c="azure" style={{ cursor: 'pointer' }} onClick={() => setMore((m) => !m)}>
            {more ? 'Less' : 'How it is locked down'}
          </Text>
        </Text>
        <Collapse expanded={more}>
          <Text size={FS.meta} c="dimmed" lh={1.55} mt={4}>
            {two ? 'Both roles' : 'The role'} (<Text span ff="monospace" size={FS.meta}>{prepared.roleName}</Text>
            {two && <>, <Text span ff="monospace" size={FS.meta}>{prepared.manageRoleName}</Text></>}) trust only{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>, require the external id{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, and require the name of the person using
            them, so CloudTrail shows who in Argus did what. They are made by one CloudFormation stack, <Text span ff="monospace" size={FS.meta}>argus-access</Text>,
            which CloudFormation does not charge for.
          </Text>
        </Collapse>
      </Box>
    </Group>
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
  ['Create the role', 'Argus connects by itself'],
] as const

/** Where the person is in the two steps: done in teal, current in azure. */
function Steps({ step }: { step: number }) {
  return (
    <SimpleGrid cols={2} spacing="sm" mb="xl" aria-label="Steps">
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
