import { Tabs } from '@mantine/core'
import { useNavigate } from '@tanstack/react-router'
import { IconFileDescription, IconTerminal2 } from '@tabler/icons-react'

/**
 * Activity is two lists: who connected where (sessions, with their
 * recordings), and everything anyone changed (the audit log). One menu item,
 * two tabs, so neither needs a name of its own in the menu.
 */
export function ActivityTabs({ value }: { value: 'sessions' | 'audit' }) {
  const navigate = useNavigate()
  return (
    <Tabs value={value} onChange={(v) => void navigate({ to: v === 'audit' ? '/audit' : '/sessions' })}>
      <Tabs.List>
        <Tabs.Tab value="sessions" leftSection={<IconTerminal2 size={16} />}>Sessions</Tabs.Tab>
        <Tabs.Tab value="audit" leftSection={<IconFileDescription size={16} />}>Audit log</Tabs.Tab>
      </Tabs.List>
    </Tabs>
  )
}
