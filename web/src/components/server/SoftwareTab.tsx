import { useQuery } from '@tanstack/react-query'
import { IconPackage } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { InstalledSoftware } from '~/components/server/InstalledSoftware'
import { applicationsQuery } from '~/lib/queries'
import type { Asset } from '~/types/domain'

/**
 * Installing and removing software on a server, in a tab of its own: it was
 * a segment of Apps, where people looking to install something did not find
 * it. Apps keeps what runs on the server; this is what is installed on it.
 */
export function SoftwareTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data } = useQuery(applicationsQuery(asset.id))
  return (
    <SectionCard title="Installed software" icon={IconPackage}
      description="What is installed on this server, installing more from the sources it already uses, and removing what is there. Nothing changes without a preview the server simulated first.">
      <InstalledSoftware asset={asset} canEdit={canEdit} apps={data?.applications ?? []} active />
    </SectionCard>
  )
}
