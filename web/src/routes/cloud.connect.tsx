import {
  Alert, Badge, Box, Button, Card, Group, Radio, SegmentedControl, SimpleGrid, Stack, Text, TextInput, ThemeIcon,
} from '@mantine/core'
import { useMutation, useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconBrandAws, IconBrandAzure, IconBrandGoogle, IconCheck, IconCircleCheck, IconEye, IconPencil, IconShieldLock,
} from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { PageBody, PageHeader } from '~/components/page'
import {
  AZURE_SUBSCRIPTION, ENV_LABEL, GCP_PROJECT, PROVIDER_LABEL, accountIdText, cloudAccountsQuery, prepareCloudAccount, useConnectCloudAccount,
  type CloudAccount, type CloudEnvironment, type CloudPrepared, type CloudProvider,
} from '~/lib/cloud'
import { AzureSetup } from '~/components/cloud/AzureSetup'
import { GcpSetup } from '~/components/cloud/GcpSetup'
import { MakeItForMe, WatchForRoles } from '~/components/cloud/ConnectRoles'
import { IdentityPicker } from '~/components/cloud/IdentityPicker'
import { RoleSetup } from '~/components/cloud/RoleSetup'
import { guard } from '~/lib/guard'
import { FS, INK, SP } from '~/theme'

/**
 * Connect an AWS account, a Google Cloud project or an Azure subscription in
 * two steps: say which one and, where Argus can change it, whether it may,
 * then give Argus access in it. What Argus looks for has a fixed name, so
 * Argus watches for it and connects by itself; nothing is copied back. Where
 * argus-control's own AWS identity is in that account, Argus can make the
 * roles itself. Argus never asks for access keys; the access trusts only
 * this control plane and carries an external id only it can compute.
 */
export const Route = createFileRoute('/cloud/connect')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: (s: Record<string, unknown>): { provider?: CloudProvider } =>
    s.provider === 'gcp' || s.provider === 'aws' || s.provider === 'azure' ? { provider: s.provider } : {},
  component: Connect,
})

const ENVS: CloudEnvironment[] = ['production', 'staging', 'development']

