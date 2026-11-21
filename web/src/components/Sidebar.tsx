import { Badge, Box, Divider, Indicator, NavLink as MantineNavLink, Stack, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { Link, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { meQuery } from '~/lib/queries'
import { IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand } from '@tabler/icons-react'
import { bottomFor, groupsFor, isItemActive, type NavCounts, type SideItem } from '~/components/nav'
import type { UserRole } from '~/types/domain'

/** The sidebar's two widths: open with labels, and folded to icons. */
export const SIDEBAR_OPEN = 232
export const SIDEBAR_FOLDED = 64

/**
 * The console's menu down the left: six groups, Settings and About at the
 * foot, and a button that folds it to a rail of icons. Folded, every entry is
 * its icon with its name on hover and focus, and a count is a dot on the icon,
 * so nothing is lost but the width.
 *
 * On a phone it is the panel the burger opens, always with its labels.
 */
export function Sidebar({ role, counts, folded, onFold, phone }: {
  role: UserRole | undefined
  counts: NavCounts
  folded: boolean
  onFold: (folded: boolean) => void
  /** Drawn as the phone's panel: labels always, no fold button. */
  phone?: boolean
}) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const tab = useRouterState({ select: (s) => (s.location.search as { tab?: unknown }).tab })
  const tabStr = typeof tab === 'string' ? tab : undefined
  const icons = folded && !phone
  const { data: me } = useQuery({ ...meQuery(), enabled: role !== undefined })
  const groups = groupsFor(role, me?.seesChecks === true)

  return (
    <Stack gap={0} h="100%" data-testid="sidebar" data-folded={icons || undefined}>
      <Box component="nav" aria-label="Main" style={{ flex: 1, overflowY: 'auto', overflowX: 'hidden' }} px={icons ? 8 : 10} pt={8}>
        {groups.map((g, gi) => (
          <Box key={g.label || 'top'} mb={6}>
            {g.label && (icons ? (
              gi > 0 && <Divider my={8} mx={6} aria-hidden />
            ) : (
              <Text size="10.5px" fw={700} c="dimmed" tt="uppercase" px={12} mt={10} mb={4} style={{ letterSpacing: '.07em' }}>
                {g.label}
              </Text>
            ))}
            {g.items.map((item) => (
              <Entry key={item.label + item.to} item={item} icons={icons} counts={counts}
                active={isItemActive(pathname, tabStr, item, g.items)} />
            ))}
          </Box>
        ))}
      </Box>
      <Box px={icons ? 8 : 10} py={8} style={{ borderTop: '1px solid var(--color-line)' }}>
        {bottomFor(role).map((item) => (
          <Entry key={item.to} item={item} icons={icons} counts={counts}
            active={isItemActive(pathname, tabStr, item, [])} />
        ))}
        {!phone && (
          <Tooltip label={folded ? 'Open the menu' : 'Fold the menu to icons'} position="right" withArrow disabled={!folded}>
            <UnstyledButton
              onClick={() => onFold(!folded)}
              aria-label={folded ? 'Open the menu' : 'Fold the menu to icons'}
              aria-pressed={folded}
              data-testid="sidebar-fold"
              className="argus-navlink argus-sidefold"
              w="100%"
              mt={4}
              style={{ display: 'flex', alignItems: 'center', gap: 12, padding: icons ? '8px 0' : '8px 12px', justifyContent: icons ? 'center' : 'flex-start', borderRadius: 8, color: 'var(--mantine-color-dimmed)' }}
            >
              {folded ? <IconLayoutSidebarLeftExpand size={18} stroke={1.6} /> : <IconLayoutSidebarLeftCollapse size={18} stroke={1.6} />}
              {!icons && <Text size="sm">Fold menu</Text>}
            </UnstyledButton>
          </Tooltip>
        )}
      </Box>
    </Stack>
  )
}

function Entry({ item, icons, counts, active }: { item: SideItem; icons: boolean; counts: NavCounts; active: boolean }) {
  const count = item.badge ? item.badge(counts) : 0
  const Icon = item.icon
  if (icons) {
    return (
      <Tooltip label={count > 0 ? `${item.label} (${count})` : item.label} position="right" withArrow openDelay={150}>
        <MantineNavLink
          component={Link}
          to={item.to as never}
          search={(item.search ?? {}) as never}
          aria-label={count > 0 ? `${item.label}, ${count} waiting` : item.label}
          active={active}
          variant="light"
          color="azure"
          className="argus-navlink argus-navlink-icon"
          data-active={active || undefined}
          py={8}
          styles={{ section: { marginInlineEnd: 0 }, body: { display: 'none' }, root: { justifyContent: 'center' } }}
          leftSection={
            <Indicator disabled={count === 0} color="rose" size={8} offset={2} processing={false}>
              <Icon size={20} stroke={1.6} />
            </Indicator>
          }
        />
      </Tooltip>
    )
  }
  return (
    <MantineNavLink
      component={Link}
      to={item.to as never}
      search={(item.search ?? {}) as never}
      label={item.label}
      active={active}
      variant="light"
      color="azure"
      leftSection={<Icon size={18} stroke={1.6} />}
      rightSection={count > 0 ? <Badge size="sm" circle color="rose" variant="filled">{count}</Badge> : null}
      className="argus-navlink"
      py={6}
    />
  )
}
