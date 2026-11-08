import { Alert, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconRefresh } from '@tabler/icons-react'
import { controlUpdateBanner } from '~/lib/live'
import { FS } from '~/theme'

/* ── Everyone's banner ──────────────────────────────────────────────────── */

/**
 * Whether argus-control is updating, for the banner every signed-in person
 * sees. Polled slowly, quickly while one runs; while the control plane is
 * restarting the request fails and the last answer is kept, which is what
 * keeps the banner up through the restart.
 */
export function useControlUpdating(enabled: boolean): { target: string } | null {
  const { data } = useQuery({
    queryKey: ['control-update-banner'],
    queryFn: controlUpdateBanner,
    enabled,
    retry: false,
    refetchInterval: (q) => (q.state.data?.active ? 3_000 : 60_000),
  })
  return data?.active ? { target: data.target ?? '' } : null
}

export function ControlUpdatingBanner({ target, unreachable }: { target: string; unreachable: boolean }) {
  return (
    <Alert color="azure" radius={0} icon={<IconRefresh size={17} />} data-testid="control-updating-banner"
      title={unreachable ? `Argus is restarting on ${target}` : `Argus is updating to ${target}`}>
      <Text size={FS.body} lh={1.5}>
        Open terminals pause for a few seconds and pick up where they were.
        {unreachable && ' Until it is back, this page shows sample data, not your fleet.'}
      </Text>
    </Alert>
  )
}
