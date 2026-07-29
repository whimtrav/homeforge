<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS, callService } from '$lib/api'
  import type { Entity, WSMessage } from '$lib/api'

  let entities = $state<Map<string, Entity>>(new Map())
  let connected = $state(false)

  onMount(() => connectWS((msg: WSMessage) => {
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
  }))

  // ── thermostat (climate.house, driven by the HomeForge brain) ──────────────
  let climate = $derived(entities.get('climate.house'))
  let a = $derived((climate?.attributes ?? {}) as Record<string, any>)
  let mode = $derived(climate?.state ?? 'off')
  let setpoint = $derived(Number(a.temperature ?? 72))
  let current = $derived(a.temp_available ? Number(a.current_temperature) : null)
  let action = $derived(String(a.hvac_action ?? 'unknown'))
  let preset = $derived(String(a.preset_mode ?? 'none'))
  let presetModes = $derived((a.preset_modes ?? ['none']) as string[])
  let tempSensors = $derived((a.temp_sensors ?? {}) as Record<string, number>)
  let minT = $derived(Number(a.min_temp ?? 60))
  let maxT = $derived(Number(a.max_temp ?? 85))

  const MODES: [string, string, string][] = [
    ['off', 'Off', '⏻'], ['cool', 'Cool', '❄️'], ['heat', 'Heat', '🔥'], ['fan_only', 'Fan', '🌀'],
  ]
  const ACTION_COLOR: Record<string, string> = {
    heating: '#f97316', cooling: '#3b82f6', fan: '#14b8a6',
    idle: 'var(--text-muted)', failsafe: '#ef4444', unknown: 'var(--text-muted)',
  }
  const ACTION_LABEL: Record<string, string> = {
    heating: 'Heating', cooling: 'Cooling', fan: 'Fan running',
    idle: 'Idle', failsafe: '⚠ Fail-safe (no temp)', unknown: '—',
  }

  const setMode = (m: string) => callService('climate', 'set_hvac_mode', 'climate.house', { hvac_mode: m })
  const setPreset = (p: string) => callService('climate', 'set_preset_mode', 'climate.house', { preset_mode: p })
  function bump(d: number) {
    if (mode === 'off' || mode === 'fan_only') return
    const v = Math.min(maxT, Math.max(minT, Math.round((setpoint + d) * 2) / 2))
    callService('climate', 'set_temperature', 'climate.house', { temperature: v })
  }
  const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)
  const sensorLabel = (topic: string) => topic.replace(/^.*\//, '').replace(/ Temperature$/i, '')

  // ── fans ───────────────────────────────────────────────────────────────────
  const prettyDev = (e: Entity) =>
    String((e.attributes as any)?.device ?? e.id.split('.')[1]).replace(/-/g, ' ')

  // Ceiling fans = LiquidFW iFan04 speed controls (number, pin_name "fan", 0..3).
  let ceilingFans = $derived(
    [...entities.values()]
      .filter((e) => e.domain === 'number' && (e.attributes as any)?.pin_name === 'fan')
      .sort((x, y) => x.id.localeCompare(y.id))
  )
  // Exhaust / bath fans = switches named *_fan or *exhaust (not the HVAC blower).
  let exhaustFans = $derived(
    [...entities.values()]
      .filter((e) => e.domain === 'switch' && /(_fan$|exhaust)/.test(e.id) && !e.id.includes('climate_control'))
      .sort((x, y) => x.id.localeCompare(y.id))
  )
  // HVAC blower — owned by the thermostat (read-only status here).
  let hvacBlower = $derived(entities.get('switch.climate_control_fan'))

  const SPEEDS = [['0', 'Off'], ['1', 'Low'], ['2', 'Med'], ['3', 'High']]
  const setFanSpeed = (id: string, n: number) => callService('number', 'set_value', id, { value: n })
  const toggleSwitch = (e: Entity) => callService('switch', e.state === 'on' ? 'turn_off' : 'turn_on', e.id)

  // ── climate brain (adaptive HVAC: pre-cool, attic-fan verdict, feels-like, learned comfort) ──
  const cb = (k: string) => entities.get(`sensor.climatebrain_${k}`)
  let cbStatus = $derived(cb('status'))
  let cbPrecool = $derived(cb('precool'))
  let cbComfort = $derived(cb('comfort'))
  let cbLoad = $derived(cb('load'))
  let cbAttic = $derived(cb('atticfan_effect'))
  let cbFeelsUp = $derived(cb('feels_upstairs'))
  let cbFeelsDown = $derived(cb('feels_downstairs'))
  let hasBrain = $derived(!!(cbStatus || cbPrecool || cbComfort || cbFeelsUp))
  const rh = (e: Entity | undefined) => (e?.attributes as any)?.humidity
  const precoolActive = $derived(!!((cbPrecool?.attributes as any)?.precooling))

  // Comfort feedback → climate-brain training data. Each tap logs the current context.
  let comfortMsg = $state('')
  async function sendComfort(zone: string, rating: string) {
    try {
      await fetch('/api/comfort', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ zone, rating }),
      })
      comfortMsg = `Logged: ${zone} feels ${rating}`
    } catch {
      comfortMsg = 'failed to log'
    }
    setTimeout(() => { comfortMsg = '' }, 3000)
  }
