import { get, send } from '~/lib/live'
import type { BulkAccessInput, BulkAccessLine, DBAssignment, DatabaseRule } from '~/types/domain'

/*
 * Giving many databases at once, and the rules for ones added later. Here
 * rather than in live.ts, which every page loads, so only the database pages
 * pay for them.
 */

/** What a grant on many databases would change. Writes nothing. */
export function planDBAccess(input: BulkAccessInput): Promise<{ lines: BulkAccessLine[] }> {
  return send('POST', '/api/v1/database-access/plan', input)
}

/** Give teams and people a level on many databases at once. */
export function giveDBAccess(input: BulkAccessInput): Promise<{ lines: BulkAccessLine[]; rules: DatabaseRule[] | null }> {
  return send('POST', '/api/v1/database-access', input)
}

/** Take teams and people off many databases at once. */
export function removeDBAccess(input: { databaseIds: string[]; teams: string[]; people: string[] }): Promise<{ lines: BulkAccessLine[] | null }> {
  return send('POST', '/api/v1/database-access/remove', input)
}

/** Every grant and rule on one server's databases. */
export function assetDBAccess(assetId: string): Promise<{ assignments: DBAssignment[]; rules: DatabaseRule[] }> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/database-access`)
}

/** Stop a rule; with takeAway, also take back what it gave. */
export function removeDBRule(assetId: string, ruleId: string, takeAway: boolean): Promise<{ lines: BulkAccessLine[] | null }> {
  return send('DELETE',
    `/api/v1/assets/${encodeURIComponent(assetId)}/database-rules/${encodeURIComponent(ruleId)}${takeAway ? '?takeAway=1' : ''}`)
}
