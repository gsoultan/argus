import { Box, Card, Group, Skeleton, Stack, Table, Text, ThemeIcon } from '@mantine/core'
import { Link } from '@tanstack/react-router'
import { IconChevronRight, type IconProps } from '@tabler/icons-react'
import type { ComponentType, ReactNode } from 'react'
import { FS, SP } from '~/theme'

/**
 * The page chrome every route is built from.
 *
 * Before this existed each route composed its own header, filter row, card
 * header and empty state inline. They had drifted into six different table row
 * heights, four ways of placing a filter, and empty states that ranged from a
 * centred sentence to nothing at all — so moving between two list pages meant
 * relearning where things were. These are the shapes; a route chooses content,
 * not layout.
 */

export interface Crumb {
  label: string
  /**
   * Omitted on the trailing crumb, which is the page you are already on.
   *
   * Always a list route — a trail never points back into a parameterised one,
   * so this stays a plain path rather than a router params pair.
   */
  to?: string
}

/**
 * Sticky page header: trail, title, one sentence, actions.
 *
 * `crumbs` replaced the "Back" buttons that used to sit inside `actions` on the
 * two detail pages — where a plain navigation control was the same size and
 * weight as "Terminate", a button that kills someone's live session. Navigation
 * belongs above the title; `actions` is now only things that *do* something.
 *
 * The title is the only `h1` and carries no adornment, because `status` beside
 * it would otherwise land in the heading's accessible name and change what
 * `getByRole('heading', { name })` matches.
 */
export function PageHeader({
  crumbs,
  title,
  status,
  description,
  actions,
}: {
  crumbs?: Crumb[]
  title: string
  /** Rendered next to the title but outside the heading — a state badge, usually. */
  status?: ReactNode
  description?: string
  actions?: ReactNode
}) {
  return (
    <Box className="argus-pagehead" px="lg" py="sm">
      {crumbs && crumbs.length > 0 && (
        <Group gap={SP.tight} mb={SP.snug} wrap="nowrap" aria-label="Breadcrumb">
          {crumbs.map((c, i) => (
            <Group gap={SP.tight} key={`${c.label}-${i}`} wrap="nowrap">
              {i > 0 && <IconChevronRight size={11} opacity={0.4} />}
              {c.to ? (
                <Text
                  component={Link}
                  to={c.to}
                  size={FS.micro}
                  c="dimmed"
                  fw={600}
                  tt="uppercase"
                  style={{ letterSpacing: '0.07em', textDecoration: 'none' }}
                  className="hover:underline"
                >
                  {c.label}
                </Text>
              ) : (
                <Text
                  size={FS.micro}
                  c="dimmed"
                  fw={600}
                  tt="uppercase"
                  style={{ letterSpacing: '0.07em' }}
                >
                  {c.label}
                </Text>
              )}
            </Group>
          ))}
        </Group>
      )}

      <Group justify="space-between" align="flex-start" wrap="nowrap" gap="md">
        <Box style={{ minWidth: 0 }}>
          <Group gap="xs" wrap="nowrap">
            <Text component="h1" fw={600} size={FS.title} lh={1.25} style={{ margin: 0 }}>
              {title}
            </Text>
            {status}
          </Group>
          {description && (
            <Text size={FS.body} c="dimmed" mt={SP.tight} maw={720} lh={1.5}>
              {description}
            </Text>
          )}
        </Box>
        {actions && (
          <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
            {actions}
          </Group>
        )}
      </Group>
    </Box>
  )
}

/** The body of every page. One padding value, defined once. */
export function PageBody({ children }: { children: ReactNode }) {
  return (
    <Box p="lg">
      <Stack gap="md">{children}</Stack>
    </Box>
  )
}

/**
 * The filter row above a table.
 *
 * Filters lived in three places depending on the page — inside the header's
 * `actions`, loose above the card, or both at once on Sessions. They are page
 * state rather than page actions, so they all sit here now, directly above the
 * data they narrow.
 */
export function Toolbar({ children, right }: { children: ReactNode; right?: ReactNode }) {
  return (
    <Group justify="space-between" align="center" gap="sm" wrap="wrap">
      <Group gap="xs" align="center" wrap="wrap">
        {children}
      </Group>
      {right && (
        <Group gap="xs" align="center" wrap="nowrap">
          {right}
        </Group>
      )}
    </Group>
  )
}

/**
 * A titled panel.
 *
 * The header — icon, title, count badge, trailing link — was hand-assembled in
 * twelve places with `p="md" pb="sm"`, `p="md" pb="xs"` and `mb={4}` variants,
 * which is why no two cards lined up.
 */
