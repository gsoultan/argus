import { Drawer, Text } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { ConnectPanel } from '~/components/connect/ConnectPanel'
import { useQuery } from '@tanstack/react-query'
import { useSessions } from '~/lib/sessions'
import { assetsQuery } from '~/lib/queries'
import { assetShortName } from '~/lib/assetType'

/**
 * The Connect drawer: opened by every Connect button, by the + in the session
 * tab bar, and by the command palette. From the right on a wide screen, as a
 * sheet from the bottom on a phone.
 */
export function ConnectDrawer() {
  const { drawerFor, closeDrawer } = useSessions()
  const phone = useMediaQuery('(max-width: 36em)') ?? false
  const host = drawerFor ?? ''
  const { data: assets } = useQuery({ ...assetsQuery({}), enabled: drawerFor !== null })
  const asset = assets?.find((a) => a.hostname === host)
  const name = asset ? assetShortName(asset) : host.split('.')[0] ?? host
  return (
    <Drawer
      opened={drawerFor !== null}
      onClose={closeDrawer}
      position={phone ? 'bottom' : 'right'}
      size={phone ? 'auto' : 520}
      radius={phone ? 'lg' : 0}
      title={<Text fw={650} size="lg">{host ? `Connect to ${name}` : 'Connect'}</Text>}
      // Above the session workspace, which covers the shell.
      zIndex={400}
      styles={{ body: { paddingBottom: 24 } }}
    >
      {drawerFor !== null && <ConnectPanel hostname={host || undefined} onDone={closeDrawer} inDrawer />}
    </Drawer>
  )
}
