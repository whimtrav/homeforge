<script lang="ts">
  import { onMount } from 'svelte'
  // Calendar event browser. Days with events get a dot (cheap existence check); clicking a day
  // fetches just that day's events (bounded), then camera / type / time-range filters narrow the
  // search. Tap an event for its snapshot or clip. HomeForge proxies Sentinel's events + media.
  type Ev = {
    id: string; camera: string; label: string; sub_label: string | null
    score: number; start_time: number; has_clip: boolean; has_snapshot: boolean; false_positive: boolean
  }

  const today = new Date()
  let year = $state(today.getFullYear())
  let month = $state(today.getMonth())
  let dots = $state<Set<string>>(new Set()) // 'YYYY-MM-DD' days that have events
  let dayEvents = $state<Ev[]>([])
  let selKey = $state('')
  let cam = $state('all')
  let label = $state('all')
  let clipsOnly = $state(false)
  let from = $state('00:00')
  let to = $state('23:59')
  let sel = $state<Ev | null>(null)
  let err = $state('')
  let loading = $state(false)

  const pad = (n: number) => String(n).padStart(2, '0')
  const keyUnix = (k: string) => Math.floor(new Date(k + 'T00:00:00').getTime() / 1000)
  const evDate = (e: Ev) => new Date(e.start_time * 1000)
  const evMin = (e: Ev) => { const d = evDate(e); return d.getHours() * 60 + d.getMinutes() }
  const tMin = (s: string) => { const [h, m] = s.split(':').map(Number); return h * 60 + m }
  const pretty = (n: string) => n.replace(/[_-]/g, ' ')
  const emoji = (l: string) => (({ person: '🧍', car: '🚗', truck: '🚚', dog: '🐕', cat: '🐈', bicycle: '🚲', bird: '🐦' }) as Record<string, string>)[l] || '⬤'
  const clockOf = (e: Ev) => { const d = evDate(e); let h = d.getHours(); const ap = h < 12 ? 'a' : 'p'; h = h % 12 || 12; return `${h}:${pad(d.getMinutes())}${ap}` }
  const snap = (id: string) => `/api/events/${encodeURIComponent(id)}/snapshot`
  const clip = (id: string) => `/api/events/${encodeURIComponent(id)}/clip`

  async function loadDots() {
    err = ''
    const dim = new Date(year, month + 1, 0).getDate()
    const checks: Promise<string | null>[] = []
    for (let d = 1; d <= dim; d++) {
      const k = `${year}-${pad(month + 1)}-${pad(d)}`
      const a = keyUnix(k), b = a + 86400
      checks.push(
        fetch(`/api/events?after=${a}&before=${b}&limit=1`)
          .then((r) => (r.ok ? r.json() : []))
          .then((l) => (Array.isArray(l) && l.length ? k : null))
          .catch(() => null)
      )
    }
    const res = await Promise.all(checks)
    dots = new Set(res.filter((x): x is string => !!x))
    // default day: today if it has events, else the latest day that does
    const tKey = `${today.getFullYear()}-${pad(today.getMonth() + 1)}-${pad(today.getDate())}`
    let def = ''
    if (year === today.getFullYear() && month === today.getMonth() && dots.has(tKey)) def = tKey
    else { const ds = [...dots].sort(); def = ds.length ? ds[ds.length - 1] : '' }
    if (def) pickDay(def); else { selKey = ''; dayEvents = [] }
  }

  async function loadDay(k: string) {
    loading = true
    const a = keyUnix(k), b = a + 86400
    try {
      const r = await fetch(`/api/events?after=${a}&before=${b}&limit=5000`)
      dayEvents = r.ok ? await r.json() : []
    } catch { dayEvents = []; err = 'Could not load that day.' }
    loading = false
  }
  function pickDay(k: string) { selKey = k; cam = 'all'; label = 'all'; from = '00:00'; to = '23:59'; loadDay(k) }
  function shiftMonth(delta: number) {
    let m = month + delta, y = year
    if (m < 0) { m = 11; y-- } else if (m > 11) { m = 0; y++ }
    month = m; year = y; loadDots()
  }
  onMount(loadDots)

  const cells = $derived.by(() => {
    const lead = new Date(year, month, 1).getDay()
    const dim = new Date(year, month + 1, 0).getDate()
    const out: { day: number; key: string; has: boolean }[] = []
    for (let i = 0; i < lead; i++) out.push({ day: 0, key: '', has: false })
    for (let d = 1; d <= dim; d++) { const k = `${year}-${pad(month + 1)}-${pad(d)}`; out.push({ day: d, key: k, has: dots.has(k) }) }
    return out
  })
  const cams = $derived(['all', ...Array.from(new Set(dayEvents.map((e) => e.camera))).sort()])
  const labels = $derived(Array.from(new Set(dayEvents.map((e) => e.label))).sort())
  const shown = $derived(
    dayEvents
      .filter((e) => (cam === 'all' || e.camera === cam) && (label === 'all' || e.label === label) && (!clipsOnly || e.has_clip))
      .filter((e) => evMin(e) >= tMin(from) && evMin(e) <= tMin(to))
      .sort((a, b) => b.start_time - a.start_time)
  )
  const selLabel = $derived(selKey ? new Date(selKey + 'T00:00').toLocaleDateString(undefined, { weekday: 'long', month: 'long', day: 'numeric' }) : '')
  const MON = ['January','February','March','April','May','June','July','August','September','October','November','December']
