import { ActionIcon, Menu, Tooltip, useMantineColorScheme, type MantineColorScheme } from '@mantine/core'
import { IconCheck, IconDeviceDesktop, IconMoon, IconSun } from '@tabler/icons-react'

const OPTIONS: { value: MantineColorScheme; label: string; Icon: typeof IconSun }[] = [
  { value: 'light', label: 'Light', Icon: IconSun },
  { value: 'dark', label: 'Dark', Icon: IconMoon },
  { value: 'auto', label: 'Match system', Icon: IconDeviceDesktop },
]

/**
 * Light, dark, or whatever the operating system is set to -- the default.
 *
 * Three choices rather than a toggle, because a toggle can only express the two
 * schemes, and someone who set their laptop to switch at sunset should not lose
 * that the first time they look at the other one. The pick is kept in this
 * browser (see COLOR_SCHEME_KEY), not on the account: it is a property of the
 * screen, and the same person may want dark on a laptop and light on a
 * projector.
 *
 * The button shows the pick, not the scheme in effect, so "Match system" reads
 * as a choice someone made rather than as whichever of sun or moon it resolved
 * to today.
 */
export function ColorSchemeMenu() {
  const { colorScheme, setColorScheme } = useMantineColorScheme()
  const current = OPTIONS.find((o) => o.value === colorScheme) ?? OPTIONS[2]!
  return (
    <Menu position="bottom-end" width={190} withArrow>
      <Menu.Target>
        <Tooltip label={`Theme: ${current.label.toLowerCase()}`}>
          <ActionIcon variant="subtle" color="slate" size="lg" aria-label={`Theme: ${current.label}`}>
            <current.Icon size={18} />
          </ActionIcon>
        </Tooltip>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Label>Theme</Menu.Label>
        {OPTIONS.map(({ value, label, Icon }) => (
          <Menu.Item
            key={value}
            leftSection={<Icon size={15} />}
            rightSection={value === colorScheme ? <IconCheck size={14} aria-hidden /> : null}
            aria-current={value === colorScheme ? 'true' : undefined}
            onClick={() => setColorScheme(value)}
          >
            {label}
          </Menu.Item>
        ))}
      </Menu.Dropdown>
    </Menu>
  )
}
