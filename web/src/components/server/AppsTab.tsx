import { Stack } from '@mantine/core'
import { ApplicationsOnServer } from '~/components/ApplicationsOnServer'
import { DatabasesOnServer } from '~/components/DatabasesOnServer'
import type { Asset } from '~/types/domain'

/**
 * What the agent found running here, and the databases on this server that
 * people open through Argus. A Postgres found running says when it is
 * already one of them.
 */
export function AppsTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  return (
    <Stack gap="md">
      <ApplicationsOnServer asset={asset} canEdit={canEdit} />
      <DatabasesOnServer asset={asset} canEdit={canEdit} />
    </Stack>
  )
}