</script>

<svelte:head><title>Climate · HomeForge</title></svelte:head>

<div class="max-w-5xl mx-auto flex flex-col gap-6">
  <div class="flex items-center gap-3">
    <h1 class="text-2xl font-bold" style="color: var(--text)">Climate</h1>
    <span class="text-xs px-2 py-0.5 rounded-full"
      style="background: var(--surface-2); color: {connected ? '#22c55e' : 'var(--text-muted)'}">
      {connected ? '● live' : '○ connecting'}
    </span>
  </div>

  <!-- Comfort feedback → climate-brain training data -->
  <section class="rounded-2xl border p-4" style="background: var(--surface); border-color: var(--border)">
    <div class="flex items-baseline justify-between mb-3">
      <h2 class="text-sm font-semibold uppercase tracking-wide" style="color: var(--text-muted)">How does it feel right now?</h2>
      {#if comfortMsg}<span class="text-xs" style="color: #22c55e">{comfortMsg}</span>{/if}
    </div>
    <div class="flex flex-col gap-2">
      {#each ['Upstairs', 'Downstairs'] as zone}
        <div class="flex items-center gap-2 flex-wrap">
          <span class="w-24 text-sm font-medium" style="color: var(--text)">{zone}</span>
          <button onclick={() => sendComfort(zone.toLowerCase(), 'cold')}
            class="px-3 py-1.5 rounded-lg text-sm" style="background: var(--surface-2); color: var(--text)">🥶 Cold</button>
          <button onclick={() => sendComfort(zone.toLowerCase(), 'good')}
            class="px-3 py-1.5 rounded-lg text-sm" style="background: var(--surface-2); color: var(--text)">👍 Good</button>
          <button onclick={() => sendComfort(zone.toLowerCase(), 'hot')}
            class="px-3 py-1.5 rounded-lg text-sm" style="background: var(--surface-2); color: var(--text)">🥵 Hot</button>
        </div>
      {/each}
    </div>
    <p class="text-xs mt-3" style="color: var(--text-muted)">Every tap is logged with the current temps · solar · outdoor → trains the climate brain's comfort model.</p>
  </section>

  <!-- Thermostat -->
  <section class="rounded-2xl border p-6" style="background: var(--surface); border-color: var(--border)">
    {#if !climate}
      <p style="color: var(--text-muted)">Waiting for the thermostat…</p>
    {:else}
      <div class="flex flex-wrap items-center gap-8">
        <!-- current + action -->
        <div class="flex flex-col items-center min-w-[9rem]">
          <div class="text-6xl font-bold leading-none" style="color: var(--text)">
            {current === null ? '—' : current.toFixed(1)}<span class="text-2xl align-top">°F</span>
          </div>
          <div class="mt-2 text-sm font-semibold" style="color: {ACTION_COLOR[action] ?? 'var(--text-muted)'}">
            {ACTION_LABEL[action] ?? action}
          </div>
        </div>

        <!-- setpoint -->
        <div class="flex flex-col items-center">
          <span class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Set to</span>
          <div class="flex items-center gap-3 mt-1">
            <button onclick={() => bump(-0.5)} disabled={mode === 'off' || mode === 'fan_only'}
              class="w-10 h-10 rounded-full text-xl font-bold disabled:opacity-30"
              style="background: var(--surface-2); color: var(--text)">−</button>
            <span class="text-4xl font-bold tabular-nums" style="color: var(--text)">
              {mode === 'off' || mode === 'fan_only' ? '—' : setpoint.toFixed(1)}
            </span>
            <button onclick={() => bump(0.5)} disabled={mode === 'off' || mode === 'fan_only'}
              class="w-10 h-10 rounded-full text-xl font-bold disabled:opacity-30"
              style="background: var(--surface-2); color: var(--text)">+</button>
          </div>
        </div>

        <!-- per-room temps -->
        <div class="flex flex-col gap-1 text-sm" style="color: var(--text-muted)">
          {#each Object.entries(tempSensors) as [topic, t]}
            <div class="flex justify-between gap-4">
              <span>{sensorLabel(topic)}</span><span class="tabular-nums" style="color: var(--text)">{Number(t).toFixed(1)}°F</span>
            </div>
          {/each}
        </div>
      </div>

      <!-- mode -->
      <div class="mt-6">
        <div class="text-xs uppercase tracking-wide mb-2" style="color: var(--text-muted)">Mode</div>
        <div class="flex flex-wrap gap-2">
          {#each MODES as [m, label, icon]}
            <button onclick={() => setMode(m)}
              class="px-4 py-2 rounded-lg text-sm font-medium border transition-colors"
              style="border-color: var(--border);
                     background: {mode === m ? 'var(--accent)' : 'var(--surface-2)'};
                     color: {mode === m ? '#fff' : 'var(--text)'}">
              {icon} {label}
            </button>
          {/each}
        </div>
      </div>

      <!-- presets -->
      <div class="mt-4">
        <div class="text-xs uppercase tracking-wide mb-2" style="color: var(--text-muted)">Preset</div>
        <div class="flex flex-wrap gap-2">
          {#each presetModes as p}
            <button onclick={() => setPreset(p)}
              class="px-3 py-1.5 rounded-lg text-sm border transition-colors"
              style="border-color: var(--border);
                     background: {preset === p ? 'var(--surface-2)' : 'transparent'};
                     color: {preset === p ? 'var(--text)' : 'var(--text-muted)'}">
              {cap(p)}
            </button>
          {/each}
        </div>
      </div>
    {/if}
  </section>

  <!-- Climate Brain -->
  {#if hasBrain}
    <section class="rounded-2xl border p-6" style="background: var(--surface); border-color: var(--border)">
      <div class="flex items-baseline justify-between mb-4 gap-3 flex-wrap">
        <h2 class="text-lg font-bold" style="color: var(--text)">🧠 Climate Brain</h2>
        {#if cbStatus}
          <span class="text-xs px-2 py-0.5 rounded-full"
            style="background: var(--surface-2); color: {precoolActive ? '#3b82f6' : 'var(--text-muted)'}">
            {cbStatus.state}
          </span>
        {/if}
      </div>

      <div class="grid gap-3" style="grid-template-columns: repeat(auto-fill, minmax(11rem, 1fr))">
        {#if cbFeelsUp}
          <div class="rounded-xl border p-3" style="border-color: var(--border); background: var(--surface-2)">
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Feels upstairs</div>
            <div class="text-2xl font-bold" style="color: var(--text)">{Number(cbFeelsUp.state).toFixed(1)}°F</div>
            {#if rh(cbFeelsUp) != null}<div class="text-xs" style="color: var(--text-muted)">{Number(rh(cbFeelsUp)).toFixed(0)}% RH</div>{/if}
          </div>
        {/if}
        {#if cbFeelsDown}
          <div class="rounded-xl border p-3" style="border-color: var(--border); background: var(--surface-2)">
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Feels downstairs</div>
            <div class="text-2xl font-bold" style="color: var(--text)">{Number(cbFeelsDown.state).toFixed(1)}°F</div>
            {#if rh(cbFeelsDown) != null}<div class="text-xs" style="color: var(--text-muted)">{Number(rh(cbFeelsDown)).toFixed(0)}% RH</div>{/if}
          </div>
        {/if}
        {#if cbComfort}
          <div class="rounded-xl border p-3" style="border-color: var(--border); background: var(--surface-2)">
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Learned comfort</div>
            <div class="text-2xl font-bold" style="color: var(--text)">{Number(cbComfort.state).toFixed(0)}°F</div>
            <div class="text-xs" style="color: var(--text-muted)">from your taps</div>
          </div>
        {/if}
        {#if cbLoad}
          <div class="rounded-xl border p-3" style="border-color: var(--border); background: var(--surface-2)">
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Heat load</div>
            <div class="text-2xl font-bold" style="color: var(--text)">{Number(cbLoad.state).toFixed(1)}°F</div>
            <div class="text-xs" style="color: var(--text-muted)">outdoor − upstairs</div>
          </div>
        {/if}
      </div>

      {#if cbPrecool}
        <div class="mt-3 rounded-xl border p-3 flex items-center gap-3"
          style="border-color: {precoolActive ? '#3b82f6' : 'var(--border)'}; background: var(--surface-2)">
          <span class="text-lg">☀️❄️</span>
          <div>
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Solar pre-cool</div>
            <div class="text-sm" style="color: var(--text)">{cbPrecool.state}</div>
          </div>
        </div>
      {/if}

      {#if cbAttic}
        <div class="mt-3 rounded-xl border p-3 flex items-center gap-3" style="border-color: var(--border); background: var(--surface-2)">
          <span class="text-lg">🏠🌀</span>
          <div>
            <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">Attic-fan experiment</div>
            <div class="text-sm" style="color: var(--text)">{cbAttic.state}</div>
          </div>
        </div>
      {/if}
    </section>
  {/if}

  <!-- Fans -->
  <section class="rounded-2xl border p-6" style="background: var(--surface); border-color: var(--border)">
    <h2 class="text-lg font-bold mb-4" style="color: var(--text)">🌀 Fans</h2>

    {#if hvacBlower}
      <div class="flex items-center justify-between py-2 border-b" style="border-color: var(--border)">
        <div>
          <div style="color: var(--text)">HVAC Blower</div>
          <div class="text-xs" style="color: var(--text-muted)">Driven by the thermostat (runs with cooling)</div>
        </div>
        <span class="text-sm font-semibold px-3 py-1 rounded-full"
          style="background: var(--surface-2); color: {hvacBlower.state === 'on' ? '#14b8a6' : 'var(--text-muted)'}">
          {hvacBlower.state === 'on' ? 'ON' : 'off'}
        </span>
      </div>
    {/if}

    {#if ceilingFans.length}
      <div class="text-xs uppercase tracking-wide mt-4 mb-2" style="color: var(--text-muted)">Ceiling fans</div>
      <div class="grid gap-3" style="grid-template-columns: repeat(auto-fill, minmax(15rem, 1fr))">
        {#each ceilingFans as f}
          <div class="rounded-xl border p-3" style="border-color: var(--border); background: var(--surface-2)">
            <div class="mb-2 capitalize" style="color: var(--text)">{prettyDev(f)}</div>
            <div class="flex gap-1">
              {#each SPEEDS as [val, label]}
                <button onclick={() => setFanSpeed(f.id, Number(val))}
                  class="flex-1 py-1.5 rounded-md text-sm border transition-colors"
                  style="border-color: var(--border);
                         background: {String(f.state) === val ? 'var(--accent)' : 'var(--surface)'};
                         color: {String(f.state) === val ? '#fff' : 'var(--text)'}">
                  {label}
                </button>
              {/each}
            </div>
          </div>
        {/each}
      </div>
    {/if}

    {#if exhaustFans.length}
      <div class="text-xs uppercase tracking-wide mt-4 mb-2" style="color: var(--text-muted)">Exhaust fans</div>
      <div class="grid gap-3" style="grid-template-columns: repeat(auto-fill, minmax(15rem, 1fr))">
        {#each exhaustFans as f}
          <button onclick={() => toggleSwitch(f)}
            class="flex items-center justify-between rounded-xl border p-3 text-left"
            style="border-color: var(--border); background: var(--surface-2)">
            <span class="capitalize" style="color: var(--text)">{prettyDev(f)}</span>
            <span class="text-sm font-semibold px-3 py-1 rounded-full"
              style="background: var(--surface); color: {f.state === 'on' ? '#14b8a6' : 'var(--text-muted)'}">
              {f.state === 'on' ? 'ON' : 'off'}
            </span>
          </button>
        {/each}
      </div>
    {/if}

    {#if !ceilingFans.length && !exhaustFans.length && !hvacBlower}
      <p style="color: var(--text-muted)">No fans found.</p>
    {/if}
  </section>
</div>
