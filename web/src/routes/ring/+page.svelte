<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { fetchEntities, connectWS, callService, type Entity } from '$lib/api'

  let ents = $state<Record<string, Entity>>({})
  let disconnect: (() => void) | null = null
  let connected = $state(false)

  onMount(async () => {
    for (const e of await fetchEntities()) ents[e.id] = e
    disconnect = connectWS((msg) => {
      connected = true
      if (msg.type === 'snapshot' && msg.entities) {
        const n: Record<string, Entity> = {}
        for (const e of msg.entities) n[e.id] = e
        ents = n
      } else if (msg.type === 'state_changed' && msg.entity) {
        ents[msg.entity.id] = msg.entity
      }
    })
  })
  onDestroy(() => disconnect?.())

  const ring = $derived(Object.values(ents).filter((e) => (e.attributes as any)?.source === 'ring'))
  const alarm = $derived(ring.find((e) => e.domain === 'alarm_control_panel'))

  type Dev = { dev: string; title: string; es: Entity[] }
  const devices = $derived.by(() => {
    const m = new Map<string, Entity[]>()
    for (const e of ring) {
      if (e.domain === 'alarm_control_panel') continue // shown separately at top
      const d = ((e.attributes as any).device as string) || 'ring'
      const l = m.get(d) ?? []
      l.push(e)
      m.set(d, l)
    }
    const out: Dev[] = []
    for (const [dev, es] of m) {
      out.push({ dev, title: titleCase(dev), es: es.slice().sort((a, b) => rank(a) - rank(b)) })
    }
    return out.sort((a, b) => a.title.localeCompare(b.title))
  })

  function titleCase(s: string) {
    return s.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase())
  }
  const attr = (e: Entity, k: string) => (e.attributes as any)?.[k]
  function isContact(e: Entity): boolean {
    const dc = String(attr(e, 'device_class') ?? '')
    return e.domain === 'binary_sensor' && ['door', 'window', 'garage_door', 'opening'].includes(dc)
  }
  // ordering: primary status first, diagnostics (battery/wifi/tamper/info) last
  function rank(e: Entity): number {
    const id = e.id
    if (e.domain === 'lock' || isContact(e)) return 0
    if (id.includes('_ding')) return 1
    if (id.includes('motion')) return 2
    if (id.includes('siren')) return 3
    if (id.includes('_battery')) return 7
    if (id.includes('_wireless')) return 8
    if (id.includes('_tamper')) return 9
    if (id.includes('_info')) return 10
    return 5
  }

  type Disp = { icon: string; text: string; tone: 'alert' | 'ok' | 'warn' | 'muted' | 'accent' }
  function disp(e: Entity): Disp {
    const s = (e.state ?? '').toLowerCase()
    const id = e.id
    if (e.domain === 'lock') return s === 'locked' ? { icon: '🔒', text: 'Locked', tone: 'ok' } : { icon: '🔓', text: 'Unlocked', tone: 'warn' }
    if (id.includes('_tamper')) return s === 'on' || s === 'tamper' ? { icon: '⚠️', text: 'Tamper', tone: 'alert' } : { icon: '✓', text: 'OK', tone: 'muted' }
    if (id.includes('_ding')) return s === 'on' ? { icon: '🔔', text: 'Ding!', tone: 'accent' } : { icon: '🔔', text: 'Idle', tone: 'muted' }
    if (id.includes('motion')) return s === 'on' ? { icon: '🏃', text: 'Motion', tone: 'alert' } : { icon: '·', text: 'Clear', tone: 'muted' }
    if (isContact(e)) return s === 'on' ? { icon: '🚪', text: 'Open', tone: 'alert' } : { icon: '🚪', text: 'Closed', tone: 'ok' }
    if (id.includes('_battery')) { const n = Number(e.state); return { icon: '🔋', text: `${e.state}%`, tone: n > 0 && n < 20 ? 'alert' : 'muted' } }
    if (id.includes('_wireless')) return { icon: '📶', text: `${e.state} dBm`, tone: 'muted' }
    if (id.includes('siren') || id.includes('motion_detection') || id.includes('_chirps') || id.includes('stream')) return s === 'on' ? { icon: '●', text: 'On', tone: 'accent' } : { icon: '○', text: 'Off', tone: 'muted' }
    if (e.domain === 'binary_sensor') return s === 'on' ? { icon: '●', text: 'On', tone: 'alert' } : { icon: '○', text: 'Off', tone: 'muted' }
    return { icon: '', text: `${e.state}${attr(e, 'unit_of_measurement') ?? ''}`, tone: 'muted' }
  }
  const compLabel = (e: Entity) => {
    const fn = String(attr(e, 'friendly_name') ?? e.id)
    const dev = titleCase((attr(e, 'device') as string) ?? '')
    const l = fn.startsWith(dev) ? fn.slice(dev.length).trim() : fn
    return l || 'Status'
  }
  const TONE: Record<string, string> = {
    alert: '#ef4444', ok: '#22c55e', warn: '#f59e0b', accent: 'var(--accent)', muted: 'var(--text-muted)',
  }
  const alarmTone = (s: string) => (s === 'disarmed' ? 'muted' : 'alert')

  // ── control ──
  const setAlarm = (svc: string) => alarm && callService('alarm_control_panel', svc, alarm.id)
  const ARM_MODES: [string, string][] = [['alarm_disarm', 'Disarm'], ['alarm_arm_home', 'Home'], ['alarm_arm_away', 'Away']]
  const activeArm = (s: string) => (s === 'disarmed' ? 'alarm_disarm' : s === 'armed_home' ? 'alarm_arm_home' : s === 'armed_away' ? 'alarm_arm_away' : '')
  function toggleEntity(e: Entity) {
    if (e.domain === 'lock') callService('lock', e.state === 'locked' ? 'unlock' : 'lock', e.id)
    else if (e.domain === 'switch') callService('switch', e.state === 'on' ? 'turn_off' : 'turn_on', e.id)
  }
  const clickable = (e: Entity) => (e.domain === 'lock' || e.domain === 'switch') && !!(e.attributes as any)?.command_topic
