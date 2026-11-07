import { useCallback, useEffect, useRef, useState } from 'react'
import { downloadBlob } from '~/lib/download'
import { FileOpError, type FileEntry, type FilesClient } from '~/lib/files/client'
import { baseName, freeName, joinPath } from '~/lib/files/paths'
import { makeZip, ZIP_LIMIT, type ZipEntry } from '~/lib/files/zip'

/**
 * The transfer queue: uploads, downloads and zips, run one at a time in the
 * order asked, because the files service is one operation at a time.
 *
 * An upload that finds its name taken stops at "conflict" and waits for a
 * person to say Replace, Keep both or Skip, while the rest of the queue
 * carries on behind it.
 */

export type TransferStatus = 'waiting' | 'active' | 'done' | 'failed' | 'conflict' | 'skipped'

export interface Transfer {
  id: number
  kind: 'upload' | 'download' | 'zip'
  /** What the person sees: the file's name, or the zip's. */
  name: string
  /** Upload: the folder it goes into. Download: the file's full path. */
  dir: string
  size: number
  bytes: number
  status: TransferStatus
  error?: string
  sha256?: string
  overwrite?: boolean
  file?: File
  /** Upload from a dropped folder: the folders under `dir` it goes into. */
  subdirs?: string[]
  /** Zip: the files that go into it. */
  members?: { path: string; size: number; mtime: string }[]
  startedAt?: number
}

let nextId = 1

