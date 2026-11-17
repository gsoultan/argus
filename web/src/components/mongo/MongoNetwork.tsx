import { Box, Modal } from '@mantine/core'
import { StoreNetworkPanel } from '~/components/network/StoreNetwork'
import type { StoreSession } from '~/components/useStoreSession'
import type { Database } from '~/types/domain'

/* Which machines can connect to a MongoDB, over its console. */

export function MongoNetwork({ s, database, opened, onClose, narrow }: {
  s: StoreSession
  database: Database
  opened: boolean
  onClose: () => void
  narrow: boolean
}) {
  return (
    <Modal opened={opened} onClose={onClose} size="xl" fullScreen={narrow} withCloseButton={false} padding={0}>
      <Box style={{ display: 'flex', flexDirection: 'column', height: narrow ? '100dvh' : '80dvh' }}>
        <StoreNetworkPanel s={s} database={database} engine="mongodb" narrow={narrow} onBack={onClose} />
      </Box>
    </Modal>
  )
}
