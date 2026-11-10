/** One argument, quoted the way SplitRedisCommand (tunnel) reads it back. */
export function redisArg(s: string): string {
  if (s !== '' && /^[A-Za-z0-9:_.\-*@/{}]+$/.test(s)) return s
  return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n').replace(/\r/g, '\\r').replace(/\t/g, '\\t')}"`
}

