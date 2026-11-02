import { Box, Text, Title } from '@mantine/core'
import { IconPlugConnected, IconPlayerPlay, IconShieldCheck } from '@tabler/icons-react'

/**
 * The page around signing in: Argus on the left, the form on the right.
 *
 * The left half is the first thing anyone sees of Argus, so it says what it
 * is and does in three lines, with the eye watching. On a phone it shrinks to
 * the mark and the line under it, and the form follows.
 *
 * All motion is CSS, so it costs no script, and the global reduced-motion rule
 * in app.tokens.css stills it for anyone who asked for that.
 */
export function AuthLayout({ children }: { children: React.ReactNode }) {
  return (
    <Box className="argus-auth">
      <Box component="aside" className="argus-auth-brand">
        <Box className="argus-auth-brand-inner">
          <WatchingEye />
          <Title order={1} className="argus-auth-title argus-rise" style={rise(1)}>
            Argus
          </Title>
          <Text className="argus-auth-lead argus-rise" style={rise(2)}>
            Every privileged session, opened in one click and recorded from the first
            keystroke.
          </Text>
          <Box component="ul" className="argus-auth-points">
            {POINTS.map((p, i) => (
              <Box component="li" key={p.title} className="argus-rise" style={rise(3 + i)}>
                <Box className="argus-auth-point-icon"><p.icon size={18} /></Box>
                <Box>
                  <Text fw={600} size="sm" c="white">{p.title}</Text>
                  <Text size="sm" className="argus-auth-muted">{p.text}</Text>
                </Box>
              </Box>
            ))}
          </Box>
          <Text size="xs" className="argus-auth-muted argus-auth-foot argus-rise" style={rise(6)}>
            Every action is tied to a person. There is no anonymous access, not even
            read-only.
          </Text>
        </Box>
      </Box>

      <Box component="main" className="argus-auth-form">
        <Box w="100%" maw={380}>{children}</Box>
      </Box>
    </Box>
  )
}

const POINTS = [
  {
    icon: IconPlugConnected,
    title: 'One click to any server',
    text: 'Connect through the Argus agent. No VPN, no keys to hand out.',
  },
  {
    icon: IconPlayerPlay,
    title: 'Every session recorded',
    text: 'Terminals and desktops replay exactly as they happened.',
  },
  {
    icon: IconShieldCheck,
    title: 'Approval when it matters',
    text: 'Root and direct logins wait for someone to say yes.',
  },
]

const rise = (step: number) => ({ '--rise-delay': `${step * 110}ms` }) as React.CSSProperties

/**
 * The mark, large and alive: it looks around, blinks now and then, and sends
 * out a slow ring, like something keeping watch.
 */
function WatchingEye() {
  return (
    <Box className="argus-eye argus-rise" style={rise(0)} aria-hidden>
      <span className="argus-eye-ring" />
      <span className="argus-eye-ring" style={{ animationDelay: '1.2s' }} />
      <span className="argus-eye-ring" style={{ animationDelay: '2.4s' }} />
      <Box className="argus-eye-tile">
        <svg width="56" height="56" viewBox="0 0 24 24" fill="none">
          <g className="argus-eye-lid">
            <path
              d="M2 12c2.6-4.6 6-7 10-7s7.4 2.4 10 7c-2.6 4.6-6 7-10 7s-7.4-2.4-10-7Z"
              stroke="#fff"
              strokeWidth={1.8}
              strokeLinejoin="round"
            />
            <circle className="argus-eye-pupil" cx="12" cy="12" r="3.4" fill="#fff" />
          </g>
        </svg>
      </Box>
    </Box>
  )
}
