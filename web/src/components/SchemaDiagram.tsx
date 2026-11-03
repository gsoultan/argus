import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Group, Loader, Modal, ScrollArea, SegmentedControl, Select, Stack, Text,
  TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { IconArrowsMaximize, IconDownload, IconFocus2, IconMinus, IconPlus, IconSearch, IconTable } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { Collect } from '~/components/DatabaseAdmin'
import {
  BOX_W, HEAD_H, MAX_ROWS, ROW_H, boxHeight, buildErd, columnY, erdColumnsSQL, erdIndexesSQL, erdLinksSQL,
  erdTablesSQL, layout, neighbours, shownTables, type Erd, type Positions,
} from '~/lib/erd'
import { bytes } from '~/components/primitives'

/*
The Diagram view: the tables of one schema and the foreign keys between
them, drawn from the catalog as the person's own role. A big schema opens
on one table and its neighbours; positions the person drags to are kept in
this browser per database and schema. The canvas is plain SVG so it can be
saved as a picture without anything from outside the page.
*/

const q = (n: string) => (/^[a-z_][a-z0-9_]*$/.test(n) ? n : `"${n.replace(/"/g, '""')}"`)
const FOCUS_OVER = 14

function loadPositions(key: string): Positions {
  try {
    const v = JSON.parse(localStorage.getItem(key) ?? '{}') as Positions
    return v && typeof v === 'object' ? v : {}
  } catch {
    return {}
  }
}

function savePositions(key: string, p: Positions): void {
  try {
    localStorage.setItem(key, JSON.stringify(p))
  } catch {
    // blocked storage: positions last for this page
  }
}

