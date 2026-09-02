import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Card, Group, Radio, Select, Stack, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconInfoCircle, IconPlugConnected, IconTerminal2, IconX } from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { LiveTerminal, type TerminalState } from '~/components/LiveTerminal'
import { assetsQuery } from '~/lib/queries'
import { GATEWAY_URL, terminalTicket } from '~/lib/live'
import { Mono } from '~/components/primitives'

export const Route = createFileRoute('/connect')({ component: Connect })

/**
 * Browser terminal.
 *
 * The gateway address and token are entered here rather than baked in, because
 * the control plane that would issue them does not exist yet. Once it does, the
 * console holds a session cookie and this form collapses to picking a host.
 */
const DEFAULT_GATEWAY = GATEWAY_URL

function Connect() {
  const { data: assets } = useQuery(assetsQuery({}))

  const [gateway, setGateway] = useState(DEFAULT_GATEWAY)
  const [requesting, setRequesting] = useState(false)
  const [denied, setDenied] = useState<string>()
  const [target, setTarget] = useState<string | null>(null)
  const [principal, setPrincipal] = useState('ops')
  const [session, setSession] = useState<{ target: string; principal: string } | null>(null)
  const [state, setState] = useState<TerminalState>('connecting')
  const [detail, setDetail] = useState<string>()

  const selected = assets?.find((a) => a.hostname === target)
  const principals = selected?.principals ?? ['ops', 'deploy']

  if (session) {
    return (
      <Box h="calc(100vh - 52px)" style={{ display: 'flex', flexDirection: 'column' }}>
        <PageHeader
          title="Terminal"
          description="Brokered through the gateway — same policy, host-key verification and recording as ssh(1)."
          actions={
            <Button
              size="xs"
              variant="light"
              color="rose"
              leftSection={<IconX size={14} />}
              onClick={() => setSession(null)}
            >
              Disconnect
            </Button>
          }
        />
        <Box style={{ flex: 1, minHeight: 0 }}>
          <LiveTerminal
            gatewayUrl={gateway}
            getTicket={async () => {
              const res = await terminalTicket(session.target, session.principal)
              if ('error' in res) {
                setDenied(res.error)
                return null
              }
              return res.ticket
            }}
            target={session.target}
            principal={session.principal}
            onStateChange={(s, d) => { setState(s); setDetail(d) }}
          />
        </Box>
      </Box>
    )
  }

  return (
    <Box>
      <PageHeader
        title="Connect"
        description="Open a session from the browser. For anyone who cannot use a native client — contractors on locked-down machines, auditors, on-call from a borrowed laptop."
      />

      <Box p="lg">
        <Stack gap="sm" maw={620}>
          {state === 'error' && detail && (
            <Alert color="rose" variant="light" icon={<IconX size={16} />} title="Connection refused">
              <Text size="xs">{detail}</Text>
            </Alert>
          )}

          <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />}>
            <Text size="xs">
              A browser session is not a lesser-controlled one. It goes through the same
              gateway code path as <Mono>ssh</Mono>: the principal is checked against the
              inventory, the target's host key is verified against its pin, the credential is
              injected so you never see it, and the session is recorded to the same
              hash-chained artefact.
            </Text>
          </Alert>

          <Card padding="md">
            <Text fw={600} size="sm" mb="sm">Gateway</Text>
            <TextInput
              size="xs"
              label="Address"
              description="Where argus-gateway serves the browser terminal"
              value={gateway}
              onChange={(e) => setGateway(e.currentTarget.value)}
            />
            <Text size="10px" c="dimmed" mt={7} lh={1.45}>
              No credential is entered here. When you open a terminal the console asks the
              control plane for a single-use ticket scoped to that host and account; it
              expires in a minute and cannot be reused.
            </Text>
          </Card>

          <Card padding="md">
            <Text fw={600} size="sm" mb="sm">Target</Text>
            <Stack gap="sm">
              <Select
                size="xs"
                label="Host"
                placeholder="Choose a host"
                searchable
                value={target}
                onChange={setTarget}
                data={assets?.map((a) => ({ value: a.hostname, label: a.hostname })) ?? []}
              />

              <Radio.Group
                label="Connect as"
                value={principal}
                onChange={setPrincipal}
              >
                <Group gap="lg" mt={6}>
                  {principals.map((p) => (
                    <Radio
                      key={p}
                      value={p}
                      size="xs"
                      label={
                        p === 'root' ? (
                          <Group gap={5}>
                            <Text size="sm">root</Text>
                            <Badge size="xs" color="rose">elevated</Badge>
                          </Group>
                        ) : p
                      }
                    />
                  ))}
                </Group>
              </Radio.Group>
            </Stack>
          </Card>

          {denied && (
            <Alert color="rose" variant="light" icon={<IconX size={16} />} title="Not permitted">
              <Text size="xs">{denied}</Text>
            </Alert>
          )}

          <Group justify="flex-end">
            <Button
              leftSection={<IconPlugConnected size={15} />}
              disabled={!target || !gateway}
              loading={requesting}
              onClick={async () => {
                if (!target) return
                setDenied(undefined)
                setRequesting(true)
                const res = await terminalTicket(target, principal)
                setRequesting(false)
                if ('error' in res) {
                  // The control plane's refusal is the authoritative one, so
                  // show what it actually said.
                  setDenied(res.error)
                  return
                }
                setSession({ target, principal })
              }}
            >
              Open terminal
            </Button>
          </Group>

          <Card padding="md" style={{ borderStyle: 'dashed' }}>
            <Group gap={7} mb={6}>
              <IconTerminal2 size={15} className="text-slate-400" />
              <Text fw={600} size="sm">Prefer your own client?</Text>
            </Group>
            <Text size="10px" c="dimmed" lh={1.45}>
              Nothing here is required. The target is encoded in the username, so ordinary
              tooling works unchanged and gets the identical controls:
            </Text>
            <Text size="11px" ff="monospace" mt={7} c="slate.2">
              ssh {principal}:{target?.split('.')[0] ?? 'HOST'}@argus.northwind.id
            </Text>
          </Card>
        </Stack>
      </Box>
    </Box>
  )
}
