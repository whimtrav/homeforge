<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS, deviceIcon } from '$lib/api'
  import type { Entity, WSMessage } from '$lib/api'

  type Dev = { key: string; name: string; room: string; icon: string; on: boolean; det: string; ents: Entity[] }

  let entities = $state<Entity[]>([])
  let positions = $state<Record<string, { x: number; y: number }>>({})
  let connected = $state(false)
  let savedMsg = $state('')
  let drag = $state<{ key: string; mode: 'new' | 'move' } | null>(null)
  let ghost = $state<{ x: number; y: number; label: string; icon: string } | null>(null)
  let svgEl: SVGSVGElement
  let trayEl: HTMLElement

  // HVAC vents (supply/return registers) — stored separately from device pins.
  type Vent = { id: string; type: 'supply' | 'return'; x: number; y: number; m: string }
  let vents = $state<Vent[]>([])
  let ventDrag = $state<{ id: string } | null>(null)

  // Rooms — the floor plan we built (1 ft = 24 px, origin = back-left, px 40,60)
  const REF = [
    { n: 'Bedroom 1', x: 40, y: 60, w: 294, h: 256, c: '#dbeafe' },
    { n: 'Closet', x: 40, y: 186, w: 58, h: 130, c: '#fef9c3', s: 1 },
    { n: 'Bathroom', x: 334, y: 60, w: 192, h: 170, c: '#dcfce7' },
    { n: 'Closet', x: 526, y: 60, w: 78, h: 170, c: '#fef9c3', s: 1 },
    { n: 'Up-Hall', x: 334, y: 230, w: 270, h: 86, c: '#f3e8ff' },
    { n: 'Bedroom 2', x: 604, y: 60, w: 260, h: 256, c: '#dbeafe' },
    { n: 'Family Room', x: 40, y: 316, w: 396, h: 338, c: '#ffe4e6' },
    { n: 'Dining Room', x: 436, y: 316, w: 168, h: 338, c: '#fff7ed' },
    { n: 'Kitchen', x: 604, y: 316, w: 260, h: 338, c: '#e0f2fe' },
    { n: 'Closet', x: 604, y: 316, w: 82, h: 68, c: '#fef9c3', s: 1 },
    { n: 'Stairs', x: 686, y: 316, w: 120, h: 68, c: '#d1d5db', s: 1 },
    { n: 'Door', x: 806, y: 316, w: 56, h: 122, c: '#9ca3af', s: 1 },
    { n: 'Garage (attached)', x: 864, y: 60, w: 312, h: 594, c: '#e5e7eb' },
  ]

  function devKey(e: Entity): string {
    if (e.attributes?.device) return e.attributes.device
    let s = e.id.split('.')[1] || e.id
    s = s.replace(/_(relay|light|fan|switch|presence|touchpad|button|neolight|uptime)$/, '')
    return s.replace(/_/g, '-')
  }
  const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)
  const friendly = (k: string) => k.split(/[-_]/).map(cap).join(' ')
  function roomOf(k: string): string {
    if (k.startsWith('elyse')) return 'Bedroom 2'
    if (k.startsWith('mainbed') || k.startsWith('masbed')) return 'Bedroom 1'
    if (k.startsWith('family')) return 'Family Room'
    if (k.startsWith('hall')) return 'Hallway'
    if (k.startsWith('dining')) return 'Dining Room'
    return 'Other'
  }
  function isOnDev(ents: Entity[]): boolean {
    for (const e of ents) {
      if (e.domain === 'switch' && /(_light|_relay)$/.test(e.id) && e.state === 'on') return true
      if (e.domain === 'number' && /_fan$/.test(e.id) && Number(e.state) > 0) return true
    }
    return false
  }
  function detail(ents: Entity[]): string {
    const bits: string[] = []
    for (const e of ents) {
      if (/_fan$/.test(e.id)) bits.push('Fan ' + e.state)
      else if (/temperature$/.test(e.id)) bits.push(e.state + '°C')
      else if (/eco2$/.test(e.id)) bits.push(e.state + 'ppm')
    }
    if (ents.some((e) => e.domain === 'switch' && /(_light|_relay)$/.test(e.id) && e.state === 'on')) bits.unshift('Light on')
    return bits.slice(0, 3).join(' · ')
  }

  let devices = $derived.by(() => {
    const m: Record<string, Entity[]> = {}
    for (const e of entities) {
      const k = devKey(e)
      ;(m[k] ||= []).push(e)
    }
    return Object.keys(m).map(
      (k): Dev => ({ key: k, name: friendly(k), room: roomOf(k), icon: deviceIcon(k, m[k]), on: isOnDev(m[k]), det: detail(m[k]), ents: m[k] })
    )
  })
  let placed = $derived(devices.filter((d) => positions[d.key]))
  let unplaced = $derived(devices.filter((d) => !positions[d.key]))
  const order = ['Bedroom 1', 'Bedroom 2', 'Family Room', 'Hallway', 'Dining Room', 'Other']
  let trayGroups = $derived.by(() => {
    const g: Record<string, Dev[]> = {}
    for (const d of unplaced) (g[d.room] ||= []).push(d)
    return order.filter((r) => g[r]).map((r) => [r, g[r].sort((a, b) => a.name.localeCompare(b.name))] as [string, Dev[]])
  })

  onMount(() => {
    fetch('/api/floorplan')
      .then((r) => r.json())
      .then((p) => (positions = p || {}))
      .catch(() => {})
    fetch('/api/floorplan/vents')
      .then((r) => r.json())
      .then((v) => (vents = Array.isArray(v) ? v : []))
      .catch(() => {})
    return connectWS((msg: WSMessage) => {
      connected = true
      if (msg.type === 'snapshot' && msg.entities) entities = msg.entities
      else if (msg.type === 'state_changed' && msg.entity) {
        const i = entities.findIndex((e) => e.id === msg.entity!.id)
        if (i >= 0) entities[i] = msg.entity
        else entities.push(msg.entity)
        entities = [...entities]
      }
    })
  })

  function toSvg(cx: number, cy: number) {
    const pt = svgEl.createSVGPoint()
    pt.x = cx
    pt.y = cy
    const q = pt.matrixTransform(svgEl.getScreenCTM()!.inverse())
    return { x: q.x, y: q.y }
  }
  const clamp = (v: number, a: number, b: number) => Math.max(a, Math.min(b, v))

  function startNew(e: PointerEvent, d: Dev) {
    e.preventDefault()
    drag = { key: d.key, mode: 'new' }
    ghost = { x: e.clientX, y: e.clientY, label: d.name, icon: d.icon }
  }
  function startMove(e: PointerEvent, key: string) {
    e.preventDefault()
    drag = { key, mode: 'move' }
  }
  function onMove(e: PointerEvent) {
    if (ventDrag) {
      const s = toSvg(e.clientX, e.clientY)
      vents = vents.map((v) => (v.id === ventDrag!.id ? { ...v, x: clamp(s.x, 20, 1180), y: clamp(s.y, 74, 700) } : v))
      return
    }
    if (!drag) return
    if (drag.mode === 'new') {
      if (ghost) ghost = { ...ghost, x: e.clientX, y: e.clientY }
    } else {
      const s = toSvg(e.clientX, e.clientY)
      positions = { ...positions, [drag.key]: { x: clamp(s.x, 20, 1180), y: clamp(s.y, 70, 690) } }
    }
  }
  function onUp(e: PointerEvent) {
    if (ventDrag) {
      saveVents()
      ventDrag = null
      return
    }
    if (!drag) return
    const tr = trayEl.getBoundingClientRect()
    const sv = svgEl.getBoundingClientRect()
    const inTray = e.clientX >= tr.left && e.clientX <= tr.right && e.clientY >= tr.top && e.clientY <= tr.bottom
    const inSvg = e.clientX >= sv.left && e.clientX <= sv.right && e.clientY >= sv.top && e.clientY <= sv.bottom
    if (inTray) {
      const p = { ...positions }
      delete p[drag.key]
      positions = p
    } else if (inSvg) {
      const s = toSvg(e.clientX, e.clientY)
      positions = { ...positions, [drag.key]: { x: clamp(s.x, 20, 1180), y: clamp(s.y, 70, 690) } }
    }
    save()
    drag = null
    ghost = null
  }

  let saveT: any
  function save() {
    savedMsg = 'saving…'
    clearTimeout(saveT)
    saveT = setTimeout(async () => {
      try {
        await fetch('/api/floorplan', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(positions) })
        savedMsg = '✓ saved'
      } catch {
        savedMsg = 'save failed'
      }
    }, 350)
  }
  function clearAll() {
    if (confirm('Remove all pins? Devices go back to the tray.')) {
      positions = {}
      save()
    }
  }

  // ── HVAC vents ────────────────────────────────────────────────────────────
  let ventSaveT: any
  function saveVents() {
    savedMsg = 'saving…'
    clearTimeout(ventSaveT)
    ventSaveT = setTimeout(async () => {
      try {
        await fetch('/api/floorplan/vents', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(vents) })
        savedMsg = '✓ saved'
      } catch {
        savedMsg = 'save failed'
      }
    }, 350)
  }
  function addVent(type: 'supply' | 'return') {
    const id = type[0] + '-' + Date.now().toString(36) + Math.floor(Math.random() * 1e4).toString(36)
    // stagger new ones near the top-left so they don't stack exactly
    const n = vents.length
    vents = [...vents, { id, type, x: 70 + (n % 8) * 30, y: 90 + Math.floor(n / 8) * 46, m: '' }]
    saveVents()
  }
  function startVentMove(e: PointerEvent, id: string) {
    e.preventDefault()
    e.stopPropagation()
    ventDrag = { id }
  }
  function setVentM(id: string, m: string) {
    vents = vents.map((v) => (v.id === id ? { ...v, m } : v))
    saveVents()
  }
  function delVent(id: string) {
    vents = vents.filter((v) => v.id !== id)
    saveVents()
  }
  const pinColor = (d: Dev) => (d.on ? 'var(--accent)' : '#8b93a1')
  const labW = (name: string) => Math.max(46, name.length * 6.4)
