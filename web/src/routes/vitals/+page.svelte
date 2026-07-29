<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { fetchEntities, connectWS, type Entity } from '$lib/api'

  // Sticky cache so the tab never flashes empty on an HF restart / WS reconnect.
  let cache = $state<Record<string, Entity>>({})
  let disconnect: (() => void) | null = null
  let now = $state(Date.now())
  let base = $state('') // this HomeForge's own origin, for the setup-URL help

  const absorb = (e: Entity) => { if (e.id.startsWith('sensor.health_')) cache[e.id] = e }
  onMount(async () => {
    base = location.origin
    for (const e of await fetchEntities()) absorb(e)
    disconnect = connectWS((msg) => {
      if (msg.type === 'snapshot' && msg.entities) { for (const e of msg.entities) absorb(e) }
      else if (msg.type === 'state_changed' && msg.entity) absorb(msg.entity)
    })
    const t = setInterval(() => (now = Date.now()), 30000)
    return () => clearInterval(t)
  })
  onDestroy(() => disconnect?.())

  const attr = (e: Entity | undefined, k: string) => (e?.attributes as any)?.[k]
  const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)
  const metricOf = (e: Entity) => (attr(e, 'metric') as string) ?? e.id.replace(/^sensor\.health_/, '')

  type Person = { slug: string; name: string; figure: string; ents: Entity[] }
  const people = $derived.by(() => {
    const m = new Map<string, Entity[]>()
    for (const e of Object.values(cache)) {
      if (!e.id.startsWith('sensor.health_')) continue
      const slug = (attr(e, 'person') as string) ?? 'unknown'
      const l = m.get(slug) ?? []
      l.push(e)
      m.set(slug, l)
    }
    const out: Person[] = []
    for (const [slug, ents] of m) {
      out.push({ slug, name: (attr(ents[0], 'person_name') as string) ?? cap(slug), figure: (attr(ents[0], 'person_figure') as string) ?? 'man', ents })
    }
    return out.sort((a, b) => a.name.localeCompare(b.name))
  })

  const ICONS: Record<string, string> = {
    weight: '⚖️', bmi: '📏', bp: '🩸', blood: '🩸', systolic: '🩸', diastolic: '🩸',
    heart: '❤️', hr: '❤️', pulse: '❤️', steps: '👣', spo2: '🫁', oxygen: '🫁',
    sleep: '😴', temp: '🌡️', fat: '🥓', calorie: '🔥', glucose: '🩸', hrv: '🧠',
    distance: '📍', respiratory: '🫁', hydration: '💧', workout: '🏃',
  }
  const iconFor = (metric: string) => { const n = metric.toLowerCase(); for (const k in ICONS) if (n.includes(k)) return ICONS[k]; return '•' }
  const label = (metric: string) => metric.replace(/_/g, ' ').replace(/\bbp\b/, 'blood pressure').replace(/\bhrv\b/, 'HRV').replace(/\bbmi\b/, 'BMI').replace(/\bspo2\b/, 'SpO₂')
  const unit = (e: Entity) => (attr(e, 'unit_of_measurement') as string) ?? ''
  const updated = (e: Entity | undefined) => (attr(e, 'updated') as string) ?? ''

  type Tile = { key: string; icon: string; value: string; unit: string; label: string; upd: string }
  function tiles(p: Person): Tile[] {
    const by = new Map<string, Entity>()
    for (const e of p.ents) by.set(metricOf(e), e)
    const out: Tile[] = []
    const sys = by.get('bp_systolic'), dia = by.get('bp_diastolic')
    if (sys && dia) {
      out.push({ key: 'bp', icon: '🩸', value: `${sys.state}/${dia.state}`, unit: 'mmHg', label: 'blood pressure', upd: updated(sys) })
      by.delete('bp_systolic'); by.delete('bp_diastolic')
    }
    for (const [metric, e] of [...by].sort((a, b) => a[0].localeCompare(b[0]))) {
      out.push({ key: metric, icon: iconFor(metric), value: String(e.state), unit: unit(e), label: label(metric), upd: updated(e) })
    }
    return out
  }

  // ── radial layout ──
  // Person sits on the LEFT; metric bubbles ring around them over an arc that opens toward the
  // figure. One ring for a few metrics, two concentric rings for many. On a narrow screen the
  // whole thing reflows into a centered bubble grid (radial clips on a phone).
  let containerW = $state(0)
  const RH = 440
  const mobile = $derived(containerW > 0 && containerW < 460)

  function rings(n: number): [number, number][] {
    return n <= 9 ? [[n, 170]] : [[Math.floor(n * 0.42), 112], [n - Math.floor(n * 0.42), 200]]
  }
  function radialPos(i: number, n: number, W: number) {
    const cx = W * 0.30, cy = RH / 2, A0 = -150, A1 = 150
    let idx = 0
    for (const [count, R] of rings(n)) {
      if (i < idx + count) {
        const j = i - idx
        const ang = (A0 + ((j + 0.5) / count) * (A1 - A0)) * Math.PI / 180
        return { x: cx + R * Math.cos(ang), y: cy + R * Math.sin(ang) }
      }
      idx += count
    }
    return { x: cx, y: cy }
  }
