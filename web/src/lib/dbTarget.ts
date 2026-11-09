import type { Database } from '~/types/domain'

/*
 * Its own module so the database page can name its neighbours without loading
 * the give-many dialogs.
 */

/** A database or broker's name the way its own page titles it. */
export function targetName(d: Pick<Database, 'engine' | 'dbname'>): string {
  if (d.engine === 'redis') return `Redis · db ${d.dbname}`
  if (d.engine === 'rabbitmq') return `RabbitMQ · ${d.dbname}`
  if (d.engine === 'kafka') return `Kafka · ${d.dbname === '*' ? 'every topic' : d.dbname}`
  if (d.engine === 'mysql') return `MySQL · ${d.dbname}`
  if (d.engine === 'mongodb') return `MongoDB · ${d.dbname}`
  return d.dbname
}