export function SchemaDiagram({ databaseId, schemas, collect, narrow, onQuery }: {
  databaseId: string
  schemas: string[]
  collect: Collect
  narrow: boolean
  onQuery: (sql: string, name: string) => void
}) {
  const [schema, setSchema] = useState(() => (schemas.includes('public') ? 'public' : schemas[0] ?? 'public'))
  const [erd, setErd] = useState<Erd | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [focus, setFocus] = useState<string | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const [whole, setWhole] = useState(false)
  const [find, setFind] = useState('')
  const [full, setFull] = useState(false)

  useEffect(() => {
    if (!schemas.includes(schema) && schemas.length) setSchema(schemas.includes('public') ? 'public' : schemas[0]!)
  }, [schemas, schema])

  useEffect(() => {
    let gone = false
    setErd(null)
    setError(null)
    void (async () => {
      const t = await collect(erdTablesSQL(schema), 2000)
      const c = t.error ? t : await collect(erdColumnsSQL(schema), 60000)
      const l = c.error ? c : await collect(erdLinksSQL(schema), 20000)
      if (gone) return
      const err = t.error ?? c.error ?? l.error
      if (err) {
        setError(err.error ?? 'the catalog could not be read')
        return
      }
      const model = buildErd(schema, t.rows, c.rows, l.rows)
      setErd(model)
      // A big schema opens on its most connected table.
      if (model.tables.length > FOCUS_OVER) {
        const deg = new Map<string, number>()
        for (const k of model.links) {
          deg.set(k.from, (deg.get(k.from) ?? 0) + 1)
          deg.set(k.to, (deg.get(k.to) ?? 0) + 1)
        }
        const top = [...model.tables].sort((a, b) => (deg.get(b.id) ?? 0) - (deg.get(a.id) ?? 0))[0]
        setFocus(top?.id ?? null)
        setSelected(top?.id ?? null)
        setWhole(false)
      } else {
        setFocus(null)
        setWhole(true)
        setSelected(null)
      }
    })()
    return () => { gone = true }
  }, [collect, schema])

  const focusOn = (id: string) => {
    setFocus(id)
    setSelected(id)
    setWhole(false)
  }

  const matches = useMemo(() => {
    const n = find.trim().toLowerCase()
    if (!erd || !n) return []
    return erd.tables.filter((t) => t.name.toLowerCase().includes(n)).slice(0, 8)
  }, [erd, find])

  const toolbar = (
    <Group gap="xs" wrap={narrow ? 'wrap' : 'nowrap'} p="xs" style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)' }}>
      <Select size="xs" w={narrow ? 120 : 170} data={schemas} value={schema} onChange={(v) => v && setSchema(v)}
        aria-label="Schema" allowDeselect={false} comboboxProps={{ withinPortal: true }} />
      <Box pos="relative" style={{ flex: narrow ? 1 : undefined }}>
        <TextInput size="xs" w={narrow ? '100%' : 200} placeholder="Find a table" leftSection={<IconSearch size={12} />}
          value={find} onChange={(e) => setFind(e.currentTarget.value)} aria-label="Find a table" />
        {matches.length > 0 && (
          <Stack gap={0} pos="absolute" top={32} left={0} w={240} style={{ zIndex: 20, background: 'var(--color-surface)', border: '1px solid var(--color-line)', borderRadius: 8, boxShadow: 'var(--mantine-shadow-md)' }}>
            {matches.map((t) => (
              <UnstyledButton key={t.id} px="sm" py={6} onClick={() => { focusOn(t.id); setFind('') }}>
                <Text size={FS.meta} ff="monospace">{t.name}</Text>
              </UnstyledButton>
            ))}
          </Stack>
        )}
      </Box>
      {!narrow && erd && (
        <SegmentedControl size="xs" value={whole ? 'all' : 'focus'}
          onChange={(v) => { if (v === 'all') setWhole(true); else if (selected ?? focus) focusOn((selected ?? focus)!) }}
          data={[{ label: 'Whole schema', value: 'all' },
            { label: `${erd.byId.get(selected ?? focus ?? '')?.name ?? 'One table'} + neighbours`, value: 'focus', disabled: !(selected ?? focus) }]} />
      )}
    </Group>
  )

  if (error) {
    return <Box>{toolbar}<Alert m="md" color="rose" variant="light">Could not read the schema: {error}</Alert></Box>
  }
  if (!erd) {
    return <Box>{toolbar}<Group p="md" gap="xs"><Loader size={14} /><Text size={FS.meta} c="dimmed">Reading the catalog…</Text></Group></Box>
  }
  if (erd.tables.length === 0) {
    return <Box>{toolbar}<Text p="md" size={FS.meta} c="dimmed">No tables in {schema} that your access can see.</Text></Box>
  }

  const shownFocus = whole ? null : focus
  const canvas = (h: number | string) => (
    <Canvas key={`${schema}:${shownFocus ?? 'all'}`} erd={erd} focus={shownFocus} selected={selected} onSelect={setSelected}
      storeKey={`argus.erd.${databaseId}.${schema}${shownFocus ? '' : '.all'}`} height={h} name={`${schema}-diagram`} />
  )
  const side = selected && erd.byId.get(selected) ? (
    <TableDetail erd={erd} id={selected} schema={schema} collect={collect} narrow={narrow}
      onFocus={() => focusOn(selected)} onQuery={onQuery} onPick={(id) => setSelected(id)} />
  ) : null

  if (narrow) {
    const list = shownTables(erd, shownFocus).map((id) => erd.byId.get(id)!)
    return (
      <Box>
        {toolbar}
        <Box pos="relative" style={{ borderBottom: '1px solid var(--color-line)' }}>
          {canvas(260)}
          <Button size="compact-xs" variant="default" pos="absolute" right={8} bottom={8} leftSection={<IconArrowsMaximize size={12} />}
            onClick={() => setFull(true)}>Full screen</Button>
        </Box>
        <Modal opened={full} onClose={() => setFull(false)} fullScreen title={`${schema} diagram`} padding={0}>
          {canvas('calc(100dvh - 60px)')}
        </Modal>
        <Group px="sm" pt="sm" gap="xs">
          <SegmentedControl size="xs" value={whole ? 'all' : 'focus'} fullWidth style={{ flex: 1 }}
            onChange={(v) => { if (v === 'all') setWhole(true); else if (selected ?? focus) focusOn((selected ?? focus)!) }}
            data={[{ label: 'All tables', value: 'all' }, { label: 'Neighbours', value: 'focus', disabled: !(selected ?? focus) }]} />
        </Group>
        <Stack gap={6} p="sm">
          {list.map((t) => {
            const fks = t.columns.filter((c) => c.fk).map((c) => c.name)
            const on = t.id === selected
            return (
              <Box key={t.id}>
                <UnstyledButton onClick={() => setSelected(on ? null : t.id)} w="100%" p="sm"
                  style={{ border: on ? '2px solid var(--color-brand)' : '1px solid var(--color-line)', borderRadius: 10, background: 'var(--color-surface)' }}>
                  <Group gap={6} wrap="nowrap">
                    <IconTable size={14} color="var(--mantine-color-dimmed)" />
                    <Text fw={600} size={FS.body} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{t.name}</Text>
                    <Text size={FS.micro} c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap' }}>{rowsText(t.rows)} · {t.columns.length} columns</Text>
                  </Group>
                  <Text size={FS.micro} c="dimmed" ff="monospace" mt={4}>{fks.length ? fks.map((f) => `${f} →`).join(' · ') : 'no foreign keys'}</Text>
                </UnstyledButton>
                {on && side}
              </Box>
            )
          })}
        </Stack>
      </Box>
    )
  }
  return (
    <Box style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      {toolbar}
      <Box style={{ flex: 1, minHeight: 0, display: 'flex' }}>
        <Box style={{ flex: 1, minWidth: 0 }}>{canvas('100%')}</Box>
        {side && (
          <ScrollArea w={300} style={{ borderLeft: '1px solid var(--color-line)', flexShrink: 0 }}>{side}</ScrollArea>
        )}
      </Box>
    </Box>
  )
}

