import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  ActionIcon, Alert, Anchor, Badge, Box, Button, Checkbox, CopyButton, Drawer, FileButton, Group, Loader,
  Menu, Modal, ScrollArea, Stack, Table, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import {
  IconArrowLeft, IconChevronRight, IconCopy, IconDots, IconDownload, IconEye, IconEyeOff, IconFile,
  IconFileZip, IconFolder, IconFolderPlus, IconHome, IconLink, IconPencil, IconPin, IconPinnedOff,
  IconRefresh, IconSearch, IconSortAscending, IconSortDescending, IconTrash, IconUpload, IconX,
} from '@tabler/icons-react'
import { relTime } from '~/components/primitives'
import {
  FileOpError, FilesClient, type FileEntry, type FilesState,
} from '~/lib/files/client'
import {
  baseName, crumbs, formatSize, joinPath, looksTextual, parentPath, validName,
} from '~/lib/files/paths'
import {
  pinnedPlaces, recentFiles, rememberFile, setPinned, setShowHiddenPref, showHiddenPref,
  STANDARD_PLACES, type Place, type RecentFile,
} from '~/lib/files/places'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK } from '~/theme'
import { PreviewPanel } from './PreviewPanel'
import { TransfersTray } from './TransfersTray'
import { droppedFiles, useTransfers } from './useTransfers'

/**
 * Files on a server, in the browser: the console's scp, and a little more.
 *
 * Every operation runs on the server as the session's account, through its
 * agent, and is written to the audit chain: uploads, downloads and previews
 * with their size and hash, new folders, renames and deletes with the path.
 * Nothing here can do what the account itself could not.
 */

export type { FilesState }

export interface FileBrowserProps {
  /** The WebSocket endpoint as an http(s) URL; see filesEndpoint. */
  endpoint: string
  /** Fetches a ticket; see LiveTerminal for why this is a callback. */
  getTicket: () => Promise<string | null>
  target: string
  principal: string
  onStateChange?: (state: FilesState, detail?: string) => void
  /** The terminal's current folder, when it reports one, for Split view. */
  follow?: string | null
  /** Narrow layout for Split view: no places column, fewer table columns. */
  compact?: boolean
}

type SortKey = 'name' | 'size' | 'mtime'