export function SectionCard({
  title,
  description,
  icon: Icon,
  iconColor = 'azure',
  badge,
  action,
  children,
  /** Set for a card whose body is a table or list that must reach the edges. */
  flush = false,
  danger = false,
}: {
  title: string
  description?: ReactNode
  icon?: ComponentType<IconProps>
  iconColor?: string
  badge?: ReactNode
  action?: ReactNode
  children: ReactNode
  flush?: boolean
  danger?: boolean
}) {
  return (
    <Card
      padding={0}
      style={danger ? { borderColor: 'var(--color-denied)' } : undefined}
      h="100%"
    >
      <Group justify="space-between" align="flex-start" wrap="nowrap" p="md" pb={SP.cozy}>
        <Box style={{ minWidth: 0 }}>
          <Group gap={SP.snug} wrap="nowrap">
            {Icon && (
              <ThemeIcon variant="light" color={iconColor} size={22} radius="sm">
                <Icon size={13} />
              </ThemeIcon>
            )}
            <Text component="h2" fw={600} size="sm" style={{ margin: 0 }}>
              {title}
            </Text>
            {badge}
          </Group>
          {description && (
            <Text size={FS.micro} c="dimmed" mt={SP.tight} lh={1.5}>
              {description}
            </Text>
          )}
        </Box>
        {action && <Box style={{ flexShrink: 0 }}>{action}</Box>}
      </Group>
      <Box px={flush ? 0 : 'md'} pb={flush ? 0 : 'md'} style={{ minWidth: 0 }}>
        {children}
      </Box>
    </Card>
  )
}

/**
 * What a page says when it has nothing to show.
 *
 * Every one of these used to be a lone grey sentence — "Queue is clear.", "No
 * assets match." — which reads identically whether the filter is too narrow,
 * the fleet is genuinely empty, or the request failed. Saying which, and what
 * to do next, is the difference between an empty screen and an answer.
 */
export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  compact = false,
}: {
  icon?: ComponentType<IconProps>
  title: string
  description?: ReactNode
  action?: ReactNode
  compact?: boolean
}) {
  return (
    <Stack align="center" gap={SP.snug} py={compact ? 'lg' : 'xl'} px="md">
      {Icon && (
        <ThemeIcon variant="light" color="slate" size={compact ? 30 : 38} radius="xl">
          <Icon size={compact ? 15 : 19} stroke={1.6} />
        </ThemeIcon>
      )}
      <Text size="sm" fw={500} ta="center">
        {title}
      </Text>
      {description && (
        <Text size={FS.meta} c="dimmed" ta="center" maw={420} lh={1.5}>
          {description}
        </Text>
      )}
      {action && <Box mt={SP.tight}>{action}</Box>}
    </Stack>
  )
}

export interface Column {
  label: ReactNode
  width?: number
  /** A header cell with no label — the actions column — still needs a th. */
  hidden?: boolean
}

/**
 * Card + horizontal scroll + table + the three states a table can be in.
 *
 * `loading` matters more than it looks: every table here rendered an empty
 * `<tbody>` while its query was in flight, which is pixel-identical to "this
 * fleet has no hosts". On a console whose job is to tell you what exists, a
 * blank table that means "wait" is a wrong answer, not a missing one.
 */
export function DataTable({
  columns,
  minWidth,
  loading = false,
  isEmpty = false,
  empty,
  footer,
  striped = true,
  children,
}: {
  columns: (Column | string)[]
  minWidth: number
  loading?: boolean
  isEmpty?: boolean
  empty?: ReactNode
  footer?: ReactNode
  striped?: boolean
  children: ReactNode
}) {
  const cols = columns.map((c) => (typeof c === 'string' ? { label: c } : c))

  return (
    <Box>
      <Card padding={0}>
        <Table.ScrollContainer minWidth={minWidth} type="native">
          <Table striped={striped ? 'even' : undefined}>
            <Table.Thead>
              <Table.Tr>
                {cols.map((c, i) => (
                  <Table.Th key={i} w={c.width}>
                    {c.hidden ? null : c.label}
                  </Table.Th>
                ))}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {loading ? <TableSkeleton rows={6} cols={cols.length} /> : children}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        {!loading && isEmpty && empty}
      </Card>
      {footer && !loading && (
        <Text size={FS.micro} c="dimmed" mt={SP.snug}>
          {footer}
        </Text>
      )}
    </Box>
  )
}

/** Placeholder rows sized like the real ones, so the layout does not jump. */
export function TableSkeleton({ rows, cols }: { rows: number; cols: number }) {
  return (
    <>
      {Array.from({ length: rows }, (_, r) => (
        <Table.Tr key={r}>
          {Array.from({ length: cols }, (_, c) => (
            <Table.Td key={c}>
              <Skeleton height={9} radius="xl" w={c === 0 ? '70%' : `${45 + ((r + c) % 4) * 12}%`} />
            </Table.Td>
          ))}
        </Table.Tr>
      ))}
    </>
  )
}