</script>

<svelte:head><title>Ring · HomeForge</title></svelte:head>

<div class="max-w-6xl mx-auto flex flex-col gap-6">
  <div class="flex items-center gap-3">
    <h1 class="text-2xl font-bold" style="color: var(--text)">Ring</h1>
    <span class="text-xs px-2 py-0.5 rounded-full" style="background: var(--surface-2); color: {connected ? '#22c55e' : 'var(--text-muted)'}">
      {connected ? '● live' : '○ connecting'}
    </span>
  </div>

  {#if ring.length === 0}
    <div class="rounded-xl p-6 border text-sm" style="border-color: var(--border); background: var(--surface); color: var(--text-muted)">
      <p style="color: var(--text)">No Ring devices yet.</p>
      <p class="mt-1">The ring-mqtt bridge publishes on connect. If this stays empty, restart it: <code>docker restart ring-mqtt</code> (discovery is non-retained, so it re-seeds on reconnect).</p>
    </div>
  {:else}
    {#if alarm}
      <section class="rounded-2xl border p-5 flex items-center gap-4 flex-wrap" style="background: var(--surface); border-color: var(--border)">
        <span class="text-3xl">🛡️</span>
        <div>
          <div class="text-xs uppercase tracking-wide" style="color: var(--text-muted)">{titleCase((attr(alarm, 'device') as string) ?? 'Alarm')}</div>
          <div class="text-2xl font-bold capitalize" style="color: {TONE[alarmTone(alarm.state)]}">{alarm.state.replace(/_/g, ' ')}</div>
        </div>
        <div class="flex gap-2 ml-auto">
          {#each ARM_MODES as [svc, label]}
            {@const on = activeArm(alarm.state) === svc}
            <button onclick={() => setAlarm(svc)}
              class="px-4 py-2 rounded-lg text-sm font-medium border transition-colors"
              style="border-color: {on ? 'var(--accent)' : 'var(--border)'};
                     background: {on ? 'var(--accent)' : 'var(--surface-2)'};
                     color: {on ? '#fff' : 'var(--text)'}">{label}</button>
          {/each}
        </div>
      </section>
    {/if}

    <div class="grid gap-4" style="grid-template-columns: repeat(auto-fill, minmax(17rem, 1fr))">
      {#each devices as d (d.dev)}
        <section class="rounded-2xl border p-4" style="background: var(--surface); border-color: var(--border)">
          <h2 class="text-sm font-bold mb-3" style="color: var(--text)">{d.title}</h2>
          <div class="flex flex-col gap-1.5">
            {#each d.es as e (e.id)}
              {@const dd = disp(e)}
              <div class="flex items-center justify-between gap-3 text-sm">
                <span style="color: var(--text-muted)">{compLabel(e)}</span>
                {#if clickable(e)}
                  <button onclick={() => toggleEntity(e)} class="font-medium whitespace-nowrap px-2 py-0.5 rounded-md border"
                    style="color: {TONE[dd.tone]}; border-color: var(--border); background: var(--surface-2)">{dd.icon} {dd.text}</button>
                {:else}
                  <span class="font-medium whitespace-nowrap" style="color: {TONE[dd.tone]}">{dd.icon} {dd.text}</span>
                {/if}
              </div>
            {/each}
          </div>
        </section>
      {/each}
    </div>
  {/if}
</div>