function rowsText(n: number): string {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M rows`
  if (n >= 1e3) return `${Math.round(n / 1e3)}k rows`
  return `${n} rows`
}

function TableDetail({ erd, id, schema, collect, narrow, onFocus, onQuery, onPick }: {
  erd: Erd; id: string; schema: string; collect: Collect; narrow: boolean
  onFocus: () => void; onQuery: (sql: string, name: string) => void; onPick: (id: string) => void
}) {
  const t = erd.byId.get(id)!
  const [indexes, setIndexes] = useState<{ name: string; def: string }[] | null>(null)
  useEffect(() => {
    let gone = false
    setIndexes(null)
    void collect(erdIndexesSQL(schema, t.name), 200).then((r) => {
      if (!gone) setIndexes(r.error ? [] : r.rows.map((x) => ({ name: x[0] ?? '', def: (x[1] ?? '').replace(/^.*? USING /, '') })))
    })
    return () => { gone = true }
  }, [collect, schema, t.name])
  const outgoing = erd.links.filter((l) => l.from === id)
  const incoming = erd.links.filter((l) => l.to === id)
  const head = (s: string) => <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} mt="sm" mb={4} style={{ letterSpacing: '.06em' }}>{s}</Text>
  return (
    <Box p={narrow ? 'sm' : 'md'}>
      {!narrow && (
        <Group gap={6} wrap="nowrap">
          <Text fw={650} size={FS.body} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{t.name}</Text>
        </Group>
      )}
      <Text size={FS.micro} c="dimmed">{rowsText(t.rows)} · {bytes(t.bytes)}{t.kind === 'v' ? ' · view' : t.kind === 'm' ? ' · materialized view' : ''}</Text>
      {head('References')}
      {outgoing.length === 0 && <Text size={FS.micro} c="dimmed">nothing</Text>}
      {outgoing.map((l) => (
        <UnstyledButton key={l.name} display="block" onClick={() => !l.external && onPick(l.to)}>
          <Text size={FS.micro} ff="monospace">{l.fromCols.join(', ')} → {l.external ?? erd.byId.get(l.to)?.name}.{l.toCols.join(', ')}</Text>
        </UnstyledButton>
      ))}
      {head('Referenced by')}
      {incoming.length === 0 && <Text size={FS.micro} c="dimmed">nothing</Text>}
      {incoming.map((l) => (
        <UnstyledButton key={l.name + l.from} display="block" onClick={() => onPick(l.from)}>
          <Text size={FS.micro} ff="monospace">{erd.byId.get(l.from)?.name}.{l.fromCols.join(', ')}</Text>
        </UnstyledButton>
      ))}
      {head('Indexes')}
      {indexes === null ? <Loader size={12} /> : indexes.length === 0 ? <Text size={FS.micro} c="dimmed">none</Text> : indexes.map((x) => (
        <Text key={x.name} size={FS.micro} ff="monospace" style={{ wordBreak: 'break-all' }}>{x.name} <Text span c="dimmed" inherit>{x.def}</Text></Text>
      ))}
      {narrow && (
        <>
          {head('Columns')}
          {t.columns.map((c) => (
            <Group key={c.name} gap={6} wrap="nowrap">
              <Text size={FS.micro} c="orange" fw={700} w={18}>{c.pk ? 'PK' : c.fk ? 'FK' : ''}</Text>
              <Text size={FS.micro} ff="monospace">{c.name}</Text>
              <Text size={FS.micro} c="dimmed" ff="monospace" ml="auto">{c.type}</Text>
            </Group>
          ))}
        </>
      )}
      <Stack gap="xs" mt="md">
        <Button size="xs" onClick={() => onQuery(`SELECT * FROM ${q(schema)}.${q(t.name)} LIMIT 100;`, t.name)}>
          Query {t.name} in a new tab
        </Button>
        <Button size="xs" variant="default" leftSection={<IconFocus2 size={14} />} onClick={onFocus}
          disabled={neighbours(erd, id).parents.length + neighbours(erd, id).children.length === 0}>
          Show only this and neighbours
        </Button>
      </Stack>
    </Box>
  )
}

/* ── The canvas ─────────────────────────────────────────────────────────── */

function Canvas({ erd, focus, selected, onSelect, storeKey, height, name }: {
  erd: Erd; focus: string | null; selected: string | null; onSelect: (id: string | null) => void
  storeKey: string; height: number | string; name: string
}) {
  const shown = useMemo(() => shownTables(erd, focus), [erd, focus])
  const [pos, setPos] = useState<Positions>(() => ({ ...layout(erd, shown, focus), ...pick(loadPositions(storeKey), shown) }))
  const [view, setView] = useState({ x: 0, y: 0, k: 1 })
  const box = useRef<HTMLDivElement | null>(null)
  const svg = useRef<SVGSVGElement | null>(null)
  const drag = useRef<{ kind: 'pan' | 'move'; id?: string; sx: number; sy: number; ox: number; oy: number } | null>(null)
  const pointers = useRef(new Map<number, { x: number; y: number }>())
  const pinch = useRef<{ d: number; k: number } | null>(null)

  const fit = useCallback(() => {
    const el = box.current
    if (!el) return
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
    for (const id of shown) {
      const p = pos[id]
      const t = erd.byId.get(id)
      if (!p || !t) continue
      minX = Math.min(minX, p.x)
      minY = Math.min(minY, p.y)
      // Room for a bracket line on the right.
      maxX = Math.max(maxX, p.x + BOX_W + 70)
      maxY = Math.max(maxY, p.y + boxHeight(t))
    }
    if (!Number.isFinite(minX)) return
    const w = el.clientWidth
    const h = el.clientHeight
    const k = Math.min(1.2, Math.max(0.2, Math.min((w - 40) / (maxX - minX), (h - 40) / (maxY - minY))))
    setView({ k, x: (w - (maxX - minX) * k) / 2 - minX * k, y: (h - (maxY - minY) * k) / 2 - minY * k })
  }, [erd, pos, shown])
  // Fit once the box has a size.
  const fitted = useRef(false)
  useEffect(() => {
    if (fitted.current || !box.current) return
    const ro = new ResizeObserver(() => {
      if (!fitted.current && box.current && box.current.clientWidth > 0) {
        fitted.current = true
        fit()
      }
    })
    ro.observe(box.current)
    return () => ro.disconnect()
  }, [fit])

  const zoom = (f: number, cx?: number, cy?: number) => setView((v) => {
    const el = box.current
    const px = cx ?? (el ? el.clientWidth / 2 : 0)
    const py = cy ?? (el ? el.clientHeight / 2 : 0)
    const k = Math.min(2.5, Math.max(0.15, v.k * f))
    return { k, x: px - ((px - v.x) * k) / v.k, y: py - ((py - v.y) * k) / v.k }
  })

  const local = (e: React.PointerEvent | React.WheelEvent) => {
    const r = box.current!.getBoundingClientRect()
    return { x: e.clientX - r.left, y: e.clientY - r.top }
  }

  const onDown = (e: React.PointerEvent, id?: string) => {
    e.stopPropagation()
    ;(e.currentTarget as Element).setPointerCapture?.(e.pointerId)
    pointers.current.set(e.pointerId, local(e))
    if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      pinch.current = { d: Math.hypot(a!.x - b!.x, a!.y - b!.y), k: view.k }
      drag.current = null
      return
    }
    const p = id ? pos[id]! : { x: view.x, y: view.y }
    drag.current = { kind: id ? 'move' : 'pan', id, sx: e.clientX, sy: e.clientY, ox: p.x, oy: p.y }
    if (id) onSelect(id)
  }
  const onMove = (e: React.PointerEvent) => {
    if (pointers.current.has(e.pointerId)) pointers.current.set(e.pointerId, local(e))
    if (pinch.current && pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      const d = Math.hypot(a!.x - b!.x, a!.y - b!.y)
      const target = Math.min(2.5, Math.max(0.15, pinch.current.k * (d / pinch.current.d)))
      zoom(target / view.k, (a!.x + b!.x) / 2, (a!.y + b!.y) / 2)
      return
    }
    const d = drag.current
    if (!d) return
    const dx = e.clientX - d.sx
    const dy = e.clientY - d.sy
    if (d.kind === 'pan') setView((v) => ({ ...v, x: d.ox + dx, y: d.oy + dy }))
    else if (d.id) setPos((p) => ({ ...p, [d.id!]: { x: d.ox + dx / view.k, y: d.oy + dy / view.k } }))
  }
  const onUp = (e: React.PointerEvent) => {
    pointers.current.delete(e.pointerId)
    if (pointers.current.size < 2) pinch.current = null
    const d = drag.current
    drag.current = null
    if (d?.kind === 'move') savePositions(storeKey, pick(pos, shown))
    if (d?.kind === 'pan' && Math.abs(e.clientX - d.sx) + Math.abs(e.clientY - d.sy) < 4) onSelect(null)
  }
  const onWheel = (e: React.WheelEvent) => {
    if (e.ctrlKey || e.metaKey) {
      const p = local(e)
      zoom(e.deltaY < 0 ? 1.1 : 1 / 1.1, p.x, p.y)
    } else {
      setView((v) => ({ ...v, x: v.x - e.deltaX, y: v.y - e.deltaY }))
    }
  }

  const exportPng = () => {
    const el = svg.current
    if (!el) return
    const clone = el.cloneNode(true) as SVGSVGElement
    // Resolve the theme's colours into the copy, so the picture has them.
    const src = el.querySelectorAll('*')
    clone.querySelectorAll('*').forEach((node, i) => {
      const cs = getComputedStyle(src[i] as Element)
      for (const p of ['fill', 'stroke', 'font-family', 'font-size', 'font-weight', 'opacity']) {
        ;(node as SVGElement).style.setProperty(p, cs.getPropertyValue(p))
      }
    })
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
    for (const id of shown) {
      const p = pos[id]!
      minX = Math.min(minX, p.x); minY = Math.min(minY, p.y)
      maxX = Math.max(maxX, p.x + BOX_W); maxY = Math.max(maxY, p.y + boxHeight(erd.byId.get(id)!))
    }
    const pad = 24
    const w = maxX - minX + pad * 2
    const h = maxY - minY + pad * 2
    clone.setAttribute('width', String(w))
    clone.setAttribute('height', String(h))
    clone.setAttribute('viewBox', `0 0 ${w} ${h}`)
    clone.querySelector('g[data-view]')?.setAttribute('transform', `translate(${pad - minX} ${pad - minY})`)
    const bg = getComputedStyle(box.current!).backgroundColor
    const img = new Image()
    const url = URL.createObjectURL(new Blob([new XMLSerializer().serializeToString(clone)], { type: 'image/svg+xml' }))
    img.onload = () => {
      const c = document.createElement('canvas')
      c.width = w * 2
      c.height = h * 2
      const ctx = c.getContext('2d')!
      ctx.scale(2, 2)
      ctx.fillStyle = bg || '#fff'
      ctx.fillRect(0, 0, w, h)
      ctx.drawImage(img, 0, 0)
      URL.revokeObjectURL(url)
      const a = document.createElement('a')
      a.href = c.toDataURL('image/png')
      a.download = `${name}.png`
      a.click()
    }
    img.src = url
  }

  const related = selected ? new Set([selected, ...neighbours(erd, selected).parents, ...neighbours(erd, selected).children]) : null
  return (
    <Box ref={box} pos="relative" h={height}
      style={{ overflow: 'hidden', touchAction: 'none', cursor: 'grab', backgroundColor: 'var(--color-void)',
        backgroundImage: 'radial-gradient(var(--color-line) 1px, transparent 1px)', backgroundSize: '18px 18px' }}
      onPointerDown={(e) => onDown(e)} onPointerMove={onMove} onPointerUp={onUp} onPointerCancel={onUp} onWheel={onWheel}>
      <svg ref={svg} width="100%" height="100%" style={{ display: 'block', fontFamily: 'var(--font-mono)' }} role="img"
        aria-label="Tables and the foreign keys between them">
        <g data-view transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
          {erd.links.map((l) => {
            const from = erd.byId.get(l.from)
            const to = erd.byId.get(l.to)
            const a = pos[l.from]
            const b = pos[l.to]
            if (!from || !to || !a || !b || !shown.includes(l.from) || !shown.includes(l.to)) return null
            const ay = a.y + columnY(from, l.fromCols[0])
            const by = b.y + columnY(to, l.toCols[0])
            // Side by side: leave one side and enter the facing one. One above
            // the other: leave and enter on the right, as a bracket, so the
            // line never runs under either box.
            const stacked = Math.abs(b.x - a.x) < BOX_W
            const right = b.x >= a.x
            const ax = stacked || right ? a.x + BOX_W : a.x
            const bx = stacked || !right ? b.x + BOX_W : b.x
            const self = l.from === l.to
            const reach = 40 + Math.min(60, Math.abs(by - ay) / 6)
            const dx = stacked ? reach : Math.max(40, Math.abs(bx - ax) / 2) * (right ? 1 : -1)
            const d = self
              ? `M${a.x + BOX_W} ${ay} c40 0 40 ${by - ay} 0 ${by - ay}`
              : stacked
                ? `M${ax} ${ay} C${ax + dx} ${ay} ${bx + dx} ${by} ${bx} ${by}`
                : `M${ax} ${ay} C${ax + dx} ${ay} ${bx - dx} ${by} ${bx} ${by}`
            const hot = selected && (l.from === selected || l.to === selected)
            return (
              <g key={l.name + l.from}>
                <path d={d} fill="none" stroke={hot ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)'} strokeWidth={hot ? 2 : 1.3} opacity={hot || !selected ? 0.9 : 0.35} />
                <circle cx={self ? a.x + BOX_W : bx} cy={by} r={3.5} fill={hot ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)'} />
              </g>
            )
          })}
          {shown.map((id) => {
            const t = erd.byId.get(id)!
            const p = pos[id]
            if (!p) return null
            const h = boxHeight(t)
            const on = id === selected
            const faded = related && !related.has(id)
            return (
              <g key={id} transform={`translate(${p.x} ${p.y})`} opacity={faded ? 0.45 : 1}
                onPointerDown={(e) => onDown(e, id)} style={{ cursor: 'move' }}>
                <rect width={BOX_W} height={h} rx={8} fill="var(--color-surface)" stroke={on ? 'var(--color-brand)' : 'var(--color-line-strong, var(--color-line))'} strokeWidth={on ? 2 : 1} />
                <path d={`M0 8 a8 8 0 0 1 8 -8 h${BOX_W - 16} a8 8 0 0 1 8 8 v${HEAD_H - 8} h-${BOX_W} z`} fill="var(--color-raised)" />
                <text x={10} y={HEAD_H / 2 + 4} fontSize={12} fontWeight={650} fill="var(--mantine-color-text)" style={{ fontFamily: 'var(--font-sans, inherit)' }}>
                  {clip(t.name, 22)}
                </text>
                <text x={BOX_W - 10} y={HEAD_H / 2 + 4} fontSize={10} textAnchor="end" fill="var(--mantine-color-dimmed)">{rowsText(t.rows).replace(' rows', '')}</text>
                {t.columns.slice(0, MAX_ROWS).map((c, i) => (
                  <g key={c.name} transform={`translate(0 ${HEAD_H + 4 + i * ROW_H})`}>
                    <text x={10} y={13} fontSize={9} fontWeight={700} fill="var(--mantine-color-orange-7)">{c.pk ? 'PK' : c.fk ? 'FK' : ''}</text>
                    <text x={32} y={13} fontSize={11} fill="var(--mantine-color-text)">{clip(c.name, 16)}</text>
                    <text x={BOX_W - 10} y={13} fontSize={10} textAnchor="end" fill="var(--mantine-color-dimmed)">{clip(c.type, 12)}</text>
                  </g>
                ))}
                {t.columns.length > MAX_ROWS && (
                  <text x={32} y={HEAD_H + 4 + MAX_ROWS * ROW_H + 13} fontSize={10} fill="var(--mantine-color-dimmed)">
                    … {t.columns.length - MAX_ROWS} more
                  </text>
                )}
              </g>
            )
          })}
        </g>
      </svg>
      <Group gap={4} pos="absolute" top={8} right={8} onPointerDown={(e) => e.stopPropagation()}>
        <Tooltip label="Zoom out"><ActionIcon variant="default" size="sm" onClick={() => zoom(1 / 1.2)} aria-label="Zoom out"><IconMinus size={14} /></ActionIcon></Tooltip>
        <Badge variant="default" radius="sm" size="lg" fw={500} style={{ minWidth: 52 }}>{Math.round(view.k * 100)}%</Badge>
        <Tooltip label="Zoom in"><ActionIcon variant="default" size="sm" onClick={() => zoom(1.2)} aria-label="Zoom in"><IconPlus size={14} /></ActionIcon></Tooltip>
        <Button size="compact-xs" variant="default" onClick={fit}>Fit</Button>
        <Button size="compact-xs" variant="default" onClick={() => { const p = layout(erd, shown, focus); setPos(p); savePositions(storeKey, {}); requestAnimationFrame(fit) }}>Tidy</Button>
        <Tooltip label="Save the diagram as a picture"><ActionIcon variant="default" size="sm" onClick={exportPng} aria-label="Export PNG"><IconDownload size={14} /></ActionIcon></Tooltip>
      </Group>
    </Box>
  )
}

function pick(p: Positions, ids: string[]): Positions {
  const out: Positions = {}
  for (const id of ids) {
    const v = p[id]
    if (v && Number.isFinite(v.x) && Number.isFinite(v.y)) out[id] = v
  }
  return out
}

function clip(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s
}
