import type { Completion, Suggestion } from '~/lib/sqlComplete'

/*
Autocomplete for a Redis command line: command names with their arguments,
the subcommands of container commands (CONFIG GET, CLIENT LIST, …), and key
names the console has already seen, where a command takes a key.

Offering a command is not permitting it. The person's ACL user decides what
runs, and a refused command comes back as Redis's own NOPERM.
*/

/** name → [arguments, does its first argument name a key] */
export const REDIS_COMMANDS: Record<string, [args: string, key: boolean]> = {
  APPEND: ['key value', true], BITCOUNT: ['key [start end]', true], BITPOS: ['key bit [start [end]]', true],
  BLMOVE: ['source destination LEFT|RIGHT LEFT|RIGHT timeout', true], BLPOP: ['key [key …] timeout', true],
  BRPOP: ['key [key …] timeout', true], BZPOPMAX: ['key [key …] timeout', true], BZPOPMIN: ['key [key …] timeout', true],
  CLIENT: ['subcommand', false], CONFIG: ['subcommand', false], COPY: ['source destination [REPLACE]', true],
  DBSIZE: ['', false], DECR: ['key', true], DECRBY: ['key decrement', true], DEL: ['key [key …]', true],
  DUMP: ['key', true], ECHO: ['message', false], EVAL: ['script numkeys [key …] [arg …]', false],
  EVALSHA: ['sha1 numkeys [key …] [arg …]', false], EXISTS: ['key [key …]', true], EXPIRE: ['key seconds', true],
  EXPIREAT: ['key unix-time', true], EXPIRETIME: ['key', true], GEOADD: ['key longitude latitude member', true],
  GEODIST: ['key member1 member2 [unit]', true], GEOPOS: ['key member [member …]', true],
  GEOSEARCH: ['key FROMMEMBER|FROMLONLAT BYRADIUS|BYBOX …', true], GET: ['key', true], GETDEL: ['key', true],
  GETEX: ['key [EX seconds|PX ms|PERSIST]', true], GETRANGE: ['key start end', true], GETSET: ['key value', true],
  HDEL: ['key field [field …]', true], HEXISTS: ['key field', true], HGET: ['key field', true], HGETALL: ['key', true],
  HINCRBY: ['key field increment', true], HINCRBYFLOAT: ['key field increment', true], HKEYS: ['key', true],
  HLEN: ['key', true], HMGET: ['key field [field …]', true], HRANDFIELD: ['key [count [WITHVALUES]]', true],
  HSCAN: ['key cursor [MATCH pattern] [COUNT count]', true], HSET: ['key field value [field value …]', true],
  HSETNX: ['key field value', true], HSTRLEN: ['key field', true], HVALS: ['key', true], INCR: ['key', true],
  INCRBY: ['key increment', true], INCRBYFLOAT: ['key increment', true], INFO: ['[section …]', false],
  KEYS: ['pattern', false], LASTSAVE: ['', false], LATENCY: ['subcommand', false], LINDEX: ['key index', true],
  LINSERT: ['key BEFORE|AFTER pivot element', true], LLEN: ['key', true],
  LMOVE: ['source destination LEFT|RIGHT LEFT|RIGHT', true], LPOP: ['key [count]', true],
  LPOS: ['key element [RANK rank] [COUNT n]', true], LPUSH: ['key element [element …]', true],
  LPUSHX: ['key element [element …]', true], LRANGE: ['key start stop', true], LREM: ['key count element', true],
  LSET: ['key index element', true], LTRIM: ['key start stop', true], MEMORY: ['subcommand', false],
  MGET: ['key [key …]', true], MSET: ['key value [key value …]', true], MSETNX: ['key value [key value …]', true],
  OBJECT: ['subcommand', false], PERSIST: ['key', true], PEXPIRE: ['key milliseconds', true],
  PEXPIREAT: ['key unix-time-ms', true], PFADD: ['key [element …]', true], PFCOUNT: ['key [key …]', true],
  PFMERGE: ['destkey [sourcekey …]', true], PING: ['[message]', false], PSETEX: ['key milliseconds value', true],
  PTTL: ['key', true], PUBLISH: ['channel message', false], PUBSUB: ['subcommand', false],
  RANDOMKEY: ['', false], RENAME: ['key newkey', true], RENAMENX: ['key newkey', true], RPOP: ['key [count]', true],
  RPUSH: ['key element [element …]', true], RPUSHX: ['key element [element …]', true],
  SADD: ['key member [member …]', true], SCAN: ['cursor [MATCH pattern] [COUNT count] [TYPE type]', false],
  SCARD: ['key', true], SCRIPT: ['subcommand', false], SDIFF: ['key [key …]', true],
  SDIFFSTORE: ['destination key [key …]', true], SET: ['key value [NX|XX] [EX seconds|PX ms|KEEPTTL] [GET]', true],
  SETEX: ['key seconds value', true], SETNX: ['key value', true], SETRANGE: ['key offset value', true],
  SINTER: ['key [key …]', true], SINTERCARD: ['numkeys key [key …]', false], SINTERSTORE: ['destination key [key …]', true],
  SISMEMBER: ['key member', true], SLOWLOG: ['subcommand', false], SMEMBERS: ['key', true],
  SMISMEMBER: ['key member [member …]', true], SMOVE: ['source destination member', true],
  SPOP: ['key [count]', true], SRANDMEMBER: ['key [count]', true], SREM: ['key member [member …]', true],
  SSCAN: ['key cursor [MATCH pattern] [COUNT count]', true], STRLEN: ['key', true], SUNION: ['key [key …]', true],
  SUNIONSTORE: ['destination key [key …]', true], TIME: ['', false], TOUCH: ['key [key …]', true],
  TTL: ['key', true], TYPE: ['key', true], UNLINK: ['key [key …]', true],
  XACK: ['key group id [id …]', true], XADD: ['key [MAXLEN n] *|id field value [field value …]', true],
  XDEL: ['key id [id …]', true], XGROUP: ['subcommand', false], XINFO: ['subcommand', false],
  XLEN: ['key', true], XPENDING: ['key group [start end count]', true], XRANGE: ['key start end [COUNT count]', true],
  XREAD: ['[COUNT n] [BLOCK ms] STREAMS key [key …] id [id …]', false], XREVRANGE: ['key end start [COUNT count]', true],
  XTRIM: ['key MAXLEN|MINID threshold', true], ZADD: ['key [NX|XX] score member [score member …]', true],
  ZCARD: ['key', true], ZCOUNT: ['key min max', true], ZINCRBY: ['key increment member', true],
  ZRANGE: ['key start stop [BYSCORE|BYLEX] [REV] [LIMIT offset count] [WITHSCORES]', true],
  ZRANGEBYSCORE: ['key min max [WITHSCORES] [LIMIT offset count]', true], ZRANK: ['key member', true],
  ZREM: ['key member [member …]', true], ZREVRANGE: ['key start stop [WITHSCORES]', true],
  ZREVRANK: ['key member', true], ZSCAN: ['key cursor [MATCH pattern] [COUNT count]', true],
  ZSCORE: ['key member', true], ZMSCORE: ['key member [member …]', true], ZPOPMAX: ['key [count]', true],
  ZPOPMIN: ['key [count]', true], ZRANDMEMBER: ['key [count [WITHSCORES]]', true],
}