export function useTransfers(client: FilesClient | null, opts: {
  /** Names already in a folder, for Keep both. */
  namesIn: (dir: string) => string[]
  /** Called after an upload or folder change lands, to refresh a listing. */
  onChanged: (dir: string) => void
  onDownloaded: (path: string) => void
}) {
  const [items, setItems] = useState<Transfer[]>([])
  const ref = useRef<Transfer[]>([])
  const running = useRef(false)
  const optsRef = useRef(opts)
  optsRef.current = opts

  const commit = useCallback((next: Transfer[]) => {
    ref.current = next
    setItems(next)
  }, [])
  const patch = useCallback((id: number, p: Partial<Transfer>) => {
    commit(ref.current.map((t) => (t.id === id ? { ...t, ...p } : t)))
  }, [commit])

  const run = useCallback(async () => {
    if (running.current || !client) return
    running.current = true
    try {
      for (;;) {
        const t = ref.current.find((x) => x.status === 'waiting')
        if (!t) break
        patch(t.id, { status: 'active', bytes: 0, startedAt: Date.now(), error: undefined })
        try {
          if (t.kind === 'upload' && t.file) {
            let dir = t.dir
            for (const sub of t.subdirs ?? []) {
              dir = joinPath(dir, sub)
              try {
                await client.mkdir(dir)
              } catch (err) {
                if (!(err instanceof FileOpError && err.exists)) throw err
              }
            }
            const r = await client.upload(joinPath(dir, t.name), t.file, Boolean(t.overwrite),
              (bytes) => patch(t.id, { bytes }))
            patch(t.id, { status: 'done', bytes: r.size, sha256: r.sha256, file: undefined })
            optsRef.current.onChanged(t.dir)
          } else if (t.kind === 'download') {
            const r = await client.download(t.dir, (bytes) => patch(t.id, { bytes }))
            downloadBlob(r.data, t.name)
            patch(t.id, { status: 'done', bytes: r.size, sha256: r.sha256 })
            optsRef.current.onDownloaded(t.dir)
          } else if (t.kind === 'zip' && t.members) {
            const entries: ZipEntry[] = []
            let before = 0
            for (const m of t.members) {
              const r = await client.download(m.path, (bytes) => patch(t.id, { bytes: before + bytes }))
              entries.push({
                name: baseName(m.path),
                data: new Uint8Array(await r.data.arrayBuffer()),
                modified: m.mtime ? new Date(m.mtime) : undefined,
              })
              before += r.size
            }
            downloadBlob(makeZip(entries), t.name)
            patch(t.id, { status: 'done', bytes: before })
          }
        } catch (err) {
          if (err instanceof FileOpError && err.exists && t.kind === 'upload' && !t.overwrite) {
            patch(t.id, { status: 'conflict', error: err.message })
          } else {
            patch(t.id, { status: 'failed', error: err instanceof Error ? err.message : String(err) })
          }
          if (client.state !== 'live') break
        }
      }
    } finally {
      running.current = false
    }
  }, [client, patch])

  useEffect(() => {
    if (items.some((t) => t.status === 'waiting')) void run()
  }, [items, run])

  const add = useCallback((t: Omit<Transfer, 'id' | 'bytes' | 'status'>[]) => {
    commit([...ref.current, ...t.map((x) => ({ ...x, id: nextId++, bytes: 0, status: 'waiting' as const }))])
  }, [commit])

  const uploadFiles = useCallback((dir: string, files: { file: File; subdirs?: string[] }[]) => {
    add(files.map(({ file, subdirs }) => ({
      kind: 'upload' as const, name: file.name, dir, size: file.size, file, subdirs,
    })))
  }, [add])

  const downloadFile = useCallback((path: string, e: FileEntry) => {
    add([{ kind: 'download', name: e.name, dir: path, size: e.size }])
  }, [add])

  /** Zips files; returns why it cannot, or null. */
  const zipFiles = useCallback((name: string, members: Transfer['members'] & object): string | null => {
    const total = members.reduce((n, m) => n + m.size, 0)
    if (total > ZIP_LIMIT || total > 2 * 1024 * 1024 * 1024) {
      return 'Together these are over 2 GB, more than a browser can zip. Download them one at a time.'
    }
    add([{ kind: 'zip', name, dir: '', size: total, members }])
    return null
  }, [add])

  const resolve = useCallback((id: number, choice: 'replace' | 'both' | 'skip') => {
    const t = ref.current.find((x) => x.id === id)
    if (!t || t.status !== 'conflict') return
    if (choice === 'skip') {
      patch(id, { status: 'skipped', file: undefined })
    } else if (choice === 'replace') {
      patch(id, { status: 'waiting', overwrite: true, error: undefined })
    } else {
      const dir = (t.subdirs ?? []).reduce((d, s) => joinPath(d, s), t.dir)
      const queued = ref.current.filter((x) => x.dir === t.dir && x.id !== id).map((x) => x.name)
      patch(id, {
        status: 'waiting', error: undefined,
        name: freeName(t.name, [...optsRef.current.namesIn(dir), ...queued]),
      })
    }
  }, [patch])

  const retry = useCallback((id: number) => {
    const t = ref.current.find((x) => x.id === id)
    if (t?.status === 'failed' && (t.kind !== 'upload' || t.file)) patch(id, { status: 'waiting', error: undefined })
  }, [patch])

  /** Takes a waiting item out of the queue. One already moving cannot be stopped mid-file. */
  const cancel = useCallback((id: number) => {
    const t = ref.current.find((x) => x.id === id)
    if (t?.status === 'waiting' || t?.status === 'conflict') patch(id, { status: 'skipped', file: undefined })
  }, [patch])

  const clearFinished = useCallback(() => {
    commit(ref.current.filter((t) => t.status === 'waiting' || t.status === 'active' || t.status === 'conflict'))
  }, [commit])

  return { items, uploadFiles, downloadFile, zipFiles, resolve, retry, cancel, clearFinished }
}

/** Files and folders dropped on the page, flattened with the folders they sat in. */
export async function droppedFiles(dt: DataTransfer): Promise<{ file: File; subdirs?: string[] }[]> {
  const out: { file: File; subdirs?: string[] }[] = []
  const entries = Array.from(dt.items ?? [])
    .map((i) => (typeof i.webkitGetAsEntry === 'function' ? i.webkitGetAsEntry() : null))
    .filter((e): e is FileSystemEntry => e !== null)
  if (entries.length === 0) {
    return Array.from(dt.files).map((file) => ({ file }))
  }
  const walk = async (entry: FileSystemEntry, dirs: string[]) => {
    if (entry.isFile) {
      const file = await new Promise<File>((res, rej) => (entry as FileSystemFileEntry).file(res, rej))
      out.push({ file, subdirs: dirs.length ? dirs : undefined })
      return
    }
    if (entry.isDirectory) {
      const reader = (entry as FileSystemDirectoryEntry).createReader()
      // readEntries hands back a batch at a time until it returns none.
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej))
        if (batch.length === 0) break
        for (const child of batch) await walk(child, [...dirs, entry.name])
      }
    }
  }
  for (const e of entries) await walk(e, [])
  return out
}
