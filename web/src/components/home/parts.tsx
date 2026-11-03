import type { ReactNode } from 'react'
import { Text } from '@mantine/core'
import type { ControlStatus } from '~/lib/live'
import { FS, SP } from '~/theme'

/** Home's pieces that more than one of its chunks draws. */

export function isStatus(v: unknown): v is ControlStatus {
  const s = v as Partial<ControlStatus> | undefined
  return Boolean(s && Array.isArray(s.problems) && s.machine && s.database && s.recordings && s.errors)
}

export function Label({ children }: { children: ReactNode }) {
  return (
    <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" mb={SP.cozy} style={{ letterSpacing: '0.05em' }}>
      {children}
    </Text>
  )
}
