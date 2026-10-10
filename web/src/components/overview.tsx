import { Box, Button, Card, Group, Select, Skeleton, Stack, Text, ThemeIcon } from '@mantine/core'
import { useNavigate } from '@tanstack/react-router'
import {
  IconCircleCheck, IconCirclePlus, IconDeviceDesktop, IconPlugConnected, IconTerminal2,
  type IconProps,
} from '@tabler/icons-react'
import { useState, type ComponentType } from 'react'
import { ButtonLink } from '~/components/links'
import type { Asset } from '~/types/domain'
import { FS, SP } from '~/theme'

/** "1 server has" / "3 servers have": the count and the agreement in one place. */
export function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}

/**
 * The first thing on the Overview: the thing most people came to do.
 *
 * Most visits to this console are someone who needs a shell or a desktop on a
 * server. They used to land on three paragraphs of warnings about coverage and
 * host keys and had to find Connect in the menu. Now the server picker is the
 * first control on the page, and the warnings are a short list underneath.
 */
export function ConnectHero({ assets }: { assets: Asset[] | undefined }) {
  const navigate = useNavigate()
  const [host, setHost] = useState<string | null>(null)

  const go = () => {
    if (!host) return
    void navigate({ to: '/connect', search: { host } })
  }

  return (
    <Card padding="lg" className="argus-hero">
      <Group justify="space-between" align="flex-end" gap="lg" wrap="wrap">
        <Box style={{ flex: '1 1 420px', minWidth: 0 }}>
          <Text component="h2" fw={600} size="lg" style={{ margin: 0 }}>
            Connect to a server
          </Text>
          <Text size="sm" c="dimmed" mt={SP.tight} mb="sm">
            SSH or Remote Desktop opens right here in the browser. Argus signs you in
            and records the session, so there is no password or key to handle.
          </Text>
          <Group gap="xs" wrap="nowrap" align="flex-start">
            <Select
              size="md"
              radius="md"
              aria-label="Server"
              placeholder={assets ? 'Type a server name' : 'Loading servers…'}
              searchable
              clearable
              nothingFoundMessage="No server by that name"
              data={(assets ?? []).map((a) => ({
                value: a.hostname,
                label: a.hostname,
              }))}
              renderOption={({ option }) => {
                const a = assets?.find((x) => x.hostname === option.value)
                const Icon = a?.protocol === 'rdp' ? IconDeviceDesktop : IconTerminal2
                return (
                  <Group gap={SP.cozy} wrap="nowrap">
                    <Icon size={15} opacity={0.6} />
                    <Text size="sm" ff="monospace">{option.label}</Text>
                    <Text size={FS.meta} c="dimmed">{a?.protocol === 'rdp' ? 'Remote Desktop' : 'SSH'}</Text>
                  </Group>
                )
              }}
              value={host}
              onChange={setHost}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && host) go()
              }}
              style={{ flex: 1, maxWidth: 520 }}
              leftSection={<IconPlugConnected size={17} opacity={0.6} />}
            />
            <Button size="md" radius="md" disabled={!host} onClick={go}>
              Connect
            </Button>
          </Group>
        </Box>
        <Stack gap={SP.tight} align="flex-start">
          <Text size={FS.meta} c="dimmed">
            Server not in the list?
          </Text>
          <ButtonLink
            size="sm"
            variant="default"
            to="/assets/add"
            leftSection={<IconCirclePlus size={16} />}
          >
            Add a server
          </ButtonLink>
        </Stack>
      </Group>
    </Card>
  )
}

export interface AttentionItem {
  key: string
  tone: 'rose' | 'amber'
  icon: ComponentType<IconProps>
  title: string
  detail: string
  action: { label: string; to: string; search?: Record<string, string> }
}

/**
 * Everything waiting on a person, one line each, worst first.
 *
 * These were full-width red and amber banners, three or four paragraphs tall,
 * stacked above everything else. Each said something true, but together they
 * pushed the page's actual contents below the fold and made an ordinary day
 * look like an incident. A list keeps every finding and its way out, and a
 * page with nothing on it now says so plainly.
 */
export function Attention({
  items,
  loading,
}: {
  items: (AttentionItem | false | 0 | undefined)[]
  loading: boolean
}) {
  const shown = items.filter((i): i is AttentionItem => Boolean(i))
  return (
    <Card padding={0}>
      <Group justify="space-between" px="lg" pt="md" pb="sm">
        <Text component="h2" fw={600} size="md" style={{ margin: 0 }}>
          Needs your attention
        </Text>
        {!loading && shown.length > 0 && (
          <Text size={FS.meta} c="dimmed">
            {plural(shown.length, 'item', 'items')}
          </Text>
        )}
      </Group>
      {loading ? (
        <Box px="lg" pb="md">
          <Skeleton height={44} radius="sm" />
        </Box>
      ) : shown.length === 0 ? (
        <Group gap="sm" px="lg" pb="md" wrap="nowrap">
          <ThemeIcon color="teal" variant="light" radius="xl" size={30}>
            <IconCircleCheck size={17} />
          </ThemeIcon>
          <Text size="sm" c="dimmed">
            Nothing needs you right now. Every server is watched and nobody is waiting on a decision.
          </Text>
        </Group>
      ) : (
        <Stack gap={0}>
          {shown.map((i) => (
            <Group
              key={i.key}
              justify="space-between"
              wrap="wrap"
              gap="sm"
              px={{ base: 'md', sm: 'lg' }}
              py="sm"
              className="argus-attention"
              data-tone={i.tone}
            >
              <Group gap="sm" wrap="nowrap" align="flex-start" style={{ minWidth: 0, flex: '1 1 320px' }}>
                <ThemeIcon color={i.tone} variant="light" radius="md" size={32} mt={2}>
                  <i.icon size={17} />
                </ThemeIcon>
                <Box style={{ minWidth: 0 }}>
                  <Text size="sm" fw={500}>
                    {i.title}
                  </Text>
                  <Text size={FS.body} c="dimmed" lh={1.5} maw={680}>
                    {i.detail}
                  </Text>
                </Box>
              </Group>
              <ButtonLink
                size="xs"
                variant="light"
                color={i.tone}
                to={i.action.to as never}
                search={i.action.search as never}
                style={{ flexShrink: 0 }}
                ml={{ base: 44, sm: 0 }}
              >
                {i.action.label}
              </ButtonLink>
            </Group>
          ))}
        </Stack>
      )}
    </Card>
  )
}
