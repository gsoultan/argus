import { Box, Group, ScrollArea, SegmentedControl, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { IconBrandWindows, IconDeviceMobile, IconSearch, IconTerminal2 } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { ReachBadge, isUp, kindOf, reachOf } from '~/components/myservers'
import { assetHostnameAside, assetShortName } from '~/lib/assetType'
import { addressesOf } from '~/lib/assetAddresses'
import { assetsQuery } from '~/lib/queries'
import { FS } from '~/theme'

type Show = 'all' | 'online' | 'offline'

/** What to call a server that has not said what it runs. */
export const KIND_NAME = { linux: 'Linux', windows: 'Windows', android: 'Android' } as const

/**
 * Every server, as a column of rows, for the left of a server's page. The
 * same list as the Servers page, narrowed by the same words, and the server
 * you are on is the lit row.
 */
export function ServerList({ currentId }: { currentId: string }) {
  const { data: assets } = useQuery(assetsQuery({}))
  const [q, setQ] = useState('')
  const [show, setShow] = useState<Show>('all')

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (assets ?? [])
      .filter((a) => !needle || a.hostname.toLowerCase().includes(needle) ||
        (a.alias ?? '').toLowerCase().includes(needle) || a.tags.some((t) => t.toLowerCase().includes(needle)) ||
        addressesOf(a).some((x) => x.address.toLowerCase().includes(needle)))
      .filter((a) => {
        if (show === 'all') return true
        return show === 'online' ? isUp(reachOf(a)) : !isUp(reachOf(a))
      })
      .sort((a, b) => assetShortName(a).localeCompare(assetShortName(b)))
  }, [assets, q, show])

  const online = (assets ?? []).filter((a) => isUp(reachOf(a))).length
  const total = assets?.length ?? 0

  return (
    <>
      <Box p="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
        <TextInput
          size="sm"
          placeholder="Find by name or IP"
          aria-label="Find a server in the list"
          leftSection={<IconSearch size={14} />}
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
        />
        <SegmentedControl
          mt="xs"
          size="xs"
          fullWidth
          value={show}
          onChange={(v) => setShow(v as Show)}
          data={[
            { value: 'all', label: `All ${total}` },
            { value: 'online', label: `Online ${online}` },
            { value: 'offline', label: `Offline ${total - online}` },
          ]}
        />
      </Box>
      <ScrollArea style={{ flex: 1 }} type="auto">
        {rows.map((a) => (
          <Link
            key={a.id}
            to="/assets/$assetId"
            params={{ assetId: a.id }}
            className="argus-splitrow"
            data-active={a.id === currentId || undefined}
            aria-current={a.id === currentId ? 'page' : undefined}
          >
            <Box
              w={32}
              h={32}
              className="grid place-items-center shrink-0"
              style={{ borderRadius: 8, background: 'var(--color-raised)', color: 'var(--color-ink-soft)' }}
            >
              {kindOf(a) === 'android' ? <IconDeviceMobile size={16} />
                : kindOf(a) === 'windows' ? <IconBrandWindows size={16} /> : <IconTerminal2 size={16} />}
            </Box>
            <Box style={{ minWidth: 0, flex: 1 }}>
              <Text size="sm" fw={600} truncate title={assetHostnameAside(a)}>{assetShortName(a)}</Text>
              <Text size={FS.micro} c="dimmed" truncate>
                {a.os || KIND_NAME[kindOf(a)]}
                {addressesOf(a)[0] && <> · <Text span inherit ff="monospace">{addressesOf(a)[0]!.address}</Text></>}
              </Text>
            </Box>
            <ReachBadge reach={reachOf(a)} />
          </Link>
        ))}
        {rows.length === 0 && (
          <Group p="md"><Text size={FS.meta} c="dimmed">No server matches.</Text></Group>
        )}
      </ScrollArea>
    </>
  )
}