</script>

<div class="wrap flex flex-col gap-6">
  <h1 class="text-xl font-bold" style="color: var(--text)">Health</h1>

  {#if people.length === 0}
    <div class="rounded-xl p-6 border text-sm" style="border-color: var(--border); background: var(--surface); color: var(--text-muted)">
      <p class="mb-2" style="color: var(--text)">No health data yet.</p>
      <p>A phone reads Health Connect on-device and POSTs to HomeForge. See the setup guide below.</p>
    </div>
  {/if}

  {#snippet bubbleC(t: { icon: string; value: string; unit: string; label: string })}
    <div class="ic">{t.icon}</div>
    <div class="val">{t.value}<span class="u">{t.unit}</span></div>
    <div class="lbl">{t.label}</div>
  {/snippet}

  {#snippet silhouette(kind: string)}
    <svg viewBox="0 0 100 220" width="54" height="119" style="fill: var(--accent); opacity: 0.92" aria-hidden="true">
      {#if kind === 'woman'}
        <circle cx="50" cy="22" r="15" />
        <rect x="23" y="42" width="9" height="52" rx="4.5" transform="rotate(11 27.5 68)" />
        <rect x="68" y="42" width="9" height="52" rx="4.5" transform="rotate(-11 72.5 68)" />
        <path d="M42 39 L58 39 Q64 39 65 51 L79 126 L21 126 L35 51 Q36 39 42 39 Z" />
        <rect x="44" y="123" width="10" height="82" rx="5" /><rect x="55" y="123" width="10" height="82" rx="5" />
      {:else if kind === 'child'}
        <circle cx="50" cy="34" r="15" />
        <rect x="29" y="52" width="9" height="44" rx="4.5" transform="rotate(9 33.5 74)" />
        <rect x="62" y="52" width="9" height="44" rx="4.5" transform="rotate(-9 66.5 74)" />
        <path d="M40 50 L60 50 Q66 50 65 59 L62 106 Q61 111 56 111 L44 111 Q39 111 38 106 L35 59 Q34 50 40 50 Z" />
        <rect x="41" y="108" width="9" height="56" rx="4.5" /><rect x="51" y="108" width="9" height="56" rx="4.5" />
      {:else}
        <circle cx="50" cy="21" r="16" />
        <rect x="19" y="41" width="11" height="62" rx="5.5" transform="rotate(8 24.5 72)" />
        <rect x="70" y="41" width="11" height="62" rx="5.5" transform="rotate(-8 75.5 72)" />
        <path d="M33 39 L67 39 Q73 39 72 49 L67 110 Q66 116 60 116 L40 116 Q34 116 33 110 L28 49 Q27 39 33 39 Z" />
        <rect x="37" y="112" width="12" height="94" rx="6" /><rect x="51" y="112" width="12" height="94" rx="6" />
      {/if}
    </svg>
  {/snippet}

  {#each people as p (p.slug)}
    {@const ts = tiles(p)}
    <section class="pcard rounded-2xl border" style="background: var(--surface); border-color: var(--border)">
      <div class="flex items-center gap-2 mb-1">
        <span class="text-base">🧑</span>
        <h2 class="text-lg font-bold" style="color: var(--text)">{p.name}</h2>
        <span class="text-xs" style="color: var(--text-muted)">· {ts.length} metrics</span>
      </div>

      <div class="radial" class:gridmode={mobile} bind:clientWidth={containerW} style={mobile ? '' : `height:${RH}px`}>
        <div class="figure" style={mobile ? '' : `left:${containerW * 0.30}px; top:${RH / 2}px; transform:translate(-50%,-50%)`}>
          {@render silhouette(p.figure)}
        </div>
        {#each ts as t, i}
          {@const pos = mobile ? null : radialPos(i, ts.length, containerW)}
          <div class="bubble" style={pos ? `left:${pos.x}px; top:${pos.y}px` : ''}>{@render bubbleC(t)}</div>
        {/each}
      </div>
    </section>
  {/each}

  <details class="rounded-xl border text-sm" style="border-color: var(--border); background: var(--surface); max-width: 680px">
    <summary class="cursor-pointer px-4 py-3 font-medium" style="color: var(--text)">📱 Set up / add a person</summary>
    <div class="px-4 pb-4" style="color: var(--text-muted)">
      <p class="mb-2">Each person pushes from their own phone (Health Connect → HC Webhook app). Set the webhook URL to:</p>
      <p class="mb-1"><strong>Bo</strong> (default, drawn as a man):</p>
      <pre class="p-3 rounded text-xs mb-2" style="background: var(--surface-2); overflow-x: auto; color: var(--text)">{base}/api/health</pre>
      <p class="mb-1"><strong>Anyone else</strong> — add <code>?person=Name&figure=woman</code> (figure = man / woman / child):</p>
      <pre class="p-3 rounded text-xs" style="background: var(--surface-2); overflow-x: auto; color: var(--text)">{base}/api/health?person=Name&figure=woman</pre>
    </div>
  </details>
</div>

<style>
  .wrap { width: 100%; max-width: 1024px; margin: 0 auto; }
  .pcard { padding: 16px 20px; max-width: 680px; }
  @media (max-width: 640px) { .pcard { padding: 12px; } }

  .radial { position: relative; width: 100%; height: 440px; margin: 0; }
  .radial.gridmode {
    position: static; height: auto; display: grid;
    grid-template-columns: repeat(auto-fit, minmax(84px, 1fr)); gap: 10px; place-items: center;
  }
  .figure { position: absolute; display: flex; align-items: center; justify-content: center; }
  .radial.gridmode .figure { position: static; transform: none; grid-column: 1 / -1; margin-bottom: 2px; }

  .bubble {
    position: absolute; width: 74px; height: 74px; border-radius: 50%;
    background: var(--surface-2); border: 1px solid var(--border);
    display: flex; flex-direction: column; align-items: center; justify-content: center;
    text-align: center; box-shadow: 0 2px 6px rgba(0, 0, 0, 0.25); transform: translate(-50%, -50%);
  }
  .radial.gridmode .bubble { position: static; transform: none; width: 84px; height: 84px; }
  .bubble .ic { font-size: 15px; line-height: 1; }
  .bubble .val { font-size: 13px; font-weight: 700; line-height: 1.05; margin-top: 1px; color: var(--text); }
  .bubble .val .u { font-size: 8px; color: var(--text-muted); margin-left: 1px; }
  .bubble .lbl { font-size: 7.5px; color: var(--text-muted); line-height: 1.05; margin-top: 1px; text-transform: capitalize; max-width: 66px; }
</style>
