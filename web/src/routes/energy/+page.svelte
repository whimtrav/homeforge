<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { fetchEntities, connectWS, type Entity } from '$lib/api'

  let ents = $state<Record<string, Entity>>({})
  let disconnect: (() => void) | null = null
  let usage = $state<{ hour: number; today: number; week: number; month: number } | null>(null)
  let usageTimer: ReturnType<typeof setInterval> | null = null
  let cycle = $state<any>(null)
  let cycleTimer: ReturnType<typeof setInterval> | null = null
  let showBillForm = $state(false)
  let billReadTo = $state('')
  let billBank = $state('')
  let billMsg = $state('')
  async function loadCycle() {
    try { const r = await fetch('/api/energy/cycle'); if (r.ok) cycle = await r.json() } catch {}
  }
  async function rectifyBill() {
    billMsg = ''
    if (!billReadTo) { billMsg = 'Enter the read date'; return }
    const r = await fetch('/api/energy/cycle/rectify', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ read_to: billReadTo, bank_kwh: parseFloat(billBank) || 0 }) })
    if (r.ok) { billMsg = 'Logged'; showBillForm = false; billReadTo = ''; billBank = ''; await loadCycle() } else billMsg = 'Failed'
  }

  onMount(async () => {
    for (const e of await fetchEntities()) ents[e.id] = e
    disconnect = connectWS((msg) => {
      if (msg.type === 'snapshot' && msg.entities) {
        const next: Record<string, Entity> = {}
        for (const e of msg.entities) next[e.id] = e
        ents = next
      } else if (msg.type === 'state_changed' && msg.entity) {
        ents[msg.entity.id] = msg.entity
      }
    })
    // Water usage (hour/today/week/month) is integrated server-side from flow history.
    const loadUsage = async () => {
      try {
        const r = await fetch('/api/water/usage')
        if (r.ok) usage = await r.json()
      } catch { /* keep last value */ }
    }
    loadUsage()
    usageTimer = setInterval(loadUsage, 30000)
    loadCycle()
    cycleTimer = setInterval(loadCycle, 60000)
  })
  onDestroy(() => {
    disconnect?.()
    if (usageTimer) clearInterval(usageTimer)
    if (cycleTimer) clearInterval(cycleTimer)
  })

  // solar sensor value (number) by metric name, or null if missing
  function sv(metric: string): number | null {
    const e = ents[`sensor.solar_${metric}`]
    if (!e) return null
    const n = parseFloat(e.state)
    return isNaN(n) ? null : n
  }
  const fmt = (n: number | null, unit = '', dp = 0) =>
    n === null ? '—' : `${n.toLocaleString(undefined, { maximumFractionDigits: dp })}${unit ? ' ' + unit : ''}`

  // headline values
  const pv = $derived(sv('pv_power'))
  const soc = $derived(sv('battery_state_of_charge'))
  const battP = $derived(sv('battery_power'))
  const grid = $derived(sv('grid_power'))
  const load = $derived(sv('load_power'))

  const strings = $derived([1, 2, 3].map((i) => ({
    i,
    p: sv(`pv_power_${i}`),
    v: sv(`pv_voltage_${i}`),
    a: sv(`pv_current_${i}`),
  })))

  // Tigo per-panel: sensor.tigo_<panel>, excluding the total/active aggregates.
  const panels = $derived(
    Object.values(ents)
      .filter((e) => e.id.startsWith('sensor.tigo_') &&
        e.id !== 'sensor.tigo_total_power' && e.id !== 'sensor.tigo_panels_active')
      .map((e) => ({ name: (e.attributes?.panel as string) ?? e.id.replace('sensor.tigo_', '').toUpperCase(), w: parseFloat(e.state) || 0 }))
      .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }))
  )
  const tigoTotal = $derived(ents['sensor.tigo_total_power']?.state ?? null)
  const tigoActive = $derived(ents['sensor.tigo_panels_active']?.state ?? null)
  const panelMax = $derived(Math.max(1, ...panels.map((p) => p.w)))

  // Emporia circuits (biggest draw first), plus mains kept separate from the Lux grid.
  // These CTs are physically UNCLAMPED (being moved to metered DualR3s) → hide their false 0.
  const EMPORIA_UNCLAMPED = new Set(['furnace', 'hot tub', 'dryer'])
  const circuits = $derived(
    Object.values(ents)
      .filter((e) => e.id.startsWith('sensor.emporia_') && e.attributes?.section === 'circuits')
      .map((e) => ({ name: e.id.replace('sensor.emporia_', '').replace(/_/g, ' '), w: parseFloat(e.state) || 0 }))
      .filter((c) => !EMPORIA_UNCLAMPED.has(c.name))
      .sort((a, b) => b.w - a.w)
  )
  const empMains = $derived(ents['sensor.emporia_mains']?.state ?? null)
  const empBalance = $derived(ents['sensor.emporia_balance']?.state ?? null)
  const circMax = $derived(Math.max(1, ...circuits.map((c) => c.w)))

  // Metered appliances = BL0939-metered LiquidFW DualR3s, one rich card each.
  // Group every sensor.<dev>_meter_<field> by device → power/voltage/current/energy.
  const APPLIANCE_META: Record<string, { icon: string; label: string; note?: string }> = {
    furnace: { icon: '🔥', label: 'Furnace' },
    washerdryer: { icon: '🧺', label: 'Washer / Dryer', note: 'shared circuit: + cat litter + sump pump' },
    dishwasher: { icon: '🍽️', label: 'Dishwasher' },
  }
  const meteredDevices = $derived.by(() => {
    const byDev: Record<string, any> = {}
    for (const e of Object.values(ents)) {
      if (e.id.startsWith('sensor.solar')) continue
      const m = e.id.match(/^sensor\.(.+)_meter_(voltage|current1|current2|power1|power2|energy1|energy2)$/)
      if (!m) continue
      ;(byDev[m[1]] ??= { dev: m[1] })[m[2]] = parseFloat(e.state) || 0
    }
    return Object.values(byDev)
      .map((d: any) => ({
        dev: d.dev,
        meta: APPLIANCE_META[d.dev] ?? { icon: '🔌', label: d.dev.replace(/_/g, ' ') },
        power: (d.power1 || 0) + (d.power2 || 0),
        power2: d.power2 || 0,
        voltage: d.voltage ?? null,
        current: (d.current1 || 0) + (d.current2 || 0),
        energy: (d.energy1 || 0) + (d.energy2 || 0),
      }))
      .sort((a, b) => b.power - a.power)
  })

  // Big appliances = the 5 key gauges. Mixed sources: DualR3 BL0939 meters + Emporia CTs.
  const num = (id: string) => {
    const e = ents[id]
    if (!e) return null
    const n = parseFloat(e.state)
    return isNaN(n) ? null : n
  }
  const es = (id: string) => ents[id]?.state ?? null
  const eb = (id: string) => ents[id]?.state === 'on'
  const bigAppliances = $derived([
    { icon: '🧺', name: 'Washer / Dryer', w: num('sensor.washerdryer_meter_power1'), src: 'meter',
      extra: eb('binary_sensor.washerdryer_running') ? `running · ${es('sensor.washerdryer_phase')}` : '' },
    { icon: '🔥', name: 'Furnace', w: num('sensor.furnace_meter_power1'), src: 'meter', extra: '' },
    { icon: '❄️', name: 'AC Unit', w: num('sensor.emporia_a_c'), src: 'emporia', extra: '' },
    { icon: '🍽️', name: 'Dishwasher', w: num('sensor.dishwasher_meter_power1'), src: 'meter', extra: '' },
    { icon: '🍳', name: 'Stove', w: num('sensor.emporia_stovetop'), src: 'emporia', extra: '' },
  ])

  // Water — Hydrific Droplet on the main line (MQTT): real-time flow + derived cumulative total.
  const water = $derived({
    present: !!ents['sensor.droplet_fe5c_flow'],
    flow: num('sensor.droplet_fe5c_flow'),
    total: num('sensor.droplet_fe5c_total'),
    signal: es('sensor.droplet_fe5c_signal'),
    online: eb('binary_sensor.droplet_fe5c_online'),
    leak: eb('binary_sensor.droplet_fe5c_high_leak') || eb('binary_sensor.droplet_fe5c_low_leak'),
  })
