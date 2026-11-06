import type { DBEngine, DBLevel } from '~/types/domain'

/**
 * What differs between the engines a database entry can be, in the
 * console's words. Levels mean the same thing everywhere -- read, write,
 * owner, DBA -- and each engine enforces them with its own permissions; these
 * blurbs say what that comes to on each.
 */
export interface EngineInfo {
  label: string
  /** What one entry is, as a noun. */
  noun: string
  /** What dbname holds, for the add form. */
  nameLabel: string
  nameHint: string
  defaults: { dbname: string; host: string; port: number; configPath?: string }
  levels: Record<DBLevel, string>
}

export const ENGINES: Record<DBEngine, EngineInfo> = {
  postgres: {
    label: 'Postgres',
    noun: 'database',
    nameLabel: 'Database name',
    nameHint: 'As Postgres knows it. The agent connects over the local socket as the postgres account.',
    defaults: { dbname: '', host: '/var/run/postgresql', port: 5432 },
    levels: {
      read: 'Look at data and run reports. The default.',
      write: 'Read and change rows: INSERT, UPDATE, DELETE.',
      owner: 'Change tables, columns and indexes too.',
      dba: 'Manage roles and grants, end other people’s queries. Never a Postgres superuser.',
    },
  },
  mysql: {
    label: 'MySQL',
    noun: 'database',
    nameLabel: 'Database name',
    nameHint: 'As MySQL or MariaDB knows it. The agent signs in as root over the server’s socket, or with an option file you name below.',
    defaults: { dbname: '', host: '127.0.0.1', port: 3306 },
    levels: {
      read: 'SELECT and SHOW VIEW on this database. The default.',
      write: 'Change rows too: INSERT, UPDATE, DELETE, and run procedures.',
      owner: 'Create, alter and drop tables, views, indexes, routines and triggers.',
      dba: 'Every privilege on this database, grant them to others, see all connections. Never SUPER, nothing on other databases.',
    },
  },
  redis: {
    label: 'Redis',
    noun: 'Redis',
    nameLabel: 'Logical db',
    nameHint: 'The db sessions open in; SELECT moves between them. One entry covers the whole Redis server.',
    defaults: { dbname: '0', host: '127.0.0.1', port: 6379, configPath: '/etc/redis/redis.conf' },
    levels: {
      read: 'Look at keys and values. Redis refuses every write.',
      write: 'Change values too. Not FLUSHDB, KEYS or anything that wipes or scans everything.',
      owner: 'Everything on the data, including FLUSHDB and KEYS. Nothing on the server: no CONFIG, no SHUTDOWN.',
      dba: 'Everything, including CONFIG and FLUSHALL. Dangerous commands still ask you to type the server’s name.',
    },
  },
  rabbitmq: {
    label: 'RabbitMQ',
    noun: 'vhost',
    nameLabel: 'Vhost',
    nameHint: 'One entry is one vhost. The agent reaches the management API on this server and manages users with rabbitmqctl.',
    defaults: { dbname: '/', host: '127.0.0.1', port: 15672 },
    levels: {
      read: 'See queues and exchanges, and peek at messages (they are put back).',
      write: 'Publish messages too.',
      owner: 'Purge queues, create and delete queues and bindings.',
      dba: 'See and close the vhost’s connections too.',
    },
  },
}

export function engineOf(e: string | undefined): EngineInfo {
  return ENGINES[(e as DBEngine) in ENGINES ? (e as DBEngine) : 'postgres']
}

/** A sessions-list protocol that is a database's statement recording. */
export function isStatementProtocol(protocol: string | undefined): boolean {
  return protocol === 'postgres' || protocol === 'mysql' || protocol === 'redis' || protocol === 'rabbitmq'
}

/** An engine the SQL workspace opens: Postgres, or MySQL and MariaDB. */
export function isSQLEngine(e: string | undefined): boolean {
  return e === undefined || e === '' || e === 'postgres' || e === 'mysql'
}
