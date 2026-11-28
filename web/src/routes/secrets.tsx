import { Alert, Anchor, Box, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconAlertTriangle } from '@tabler/icons-react'
import { PageBody, PageHeader, RoutePending } from '~/components/page'
import { meQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { SectionSlot, type SectionError } from '~/mf/host'
import { FS } from '~/theme'

/*
 * Secrets: argus-secret's own screens, loaded at run time (src/mf). The shell
 * draws this page only when they cannot be: not connected, or the screens
 * are missing or built for another console.
 */
export const Route = createFileRoute('/secrets')({
  validateSearch: (s: Record<string, unknown>): { tab?: string } =>
    typeof s.tab === 'string' && s.tab ? { tab: s.tab } : {},
  component: SecretsPage,
})

function SecretsPage() {
  const { tab } = Route.useSearch()
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())

  if (!isConfigured()) {
    return (
      <Shell>
        <Alert color="slate"><Text size={FS.body}>Secrets are kept on your organization’s secrets servers. Connect this console to Argus to see them.</Text></Alert>
      </Shell>
    )
  }
  if (!me) return <RoutePending />
  return (
    <SectionSlot
      name="secrets"
      context={{
        person: { email: me.email, name: me.displayName || me.email, role: me.role },
        tab,
        setTab: (t) => void navigate({ to: '/secrets', search: t ? { tab: t } : {} }),
        navigate: (to) => void navigate({ to }),
      }}
      fallback={(err) => <Unavailable err={err} />}
    />
  )
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <Box>
      <PageHeader title="Secrets" description="Passwords, keys and tokens, kept on your organization’s secrets servers." />
      <PageBody>{children}</PageBody>
    </Box>
  )
}

function Unavailable({ err }: { err: SectionError }) {
  const navigate = useNavigate()
  const title = err.kind === 'version' ? 'These screens are for another version of Argus' : 'The secrets screens did not load'
  return (
    <Shell>
      <Alert color="rose" icon={<IconAlertTriangle size={16} />} title={title}>
        <Text size={FS.body}>
          {err.kind === 'version' ? `Update argus-control: ${err.message}.` : `${err.message}.`}{' '}
          Servers and apps still read their secrets; only this page is affected. The servers themselves are under{' '}
          <Anchor component="button" size={FS.body} onClick={() => void navigate({ to: '/settings', search: { section: 'secrets-servers' } })}>
            Settings › Secrets servers
          </Anchor>.
        </Text>
      </Alert>
    </Shell>
  )
}