</script>

<svelte:window onpointermove={onMove} onpointerup={onUp} />

<div class="fp-wrap">
  <aside class="fp-tray" bind:this={trayEl}>
    <div class="fp-hint"><b>Drag a device</b> onto the map to where it sits in the room. Drag a pin back here to remove it. Saves automatically.</div>
    {#each trayGroups as [room, ds] (room)}
      <div class="fp-roomh">{room}</div>
      {#each ds as d (d.key)}
        <div class="fp-chip" onpointerdown={(e) => startNew(e, d)}>
          <span class="fp-ic">{d.icon}</span>
          <div><div class="fp-nm">{d.name}</div><div class="fp-rm">{d.room}</div></div>
        </div>
      {/each}
    {/each}
    {#if unplaced.length === 0}<div class="fp-done">✓ All devices placed</div>{/if}
  </aside>

  <div class="fp-map">
    <div class="fp-bar">
      <span class="fp-conn"><span class="fp-dot" class:live={connected}></span>{connected ? devices.length + ' devices live' : 'connecting…'}</span>
      <span class="fp-saved">{savedMsg}</span>
      <button class="fp-add sup" onclick={() => addVent('supply')}>＋ Supply</button>
      <button class="fp-add ret" onclick={() => addVent('return')}>＋ Return</button>
      <button class="fp-clear" onclick={clearAll}>Clear pins</button>
    </div>
    <svg bind:this={svgEl} viewBox="0 0 1200 720" class="fp-svg">
      {#each REF as r}
        <rect x={r.x} y={r.y} width={r.w} height={r.h} fill={r.c} stroke="#c9ccd2" stroke-width="1.5"></rect>
        <text x={r.x + r.w / 2} y={r.y + (r.s ? 15 : 17)} fill="#7b818c" font-size={r.s ? 9 : 12} font-weight="600" text-anchor="middle" opacity="0.85">{r.n}</text>
      {/each}
      <text x="330" y="18" fill="#9aa0aa" font-size="12" font-weight="600" text-anchor="middle">[ BACK OF HOUSE ]</text>
      <text x="240" y="712" fill="#9aa0aa" font-size="12" font-weight="600" text-anchor="middle">[ FRONT - toward Avenue B ]</text>
      {#each placed as d (d.key)}
        {@const p = positions[d.key]}
        <g class="fp-pin" transform={`translate(${p.x},${p.y})`} onpointerdown={(e) => startMove(e, d.key)}>
          {#if d.on}<circle r="20" fill="none" stroke={pinColor(d)} stroke-width="2" opacity="0.35"></circle>{/if}
          <circle r="15" fill="#1c1f26" stroke={pinColor(d)} stroke-width="2.5"></circle>
          <text y="5" font-size="15" text-anchor="middle">{d.icon}</text>
          {#if d.det}
            <rect x={-labW(d.det) / 2} y="18" width={labW(d.det)} height="13" rx="4" fill="#1c1f26" opacity="0.9"></rect>
            <text y="27" font-size="8" text-anchor="middle" fill={pinColor(d)}>{d.det}</text>
          {/if}
          <title>{d.name}{d.det ? ' — ' + d.det : ''}</title>
        </g>
      {/each}
      {#each vents as v (v.id)}
        <foreignObject x={v.x - 27} y={v.y - 21} width="54" height="42" style="overflow: visible">
          <div class="vent {v.type}" xmlns="http://www.w3.org/1999/xhtml" onpointerdown={(e) => startVentMove(e, v.id)}>
            <button class="vent-x" onpointerdown={(e) => { e.stopPropagation(); delVent(v.id) }} title="remove vent">×</button>
            <div class="vent-hd">{v.type === 'supply' ? '⬇ SUP' : '⬆ RET'}</div>
            <input
              class="vent-m"
              value={v.m}
              placeholder="—"
              onpointerdown={(e) => e.stopPropagation()}
              oninput={(e) => setVentM(v.id, (e.currentTarget as HTMLInputElement).value)}
            />
          </div>
        </foreignObject>
      {/each}
    </svg>
  </div>
</div>

{#if ghost}
  <div class="fp-ghost" style="left:{ghost.x + 12}px; top:{ghost.y + 12}px">{ghost.icon} {ghost.label}</div>
{/if}

<style>
  .fp-wrap { display: flex; gap: 14px; height: calc(100vh - 118px); }
  .fp-tray { width: 240px; flex-shrink: 0; overflow-y: auto; background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 10px; }
  .fp-hint { font-size: 11.5px; color: var(--text-muted); line-height: 1.5; margin: 2px 4px 10px; }
  .fp-roomh { font-size: 11px; letter-spacing: 0.6px; text-transform: uppercase; color: var(--text-muted); opacity: 0.75; margin: 12px 4px 6px; font-weight: 600; }
  .fp-chip { display: flex; align-items: center; gap: 9px; background: var(--surface-2); border: 1px solid var(--border); border-radius: 9px; padding: 8px 10px; margin: 6px 0; cursor: grab; user-select: none; touch-action: none; }
  .fp-chip:hover { border-color: var(--accent); }
  .fp-ic { font-size: 16px; width: 20px; text-align: center; }
  .fp-nm { font-size: 12.5px; font-weight: 600; color: var(--text); }
  .fp-rm { font-size: 10.5px; color: var(--text-muted); }
  .fp-done { font-size: 12px; color: var(--success); margin: 10px 4px; }
  .fp-map { flex: 1; display: flex; flex-direction: column; min-width: 0; }
  .fp-bar { display: flex; align-items: center; gap: 12px; margin-bottom: 8px; font-size: 12px; color: var(--text-muted); }
  .fp-conn { display: flex; align-items: center; gap: 6px; }
  .fp-dot { width: 8px; height: 8px; border-radius: 50%; background: var(--text-muted); }
  .fp-dot.live { background: var(--success); box-shadow: 0 0 8px var(--success); }
  .fp-add { margin-left: 4px; border: 1px solid var(--border); border-radius: 7px; padding: 6px 11px; font-size: 12px; font-weight: 700; cursor: pointer; }
  .fp-add.sup { background: #dbeafe; color: #1e3a8a; border-color: #3b82f6; }
  .fp-add.ret { background: #fef3c7; color: #92400e; border-color: #f59e0b; }
  .fp-add:hover { filter: brightness(0.96); }
  .fp-clear { margin-left: auto; background: var(--surface-2); color: var(--text); border: 1px solid var(--border); border-radius: 7px; padding: 6px 11px; font-size: 12px; cursor: pointer; }
  .fp-clear:hover { border-color: var(--accent); }
  /* HVAC vents */
  .vent { position: relative; width: 54px; box-sizing: border-box; border-radius: 7px; border: 1.5px solid; padding: 2px 3px 3px; text-align: center; cursor: grab; user-select: none; touch-action: none; box-shadow: 0 2px 6px rgba(0, 0, 0, 0.3); font-family: Inter, system-ui, sans-serif; }
  .vent:active { cursor: grabbing; }
  .vent.supply { background: #dbeafe; border-color: #3b82f6; color: #1e3a8a; }
  .vent.return { background: #fef3c7; border-color: #f59e0b; color: #92400e; }
  .vent-hd { font-size: 8px; font-weight: 800; letter-spacing: 0.3px; line-height: 1.15; }
  .vent-m { width: 100%; box-sizing: border-box; border: none; border-radius: 4px; text-align: center; font-size: 11px; font-weight: 700; padding: 1px 0; margin-top: 1px; background: rgba(255, 255, 255, 0.9); color: #111; }
  .vent-m:focus { outline: 2px solid currentColor; }
  .vent-x { position: absolute; top: -7px; right: -7px; width: 15px; height: 15px; border-radius: 50%; border: none; background: #ef4444; color: #fff; font-size: 11px; line-height: 15px; text-align: center; cursor: pointer; padding: 0; }
  .fp-svg { background: #fbfbfc; border-radius: 10px; flex: 1; min-height: 0; width: 100%; touch-action: none; box-shadow: 0 4px 20px rgba(0, 0, 0, 0.25); }
  .fp-pin { cursor: grab; }
  .fp-pin:active { cursor: grabbing; }
  .fp-ghost { position: fixed; pointer-events: none; z-index: 50; background: var(--surface-2); border: 1px solid var(--accent); border-radius: 9px; padding: 6px 10px; font-size: 12.5px; color: var(--text); display: flex; gap: 8px; box-shadow: 0 6px 20px rgba(0, 0, 0, 0.4); }
</style>
