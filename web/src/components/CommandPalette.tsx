import { useEffect, useMemo, useRef, useState } from 'react'
import { Badge, Box, Group, Modal, ScrollArea, Text, TextInput, UnstyledButton } from '@mantine/core'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { IconCornerDownLeft, IconPlug, IconPlugConnected, IconSearch } from '@tabler/icons-react'
import type { IconProps } from '@tabler/icons-react'
import type { ComponentType } from 'react'
import { assetsQuery, databasesQuery, meQuery, sessionsQuery, usersQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { useSessions } from '~/lib/sessions'
import { FS, SP } from '~/theme'
import { pagesFor } from '~/components/nav'
import { isBroker } from '~/lib/engines'
import { assetName, assetShortName } from '~/lib/assetType'

/**
 * Jump to anything from anywhere.
 *
 * A console with nine sections, a few hundred hosts and thousands of sessions
 * had no way to reach a named thing except by choosing the right page and then
 * typing into that page's own filter — which means knowing, before you start,
 * which page a hostname lives on. This asks for the name instead.
 *
 * Results are grouped by kind and capped per group, so a common substring like
 * "db" cannot bury the page you wanted under forty session rows.
 */

interface Item {
  id: string
  label: string
  hint?: string
  kind: 'Open now' | 'Connect' | 'Page' | 'Host' | 'Database' | 'Broker' | 'Session' | 'User'
  icon?: ComponentType<IconProps>
  go: () => void
}

const PER_GROUP = 5

const WAY_WORD: Record<string, string> = {
  terminal: 'Terminal', files: 'Files', split: 'Terminal and files', desktop: 'Desktop', screen: 'Screen sharing', device: 'Phone',
}

export function CommandPalette({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const navigate = useNavigate()
  const { ask, sessions: open, focus } = useSessions()
  const [q, setQ] = useState('')
  const [cursor, setCursor] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)

  // Only fetched while the palette is open, so opening the console does not pay
  // for a search index nobody has asked for yet.
  const { data: assets } = useQuery({ ...assetsQuery({}), enabled: opened })
  const { data: sessions } = useQuery({ ...sessionsQuery(), enabled: opened })
  const { data: dbs } = useQuery({ ...databasesQuery(), enabled: opened })
  const { data: me } = useQuery(meQuery())
  const whole = seesWholeFleet(me?.role)
  // People are an administrator's page; nobody else is offered it in the menu.
  const { data: users } = useQuery({ ...usersQuery(), enabled: opened && whole })

  const items = useMemo<Item[]>(() => {
    const needle = q.trim().toLowerCase()
    const out: Item[] = []

    // The sessions already open come first, so Ctrl K is also how to get
    // back to one from anywhere in the console.
    for (const s of open) {
      const host = s.target.split('.')[0]
      if (needle && !`${s.principal}@${s.target} ${s.way}`.toLowerCase().includes(needle)) continue
      out.push({
        id: `open:${s.id}`,
        label: `${s.principal}@${host}`,
        hint: WAY_WORD[s.way],
        kind: 'Open now',
        icon: IconPlugConnected,
        go: () => {
          focus(s.id)
          void navigate({ to: '/connect', search: {} })
        },
      })
    }

    // Typing a server's name offers to connect to it straight away; its
    // administration page is still under Hosts below.
    if (needle) {
      // Ranked by the server's own name, so "win" finds win-01 before every
      // host under northwind.id.
      const rank = (a: { hostname: string; address: string; alias?: string }) => {
        const name = a.hostname.split('.')[0]!.toLowerCase()
        const alias = (a.alias ?? '').toLowerCase()
        if (name.startsWith(needle) || (alias && alias.startsWith(needle))) return 0
        if (name.includes(needle) || alias.includes(needle)) return 1
        return `${a.hostname} ${a.address}`.toLowerCase().includes(needle) ? 2 : -1
      }
      const reachable = (assets ?? []).slice(0, 400)
        .map((a) => ({ a, r: rank(a) }))
        .filter((x) => x.r >= 0)
        .sort((x, y) => x.r - y.r)
        .slice(0, PER_GROUP)
      for (const { a } of reachable) {
        out.push({
          id: `connect:${a.id}`,
          label: `Connect to ${assetShortName(a)}`,
          hint: a.hostname,
          kind: 'Connect',
          icon: IconPlug,
          go: () => ask(a.hostname),
        })
      }
    }

    for (const p of pagesFor(me?.role)) {
      if (needle && !p.label.toLowerCase().includes(needle)) continue
      out.push({
        id: `page:${p.to}`,
        label: p.label,
        hint: p.to,
        kind: 'Page',
        icon: p.icon,
        go: () => navigate({ to: p.to as never }),
      })
    }

    if (needle) {
      for (const a of (assets ?? []).slice(0, 400)) {
        if (!`${a.alias ?? ''} ${a.hostname} ${a.address} ${a.os}`.toLowerCase().includes(needle)) continue
        if (out.filter((i) => i.kind === 'Host').length >= PER_GROUP) break
        out.push({
          id: `asset:${a.id}`,
          label: assetName(a),
          hint: `${a.alias ? `${a.hostname} · ` : ''}${a.address}:${a.port} · ${a.os}`,
          kind: 'Host',
          // Someone who is only given servers wants a terminal, not the
          // administration page for the host.
          go: () =>
            whole
              ? navigate({ to: '/assets/$assetId', params: { assetId: a.id } })
              : ask(a.hostname),
        })
      }

      for (const d of dbs?.databases ?? []) {
        if (!`${d.dbname} ${d.hostname} ${d.label}`.toLowerCase().includes(needle)) continue
        const kind = isBroker(d.engine) ? 'Broker' : 'Database'
        if (out.filter((i) => i.kind === kind).length >= PER_GROUP) continue
        out.push({
          id: `db:${d.id}`,
          label: d.dbname,
          hint: `${d.hostname.split('.')[0]} · ${d.label} · ${dbs?.access[d.id]?.level || 'ask for access'}`,
          kind,
          go: () => isBroker(d.engine)
            ? navigate({ to: '/brokers/$brokerId', params: { brokerId: d.id } })
            : navigate({ to: '/databases/$dbId', params: { dbId: d.id } }),
        })
      }

      for (const s of (sessions ?? []).slice(0, 400)) {
        if (!`${s.userEmail} ${s.assetHostname} ${s.principal}`.toLowerCase().includes(needle)) {
          continue
        }
        if (out.filter((i) => i.kind === 'Session').length >= PER_GROUP) break
        out.push({
          id: `session:${s.id}`,
          label: `${s.principal}@${s.assetHostname.split('.')[0]}`,
          hint: `${s.userEmail} · ${s.state}`,
          kind: 'Session',
          go: () => navigate({ to: '/sessions/$sessionId', params: { sessionId: s.id } }),
        })
      }

      for (const u of users ?? []) {
        if (!`${u.displayName} ${u.email} ${u.role}`.toLowerCase().includes(needle)) continue
        if (out.filter((i) => i.kind === 'User').length >= PER_GROUP) break
        out.push({
          id: `user:${u.id}`,
          label: u.displayName,
          hint: `${u.email} · ${u.role}`,
          kind: 'User',
          go: () => navigate({ to: '/users' }),
        })
      }
    }

    return out
  }, [q, assets, dbs, sessions, users, navigate, me?.role, whole, open, focus, ask])

  // The highlighted row must never point past the end of a list that just got
  // shorter because another character was typed.
  useEffect(() => setCursor(0), [q])

  useEffect(() => {
    if (!opened) setQ('')
  }, [opened])

  const choose = (item: Item | undefined) => {
    if (!item) return
    item.go()
    onClose()
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((c) => Math.min(c + 1, items.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((c) => Math.max(c - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      choose(items[cursor])
    }
  }

  // Keeps the highlighted row inside the scroll viewport when the cursor is
  // driven by the keyboard rather than the pointer.
  useEffect(() => {
    listRef.current
      ?.querySelector('[data-active="true"]')
      ?.scrollIntoView({ block: 'nearest' })
  }, [cursor])

  let lastKind: string | null = null

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      withCloseButton={false}
      padding={0}
      size={560}
      yOffset="12vh"
      styles={{ body: { padding: 0 } }}
      aria-label="Search Argus"
    >
      <TextInput
        autoFocus
        size="md"
        variant="unstyled"
        placeholder="Connect to a server, or search pages, sessions and people…"
        leftSection={<IconSearch size={16} />}
        value={q}
        onChange={(e) => setQ(e.currentTarget.value)}
        onKeyDown={onKeyDown}
        // paddingLeft leaves room for leftSection; a plain paddingInline
        // overrode Mantine's own reservation and the magnifier landed on top
        // of the first two characters typed.
        styles={{ input: { paddingLeft: 42, paddingRight: 16, height: 46 } }}
        leftSectionWidth={42}
      />
      <Box style={{ borderTop: '1px solid var(--color-line)' }}>
        <ScrollArea.Autosize mah={360} viewportRef={listRef}>
          {items.length === 0 ? (
            <Text size={FS.meta} c="dimmed" ta="center" py="xl">
              {q.trim() ? `Nothing matches “${q.trim()}”.` : 'Type to search.'}
            </Text>
          ) : (
            items.map((item, i) => {
              const header = item.kind !== lastKind ? item.kind : null
              lastKind = item.kind
              const active = i === cursor
              const Icon = item.icon
              return (
                <Box key={item.id}>
                  {header && (
                    <Text
                      size={FS.micro}
                      c="dimmed"
                      fw={600}
                      tt="uppercase"
                      px="md"
                      pt={SP.cozy}
                      pb={SP.tight}
                      style={{ letterSpacing: '0.08em' }}
                    >
                      {header}
                    </Text>
                  )}
                  <UnstyledButton
                    w="100%"
                    px="md"
                    py={SP.cozy}
                    data-active={active}
                    onMouseMove={() => setCursor(i)}
                    onClick={() => choose(item)}
                    style={{
                      display: 'block',
                      background: active ? 'var(--color-raised)' : undefined,
                      borderLeft: active
                        ? '2px solid var(--color-brand-bright)'
                        : '2px solid transparent',
                    }}
                  >
                    <Group gap={SP.cozy} wrap="nowrap">
                      {Icon && <Icon size={15} stroke={1.7} opacity={0.7} />}
                      <Box style={{ minWidth: 0, flex: 1 }}>
                        <Text size={FS.meta} truncate>
                          {item.label}
                        </Text>
                        {item.hint && (
                          <Text size={FS.micro} c="dimmed" truncate>
                            {item.hint}
                          </Text>
                        )}
                      </Box>
                      {active && <IconCornerDownLeft size={13} opacity={0.5} />}
                    </Group>
                  </UnstyledButton>
                </Box>
              )
            })
          )}
        </ScrollArea.Autosize>
      </Box>
      <Group
        gap="md"
        px="md"
        py={SP.snug}
        style={{ borderTop: '1px solid var(--color-line)' }}
      >
        <Hint keys="↑↓" label="navigate" />
        <Hint keys="↵" label="open" />
        <Hint keys="esc" label="close" />
      </Group>
    </Modal>
  )
}

function Hint({ keys, label }: { keys: string; label: string }) {
  return (
    <Group gap={SP.tight} wrap="nowrap">
      <Badge size="xs" variant="default" color="slate" className="argus-digest">
        {keys}
      </Badge>
      <Text size={FS.micro} c="dimmed">
        {label}
      </Text>
    </Group>
  )
}
