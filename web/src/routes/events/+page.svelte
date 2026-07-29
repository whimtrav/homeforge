<script lang="ts">
  import { onMount } from 'svelte'
  // Native events view: HomeForge proxies Sentinel's events + snapshots + clips (all behind the
  // login). Grid of detections; tap one to see the snapshot or play its clip.
  type Ev = {
    id: string; camera: string; label: string; sub_label: string | null
    score: number; start_time: number; has_clip: boolean; has_snapshot: boolean
    false_positive: boolean
  }
  let events = $state<Ev[]>([])
  let sel = $state<Ev | null>(null)
  let err = $state('')
  let cam = $state('all')
  let now = $state(Date.now())

  async function load() {
    try {
      const r = await fetch('/api/events?limit=150')
      if (r.ok) {
        const d = await r.json()
        events = (Array.isArray(d) ? d : []).sort((a: Ev, b: Ev) => b.start_time - a.start_time)
        err = ''
      } else err = 'Could not load events.'
    } catch { err = 'Could not reach HomeForge.' }
  }
  onMount(() => {
    load()
    const t = setInterval(() => { now = Date.now(); load() }, 15000)
    return () => clearInterval(t)
  })

  const cams = $derived(['all', ...Array.from(new Set(events.map((e) => e.camera))).sort()])
  const shown = $derived(cam === 'all' ? events : events.filter((e) => e.camera === cam))
  const snap = (id: string) => `/api/events/${encodeURIComponent(id)}/snapshot`
  const clip = (id: string) => `/api/events/${encodeURIComponent(id)}/clip`
  const pretty = (n: string) => n.replace(/[_-]/g, ' ')
  const emoji = (l: string) =>
    (({ person: '🧍', car: '🚗', truck: '🚚', dog: '🐕', cat: '🐈', bicycle: '🚲', bird: '🐦' }) as Record<string, string>)[l] || '⬤'
  function ago(unixSec: number) {
    const s = Math.max(0, Math.floor((now - unixSec * 1000) / 1000))
    if (s < 60) return s + 's ago'
    if (s < 3600) return Math.floor(s / 60) + 'm ago'
    if (s < 86400) return Math.floor(s / 3600) + 'h ago'
    return Math.floor(s / 86400) + 'd ago'
  }
</script>

<svelte:head><title>HomeForge · Events</title></svelte:head>
<svelte:window onkeydown={(e) => { if (e.key === 'Escape') sel = null }} />

<div class="wrap">
  <div class="head">
    <h1>Events</h1>
    <a class="nvr" href="/nvr/" target="_blank" rel="noopener">Open full NVR ↗</a>
  </div>

  {#if err}<div class="err">{err}</div>{/if}

  {#if events.length}
    <div class="filters">
      {#each cams as c}
        <button class="chip" class:active={cam === c} onclick={() => (cam = c)}>{c === 'all' ? 'All' : pretty(c)}</button>
      {/each}
    </div>
  {/if}

  <div class="grid">
    {#each shown as e (e.id)}
      <button class="card" class:fp={e.false_positive} onclick={() => (sel = e)}>
        <img src={snap(e.id)} alt={e.label} loading="lazy" />
        {#if e.has_clip}<span class="play">▶</span>{/if}
        <div class="meta">
          <div class="r1"><span class="lbl">{emoji(e.label)} {e.sub_label || e.label}</span><span class="ago">{ago(e.start_time)}</span></div>
          <div class="r2">{pretty(e.camera)} · {(e.score * 100).toFixed(0)}%</div>
        </div>
      </button>
    {/each}
  </div>
  {#if shown.length === 0 && !err}<p class="muted">No events.</p>{/if}
</div>

{#if sel}
  <div class="overlay" onclick={(e) => { if (e.target === e.currentTarget) sel = null }} role="presentation">
    <div class="viewer">
      {#if sel.has_clip}
        <!-- svelte-ignore a11y_media_has_caption -->
        <video src={clip(sel.id)} poster={snap(sel.id)} controls playsinline preload="metadata"></video>
      {:else}
        <img src={snap(sel.id)} alt={sel.label} />
      {/if}
      <div class="vmeta">
        <span><strong>{emoji(sel.label)} {sel.sub_label || sel.label}</strong> · {pretty(sel.camera)} · {ago(sel.start_time)}</span>
        <button class="close" onclick={() => (sel = null)}>Close</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .wrap { width: 100%; max-width: 1200px; margin: 0 auto; }
  .head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
  h1 { font-size: 20px; font-weight: 700; color: var(--text); }
  .nvr { font-size: 13px; color: var(--accent); text-decoration: none; white-space: nowrap; }
  .err { background: rgba(239,68,68,.12); color:#fca5a5; border:1px solid rgba(239,68,68,.3); border-radius:8px; padding:8px 10px; font-size:13px; margin-bottom:12px; }
  .muted { color: var(--text-muted); font-size: 14px; }
  .filters { display: flex; gap: 8px; flex-wrap: wrap; margin-bottom: 14px; }
  .chip { padding: 5px 12px; border-radius: 999px; border: 1px solid var(--border); background: var(--surface); color: var(--text-muted); font-size: 13px; cursor: pointer; text-transform: capitalize; }
  .chip.active { background: var(--surface-2); color: var(--text); border-color: var(--accent); }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 12px; }
  .card { padding:0; border:1px solid var(--border); border-radius:12px; overflow:hidden; background:#000; cursor:pointer; position:relative; aspect-ratio:16/9; text-align:left; }
  .card.fp { opacity: .5; }
  .card img { width:100%; height:100%; object-fit:cover; display:block; }
  .play { position:absolute; top:8px; right:8px; width:26px; height:26px; border-radius:50%; background:rgba(0,0,0,.55); color:#fff; font-size:11px; display:flex; align-items:center; justify-content:center; }
  .meta { position:absolute; left:0; right:0; bottom:0; padding:6px 8px; background:linear-gradient(transparent, rgba(0,0,0,.75)); }
  .r1 { display:flex; align-items:center; justify-content:space-between; gap:6px; }
  .lbl { color:#fff; font-size:12px; font-weight:600; text-transform:capitalize; text-shadow:0 1px 2px rgba(0,0,0,.8); }
  .ago { color:#cbd5e1; font-size:10px; white-space:nowrap; }
  .r2 { color:#94a3b8; font-size:10px; text-transform:capitalize; margin-top:1px; }
  .overlay { position:fixed; inset:0; z-index:50; background:rgba(0,0,0,.92); display:flex; align-items:center; justify-content:center; padding:12px; }
  .viewer { display:flex; flex-direction:column; gap:10px; max-width:900px; width:100%; align-items:center; }
  .viewer video, .viewer img { max-width:100%; max-height:80vh; border-radius:8px; background:#000; }
  .vmeta { display:flex; align-items:center; justify-content:space-between; gap:12px; width:100%; color:#e2e8f0; font-size:14px; }
  .close { background:var(--surface-2); color:var(--text); border:1px solid var(--border); border-radius:8px; padding:6px 14px; font-size:13px; cursor:pointer; }
</style>
