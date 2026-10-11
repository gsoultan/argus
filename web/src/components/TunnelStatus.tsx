import { Badge, Tooltip } from '@mantine/core'
import type { AgentTunnel } from '~/lib/live'

/**
 * What the console says about an agent's tunnel.
 *
 * Only what the last ping established. A tunnel no replica has pinged
 * recently arrives from the control plane as not connected, so nothing here
 * has to guess at staleness.
 */
export function describeTunnel(t: AgentTunnel | undefined): {
  label: string
  color: string
  detail: string
} {
  if (!t || !t.connected) {
    return {
      label: 'Not connected',
      color: 'slate',
      detail:
        'The agent has no connection to the control plane. Sessions through Argus cannot reach this server until it reconnects.',
    }
  }
  const rtt = t.rttMillis == null ? '' : ` · ${formatRtt(t.rttMillis)}`
  if (t.transport === 'quic') {
    return {
      label: `Connected${rtt}`,
      color: 'teal',
      detail: 'Over QUIC, the fast path.',
    }
  }
  return {
    label: `Connected over HTTPS${rtt}`,
    color: 'amber',
    detail:
      'UDP to the control plane looks blocked, so the agent is using the HTTPS fallback. It works, but sessions are less smooth on a lossy network.',
  }
}

function formatRtt(ms: number): string {
  return ms < 10 ? `${ms.toFixed(1)} ms` : `${Math.round(ms)} ms`
}

export function TunnelBadge({ tunnel }: { tunnel: AgentTunnel | undefined }) {
  const d = describeTunnel(tunnel)
  return (
    <Tooltip label={d.detail} multiline w={280}>
      <Badge color={d.color} variant="light" size="sm">{d.label}</Badge>
    </Tooltip>
  )
}
