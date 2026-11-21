import { ActionIcon, Group, Select, Tooltip } from '@mantine/core'
import { IconChevronLeft as IconPrev, IconChevronRight as IconNext } from '@tabler/icons-react'

/**
 * Previous, next, and a search box over every item of one kind: how a page
 * for one asset, database or broker moves to the next without a list down
 * its side. `what` names the kind in the labels ("asset", "database").
 */
export function ItemSwitcher({ items, currentId, onGo, what, width = 220 }: {
  items: { value: string; label: string }[]
  currentId: string
  onGo: (id: string) => void
  what: string
  width?: number
}) {
  const i = items.findIndex((a) => a.value === currentId)
  if (items.length < 2) return null
  return (
    <Group gap={4} wrap="nowrap" data-testid={`${what}-switcher`}>
      <Tooltip label={`Previous ${what}`}>
        <ActionIcon variant="default" size={36} disabled={i <= 0} onClick={() => items[i - 1] && onGo(items[i - 1]!.value)} aria-label={`Previous ${what}`}>
          <IconPrev />
        </ActionIcon>
      </Tooltip>
      <Select data={items} value={i >= 0 ? currentId : null} onChange={(v) => v && onGo(v)} searchable w={{ base: 150, sm: width }}
        aria-label={`Go to another ${what}`} placeholder={`Find a ${what}`}
        nothingFoundMessage={`No ${what} by that name`} comboboxProps={{ withinPortal: true }} />
      <Tooltip label={`Next ${what}`}>
        <ActionIcon variant="default" size={36} disabled={i < 0 || i >= items.length - 1} onClick={() => items[i + 1] && onGo(items[i + 1]!.value)} aria-label={`Next ${what}`}>
          <IconNext />
        </ActionIcon>
      </Tooltip>
    </Group>
  )
}
