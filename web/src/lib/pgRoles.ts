/**
 * The role Argus gives a level in a Postgres database: tunnel.PGLevelRole,
 * the same name the agent makes. A name that is not already lowercase
 * letters, digits and _ (or is too long) gets a hash of the database's name.
 */
export async function pgLevelRole(level: string, database: string): Promise<string> {
  // Byte by byte, as Go does: a non-ASCII letter is one _ per UTF-8 byte.
  const bytes = new TextEncoder().encode(database)
  const b = Array.from(bytes, (x) => {
    const c = String.fromCharCode(x)
    return /[A-Z]/.test(c) ? c.toLowerCase() : /[a-z0-9_]/.test(c) ? c : '_'
  }).join('')
  let name = `argus_${level}_${b}`
  if (name.length > 63 || b !== database) {
    const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
    const hex = Array.from(sum.slice(0, 4), (x) => x.toString(16).padStart(2, '0')).join('')
    name = `${name.slice(0, 54)}_${hex}`
  }
  return name
}