</script>

<div class="max-w-5xl mx-auto flex flex-col gap-6">
  <h1 class="text-xl font-bold" style="color: var(--text)">Energy</h1>
  {#if cycle}
  <section>
    <div class="rounded-xl p-4 border" style="border-color: {cycle.needs_bill ? 'var(--accent)' : 'var(--border)'}; background: var(--surface)">
      <div class="flex items-center justify-between flex-wrap gap-2">
        <h2 class="text-sm font-semibold uppercase tracking-wide" style="color: var(--text-muted)">Billing Cycle</h2>
        <div class="text-xs" style="color: var(--text-muted)">since {cycle.cycle_start} · day {cycle.day} of ~{cycle.expected_days}</div>
      </div>
      <!-- Made vs used: GREEN when generation ≥ consumption (net positive), RED when used more than made. Yesterday · today-so-far · whole cycle. -->
      <div class="grid gap-3 mt-3" style="grid-template-columns: repeat(3, 1fr)">
        <div class="rounded-lg p-3 text-center" style="background: {cycle.yesterday_made_net >= 0 ? 'rgba(78,161,114,0.13)' : 'rgba(224,106,92,0.13)'}">
          <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Yesterday</div>
          <div class="text-2xl font-bold" style="color: {cycle.yesterday_made_net >= 0 ? '#4ea172' : '#e06a5c'}">{cycle.yesterday_made_net > 0 ? '+' : ''}{cycle.yesterday_made_net}</div>
          <div class="text-xs" style="color: var(--text-muted)">kWh · {cycle.yesterday_made_net >= 0 ? 'made more' : 'used more'}</div>
        </div>
        <div class="rounded-lg p-3 text-center" style="background: {cycle.today_made_net >= 0 ? 'rgba(78,161,114,0.13)' : 'rgba(224,106,92,0.13)'}">
          <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Today so far</div>
          <div class="text-2xl font-bold" style="color: {cycle.today_made_net >= 0 ? '#4ea172' : '#e06a5c'}">{cycle.today_made_net > 0 ? '+' : ''}{cycle.today_made_net}</div>
          <div class="text-xs" style="color: var(--text-muted)">kWh · {cycle.today_made_net >= 0 ? 'made more' : 'used more'}</div>
        </div>
        <div class="rounded-lg p-3 text-center" style="background: {cycle.made_net >= 0 ? 'rgba(78,161,114,0.13)' : 'rgba(224,106,92,0.13)'}">
          <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">This cycle</div>
          <div class="text-2xl font-bold" style="color: {cycle.made_net >= 0 ? '#4ea172' : '#e06a5c'}">{cycle.made_net > 0 ? '+' : ''}{cycle.made_net}</div>
          <div class="text-xs" style="color: var(--text-muted)">day {cycle.day} · {cycle.made_net >= 0 ? 'made more' : 'used more'}</div>
        </div>
      </div>
      <div class="grid gap-3 mt-3" style="grid-template-columns: repeat(auto-fit, minmax(105px, 1fr))">
        <div><div class="text-xl font-bold" style="color: var(--text)">{cycle.grid_import}</div><div class="text-xs" style="color: var(--text-muted)">Import kWh</div></div>
        <div><div class="text-xl font-bold" style="color: var(--text)">{cycle.grid_export}</div><div class="text-xs" style="color: var(--text-muted)">Export kWh</div></div>
        <div><div class="text-xl font-bold" style="color: var(--text)">{cycle.grid_net > 0 ? '+' : ''}{cycle.grid_net}</div><div class="text-xs" style="color: var(--text-muted)">Grid net {cycle.grid_net > 0 ? '(import)' : '(export)'}</div></div>
        <div><div class="text-xl font-bold" style="color: var(--text)">{cycle.generation}</div><div class="text-xs" style="color: var(--text-muted)">Generated</div></div>
        <div><div class="text-xl font-bold" style="color: var(--text)">{cycle.consumption}</div><div class="text-xs" style="color: var(--text-muted)">Used</div></div>
        <div><div class="text-xl font-bold" style="color: var(--accent)">{cycle.bank_kwh}</div><div class="text-xs" style="color: var(--text-muted)">Credit bank{cycle.bank_delta ? ' ' + (cycle.bank_delta > 0 ? '+' : '') + cycle.bank_delta : ''}</div></div>
      </div>
      {#if cycle.needs_bill || showBillForm}
      <div class="mt-3 pt-3 border-t" style="border-color: var(--border)">
        {#if cycle.needs_bill && !showBillForm}
          <div class="flex items-center justify-between flex-wrap gap-2">
            <span class="text-sm" style="color: var(--accent)">New bill likely ready — log it to rectify the cycle</span>
            <button class="text-sm px-3 py-1 rounded-lg" style="background: var(--accent); color: #fff" onclick={() => showBillForm = true}>Log bill</button>
          </div>
        {/if}
        {#if showBillForm}
          <div class="flex items-end gap-2 flex-wrap">
            <label class="text-xs" style="color: var(--text-muted)">Read date<br /><input type="date" bind:value={billReadTo} class="mt-1 px-2 py-1 rounded border" style="background: var(--surface); color: var(--text); border-color: var(--border)" /></label>
            <label class="text-xs" style="color: var(--text-muted)">Bank kWh<br /><input type="number" bind:value={billBank} placeholder="2020" class="mt-1 px-2 py-1 rounded border w-24" style="background: var(--surface); color: var(--text); border-color: var(--border)" /></label>
            <button class="text-sm px-3 py-1 rounded-lg" style="background: var(--accent); color: #fff" onclick={rectifyBill}>Save</button>
            <button class="text-sm px-3 py-1 rounded-lg border" style="border-color: var(--border); color: var(--text-muted)" onclick={() => showBillForm = false}>Cancel</button>
            {#if billMsg}<span class="text-xs" style="color: var(--text-muted)">{billMsg}</span>{/if}
          </div>
        {/if}
      </div>
      {/if}
    </div>
  </section>
  {/if}

  <!-- ── Big Appliances (the 5 that matter) — DualR3 meters + Emporia CTs ── -->
  <section>
    <h2 class="text-sm font-semibold uppercase tracking-wide mb-3" style="color: var(--text-muted)">Big Appliances</h2>
    <div class="grid gap-3" style="grid-template-columns: repeat(auto-fit, minmax(150px, 1fr))">
      {#each bigAppliances as a}
        <div class="rounded-xl p-4 border" style="border-color: {a.w !== null && a.w > 10 ? 'var(--accent)' : 'var(--border)'}; background: var(--surface)">
          <div class="flex items-center justify-between">
            <span class="text-2xl">{a.icon}</span>
            <span class="text-[10px] uppercase tracking-wide" style="color: var(--text-muted); opacity: .6">{a.src}</span>
          </div>
          <div class="text-2xl font-bold mt-1" style="color: {a.w !== null && a.w > 10 ? 'var(--text)' : 'var(--text-muted)'}">{fmt(a.w, 'W')}</div>
          <div class="text-xs" style="color: var(--text-muted)">{a.name}</div>
          {#if a.extra}<div class="text-xs mt-1" style="color: var(--accent)">{a.extra}</div>{/if}
        </div>
      {/each}
    </div>
  </section>

  <!-- ── Solar (EG4 inverter via solar-assistant) ── -->
  <section>
    <h2 class="text-sm font-semibold uppercase tracking-wide mb-3" style="color: var(--text-muted)">
      Solar · Battery · Grid
    </h2>
    <div class="grid gap-3" style="grid-template-columns: repeat(auto-fit, minmax(150px, 1fr))">
      <!-- Solar production -->
      <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
        <div class="text-2xl">☀️</div>
        <div class="text-2xl font-bold mt-1" style="color: var(--text)">{fmt(pv, 'W')}</div>
        <div class="text-xs" style="color: var(--text-muted)">Solar production</div>
      </div>
      <!-- Battery -->
      <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
        <div class="text-2xl">🔋</div>
        <div class="text-2xl font-bold mt-1" style="color: var(--text)">{fmt(soc, '%')}</div>
        <div class="text-xs" style="color: var(--text-muted)">
          Battery · {battP === null ? '—' : battP >= 0 ? `charging ${fmt(battP, 'W')}` : `discharging ${fmt(-battP, 'W')}`}
        </div>
      </div>
      <!-- Grid -->
      <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
        <div class="text-2xl">⚡</div>
        <div class="text-2xl font-bold mt-1" style="color: {grid === null ? 'var(--text)' : grid < 0 ? '#4ade80' : '#f87171'}">
          {grid === null ? '—' : fmt(Math.abs(grid), 'W')}
        </div>
        <div class="text-xs" style="color: var(--text-muted)">
          Grid · {grid === null ? '—' : grid < 0 ? 'exporting' : 'importing'}
        </div>
      </div>
      <!-- Load -->
      <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
        <div class="text-2xl">🏠</div>
        <div class="text-2xl font-bold mt-1" style="color: var(--text)">{fmt(load, 'W')}</div>
        <div class="text-xs" style="color: var(--text-muted)">Home load</div>
      </div>
    </div>

    <!-- PV strings + battery/grid detail -->
    <div class="grid gap-3 mt-3" style="grid-template-columns: repeat(auto-fit, minmax(150px, 1fr))">
      {#each strings as s}
        <div class="rounded-lg p-3 border" style="border-color: var(--border); background: var(--surface-2)">
          <div class="text-xs font-medium" style="color: var(--text-muted)">PV String {s.i}</div>
          <div class="text-lg font-semibold" style="color: var(--text)">{fmt(s.p, 'W')}</div>
          <div class="text-xs" style="color: var(--text-muted)">{fmt(s.v, 'V', 1)} · {fmt(s.a, 'A', 1)}</div>
        </div>
      {/each}
    </div>

    <!-- Today's energy totals -->
    <div class="grid gap-3 mt-3" style="grid-template-columns: repeat(auto-fit, minmax(130px, 1fr))">
      {#each [['pv_energy','Solar'],['load_energy','Load'],['grid_energy_in','Grid in'],['grid_energy_out','Grid out'],['battery_energy_in','Batt in'],['battery_energy_out','Batt out']] as [m, label]}
        <div class="rounded-lg p-3 border text-center" style="border-color: var(--border); background: var(--surface)">
          <div class="text-lg font-semibold" style="color: var(--text)">{fmt(sv(m), 'kWh', 1)}</div>
          <div class="text-xs" style="color: var(--text-muted)">{label}</div>
        </div>
      {/each}
    </div>
  </section>

  <!-- ── Panels (Tigo per-panel, local CCA) ── -->
  <section>
    <div class="flex items-baseline justify-between mb-3">
      <h2 class="text-sm font-semibold uppercase tracking-wide" style="color: var(--text-muted)">Panels (Tigo)</h2>
      <span class="text-xs" style="color: var(--text-muted)">
        {tigoActive ?? '—'} active · {tigoTotal === null ? '—' : `${(+tigoTotal).toLocaleString()} W`}
      </span>
    </div>
    {#if panels.length === 0}
      <div class="rounded-xl p-6 border text-sm" style="border-color: var(--border); background: var(--surface); color: var(--text-muted)">
        Waiting for Tigo CCA data…
      </div>
    {:else}
      <div class="grid gap-2" style="grid-template-columns: repeat(auto-fill, minmax(84px, 1fr))">
        {#each panels as p}
          <div class="rounded-lg p-2 border text-center" style="border-color: var(--border); background: var(--surface)">
            <div class="text-xs font-medium" style="color: var(--text-muted)">{p.name}</div>
            <div class="text-sm font-semibold" style="color: {p.w > 0 ? 'var(--text)' : '#f87171'}">{p.w} W</div>
            <div class="h-1 rounded mt-1" style="background: var(--surface-2)">
              <div class="h-1 rounded" style="width: {Math.round((p.w / panelMax) * 100)}%; background: var(--accent)"></div>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>

  <!-- ── Emporia circuits (mains kept separate from the Lux grid reading above) ── -->
  <section>
    <div class="flex items-baseline justify-between mb-3">
      <h2 class="text-sm font-semibold uppercase tracking-wide" style="color: var(--text-muted)">Circuits (Emporia Vue)</h2>
      <span class="text-xs" style="color: var(--text-muted)">
        Mains {empMains === null ? '—' : `${(+empMains).toLocaleString()} W`}{empBalance === null ? '' : ` · other ${(+empBalance).toLocaleString()} W`}
      </span>
    </div>
    <p class="text-xs mb-2" style="color: var(--text-muted)">furnace · hot tub · dryer CTs unclamped — moving to metered DualR3s (furnace &amp; washer/dryer now under Metered appliances)</p>
    {#if circuits.length === 0}
      <div class="rounded-xl p-6 border text-sm" style="border-color: var(--border); background: var(--surface); color: var(--text-muted)">
        Waiting for Emporia data…
      </div>
    {:else}
      <div class="grid gap-2" style="grid-template-columns: repeat(auto-fill, minmax(150px, 1fr))">
        {#each circuits as c}
          <div class="rounded-lg p-3 border" style="border-color: var(--border); background: var(--surface)">
            <div class="flex items-baseline justify-between">
              <span class="text-xs font-medium capitalize truncate" style="color: var(--text-muted)">{c.name}</span>
              <span class="text-sm font-semibold" style="color: var(--text)">{c.w} W</span>
            </div>
            <div class="h-1 rounded mt-1.5" style="background: var(--surface-2)">
              <div class="h-1 rounded" style="width: {Math.round((c.w / circMax) * 100)}%; background: var(--accent)"></div>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>

  <!-- ── Metered appliances — LiquidFW BL0939 DualR3s, one card each ── -->
  <section>
    <h2 class="text-sm font-semibold uppercase tracking-wide mb-3" style="color: var(--text-muted)">Metered appliances</h2>
    {#if meteredDevices.length === 0}
      <div class="rounded-xl p-6 border text-sm" style="border-color: var(--border); background: var(--surface); color: var(--text-muted)">
        No metered appliances yet.
      </div>
    {:else}
      <div class="grid gap-3" style="grid-template-columns: repeat(auto-fit, minmax(190px, 1fr))">
        {#each meteredDevices as d}
          <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
            <div class="flex items-center justify-between">
              <span class="text-2xl">{d.meta.icon}</span>
              <span class="text-xs font-medium capitalize" style="color: var(--text-muted)">{d.meta.label}</span>
            </div>
            <div class="text-2xl font-bold mt-1" style="color: {d.power > 5 ? 'var(--text)' : 'var(--text-muted)'}">{fmt(d.power, 'W')}</div>
            <div class="text-xs mt-0.5" style="color: var(--text-muted)">{fmt(d.voltage, 'V', 0)} · {fmt(d.current, 'A', 1)}</div>
            <div class="text-xs mt-1" style="color: var(--text-muted)">{fmt(d.energy, 'kWh', 2)} <span style="opacity:.7">since boot</span></div>
            {#if d.meta.note}
              <div class="text-xs mt-1.5 italic" style="color: var(--text-muted); opacity:.8">{d.meta.note}</div>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
  </section>

  <!-- ── Water — Hydrific Droplet (main line, MQTT) ── -->
  {#if water.present}
    <section>
      <h2 class="text-sm font-semibold uppercase tracking-wide mb-3" style="color: var(--text-muted)">Water (Droplet — main line)</h2>
      <div class="grid gap-3" style="grid-template-columns: repeat(auto-fit, minmax(190px, 1fr))">
        <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface)">
          <div class="flex items-center justify-between">
            <span class="text-2xl">💧</span>
            <span class="text-xs font-medium" style="color: var(--text-muted)">Flow</span>
          </div>
          <div class="text-2xl font-bold mt-1" style="color: {(water.flow ?? 0) > 0 ? '#2563eb' : 'var(--text-muted)'}">{fmt(water.flow, 'gal/min', 1)}</div>
          <div class="text-xs mt-0.5" style="color: var(--text-muted)">{water.online ? 'online' : 'offline'} · {water.signal ?? '—'}</div>
        </div>
        <div class="rounded-xl p-4 border" style="border-color: var(--border); background: var(--surface); grid-column: span 1">
          <div class="flex items-center justify-between">
            <span class="text-2xl">🚰</span>
            <span class="text-xs font-medium" style="color: var(--text-muted)">Water used</span>
          </div>
          <div class="mt-2 flex flex-col gap-1 text-sm">
            <div class="flex justify-between"><span style="color: var(--text-muted)">This hour</span><span class="font-semibold" style="color: var(--text)">{fmt(usage?.hour ?? null, 'gal', 2)}</span></div>
            <div class="flex justify-between"><span style="color: var(--text-muted)">Today</span><span class="font-semibold" style="color: var(--text)">{fmt(usage?.today ?? null, 'gal', 2)}</span></div>
            <div class="flex justify-between"><span style="color: var(--text-muted)">This week</span><span class="font-semibold" style="color: var(--text)">{fmt(usage?.week ?? null, 'gal', 1)}</span></div>
            <div class="flex justify-between"><span style="color: var(--text-muted)">This month</span><span class="font-semibold" style="color: var(--text)">{fmt(usage?.month ?? null, 'gal', 1)}</span></div>
          </div>
        </div>
        {#if water.leak}
          <div class="rounded-xl p-4 border" style="border-color: #b91c1c; background: var(--surface)">
            <div class="flex items-center justify-between">
              <span class="text-2xl">⚠️</span>
              <span class="text-xs font-medium" style="color: #b91c1c">Leak alert</span>
            </div>
            <div class="text-lg font-bold mt-1" style="color: #b91c1c">Leak detected</div>
          </div>
        {/if}
      </div>
    </section>
  {/if}
</div>
