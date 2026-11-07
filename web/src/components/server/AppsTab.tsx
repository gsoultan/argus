import { Stack } from '@mantine/core'
import { AndroidAppsOnDevice } from '~/components/server/AndroidAppsOnDevice'
import { ApplicationsOnServer } from '~/components/ApplicationsOnServer'
import { DatabasesOnServer } from '~/components/DatabasesOnServer'
import type { Asset } from '~/types/domain'

/**
 * A device shows the apps installed on it; a server shows what the agent
 * found running and the databases people open through Argus. Both are the
 * "what runs here" tab, in each kind's own words.
 */
export function AppsTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  if (asset.kind === 'android') {
    return <AndroidAppsOnDevice asset={asset} />
  }
  return (
    <Stack gap="md">
      <ApplicationsOnServer asset={asset} canEdit={canEdit} />
      <DatabasesOnServer asset={asset} canEdit={canEdit} />
    </Stack>
  )
}
