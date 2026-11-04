import { ActionIcon, Box, Tooltip } from '@mantine/core'
import { IconLayoutSidebarLeftCollapse, IconLayoutSidebarLeftExpand } from '@tabler/icons-react'
import { useState, type ReactNode } from 'react'
import './split.css'

/**
 * A list beside the thing you opened from it.
 *
 * On a wide screen (88em and up) a server's or a database's page keeps the
 * list it came from down its left edge, so moving to the next one is one
 * click rather than Back and find it again. Below that the list is not drawn
 * at all and the page is what it always was.
 *
 * The list can be put away -- a query console or a terminal wants every
 * column -- and that choice is remembered per list in this browser. Storage
 * is a convenience: when it is unavailable the list simply starts open.
 */
export function SplitView({
  list,
  children,
  storageKey,
  label,
}: {
  list: ReactNode
  children: ReactNode
  /** Remembers whether this list was put away. */
  storageKey: string
  /** What the list is, for the toggle: "servers", "databases". */
  label: string
}) {
  const key = `argus.split.${storageKey}`
  const [hidden, setHidden] = useState<boolean>(() => {
    try {
      return window.localStorage.getItem(key) === 'hidden'
    } catch {
      return false
    }
  })
  const toggle = () => {
    const next = !hidden
    setHidden(next)
    try {
      window.localStorage.setItem(key, next ? 'hidden' : 'shown')
    } catch {
      // Not remembered, and that is all.
    }
  }

  return (
    <Box className="argus-split" data-hidden={hidden || undefined}>
      <Box component="aside" className="argus-split-list" aria-label={`All ${label}`}>
        {list}
      </Box>
      <Box className="argus-split-detail">
        <Tooltip label={hidden ? `Show the list of ${label}` : `Hide the list of ${label}`} position="right">
          <ActionIcon
            className="argus-split-toggle"
            variant="default"
            size="md"
            radius="xl"
            onClick={toggle}
            aria-label={hidden ? `Show the list of ${label}` : `Hide the list of ${label}`}
            aria-expanded={!hidden}
          >
            {hidden ? <IconLayoutSidebarLeftExpand size={15} /> : <IconLayoutSidebarLeftCollapse size={15} />}
          </ActionIcon>
        </Tooltip>
        {children}
      </Box>
    </Box>
  )
}
