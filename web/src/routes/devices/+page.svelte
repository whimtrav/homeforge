<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS, entityIcon, isOn, callService } from '$lib/api'
  import type { Entity, WSMessage } from '$lib/api'

  let entities = $state<Map<string, Entity>>(new Map())
  let connected = $state(false)
  let search = $state('')

  onMount(() => {
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

  const domainLabel: Record<string, string> = {
    light: 'Lights',
    switch: 'Switches',
    binary_sensor: 'Binary Sensors',
    sensor: 'Sensors',
    lock: 'Locks',
    climate: 'Climate',
    alarm_control_panel: 'Alarm',
    camera: 'Cameras',
  }

  const domainOrder = ['light', 'switch', 'binary_sensor', 'sensor', 'lock', 'climate', 'alarm_control_panel', 'camera']

  let filtered = $derived.by(() => {
    const q = search.toLowerCase()
    return [...entities.values()].filter(e =>
      !q || e.id.includes(q) || e.name.toLowerCase().includes(q) || e.state.toLowerCase().includes(q)
    )
  })

  let grouped = $derived.by(() => {
    const map = new Map<string, Entity[]>()
    for (const e of filtered) {
      const list = map.get(e.domain) ?? []
      list.push(e)
      map.set(e.domain, list)
    }
    // Sort each group by name
    for (const [, list] of map) list.sort((a, b) => a.name.localeCompare(b.name))
    return map
  })

  let orderedDomains = $derived.by(() => {
    const known = domainOrder.filter(d => grouped.has(d))
    const other = [...grouped.keys()].filter(d => !domainOrder.includes(d)).sort()
    return [...known, ...other]
  })

  const toggleable = new Set(['light', 'switch'])

  async function toggle(e: Entity) {
    const on = isOn(e)
    const m = new Map(entities)
    m.set(e.id, { ...e, state: on ? 'off' : 'on' })
    entities = m
    await callService(e.domain, on ? 'turn_off' : 'turn_on', e.id)
  }

  function stateColor(e: Entity): string {
    if (e.state === 'unknown') return 'var(--text-muted)'
    if (isOn(e)) return 'var(--success)'
    if (e.state === 'off' || e.state === 'OFF') return 'var(--text-muted)'
    return 'var(--text)'
  }
</script>

<div class="max-w-7xl mx-auto space-y-2">
  <!-- Header -->
  <div class="flex items-center justify-between mb-6">
    <div>
      <h1 class="text-xl font-semibold" style="color: var(--text)">Devices</h1>
      <p class="text-sm mt-0.5" style="color: var(--text-muted)">
        <span class="inline-block w-2 h-2 rounded-full mr-1" style="background:{connected ? 'var(--success)' : 'var(--danger)'}"></span>
        {filtered.size ?? [...filtered].length} of {entities.size} entities
      </p>
    </div>
    <input
      type="text"
      placeholder="Search entities…"
      bind:value={search}
      class="px-3 py-1.5 rounded-lg text-sm outline-none w-56"
      style="background: var(--surface-2); border: 1px solid var(--border); color: var(--text)"
    />
  </div>

  <!-- Domain sections -->
  {#each orderedDomains as domain}
    {@const group = grouped.get(domain)!}
    <section class="card p-0 overflow-hidden mb-4">
      <!-- Domain header -->
      <div class="flex items-center gap-2 px-4 py-2.5 border-b" style="border-color: var(--border); background: var(--surface-2)">
        <span class="text-base">{entityIcon(domain)}</span>
        <span class="text-sm font-semibold" style="color: var(--text)">{domainLabel[domain] ?? domain}</span>
        <span class="ml-auto text-xs px-2 py-0.5 rounded-full" style="background: var(--surface-3); color: var(--text-muted)">{group.length}</span>
      </div>

      <!-- Entity rows -->
      <table class="w-full text-sm border-collapse">
        <thead>
          <tr style="border-bottom: 1px solid var(--border)">
            <th class="text-left px-4 py-2 text-xs font-medium" style="color: var(--text-muted); width: 35%">Name</th>
            <th class="text-left px-4 py-2 text-xs font-medium" style="color: var(--text-muted); width: 30%">Entity ID</th>
            <th class="text-left px-4 py-2 text-xs font-medium" style="color: var(--text-muted); width: 20%">State</th>
            <th class="px-4 py-2 text-xs font-medium" style="color: var(--text-muted); width: 15%"></th>
          </tr>
        </thead>
        <tbody>
          {#each group as entity (entity.id)}
            <tr
              class="border-b transition-colors hover:bg-white/5"
              style="border-color: var(--border)"
            >
              <td class="px-4 py-2.5 font-medium truncate max-w-0" style="color: var(--text)">{entity.name}</td>
              <td class="px-4 py-2.5 font-mono text-xs truncate max-w-0" style="color: var(--text-muted)">{entity.id}</td>
              <td class="px-4 py-2.5">
                <span
                  class="inline-flex items-center gap-1.5 text-xs font-semibold"
                  style="color: {stateColor(entity)}"
                >
                  {#if isOn(entity)}
                    <span class="w-1.5 h-1.5 rounded-full inline-block" style="background: var(--success)"></span>
                  {/if}
                  {entity.state}
                </span>
              </td>
              <td class="px-4 py-2.5 text-right">
                {#if toggleable.has(entity.domain)}
                  <button
                    onclick={() => toggle(entity)}
                    class="text-xs px-3 py-1 rounded-md transition-all"
                    style="background: {isOn(entity) ? 'color-mix(in srgb, var(--accent) 20%, transparent)' : 'var(--surface-3)'};
                           color: {isOn(entity) ? 'var(--accent)' : 'var(--text-muted)'};
                           border: 1px solid {isOn(entity) ? 'var(--accent)' : 'var(--border)'}"
                  >
                    {isOn(entity) ? 'Turn Off' : 'Turn On'}
                  </button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/each}

  {#if entities.size === 0 && connected}
    <div class="text-center py-24" style="color: var(--text-muted)">
      <p class="text-5xl mb-4">📡</p>
      <p class="text-lg font-medium" style="color: var(--text)">No entities yet</p>
      <p class="text-sm mt-1">Waiting for device messages…</p>
    </div>
  {/if}
</div>