</script>

<svelte:head><title>HomeForge · Events</title></svelte:head>
<svelte:window onkeydown={(e) => { if (e.key === 'Escape') sel = null }} />

<div class="wrap">
  <div class="top"><h1>Events</h1><a class="nvr" href="/nvr/" target="_blank" rel="noopener">Open full NVR ↗</a></div>
  {#if err}<div class="err">{err}</div>{/if}

  <div class="layout">
    <div class="card cal">
      <div class="ch"><button onclick={() => shiftMonth(-1)} aria-label="Previous month">‹</button><span class="mo">{MON[month]} {year}</span><button onclick={() => shiftMonth(1)} aria-label="Next month">›</button></div>
      <div class="dow">{#each ['Su','Mo','Tu','We','Th','Fr','Sa'] as d}<span>{d}</span>{/each}</div>
      <div class="days">
        {#each cells as c}
          {#if c.day === 0}<div class="day empty"></div>
          {:else}
            <button class="day" class:has={c.has} class:sel={c.key === selKey} onclick={() => pickDay(c.key)}>
              {c.day}{#if c.has}<span class="dot"></span>{/if}
            </button>
          {/if}
        {/each}
      </div>
    </div>

    <div class="card panel">
      {#if !selKey}
        <p class="muted">Pick a day with a dot to see its events.</p>
      {:else}
        <div class="ph">{selLabel} · <span class="muted">{loading ? 'loading…' : `${shown.length} event${shown.length === 1 ? '' : 's'}`}</span></div>
        <div class="filters">
          {#each cams as c}<button class="chip" class:active={cam === c} onclick={() => (cam = c)}>{c === 'all' ? 'All cams' : pretty(c)}</button>{/each}
          {#each labels as l}<button class="chip" class:active={label === l} onclick={() => (label = label === l ? 'all' : l)}>{emoji(l)} {l}</button>{/each}
          <button class="chip" class:active={clipsOnly} onclick={() => (clipsOnly = !clipsOnly)}>▶ clips</button>
          <div class="time"><span>from</span><input type="time" bind:value={from} /><span>to</span><input type="time" bind:value={to} /></div>
        </div>
        <div class="evgrid">
          {#each shown as e (e.id)}
            <button class="ev" class:fp={e.false_positive} onclick={() => (sel = e)}>
              <img src={snap(e.id)} alt={e.label} loading="lazy" />
              {#if e.has_clip}<span class="pl">▶</span>{/if}
              <div class="m"><div class="t">{emoji(e.label)} {e.sub_label || e.label}</div><div class="c">{pretty(e.camera)} · {clockOf(e)}</div></div>
            </button>
          {/each}
        </div>
        {#if !loading && shown.length === 0}<p class="muted">No events match these filters.</p>{/if}
      {/if}
    </div>
  </div>
</div>

{#if sel}
  <div class="overlay" onclick={(e) => { if (e.target === e.currentTarget) sel = null }} role="presentation">
    <div class="viewer">
      {#if sel.has_clip}
        <!-- svelte-ignore a11y_media_has_caption -->
        <video src={clip(sel.id)} poster={snap(sel.id)} controls playsinline preload="metadata"></video>
      {:else}<img src={snap(sel.id)} alt={sel.label} />{/if}
      <div class="vmeta"><span><strong>{emoji(sel.label)} {sel.sub_label || sel.label}</strong> · {pretty(sel.camera)} · {clockOf(sel)}</span><button class="close" onclick={() => (sel = null)}>Close</button></div>
    </div>
  </div>
{/if}

<style>
  .wrap { width: 100%; max-width: 1120px; margin: 0 auto; }
  .top { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
  h1 { font-size: 20px; font-weight: 700; color: var(--text); }
  .nvr { font-size: 13px; color: var(--accent); text-decoration: none; white-space: nowrap; }
  .err { background: rgba(239,68,68,.12); color:#fca5a5; border:1px solid rgba(239,68,68,.3); border-radius:8px; padding:8px 10px; font-size:13px; margin-bottom:12px; }
  .muted { color: var(--text-muted); font-size: 14px; }
  .layout { display: grid; grid-template-columns: 300px 1fr; gap: 16px; }
  @media (max-width: 720px) { .layout { grid-template-columns: 1fr; } }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 14px; }
  .cal { align-self: start; }
  .ch { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; }
  .ch button { background: var(--surface-2); border: 1px solid var(--border); color: var(--text); border-radius: 8px; width: 30px; height: 30px; cursor: pointer; font-size: 15px; }
  .ch .mo { font-weight: 600; font-size: 14px; color: var(--text); }
  .dow { display: grid; grid-template-columns: repeat(7,1fr); gap: 4px; margin-bottom: 4px; }
  .dow span { text-align: center; font-size: 10px; color: var(--text-muted); }
  .days { display: grid; grid-template-columns: repeat(7,1fr); gap: 4px; }
  .day { aspect-ratio: 1; border-radius: 8px; border: 1px solid transparent; background: transparent; color: var(--text-muted); font-size: 12px; position: relative; padding: 5px 6px; text-align: left; cursor: default; }
  .day.has { color: var(--text); background: var(--surface-2); cursor: pointer; }
  .day.sel { border-color: var(--accent); color: #fff; background: var(--surface-3); }
  .day .dot { position: absolute; bottom: 5px; right: 6px; width: 5px; height: 5px; border-radius: 50%; background: var(--accent-hover); }
  .ph { font-weight: 600; font-size: 15px; color: var(--text); margin-bottom: 12px; }
  .filters { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 14px; }
  .chip { padding: 4px 11px; border-radius: 999px; border: 1px solid var(--border); background: var(--surface); color: var(--text-muted); font-size: 12px; cursor: pointer; text-transform: capitalize; }
  .chip.active { background: var(--surface-2); color: var(--text); border-color: var(--accent); }
  .time { display: flex; align-items: center; gap: 6px; margin-left: auto; font-size: 12px; color: var(--text-muted); }
  .time input { background: var(--surface-2); border: 1px solid var(--border); color: var(--text); border-radius: 7px; padding: 4px 6px; font-size: 12px; }
  .evgrid { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 10px; }
  .ev { padding:0; border:1px solid var(--border); border-radius:10px; overflow:hidden; background:#000; position:relative; aspect-ratio:16/9; cursor:pointer; text-align:left; }
  .ev.fp { opacity:.5; }
  .ev img { width:100%; height:100%; object-fit:cover; display:block; }
  .ev .pl { position:absolute; top:5px; right:6px; font-size:10px; background:rgba(0,0,0,.55); color:#fff; border-radius:50%; width:20px; height:20px; display:flex; align-items:center; justify-content:center; }
  .ev .m { position:absolute; left:0; right:0; bottom:0; padding:4px 6px; background:linear-gradient(transparent, rgba(0,0,0,.8)); }
  .ev .m .t { color:#fff; font-size:11px; font-weight:600; text-transform:capitalize; text-shadow:0 1px 2px rgba(0,0,0,.8); }
  .ev .m .c { color:#94a3b8; font-size:10px; text-transform:capitalize; }
  .overlay { position:fixed; inset:0; z-index:50; background:rgba(0,0,0,.92); display:flex; align-items:center; justify-content:center; padding:12px; }
  .viewer { display:flex; flex-direction:column; gap:10px; max-width:900px; width:100%; align-items:center; }
  .viewer video, .viewer img { max-width:100%; max-height:80vh; border-radius:8px; background:#000; }
  .vmeta { display:flex; align-items:center; justify-content:space-between; gap:12px; width:100%; color:#e2e8f0; font-size:14px; }
  .close { background:var(--surface-2); color:var(--text); border:1px solid var(--border); border-radius:8px; padding:6px 14px; font-size:13px; cursor:pointer; }
</style>
