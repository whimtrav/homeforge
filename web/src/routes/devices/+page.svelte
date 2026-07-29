<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS, isOn, callService, deviceIcon } from '$lib/api'
  import type { Entity, WSMessage } from '$lib/api'

  let entities = $state<Map<string, Entity>>(new Map())
  let pins = $state<Record<string, { x: number; y: number }>>({})
  let connected = $state(false)
  let search = $state('')
  let openKey = $state<string | null>(null)   // device open in the modal

  onMount(() => {
    fetch('/api/floorplan').then(r => r.json()).then(d => { pins = d || {} }).catch(() => {})
    return connectWS((msg: WSMessage) => {
      connected = true
      if (msg.type === 'snapshot' && msg.entities) {
        const m = new Map<string, Entity>()
        for (const e of msg.entities) m.set(e.id, e)
        entities = m
      } else if (msg.type === 'state_changed' && msg.entity) {
        const m = new Map(entities)
        m.set(msg.entity.id, msg.entity)
        entities = m
      }
    })
  })

  // Rooms = the non-structural rectangles from the floor plan (viewBox 1200x720).
  // A device's room = which rectangle its floor-plan pin sits in; unplaced -> Uncategorized.
  const ROOMS = [
    { n: 'Bedroom 1', x: 40, y: 60, w: 294, h: 256 },
    { n: 'Bathroom', x: 334, y: 60, w: 192, h: 170 },
    { n: 'Up-Hall', x: 334, y: 230, w: 270, h: 86 },
    { n: 'Bedroom 2', x: 604, y: 60, w: 260, h: 256 },
    { n: 'Family Room', x: 40, y: 316, w: 396, h: 338 },
    { n: 'Dining Room', x: 436, y: 316, w: 168, h: 338 },
    { n: 'Kitchen', x: 604, y: 316, w: 260, h: 338 },
    { n: 'Garage', x: 864, y: 60, w: 312, h: 594 },
  ]
  const ROOM_ORDER = [...ROOMS.map(r => r.n), 'Uncategorized']

  type Dev = { key: string; name: string; entities: Entity[] }

  // Group key: keep all of a physical device's entities together. WiZ/WLED group by
  // mac/host; LiquidFW/Tasmota/Z2M by their device tag; orphan diagnostic sensors
  // (uptime/rssi/signal that lack a device tag) fold into their parent by id-prefix.
  function groupKey(e: Entity): string {
    const a = e.attributes as any
    if (a.wiz_mac) return 'wiz:' + a.wiz_mac
    if (a.wled_host) return 'wled:' + a.wled_host
    if (a.device) return a.device
    if (a.z2m_topic) return a.z2m_topic
    if (a.tasmota_topic) return a.tasmota_topic
    const base = e.id.split('.').slice(1).join('.').replace(/_(uptime|rssi|signal)$/, '')
    return base.replace(/_/g, '-')
  }

  const SUFFIX = /_(uptime|rssi|signal|brightness|temp|temperature|humidity|effect|r|g|b|color_temp)$/

  let devices = $derived.by(() => {
    const map = new Map<string, Entity[]>()
    for (const e of entities.values()) {
      const k = groupKey(e)
      const l = map.get(k) ?? []
      l.push(e)
      map.set(k, l)
    }
    const out: Dev[] = []
    for (const [key, list] of map) {
      let name = ''
      for (const e of list) { const d = (e.attributes as any).device; if (d) { name = d; break } }
      if (!name) {
        const slugs = list.map(e => e.id.split('.').slice(1).join('.'))
        const base = slugs.slice().sort((a, b) => a.length - b.length)[0]
        name = base.replace(SUFFIX, '').replace(/_/g, '-')
      }
      out.push({ key, name, entities: list })
    }
    return out
  })

  function pinRoom(key: string): string | null {
    const p = pins[key]
    if (!p) return null
    for (const r of ROOMS) if (p.x >= r.x && p.x < r.x + r.w && p.y >= r.y && p.y < r.y + r.h) return r.n
    return null
  }
  function groupEntity(d: Dev): Entity | undefined {
    return d.entities.find(e => ((e.attributes as any) || {}).group)
  }
  function roomOf(dev: Dev): string {
    const direct = pinRoom(dev.name)
    if (direct) return direct
    // A group inherits the room of its first placed member.
    const g = groupEntity(dev)
    if (g) {
      const members = (((g.attributes as any).members) || []) as string[]
      for (const mid of members) {
        const key = mid.split('.').slice(1).join('.').replace(/_/g, '-')
        const r = pinRoom(key)
        if (r) return r
      }
    }
    return 'Uncategorized'
  }

  let sections = $derived.by(() => {
    const q = search.toLowerCase()
    const map = new Map<string, Dev[]>()
    for (const n of ROOM_ORDER) map.set(n, [])
    for (const dev of devices) {
      if (q && !dev.name.toLowerCase().includes(q) && !dev.entities.some(e => e.id.toLowerCase().includes(q))) continue
      map.get(roomOf(dev))!.push(dev)
    }
    const res: { room: string; devs: Dev[] }[] = []
    for (const n of ROOM_ORDER) {
      const d = map.get(n)!
      if (d.length) { d.sort((a, b) => a.name.localeCompare(b.name)); res.push({ room: n, devs: d }) }
    }
    return res
  })

  // --- device helpers ---
  const CONTROL = new Set(['light', 'switch'])
  function isFanSpeed(e: Entity): boolean {
    return e.domain === 'number' && ((e.attributes as any).pin_name === 'fan' || e.id.endsWith('_fan'))
  }
  // A settable number control: fan-speed numbers, or any number carrying a vesync_cmd.
  function isNumCtl(e: Entity): boolean {
    return e.domain === 'number' && (isFanSpeed(e) || (e.attributes as any).vesync_cmd != null)
  }
  function numAttr(e: Entity, k: string, d: number): number {
    const v = (e.attributes as any)[k]; return typeof v === 'number' ? v : d
  }
  function steps(e: Entity): number[] {
    const min = numAttr(e, 'min', 0), max = numAttr(e, 'max', 3), st = numAttr(e, 'step', 1)
    const a: number[] = []; for (let v = min; v <= max; v += st) a.push(v); return a
  }
  function bigRange(e: Entity): boolean {
    const min = numAttr(e, 'min', 0), max = numAttr(e, 'max', 3), st = numAttr(e, 'step', 1)
    return (max - min) / st > 6
  }
  function btnLabel(e: Entity, v: number): string {
    if (v === 0) return ((e.attributes as any).zero_label as string) || 'Off'
    return String(v)
  }
  function controls(d: Dev) { return d.entities.filter(e => CONTROL.has(e.domain) || isNumCtl(e)) }
  function sensors(d: Dev) { return d.entities.filter(e => !CONTROL.has(e.domain) && !isNumCtl(e)) }
  function switches(d: Dev) { return d.entities.filter(e => CONTROL.has(e.domain)) }
  function anyOn(d: Dev) { return switches(d).some(isOn) }

  // Device-type icon — shared classifier in $lib/api (keeps Devices + Floor Plan in sync).
  const devIcon = (d: Dev): string => deviceIcon(d.name, d.entities)

  function summary(d: Dev): string {
    const g = groupEntity(d)
    if (g) {
      const n = ((((g.attributes as any).members) || []) as string[]).length
      return `group · ${n} lights · ${isOn(g) ? 'on' : 'off'}`
    }
    const sw = switches(d)
    const parts: string[] = []
    if (sw.length) parts.push(`${sw.filter(isOn).length}/${sw.length} on`)
    const fan = d.entities.find(isFanSpeed)
    if (fan) parts.push(fan.state === '0' ? 'fan off' : `fan ${fan.state}`)
    if (parts.length) return parts.join(' · ')
    const t = d.entities.find(e => e.id.endsWith('_temp') || e.id.endsWith('temperature'))
    const h = d.entities.find(e => e.id.endsWith('_humidity'))
    const bits: string[] = []
    if (t) bits.push(`${t.state}°`)
    if (h) bits.push(`${h.state}%`)
    if (bits.length) return bits.join(' / ')
    return `${d.entities.length} ${d.entities.length === 1 ? 'entity' : 'entities'}`
  }

  async function toggle(e: Entity) {
    const on = isOn(e)
    const m = new Map(entities); m.set(e.id, { ...e, state: on ? 'off' : 'on' }); entities = m
    await callService(e.domain, on ? 'turn_off' : 'turn_on', e.id)
  }
  async function setNumber(e: Entity, value: number) {
    const m = new Map(entities); m.set(e.id, { ...e, state: String(value) }); entities = m
    await callService(e.domain, 'set_value', e.id, { value })
  }

  let openDev = $derived(openKey ? (devices.find(d => d.key === openKey) ?? null) : null)
  let deviceCount = $derived(devices.length)