export function FileBrowser({
  endpoint, getTicket, target, principal, onStateChange, follow, compact = false,
}: FileBrowserProps) {
  const phone = useMediaQuery('(max-width: 48em)') ?? false
  const narrow = compact || phone

  const [client, setClient] = useState<FilesClient | null>(null)
  const [state, setState] = useState<FilesState>('connecting')
  const [detail, setDetail] = useState<string>()
  const onStateRef = useRef(onStateChange)
  onStateRef.current = onStateChange

  const [path, setPath] = useState('~')
  const [entries, setEntries] = useState<FileEntry[]>([])
  const [truncated, setTruncated] = useState(false)
  const [loading, setLoading] = useState(false)
  const [listError, setListError] = useState<string | null>(null)
  const [history, setHistory] = useState<string[]>([])

  const [filter, setFilter] = useState('')
  const [hidden, setHidden] = useState(showHiddenPref)
  const [sort, setSort] = useState<{ key: SortKey; desc: boolean }>({ key: 'name', desc: false })
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [previewing, setPreviewing] = useState<string | null>(null)
  // The toolbar has less room beside an open preview: fold it as when narrow.
  const tight = narrow || !!previewing
  const [renaming, setRenaming] = useState<FileEntry | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<FileEntry | null>(null)
  const [sheet, setSheet] = useState<FileEntry | null>(null)
  const [placesOpen, setPlacesOpen] = useState(false)
  const [dragging, setDragging] = useState(false)
  const [following, setFollowing] = useState(true)
  const [pins, setPins] = useState<Place[]>(() => pinnedPlaces(target, principal))
  const [recent, setRecent] = useState<RecentFile[]>(() => recentFiles(target, principal))
  const pathRef = useRef(path)
  pathRef.current = path

  // One connection for the life of this view.
  useEffect(() => {
    const c = new FilesClient({
      endpoint, getTicket, target, principal,
      onState: (s, d) => {
        setState(s)
        setDetail(d)
        onStateRef.current?.(s, d)
      },
    })
    setClient(c)
    setState('connecting')
    void c.open()
    return () => c.close()
    // getTicket is deliberately not a dependency: a new ticket is a new session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, target, principal])

  const list = useCallback(async (p: string, opts: { push?: boolean } = {}) => {
    if (!client) return
    setLoading(true)
    setListError(null)
    try {
      const r = await client.list(p)
      if (opts.push !== false && r.path !== pathRef.current) {
        setHistory((h) => [...h.slice(-49), pathRef.current])
      }
      setPath(r.path)
      setEntries(r.entries)
      setTruncated(r.truncated)
      setSelected(new Set())
    } catch (err) {
      setListError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [client])

  useEffect(() => {
    if (state === 'live') void list('~', { push: false })
    // Listed once when the connection opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state])

  // Split view: go where the terminal goes.
  useEffect(() => {
    if (state !== 'live' || !follow || !following) return
    if (follow !== pathRef.current) void list(follow)
  }, [follow, following, state, list])

  const transfers = useTransfers(client, {
    namesIn: (dir) => (dir === pathRef.current ? entries.map((e) => e.name) : []),
    onChanged: (dir) => {
      if (dir === pathRef.current) void list(dir, { push: false })
    },
    onDownloaded: (p) => setRecent(rememberFile(target, principal, p)),
  })

  const live = state === 'live'
  const visible = useMemo(() => {
    const needle = filter.trim().toLowerCase()
    const out = entries.filter((e) => (hidden || !e.name.startsWith('.')) && (!needle || e.name.toLowerCase().includes(needle)))
    const dir = sort.desc ? -1 : 1
    out.sort((a, b) => {
      if (a.dir !== b.dir) return a.dir ? -1 : 1
      if (sort.key === 'size') return (a.size - b.size) * dir || a.name.localeCompare(b.name)
      if (sort.key === 'mtime') return (Date.parse(a.mtime || '0') - Date.parse(b.mtime || '0')) * dir || a.name.localeCompare(b.name)
      return a.name.localeCompare(b.name) * dir
    })
    return out
  }, [entries, filter, hidden, sort])
  const hiddenCount = useMemo(() => entries.filter((e) => e.name.startsWith('.')).length, [entries])

  const open = (e: FileEntry) => {
    if (e.dir) void list(joinPath(path, e.name))
    else if (phone) setSheet(e)
    else setPreviewing(joinPath(path, e.name))
  }
  const download = (e: FileEntry) => transfers.downloadFile(joinPath(path, e.name), e)
  const upload = (files: File[] | { file: File; subdirs?: string[] }[]) => {
    const list = (files as unknown[]).map((f) => (f instanceof File ? { file: f } : (f as { file: File; subdirs?: string[] })))
    if (list.length) transfers.uploadFiles(path, list)
  }

  const changeFailed = (title: string, err: unknown) => {
    if (err instanceof FileOpError && err.unsupported) {
      notifyError(title, new Error('The agent on this server is older than the console and cannot do this yet. Update the agent.'))
    } else {
      notifyError(title, err)
    }
  }

  const pinned = pins.some((p) => p.path === path)
  const togglePin = () => {
    const next = pinned ? pins.filter((p) => p.path !== path) : [...pins, { label: baseName(path) || path, path }]
    setPins(next)
    setPinned(target, principal, next)
  }

  const selectedEntries = visible.filter((e) => selected.has(e.name))
  const zipSelected = () => {
    const files = selectedEntries.filter((e) => !e.dir)
    if (files.length === 0) return
    const why = transfers.zipFiles(`${target.split('.')[0]}-${baseName(path) || 'files'}.zip`,
      files.map((e) => ({ path: joinPath(path, e.name), size: e.size, mtime: e.mtime })))
    if (why) notifyError('Not zipped', new Error(why))
    else setSelected(new Set())
  }

  if (state === 'connecting') {
    return (
      <Group gap="xs" p="xl" justify="center">
        <Loader size="xs" />
        <Text size={FS.meta} c="dimmed">Opening files through the server&apos;s agent…</Text>
      </Group>
    )
  }

  const toolbar = (
    <Group gap="xs" wrap="nowrap" px={narrow ? 'sm' : 'lg'} py="sm" className="argus-files-bar">
      <Tooltip label="Back">
        <ActionIcon variant="default" size="lg" aria-label="Back" disabled={!live || history.length === 0}
          onClick={() => {
            const prev = history[history.length - 1]
            if (prev === undefined) return
            setHistory((h) => h.slice(0, -1))
            void list(prev, { push: false })
          }}>
          <IconArrowLeft size={16} />
        </ActionIcon>
      </Tooltip>
      <PathBar path={path} disabled={!live} onGo={(p) => void list(p)} compact={tight} />
      {!tight && (
        <TextInput w={200} placeholder="Filter this folder" leftSection={<IconSearch size={14} />}
          value={filter} onChange={(e) => setFilter(e.currentTarget.value)} aria-label="Filter this folder"
          rightSection={filter ? <ActionIcon size="sm" variant="subtle" color="slate" onClick={() => setFilter('')} aria-label="Clear filter"><IconX size={12} /></ActionIcon> : null} />
      )}
      {!tight && (
        <Tooltip label={hidden ? 'Hide hidden files' : `Show hidden files${hiddenCount ? ` (${hiddenCount})` : ''}`}>
          <ActionIcon variant={hidden ? 'light' : 'default'} size="lg" aria-pressed={hidden}
            aria-label="Show hidden files" onClick={() => { setHidden(!hidden); setShowHiddenPref(!hidden) }}>
            {hidden ? <IconEye size={16} /> : <IconEyeOff size={16} />}
          </ActionIcon>
        </Tooltip>
      )}
      <Tooltip label="Refresh">
        <ActionIcon variant="default" size="lg" aria-label="Refresh" disabled={!live || loading}
          onClick={() => void list(path, { push: false })}>
          {loading ? <Loader size={14} /> : <IconRefresh size={16} />}
        </ActionIcon>
      </Tooltip>
      {tight ? (
        <Menu position="bottom-end" withinPortal>
          <Menu.Target>
            <ActionIcon variant="default" size="lg" aria-label="More"><IconDots size={16} /></ActionIcon>
          </Menu.Target>
          <Menu.Dropdown>
            <Menu.Item leftSection={<IconFolderPlus size={14} />} disabled={!live} onClick={() => setCreating(true)}>New folder</Menu.Item>
            <Menu.Item leftSection={hidden ? <IconEyeOff size={14} /> : <IconEye size={14} />}
              onClick={() => { setHidden(!hidden); setShowHiddenPref(!hidden) }}>
              {hidden ? 'Hide hidden files' : 'Show hidden files'}
            </Menu.Item>
            <Menu.Item leftSection={pinned ? <IconPinnedOff size={14} /> : <IconPin size={14} />} onClick={togglePin}>
              {pinned ? 'Unpin this folder' : 'Pin this folder'}
            </Menu.Item>
            {phone && <Menu.Item leftSection={<IconHome size={14} />} onClick={() => setPlacesOpen(true)}>Places</Menu.Item>}
          </Menu.Dropdown>
        </Menu>
      ) : (
        <Button variant="default" leftSection={<IconFolderPlus size={16} />} disabled={!live} onClick={() => setCreating(true)}>
          New folder
        </Button>
      )}
      {!phone && (
        <FileButton multiple onChange={(f) => upload(f)} disabled={!live}>
          {(props) => (
            <Button leftSection={<IconUpload size={16} />} disabled={!live} {...props}>Upload</Button>
          )}
        </FileButton>
      )}
    </Group>
  )

  const notices = (
    <>
      {!live && (
        <Alert color={state === 'error' ? 'rose' : 'slate'} m="md" title="File transfer closed">
          {detail ? `${detail}. ` : ''}Open it again from the server to carry on.
        </Alert>
      )}
      {listError && (
        <Alert color="rose" m="md" withCloseButton onClose={() => setListError(null)} title="Could not open that folder">
          {listError}
        </Alert>
      )}
      {compact && follow && (
        <Group gap="xs" px="lg" py={6} className="argus-files-follow">
          <Text size={FS.micro}>
            {following ? 'Following the terminal’s folder.' : 'Not following the terminal.'}
          </Text>
          <Anchor size={FS.micro} fw={600} component="button" onClick={() => setFollowing(!following)}>
            {following ? 'Stop following' : 'Follow again'}
          </Anchor>
        </Group>
      )}
    </>
  )

  const body = phone ? (
    <PhoneList entries={visible} onOpen={open} onMore={setSheet} />
  ) : (
    <FileTable
      entries={visible} compact={compact} sort={sort} onSort={(key) => setSort((s) => ({ key, desc: s.key === key ? !s.desc : key !== 'name' }))}
      selected={selected} onSelect={setSelected} live={live} active={previewing}
      onOpen={open} onDownload={download} onPreview={(e) => setPreviewing(joinPath(path, e.name))}
      onRename={setRenaming} onDelete={setDeleting} path={path}
    />
  )

  const empty = live && !loading && visible.length === 0 && !listError && (
    <Stack align="center" gap={4} py={48}>
      <IconFolder size={28} opacity={0.4} />
      <Text size="sm" fw={500}>{entries.length === 0 ? 'This folder is empty' : 'Nothing matches'}</Text>
      <Text size={FS.meta} c="dimmed">
        {entries.length === 0 ? 'Drop files here to upload them.' : filter ? 'Clear the filter to see everything.' : `${hiddenCount} hidden ${hiddenCount === 1 ? 'file' : 'files'}.`}
      </Text>
    </Stack>
  )

  return (
    <Box
      className="argus-files"
      data-compact={compact || undefined}
      onDragOver={(e) => {
        if (!live || !e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={(e) => {
        if (e.currentTarget.contains(e.relatedTarget as Node | null)) return
        setDragging(false)
      }}
      onDrop={(e) => {
        if (!live) return
        e.preventDefault()
        setDragging(false)
        void droppedFiles(e.dataTransfer).then(upload, (err) => notifyError('Could not read what was dropped', err))
      }}
    >
      {!narrow && (
        <Places
          path={path} pins={pins} recent={recent} pinned={pinned} onTogglePin={togglePin}
          onGo={(p) => void list(p)} onRecent={(p) => { void list(parentPath(p)); setPreviewing(p) }}
        />
      )}
      <Box className="argus-files-main">
        {toolbar}
        {notices}
        <ScrollArea className="argus-files-scroll" type="auto">
          {body}
          {empty}
          {truncated && (
            <Text size={FS.micro} c="dimmed" px="lg" py="sm">
              Showing the first 5,000 entries. Type a path in the bar above to go straight to a folder.
            </Text>
          )}
        </ScrollArea>
        {selectedEntries.length > 0 && !phone && (
          <Group className="argus-files-selbar" gap="sm" wrap="nowrap">
            <Text size="sm" fw={600}>{selectedEntries.length} selected</Text>
            <Text size={FS.meta} c="dimmed">{formatSize(selectedEntries.reduce((n, e) => n + (e.dir ? 0 : e.size), 0))}</Text>
            <Button size="xs" leftSection={<IconFileZip size={14} />} onClick={zipSelected}
              disabled={selectedEntries.every((e) => e.dir)}>
              Download as .zip
            </Button>
            <CopyButton value={selectedEntries.map((e) => joinPath(path, e.name)).join('\n')}>
              {({ copied, copy }) => (
                <Button size="xs" variant="default" onClick={copy}>{copied ? 'Copied' : 'Copy paths'}</Button>
              )}
            </CopyButton>
            <ActionIcon variant="subtle" color="slate" aria-label="Clear selection" onClick={() => setSelected(new Set())}>
              <IconX size={14} />
            </ActionIcon>
          </Group>
        )}
        {phone && live && (
          <FileButton multiple onChange={(f) => upload(f)}>
            {(props) => (
              <Button className="argus-files-fab" size="md" radius="xl" leftSection={<IconUpload size={18} />} {...props}>
                Upload
              </Button>
            )}
          </FileButton>
        )}
        {dragging && (
          <Stack className="argus-files-drop" align="center" justify="center" gap={6}>
            <IconUpload size={36} color={INK.brand} />
            <Text fw={600} size="lg">Drop to upload to {path}</Text>
            <Text size={FS.meta} c="dimmed">Folders keep their structure. Every file is recorded.</Text>
          </Stack>
        )}
        <TransfersTray
          items={transfers.items} docked={phone}
          onResolve={transfers.resolve} onRetry={transfers.retry} onCancel={transfers.cancel}
          onClear={transfers.clearFinished}
        />
      </Box>
      {previewing && client && !phone && (
        <Box className="argus-files-side">
          <PreviewPanel
            client={client} path={previewing} onClose={() => setPreviewing(null)}
            onDownload={() => {
              const e = entries.find((x) => joinPath(path, x.name) === previewing)
              transfers.downloadFile(previewing, e ?? { name: baseName(previewing), dir: false, size: 0, mode: '', mtime: '' })
            }}
            onOpened={(p) => setRecent(rememberFile(target, principal, p))}
          />
        </Box>
      )}

      {/* Phone: a file's actions, then its preview, as sheets. */}
      <Drawer opened={!!sheet} onClose={() => setSheet(null)} position="bottom" size="auto" withCloseButton={false}
        radius="lg" title={null}>
        {sheet && (
          <Stack gap="sm" pb="md">
            <Group gap="sm" wrap="nowrap">
              <IconFile size={20} opacity={0.6} />
              <Box style={{ minWidth: 0 }}>
                <Text fw={600} truncate>{sheet.name}</Text>
                <Text size={FS.micro} c="dimmed">{formatSize(sheet.size)} · {sheet.mtime ? relTime(sheet.mtime) : ''}</Text>
              </Box>
            </Group>
            {looksTextual(sheet.name) && (
              <Button size="md" fullWidth onClick={() => { setPreviewing(joinPath(path, sheet.name)); setSheet(null) }}>Preview</Button>
            )}
            <Button size="md" fullWidth variant="default" leftSection={<IconDownload size={16} />}
              onClick={() => { download(sheet); setSheet(null) }}>Download</Button>
            <CopyButton value={joinPath(path, sheet.name)}>
              {({ copied, copy }) => (
                <Button size="md" fullWidth variant="default" leftSection={<IconCopy size={16} />} onClick={copy}>
                  {copied ? 'Copied' : 'Copy path'}
                </Button>
              )}
            </CopyButton>
            <Group grow gap="sm">
              <Button size="md" variant="default" onClick={() => { setRenaming(sheet); setSheet(null) }}>Rename</Button>
              <Button size="md" variant="default" color="rose" c={INK.denied} onClick={() => { setDeleting(sheet); setSheet(null) }}>Delete</Button>
            </Group>
          </Stack>
        )}
      </Drawer>
      <Drawer opened={phone && !!previewing} onClose={() => setPreviewing(null)} position="bottom" size="100%"
        withCloseButton={false} padding={0}>
        {previewing && client && phone && (
          <Box h="100dvh">
            <PreviewPanel client={client} path={previewing} onClose={() => setPreviewing(null)}
              onDownload={() => transfers.downloadFile(previewing, entries.find((x) => joinPath(path, x.name) === previewing)
                ?? { name: baseName(previewing), dir: false, size: 0, mode: '', mtime: '' })}
              onOpened={(p) => setRecent(rememberFile(target, principal, p))} />
          </Box>
        )}
      </Drawer>
      <Drawer opened={placesOpen} onClose={() => setPlacesOpen(false)} position="bottom" size="auto" radius="lg" title="Places">
        <Places path={path} pins={pins} recent={recent} pinned={pinned} onTogglePin={togglePin} flat
          onGo={(p) => { setPlacesOpen(false); void list(p) }}
          onRecent={(p) => { setPlacesOpen(false); void list(parentPath(p)); setPreviewing(p) }} />
      </Drawer>

      <NameModal
        opened={creating} title="New folder" label="Folder name" confirm="Create folder" initial=""
        taken={entries.map((e) => e.name)} onClose={() => setCreating(false)}
        onSubmit={async (name) => {
          if (!client) return
          try {
            await client.mkdir(joinPath(path, name))
            setCreating(false)
            void list(path, { push: false })
          } catch (err) {
            changeFailed('Folder not created', err)
          }
        }}
      />
      <NameModal
        opened={!!renaming} title={`Rename ${renaming?.name ?? ''}`} label="New name" confirm="Rename"
        initial={renaming?.name ?? ''} taken={entries.map((e) => e.name).filter((n) => n !== renaming?.name)}
        onClose={() => setRenaming(null)}
        onSubmit={async (name) => {
          if (!client || !renaming) return
          if (name === renaming.name) {
            setRenaming(null)
            return
          }
          try {
            await client.rename(joinPath(path, renaming.name), joinPath(path, name))
            setRenaming(null)
            void list(path, { push: false })
          } catch (err) {
            changeFailed('Not renamed', err)
          }
        }}
      />
      <Modal opened={!!deleting} onClose={() => setDeleting(null)} title={`Delete ${deleting?.name ?? ''}?`} centered>
        {deleting && (
          <Stack gap="md">
            <Text size="sm" lh={1.5}>
              {deleting.dir
                ? 'The folder is deleted only if it is empty. '
                : 'The file is deleted from the server for good. There is no bin to restore it from. '}
              The delete is recorded in Activity with your name.
            </Text>
            <Text size={FS.meta} ff="monospace" c="dimmed">{joinPath(path, deleting.name)}</Text>
            <Group justify="flex-end" gap="sm">
              <Button variant="default" onClick={() => setDeleting(null)}>Cancel</Button>
              <Button color="rose" onClick={async () => {
                if (!client) return
                const name = deleting.name
                try {
                  await client.remove(joinPath(path, name))
                  setDeleting(null)
                  notifyOk('Deleted', `${name} is gone from ${target.split('.')[0]}.`)
                  void list(path, { push: false })
                } catch (err) {
                  changeFailed('Not deleted', err)
                }
              }}>
                Delete
              </Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </Box>
  )
}

/** Breadcrumbs that turn into a text box when clicked, for typing a path. */
function PathBar({ path, disabled, onGo, compact }: {
  path: string
  disabled: boolean
  onGo: (p: string) => void
  compact: boolean
}) {
  const [editing, setEditing] = useState(false)
  const [typed, setTyped] = useState(path)
  useEffect(() => setTyped(path), [path])
  const parts = crumbs(path)
  const shown = compact && parts.length > 3 ? [{ label: '…', path: parts[parts.length - 3]!.path }, ...parts.slice(-2)] : parts

  if (editing) {
    return (
      <TextInput
        style={{ flex: 1, minWidth: 0 }} autoFocus value={typed} aria-label="Folder path"
        onChange={(e) => setTyped(e.currentTarget.value)}
        onBlur={() => setEditing(false)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            setEditing(false)
            if (typed.trim()) onGo(typed.trim())
          }
          if (e.key === 'Escape') {
            setTyped(path)
            setEditing(false)
          }
        }}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
      />
    )
  }
  return (
    <Group className="argus-files-crumbs" gap={2} wrap="nowrap" style={{ flex: 1, minWidth: 0 }}
      onClick={(e) => {
        if (!disabled && e.target === e.currentTarget) setEditing(true)
      }}
      role="navigation" aria-label="Folder">
      {shown.map((c, i) => (
        <Group key={c.path + i} gap={2} wrap="nowrap" style={{ minWidth: 0 }}>
          <UnstyledButton className="argus-files-crumb" data-last={i === shown.length - 1 || undefined}
            disabled={disabled} onClick={() => onGo(c.path)}>
            {c.label}
          </UnstyledButton>
          {i < shown.length - 1 && <IconChevronRight size={12} opacity={0.4} style={{ flexShrink: 0 }} />}
        </Group>
      ))}
      {!compact && (
        <UnstyledButton className="argus-files-crumb-edit" disabled={disabled} onClick={() => setEditing(true)}>
          Type a path
        </UnstyledButton>
      )}
    </Group>
  )
}

function Places({ path, pins, recent, pinned, onTogglePin, onGo, onRecent, flat }: {
  path: string
  pins: Place[]
  recent: RecentFile[]
  pinned: boolean
  onTogglePin: () => void
  onGo: (p: string) => void
  onRecent: (p: string) => void
  flat?: boolean
}) {
  const item = (label: ReactNode, p: string, icon: ReactNode, onClick: () => void, extra?: ReactNode) => (
    <UnstyledButton key={p} className="argus-files-place" data-active={p === path || undefined} onClick={onClick}>
      {icon}
      <Text size="sm" truncate style={{ flex: 1, minWidth: 0 }}>{label}</Text>
      {extra}
    </UnstyledButton>
  )
  return (
    <Box className={flat ? undefined : 'argus-files-places'} component="nav" aria-label="Places">
      <Text className="argus-files-places-h">Places</Text>
      {STANDARD_PLACES.map((p) => item(p.label, p.path, p.path === '~' ? <IconHome size={16} /> : <IconFolder size={16} />, () => onGo(p.path)))}
      {pins.length > 0 && <Text className="argus-files-places-h">Pinned</Text>}
      {pins.map((p) => item(p.label, p.path, <IconPin size={16} />, () => onGo(p.path)))}
      <UnstyledButton className="argus-files-place subtle" onClick={onTogglePin}>
        {pinned ? <IconPinnedOff size={16} /> : <IconPin size={16} />}
        <Text size={FS.meta}>{pinned ? 'Unpin this folder' : 'Pin this folder'}</Text>
      </UnstyledButton>
      {recent.length > 0 && <Text className="argus-files-places-h">Recent</Text>}
      {recent.map((r) => item(baseName(r.path), r.path, <IconFile size={16} opacity={0.6} />, () => onRecent(r.path),
        <Text size={FS.micro} c="dimmed" style={{ flexShrink: 0 }}>{relTime(new Date(r.at).toISOString())}</Text>))}
      {!flat && (
        <Text size={FS.micro} c="dimmed" px="sm" mt="md" lh={1.45}>
          Pinned folders and recent files are kept in this browser, for this server and account.
        </Text>
      )}
    </Box>
  )
}

function FileTable({
  entries, compact, sort, onSort, selected, onSelect, live, active, onOpen, onDownload, onPreview, onRename,
  onDelete, path,
}: {
  entries: FileEntry[]
  compact: boolean
  sort: { key: SortKey; desc: boolean }
  onSort: (k: SortKey) => void
  selected: Set<string>
  onSelect: (s: Set<string>) => void
  live: boolean
  active: string | null
  onOpen: (e: FileEntry) => void
  onDownload: (e: FileEntry) => void
  onPreview: (e: FileEntry) => void
  onRename: (e: FileEntry) => void
  onDelete: (e: FileEntry) => void
  path: string
}) {
  const all = entries.length > 0 && entries.every((e) => selected.has(e.name))
  const head = (label: string, key: SortKey, align?: 'right') => (
    <Table.Th style={{ textAlign: align }} aria-sort={sort.key === key ? (sort.desc ? 'descending' : 'ascending') : undefined}>
      <UnstyledButton className="argus-files-sort" onClick={() => onSort(key)}>
        {label}
        {sort.key === key && (sort.desc ? <IconSortDescending size={12} /> : <IconSortAscending size={12} />)}
      </UnstyledButton>
    </Table.Th>
  )
  return (
    <Table className="argus-files-table" highlightOnHover verticalSpacing={10} horizontalSpacing="md" striped={false}>
      <Table.Thead>
        <Table.Tr>
          {!compact && (
            <Table.Th w={44}>
              <Checkbox size="xs" aria-label="Select all" checked={all}
                indeterminate={!all && selected.size > 0}
                onChange={() => onSelect(all ? new Set() : new Set(entries.map((e) => e.name)))} />
            </Table.Th>
          )}
          {head('Name', 'name')}
          {head('Size', 'size', 'right')}
          {head('Modified', 'mtime')}
          <Table.Th w={compact ? 80 : 170} />
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {entries.map((e) => {
          const full = joinPath(path, e.name)
          const readOnly = !e.dir && e.mode && e.mode[2] !== 'w'
          return (
            <Table.Tr key={e.name} data-selected={selected.has(e.name) || undefined} data-active={active === full || undefined}
              className="argus-files-row" onDoubleClick={() => live && onOpen(e)}>
              {!compact && (
                <Table.Td>
                  <Checkbox size="xs" aria-label={`Select ${e.name}`} checked={selected.has(e.name)}
                    onChange={() => {
                      const next = new Set(selected)
                      if (next.has(e.name)) next.delete(e.name)
                      else next.add(e.name)
                      onSelect(next)
                    }} />
                </Table.Td>
              )}
              <Table.Td>
                <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
                  {e.link ? <IconLink size={16} color="var(--mantine-color-violet-6)" style={{ flexShrink: 0 }} />
                    : e.dir ? <IconFolder size={16} color={INK.brand} style={{ flexShrink: 0 }} />
                      : <IconFile size={16} opacity={0.55} style={{ flexShrink: 0 }} />}
                  <UnstyledButton className="argus-files-name" disabled={!live} onClick={() => onOpen(e)}
                    title={e.dir ? `Open ${e.name}` : `Preview ${e.name}`}>
                    {e.name}{e.dir && !e.link ? '/' : ''}
                  </UnstyledButton>
                  {e.link && <Text size={FS.micro} c="dimmed" truncate>→ {e.link}</Text>}
                  {readOnly && <Badge size="xs" variant="light" color="slate">Read only</Badge>}
                </Group>
              </Table.Td>
              <Table.Td ta="right" c="dimmed" style={{ whiteSpace: 'nowrap' }}>{e.dir ? '' : formatSize(e.size)}</Table.Td>
              <Table.Td c="dimmed" style={{ whiteSpace: 'nowrap' }}>{e.mtime ? relTime(e.mtime) : ''}</Table.Td>
              <Table.Td>
                <Group gap={4} justify="flex-end" wrap="nowrap" className="argus-files-actions">
                  {!e.dir && !compact && looksTextual(e.name) && (
                    <Button size="compact-xs" variant="default" disabled={!live} onClick={() => onPreview(e)}>Preview</Button>
                  )}
                  {!e.dir && (
                    <Tooltip label="Download">
                      <ActionIcon variant="default" disabled={!live} aria-label={`Download ${e.name}`} onClick={() => onDownload(e)}>
                        <IconDownload size={14} />
                      </ActionIcon>
                    </Tooltip>
                  )}
                  <Menu position="bottom-end" withinPortal>
                    <Menu.Target>
                      <ActionIcon variant="default" disabled={!live} aria-label={`More for ${e.name}`}><IconDots size={14} /></ActionIcon>
                    </Menu.Target>
                    <Menu.Dropdown>
                      {e.dir && <Menu.Item leftSection={<IconFolder size={14} />} onClick={() => onOpen(e)}>Open</Menu.Item>}
                      {!e.dir && compact && looksTextual(e.name) && (
                        <Menu.Item leftSection={<IconEye size={14} />} onClick={() => onPreview(e)}>Preview</Menu.Item>
                      )}
                      <Menu.Item leftSection={<IconCopy size={14} />} onClick={() => void navigator.clipboard?.writeText(full)}>
                        Copy path
                      </Menu.Item>
                      <Menu.Item leftSection={<IconPencil size={14} />} onClick={() => onRename(e)}>Rename</Menu.Item>
                      <Menu.Divider />
                      <Menu.Item leftSection={<IconTrash size={14} />} color="rose" onClick={() => onDelete(e)}>Delete</Menu.Item>
                    </Menu.Dropdown>
                  </Menu>
                </Group>
              </Table.Td>
            </Table.Tr>
          )
        })}
      </Table.Tbody>
    </Table>
  )
}

function PhoneList({ entries, onOpen, onMore }: {
  entries: FileEntry[]
  onOpen: (e: FileEntry) => void
  onMore: (e: FileEntry) => void
}) {
  return (
    <Stack gap={0} pb={96}>
      {entries.map((e) => (
        <UnstyledButton key={e.name} className="argus-files-prow" onClick={() => (e.dir ? onOpen(e) : onMore(e))}>
          {e.link ? <IconLink size={20} color="var(--mantine-color-violet-6)" />
            : e.dir ? <IconFolder size={20} color={INK.brand} /> : <IconFile size={20} opacity={0.55} />}
          <Box style={{ flex: 1, minWidth: 0 }}>
            <Text size="sm" fw={500} truncate>{e.name}</Text>
            <Text size={FS.micro} c="dimmed" truncate>
              {e.link ? `→ ${e.link}` : [e.dir ? '' : formatSize(e.size), e.mtime ? relTime(e.mtime) : ''].filter(Boolean).join(' · ')}
            </Text>
          </Box>
          {e.dir ? <IconChevronRight size={16} opacity={0.5} /> : <IconDots size={16} opacity={0.6} />}
        </UnstyledButton>
      ))}
    </Stack>
  )
}

function NameModal({ opened, title, label, confirm, initial, taken, onClose, onSubmit }: {
  opened: boolean
  title: string
  label: string
  confirm: string
  initial: string
  taken: string[]
  onClose: () => void
  onSubmit: (name: string) => Promise<void>
}) {
  const [name, setName] = useState(initial)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (opened) setName(initial)
  }, [opened, initial])
  const problem = validName(name) ?? (taken.includes(name.trim()) ? 'Something here already has that name.' : null)
  const submit = async () => {
    if (problem) return
    setBusy(true)
    try {
      await onSubmit(name.trim())
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={title} centered>
      <Stack gap="md">
        <TextInput data-autofocus label={label} value={name} onChange={(e) => setName(e.currentTarget.value)}
          error={name && problem ? problem : undefined}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void submit()
          }} />
        <Text size={FS.micro} c="dimmed">Recorded in Activity with your name.</Text>
        <Group justify="flex-end" gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} disabled={!!problem} onClick={() => void submit()}>{confirm}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