function Connect() {
  const navigate = useNavigate()
  const provider: CloudProvider = Route.useSearch().provider ?? 'aws'
  const gcp = provider === 'gcp'
  const azure = provider === 'azure'
  const setProvider = (p: CloudProvider) => { reset(); void navigate({ to: '/cloud/connect', search: p === 'aws' ? {} : { provider: p }, replace: true }) }
  const [step, setStep] = useState(0)
  const [name, setName] = useState('')
  const [accountId, setAccountId] = useState('')
  const [env, setEnv] = useState<CloudEnvironment>('production')
  const [manage, setManage] = useState(false)
  const [done, setDone] = useState<CloudAccount | null>(null)
  const bases = useQuery(cloudAccountsQuery()).data?.bases?.filter((b) => (b.provider || 'aws') === provider)
  const [picked, setPicked] = useState('')
  // The default when this organization may use it, else the only one.
  const identity = picked || (bases?.find((b) => b.default) ?? (bases?.length === 1 ? bases[0] : undefined))?.identity || ''
  const prepare = useMutation({ mutationFn: (id: string) => prepareCloudAccount(id, manage, identity, '', provider) })
  const connect = useConnectCloudAccount()
  const prepared: CloudPrepared | undefined = prepare.data

  const digits = gcp || azure ? accountId.trim().toLowerCase() : accountId.replace(/[\s-]/g, '')
  const idOk = azure ? AZURE_SUBSCRIPTION.test(digits) : gcp ? GCP_PROJECT.test(digits) : /^\d{12}$/.test(digits)
  const nameOk = name.trim().length > 0 && name.trim().length <= 64

  const next = () => prepare.mutate(digits, { onSuccess: () => { connect.reset(); setStep(1) } })
  const submit = () => connect.mutate(
    { provider, name: name.trim(), accountId: digits, environment: env, manage, identity: prepared?.identity ?? identity },
    { onSuccess: (a) => { setDone(a); setStep(2) } },
  )
  const reset = () => { setStep(0); setName(''); setAccountId(''); setManage(false); setPicked(''); setDone(null); prepare.reset(); connect.reset() }
  const choose = (bases?.length ?? 0) > 1

  return (
    <Box>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: azure ? 'Connect a subscription' : gcp ? 'Connect a project' : 'Connect an account' }]}
        title={azure ? 'Connect an Azure subscription' : gcp ? 'Connect a Google Cloud project' : 'Connect an AWS account'}
        description={azure
          ? 'Argus reads the subscription through the Reader role you give it there. No keys are asked for or stored.'
          : gcp
            ? 'Argus reads the project through a service account in it. No keys are asked for or stored.'
            : 'Argus reads the account through a role in it. No access keys are asked for or stored.'} />
      <PageBody>
        <Box maw={820} mx="auto">
          <Steps step={step} provider={provider} />

          {step === 0 && (
            <Card padding="lg">
              <Stack gap="md">
                <Box>
                  <Text size="sm" fw={500} mb={6}>Cloud</Text>
                  <SegmentedControl value={provider} onChange={(v) => setProvider(v as CloudProvider)} aria-label="Cloud"
                    data={[
                      { value: 'aws', label: <Group gap={6} wrap="nowrap"><IconBrandAws size={16} />{PROVIDER_LABEL.aws}</Group> },
                      { value: 'gcp', label: <Group gap={6} wrap="nowrap"><IconBrandGoogle size={15} />{PROVIDER_LABEL.gcp}</Group> },
                      { value: 'azure', label: <Group gap={6} wrap="nowrap"><IconBrandAzure size={15} />{PROVIDER_LABEL.azure}</Group> },
                    ]} />
                </Box>
                <TextInput label="Name in Argus" description="How people here call it, for example payments-prod." placeholder="payments-prod"
                  value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={64} data-autofocus />
                {azure ? (
                  <TextInput label="Subscription id" description="Shown under Subscriptions in the Azure portal, on the subscription's Overview."
                    placeholder="6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90" value={accountId} onChange={(e) => setAccountId(e.currentTarget.value)}
                    error={accountId && !idOk ? 'A subscription id is a GUID: 32 letters a to f and digits, in five groups' : undefined} styles={MONO} />
                ) : gcp ? (
                  <TextInput label="Project id" description="Shown in the project picker at the top of the Google Cloud console, next to the project's name."
                    placeholder="payments-prod-418203" value={accountId} onChange={(e) => setAccountId(e.currentTarget.value)}
                    error={accountId && !idOk ? 'A project id is 6 to 30 lower case letters, digits and dashes, starting with a letter' : undefined} styles={MONO} />
                ) : (
                  <TextInput label="AWS account id" description="12 digits, shown under your name at the top right of the AWS console."
                    placeholder="4127 8893 1204" value={accountId} onChange={(e) => setAccountId(e.currentTarget.value)}
                    error={accountId && !idOk ? 'An AWS account id is 12 digits' : undefined} styles={MONO} />
                )}
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
                        text={azure
                          ? 'See VMs, disks, snapshots, images, network security groups, virtual networks and public addresses. Nothing in the subscription can be changed from Argus.'
                          : gcp
                          ? 'See VMs, disks, firewalls, networks and addresses. Nothing in the project can be changed from Argus.'
                          : 'See instances, disks and firewalls. Nothing in the account can be changed from Argus.'} />
                      <Choice value="manage" icon={IconPencil} title="Read and change"
                        text={azure
                          ? 'Also start, stop and restart VMs, change their size and their tags, and make, grow, attach, snapshot and delete disks, with approvals and the audit log. More changes come later.'
                          : gcp
                          ? 'Also start, stop and reset VMs, change their machine type, labels and deletion protection, make, grow, attach, detach, snapshot and delete disks, reserve and assign static addresses, launch and remove VMs, make and delete images, and make and change firewall rules, with approvals and the audit log. More changes come later.'
                          : 'Also launch, stop, resize and remove instances, with approvals and the audit log.'} />
                    </SimpleGrid>
                  </Radio.Group>
                </Box>
                {choose && bases && <IdentityPicker bases={bases} value={identity} onChange={setPicked} />}
                {prepare.error && <Alert color="amber" variant="light" title="Argus cannot make the role yet">{(prepare.error as Error).message}</Alert>}
                <Group justify="space-between" mt="xs">
                  <ButtonLink to="/cloud" variant="default">Cancel</ButtonLink>
                  <Button disabled={!idOk || !nameOk || (choose && !identity)} loading={prepare.isPending} onClick={next}>
                    {gcp || azure ? 'Next: give Argus access' : 'Next: create the role'}
                  </Button>
                </Group>
              </Stack>
            </Card>
          )}

          {step === 1 && prepared && (
            <Card padding="lg">
              <Stack gap="lg">
                {azure ? <AzureSummary prepared={prepared} /> : gcp ? <GcpSummary prepared={prepared} /> : <RoleSummary prepared={prepared} digits={digits} />}
                {prepared.canDeploy && <MakeItForMe prepared={prepared} name={name.trim()} manage={manage} />}
                <Box style={{ minWidth: 0 }}>
                  {prepared.canDeploy && <Text fw={600} size={FS.body} mb="xs">Or make it yourself</Text>}
                  {azure ? <AzureSetup prepared={prepared} /> : gcp ? <GcpSetup prepared={prepared} /> : <RoleSetup prepared={prepared} />}
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
                  {azure
                    ? `Argus is reading its ${done.regions.length} regions now. VMs, disks and network security groups appear in a minute or two, and are read again every five minutes.`
                    : gcp
                    ? `Argus is reading its ${done.regions.filter((r) => r !== 'global').length} regions now. VMs, disks and firewalls appear in a minute or two, and are read again every five minutes.`
                    : `Argus is reading its ${done.regions.length} regions now. Instances, disks and firewalls appear in a minute or two, and are read again every five minutes.`}
                </Text>
                <Group gap={SP.snug} mt={4}>
                  <Badge variant="light" color="slate" radius="sm" tt="none">{ENV_LABEL[done.environment]}</Badge>
                  <Badge variant="light" color={done.access === 'manage' ? 'azure' : 'slate'} radius="sm" tt="none">
                    {done.access === 'manage' ? 'Read and change' : 'Read only'}
                  </Badge>
                  {!azure && <Text size={FS.micro} c={INK.brand} ff="monospace">{done.identityArn}</Text>}
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
          Argus needs {two ? 'two roles' : 'a read-only role'} in {accountIdText('aws', digits)}
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
        {more && (
          <Text size={FS.meta} c="dimmed" lh={1.55} mt={4}>
            {two ? 'Both roles' : 'The role'} (<Text span ff="monospace" size={FS.meta}>{prepared.roleName}</Text>
            {two && <>, <Text span ff="monospace" size={FS.meta}>{prepared.manageRoleName}</Text></>}) trust only{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>, require the external id{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, and require the name of the person using
            them, so CloudTrail shows who in Argus did what. They are made by one CloudFormation stack, <Text span ff="monospace" size={FS.meta}>argus-access</Text>,
            which CloudFormation does not charge for.
          </Text>
        )}
      </Box>
    </Group>
  )
}

/** What argus-read is and may do in a project, with the details one click away. */
function GcpSummary({ prepared }: { prepared: CloudPrepared }) {
  const [more, setMore] = useState(false)
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <ThemeIcon variant="light" color="azure" size={34} radius="md"><IconShieldLock size={18} /></ThemeIcon>
      <Box style={{ minWidth: 0 }}>
        <Text fw={600}>{prepared.manageRoleName
          ? `Argus needs two service accounts in ${prepared.accountId}`
          : `Argus needs a read-only service account in ${prepared.accountId}`}</Text>
        <Text size={FS.meta} c="dimmed" lh={1.55}>
          One command makes <Text span ff="monospace" size={FS.meta}>argus-read</Text>. It can list and describe Compute Engine, never read
          what is on a disk or change anything.{prepared.manageRoleName && <> It also makes{' '}
          <Text span ff="monospace" size={FS.meta}>argus-manage</Text>, which can change Compute Engine and nothing outside it: no IAM, so it
          cannot grant itself more.</>}{prepared.manageRoleName ? ' Neither has a key, and they cost nothing.' : ' It has no key, and costs nothing.'}{' '}
          <Text span size={FS.meta} c="azure" style={{ cursor: 'pointer' }} onClick={() => setMore((m) => !m)}>
            {more ? 'Less' : 'How it is locked down'}
          </Text>
        </Text>
        {more && (
          <Text size={FS.meta} c="dimmed" lh={1.55} mt={4} style={{ overflowWrap: 'anywhere' }}>
            Only <Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>, argus-control's own service account, may ask Google for
            a short-lived token for it (Service Account Token Creator, on Argus's own accounts alone). Its description carries the external id{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, made for this organization and project, and Argus refuses
            the account without it. Every call names the person it is for in its user agent, which Google's audit log keeps.
          </Text>
        )}
      </Box>
    </Group>
  )
}

/** What the Reader assignment is and lets Argus do in a subscription, with the details one click away. */
function AzureSummary({ prepared }: { prepared: CloudPrepared }) {
  const [more, setMore] = useState(false)
  const two = !!prepared.manageRoleName
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <ThemeIcon variant="light" color="azure" size={34} radius="md"><IconShieldLock size={18} /></ThemeIcon>
      <Box style={{ minWidth: 0 }}>
        <Text fw={600}>Argus needs {two ? 'two roles' : 'the Reader role'} in {prepared.accountId}</Text>
        <Text size={FS.meta} c="dimmed" lh={1.55}>
          One command gives argus-control Azure's own <Text span ff="monospace" size={FS.meta}>Reader</Text> role at the subscription. It can
          list and describe resources, never read what is on a disk, a secret or a key, or change anything.{' '}
          {two
            ? <>The same command makes a custom role, <Text span ff="monospace" size={FS.meta}>{prepared.manageRoleName}</Text>, that can start,
              stop, restart, resize and tag VMs, change disks and snapshots, and nothing else: no access control, so it cannot grant itself more. Neither costs anything.{' '}</>
            : 'Nothing is made in the subscription but that assignment, and it costs nothing. '}
          <Text span size={FS.meta} c="azure" style={{ cursor: 'pointer' }} onClick={() => setMore((m) => !m)}>
            {more ? 'Less' : 'How it is locked down'}
          </Text>
        </Text>
        {more && (
          <Text size={FS.meta} c="dimmed" lh={1.55} mt={4} style={{ overflowWrap: 'anywhere' }}>
            {two ? 'Both roles are' : 'The role is'} given to <Text span ff="monospace" size={FS.meta}>{prepared.principal}</Text>, argus-control's own application in
            Microsoft Entra, and to nothing else. {two ? "Each assignment's" : "The assignment's"} description carries the external id{' '}
            <Text span ff="monospace" size={FS.meta}>{prepared.externalId}</Text>, made for this organization and subscription, and Argus
            refuses to {two ? 'read or change' : 'read'} the subscription without it.{two && ' Azure gives Argus one identity, not one per role as AWS and Google Cloud do, so what keeps reading and changing apart is which assignments exist, and Argus itself.'} Azure's activity log names that application, not a person: Azure has no
            field for one, so who asked for what is in Argus's own audit log. Remove {two ? 'an assignment' : 'the assignment'} under Access control (IAM) and
            Argus {two ? 'loses what it gave' : 'is out'} at once.
          </Text>
        )}
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

const GCP_STEPS = [
  ['The project', 'Name and project id'],
  ['Give Argus access', 'Argus connects by itself'],
] as const

const AZURE_STEPS = [
  ['The subscription', 'Name and subscription id'],
  ['Give Argus access', 'Argus connects by itself'],
] as const

/** Where the person is in the two steps: done in teal, current in azure. */
function Steps({ step, provider }: { step: number; provider: CloudProvider }) {
  return (
    <SimpleGrid cols={2} spacing="sm" mb="xl" aria-label="Steps">
      {(provider === 'azure' ? AZURE_STEPS : provider === 'gcp' ? GCP_STEPS : STEPS).map(([t, d], i) => {
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