</script>

<div class="max-w-7xl mx-auto">
  <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 mb-5">
    <div>
      <h1 class="text-xl font-semibold" style="color: var(--text)">Devices</h1>
      <p class="text-sm mt-0.5" style="color: var(--text-muted)">
        <span class="inline-block w-2 h-2 rounded-full mr-1 align-middle" style="background:{connected ? 'var(--success)' : 'var(--danger)'}"></span>
        {deviceCount} devices &middot; grouped by room (floor plan)
      </p>
    </div>
    <input type="text" placeholder="Search…" bind:value={search}
      class="px-3 py-1.5 rounded-lg text-sm outline-none w-full sm:w-56 shrink-0"
      style="background: var(--surface-2); border: 1px solid var(--border); color: var(--text)" />
  </div>

  {#each sections as section (section.room)}
    <div class="mb-6">
      <div class="flex items-center gap-2 mb-2.5">
        <h2 class="text-sm font-semibold uppercase tracking-wide" style="color: var(--text-muted)">{section.room}</h2>
        <span class="text-xs px-1.5 rounded-full" style="background: var(--surface-3); color: var(--text-muted)">{section.devs.length}</span>
        <div class="flex-1 h-px" style="background: var(--border)"></div>
      </div>
      <div class="dev-grid">
        {#each section.devs as dev (dev.key)}
          <div class="dev-box group" role="button" tabindex="0"
            style="border-color: {anyOn(dev) ? 'var(--accent)' : 'var(--border)'}"
            onclick={() => openKey = dev.key}
            onkeydown={(e) => { if (e.key === 'Enter') openKey = dev.key }}>
            <div class="flex items-center gap-2">
              <span class="text-lg leading-none">{devIcon(dev)}</span>
              <span class="w-2 h-2 rounded-full ml-auto flex-shrink-0" style="background: {anyOn(dev) ? 'var(--success)' : 'var(--surface-3)'}"></span>
            </div>
            <div class="text-sm font-semibold mt-2 truncate" style="color: var(--text)" title={dev.name}>{dev.name}</div>
            <div class="text-xs mt-0.5 truncate" style="color: var(--text-muted)">{summary(dev)}</div>

            <div class="glance">
              <div class="text-xs font-semibold mb-1" style="color: var(--text)">{dev.name}</div>
              {#each dev.entities as e (e.id)}
                <div class="flex justify-between gap-3 text-xs py-0.5">
                  <span class="truncate" style="color: var(--text-muted)">{e.name}</span>
                  <span class="flex-shrink-0" style="color: {isOn(e) ? 'var(--success)' : 'var(--text)'}">{e.state}{(e.attributes as any).unit_of_measurement ?? ''}</span>
                </div>
              {/each}
            </div>
          </div>
        {/each}
      </div>
    </div>
  {/each}

  {#if entities.size === 0 && connected}
    <div class="text-center py-24" style="color: var(--text-muted)">
      <p class="text-5xl mb-4">📡</p>
      <p class="text-lg font-medium" style="color: var(--text)">No devices yet</p>
    </div>
  {/if}
</div>

{#if openDev}
  <div class="modal-overlay" role="button" tabindex="-1"
    onclick={() => openKey = null} onkeydown={(e) => { if (e.key === 'Escape') openKey = null }}>
    <div class="modal-card" role="dialog" tabindex="0" onclick={(e) => e.stopPropagation()} onkeydown={() => {}}>
      <div class="flex items-center gap-2 mb-3">
        <span class="text-xl">{devIcon(openDev)}</span>
        <span class="text-base font-semibold" style="color: var(--text)">{openDev.name}</span>
        <button class="ml-auto text-lg px-2 leading-none" style="color: var(--text-muted)" onclick={() => openKey = null}>✕</button>
      </div>

      {#if controls(openDev).length}
        {#each controls(openDev) as e (e.id)}
          <div class="flex items-center justify-between py-2.5 border-b" style="border-color: var(--border)">
            <span class="text-sm" style="color: var(--text)">{e.name}</span>
            {#if isNumCtl(e)}
              {#if bigRange(e)}
                <div class="flex items-center gap-2">
                  <input type="range" min={numAttr(e, 'min', 0)} max={numAttr(e, 'max', 100)}
                    step={numAttr(e, 'step', 1)} value={e.state}
                    onchange={(ev) => setNumber(e, +(ev.currentTarget as HTMLInputElement).value)}
                    style="accent-color: var(--accent)" />
                  <span class="text-xs w-12 text-right" style="color: var(--text)">{e.state}{(e.attributes as any).unit_of_measurement ?? ''}</span>
                </div>
              {:else}
                <div class="inline-flex gap-1 flex-wrap">
                  {#each steps(e) as spd}
                    {@const active = e.state === String(spd)}
                    <button onclick={() => setNumber(e, spd)} class="text-xs px-2 py-1 rounded-md"
                      style="background: {active ? 'color-mix(in srgb, var(--accent) 20%, transparent)' : 'var(--surface-3)'};
                             color: {active ? 'var(--accent)' : 'var(--text-muted)'};
                             border: 1px solid {active ? 'var(--accent)' : 'var(--border)'}">{btnLabel(e, spd)}</button>
                  {/each}
                </div>
              {/if}
            {:else}
              <button onclick={() => toggle(e)} class="text-xs px-4 py-1.5 rounded-md"
                style="background: {isOn(e) ? 'color-mix(in srgb, var(--accent) 20%, transparent)' : 'var(--surface-3)'};
                       color: {isOn(e) ? 'var(--accent)' : 'var(--text-muted)'};
                       border: 1px solid {isOn(e) ? 'var(--accent)' : 'var(--border)'}">{isOn(e) ? 'On' : 'Off'}</button>
            {/if}
          </div>
        {/each}
      {/if}

      {#if sensors(openDev).length}
        <div class="text-xs uppercase tracking-wide mt-3 mb-1" style="color: var(--text-muted)">Details</div>
        {#each sensors(openDev) as e (e.id)}
          <div class="flex justify-between gap-4 py-1 text-sm">
            <span class="truncate" style="color: var(--text-muted)">{e.name}</span>
            <span class="flex-shrink-0" style="color: {isOn(e) ? 'var(--success)' : 'var(--text)'}">{e.state}{(e.attributes as any).unit_of_measurement ?? ''}</span>
          </div>
        {/each}
      {/if}
    </div>
  </div>
{/if}

<style>
  .dev-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(158px, 1fr));
    gap: 10px;
  }
  .dev-box {
    position: relative;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 12px;
    cursor: pointer;
    transition: transform 0.08s ease, box-shadow 0.12s ease, border-color 0.12s ease;
  }
  .dev-box:hover {
    transform: translateY(-1px);
    box-shadow: 0 4px 14px rgba(0, 0, 0, 0.25);
    z-index: 20;
  }
  .glance {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    min-width: 100%;
    max-width: 260px;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 8px 10px;
    box-shadow: 0 6px 20px rgba(0, 0, 0, 0.35);
    opacity: 0;
    pointer-events: none;
    transform: translateY(-4px);
    transition: opacity 0.12s ease, transform 0.12s ease;
    z-index: 30;
    white-space: nowrap;
  }
  .dev-box:hover .glance { opacity: 1; transform: translateY(0); }
  .modal-overlay {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.55);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
    padding: 16px;
  }
  .modal-card {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 14px;
    padding: 18px 20px;
    width: 100%;
    max-width: 380px;
    max-height: 80vh;
    overflow-y: auto;
    box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
  }
</style>
