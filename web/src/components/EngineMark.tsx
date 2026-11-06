import type { DBEngine } from '~/types/domain'
import { ENGINES } from '~/lib/engines'

/*
The mark for a database's engine: two letters in a neutral tile, and a shape
per engine (rounded square for Postgres, a leaf for MySQL and MongoDB, circle for
Redis, square for RabbitMQ). It is neutral on purpose. teal, amber, rose and sky mean verified,
pending, denied and live in this console, and azure is the brand, so an
engine told apart by colour would borrow a meaning it does not have.
*/

const LETTERS: Record<DBEngine, string> = { postgres: 'Pg', mysql: 'My', mongodb: 'Mg', redis: 'Rd', rabbitmq: 'Mq', kafka: 'Kf' }
const RADIUS: Record<DBEngine, string> = { postgres: '28%', mysql: '12% 40%', mongodb: '40% 12%', redis: '50%', rabbitmq: '12%', kafka: '50% 12%' }

export function EngineMark({ engine, size = 32 }: { engine: DBEngine; size?: number }) {
  return (
    <span
      role="img"
      aria-label={ENGINES[engine]?.label ?? engine}
      title={ENGINES[engine]?.label ?? engine}
      style={{
        width: size, height: size, flexShrink: 0, borderRadius: RADIUS[engine] ?? '28%',
        display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
        background: 'var(--color-raised)', border: '1px solid var(--color-line)',
        color: 'var(--mantine-color-text)', fontWeight: 700, fontSize: Math.round(size * 0.36), letterSpacing: '-0.02em',
      }}
    >
      {LETTERS[engine] ?? '?'}
    </span>
  )
}
