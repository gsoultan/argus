/**
 * Client-side file export.
 *
 * Evidence leaves the console the same way it arrived — assembled in the
 * browser from what the user is already looking at. Nothing is round-tripped
 * through a server to produce it, so an export cannot differ from the thing on
 * screen, and an auditor's download does not depend on an endpoint being up.
 */

function save(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  a.remove()
  // Revoked on the next frame rather than immediately: Safari has not finished
  // reading the blob when click() returns, and revoking early yields an empty
  // file with no error anywhere.
  requestAnimationFrame(() => URL.revokeObjectURL(url))
}

export function downloadText(text: string, filename: string, type = 'text/plain'): void {
  save(new Blob([text], { type: `${type};charset=utf-8` }), filename)
}

export function downloadJSON(value: unknown, filename: string): void {
  save(
    new Blob([JSON.stringify(value, null, 2)], { type: 'application/json;charset=utf-8' }),
    filename,
  )
}

/** Filesystem-safe stamp for export filenames, e.g. 20260904-141205. */
export function stamp(d = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return (
    `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}` +
    `-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
  )
}