export const REDIS_SUBCOMMANDS: Record<string, string[]> = {
  CLIENT: ['LIST', 'INFO', 'ID', 'GETNAME', 'SETNAME', 'KILL', 'PAUSE', 'UNPAUSE', 'NO-EVICT'],
  CONFIG: ['GET', 'SET', 'RESETSTAT', 'REWRITE'],
  LATENCY: ['LATEST', 'HISTORY', 'DOCTOR', 'RESET'],
  MEMORY: ['USAGE', 'STATS', 'DOCTOR'],
  OBJECT: ['ENCODING', 'FREQ', 'IDLETIME', 'REFCOUNT'],
  PUBSUB: ['CHANNELS', 'NUMSUB', 'NUMPAT', 'SHARDCHANNELS'],
  SCRIPT: ['EXISTS', 'FLUSH', 'LOAD'],
  SLOWLOG: ['GET', 'LEN', 'RESET'],
  XGROUP: ['CREATE', 'DESTROY', 'CREATECONSUMER', 'DELCONSUMER', 'SETID'],
  XINFO: ['STREAM', 'GROUPS', 'CONSUMERS'],
}

// Subcommands whose next argument is a key.
const SUB_TAKES_KEY = new Set(['MEMORY USAGE', 'OBJECT ENCODING', 'OBJECT FREQ', 'OBJECT IDLETIME',
  'OBJECT REFCOUNT', 'XINFO STREAM', 'XINFO GROUPS', 'XINFO CONSUMERS', 'XGROUP CREATE', 'XGROUP DESTROY',
  'XGROUP SETID', 'XGROUP CREATECONSUMER', 'XGROUP DELCONSUMER'])

const MAX_ITEMS = 60

/**
 * What to offer at `cursor` in a one-line Redis command. `keys` are names the
 * key browser has already listed; nothing extra is fetched for them.
 */
export function completeRedis(text: string, cursor: number, keys: readonly string[] = [], explicit = false): Completion | null {
  const head = text.slice(0, cursor)
  // Inside a quoted argument there is nothing to complete.
  let quote: string | null = null
  for (const c of head) {
    if (quote) { if (c === quote) quote = null } else if (c === '"' || c === "'") quote = c
  }
  if (quote) return null
  const m = /(\S*)$/.exec(head)!
  const prefix = m[1]!
  const from = cursor - prefix.length
  let to = cursor
  while (to < text.length && !/\s/.test(text[to]!)) to++
  const words = head.slice(0, from).trim().split(/\s+/).filter(Boolean)
  if (!explicit && !prefix) return null

  const upperTyped = prefix.toUpperCase()
  const lowerCase = prefix !== '' && prefix === prefix.toLowerCase()
  const style = (w: string) => (lowerCase ? w.toLowerCase() : w)
  const items: Suggestion[] = []

  if (words.length === 0) {
    for (const [name, [args]] of Object.entries(REDIS_COMMANDS)) {
      if (name.startsWith(upperTyped)) items.push({ label: name, insert: style(name), kind: 'command', detail: args })
    }
  } else {
    const cmd = words[0]!.toUpperCase()
    const subs = REDIS_SUBCOMMANDS[cmd]
    if (subs && words.length === 1) {
      for (const s of subs) if (s.startsWith(upperTyped)) items.push({ label: s, insert: style(s), kind: 'keyword', detail: cmd })
    } else {
      const spec = REDIS_COMMANDS[cmd]
      const sub = words.length >= 2 ? `${cmd} ${words[1]!.toUpperCase()}` : ''
      const keyHere = (spec?.[1] && !subs) || (sub && SUB_TAKES_KEY.has(sub) && words.length === 2)
      if (keyHere) {
        for (const k of keys) {
          if (k.startsWith(prefix)) items.push({ label: k, insert: /[\s"']/.test(k) ? JSON.stringify(k) : k, kind: 'key' })
          if (items.length >= MAX_ITEMS) break
        }
      }
    }
  }
  items.sort((a, b) => a.label.length - b.label.length || a.label.localeCompare(b.label))
  const out = items.slice(0, MAX_ITEMS)
  if (out.length === 0) return null
  return { from, to, prefix, items: out }
}
