<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  // Native camera grid: HomeForge proxies each camera's latest JPEG frame (/api/cameras/*),
  // refreshed ~1/s for a live-glance view that works on any device. The full NVR (recordings /
  // event review) stays one click away for desktop. No iframe, no WebSocket, no path-rewriting.
  type Cam = { name: string; online: boolean }
  let cams = $state<Cam[]>([])
  let t = $state(Date.now())
  let sel = $state<Cam | null>(null)
  let err = $state('')
  let frameTimer: ReturnType<typeof setInterval>
  let listTimer: ReturnType<typeof setInterval>

  async function load() {
    try {
      const r = await fetch('/api/cameras')
      if (r.ok) {
        const data = await r.json()
        cams = (Array.isArray(data) ? data : []).map((c: any) => ({ name: c.name, online: !!c.online }))
        err = ''
      } else err = 'Could not load cameras.'
    } catch { err = 'Could not reach HomeForge.' }
  }

  onMount(() => {
    load()
    frameTimer = setInterval(() => (t = Date.now()), 1000) // refresh frames ~1/s
    listTimer = setInterval(load, 30000) // refresh list + online status
  })
  onDestroy(() => { clearInterval(frameTimer); clearInterval(listTimer) })

  const frame = (name: string, ts: number) => `/api/cameras/${encodeURIComponent(name)}/frame?t=${ts}`
  const pretty = (n: string) => n.replace(/[_-]/g, ' ')
</script>

<svelte:head><title>HomeForge · Cameras</title></svelte:head>

<div class="wrap">
  <div class="head">
    <h1>Cameras</h1>
    <a class="nvr" href="/nvr/" target="_blank" rel="noopener">Open full NVR ↗</a>
  </div>

  {#if err}<div class="err">{err}</div>{/if}

  <div class="grid">
    {#each cams as c (c.name)}
      <button class="tile" class:off={!c.online} onclick={() => (sel = c)}>
        <img src={frame(c.name, t)} alt={c.name} />
        {#if !c.online}<div class="offlbl">offline</div>{/if}
        <div class="cap">
          <span class="dot" style="background:{c.online ? 'var(--success)' : 'var(--danger)'}"></span>
          <span class="nm">{pretty(c.name)}</span>
        </div>
      </button>
    {/each}
  </div>
  {#if cams.length === 0 && !err}<p class="muted">Loading cameras…</p>{/if}
</div>

{#if sel}
  <button class="overlay" onclick={() => (sel = null)} aria-label="Close">
    <img src={frame(sel.name, t)} alt={sel.name} />
    <div class="ocap">{pretty(sel.name)} · tap to close</div>
  </button>
{/if}

<style>
  .wrap { width: 100%; max-width: 1200px; margin: 0 auto; }
  .head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
  h1 { font-size: 20px; font-weight: 700; color: var(--text); }
  .nvr { font-size: 13px; color: var(--accent); text-decoration: none; white-space: nowrap; }
  .err { background: rgba(239,68,68,.12); color:#fca5a5; border:1px solid rgba(239,68,68,.3); border-radius:8px; padding:8px 10px; font-size:13px; margin-bottom:12px; }
  .muted { color: var(--text-muted); font-size: 14px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 12px; }
  .tile { padding:0; border:1px solid var(--border); border-radius:12px; overflow:hidden; background:#000; cursor:pointer; position:relative; aspect-ratio:16/9; }
  .tile.off { opacity:.55; }
  .tile img { width:100%; height:100%; object-fit:cover; display:block; }
  .offlbl { position:absolute; inset:0; display:flex; align-items:center; justify-content:center; color:#fca5a5; font-size:13px; font-weight:600; text-transform:uppercase; letter-spacing:.05em; }
  .cap { position:absolute; left:0; right:0; bottom:0; display:flex; align-items:center; gap:6px; padding:6px 8px; background:linear-gradient(transparent, rgba(0,0,0,.65)); }
  .dot { width:8px; height:8px; border-radius:50%; display:inline-block; flex:0 0 auto; }
  .nm { color:#fff; font-size:12px; font-weight:600; text-transform:capitalize; text-shadow:0 1px 2px rgba(0,0,0,.8); }
  .overlay { position:fixed; inset:0; z-index:50; background:rgba(0,0,0,.92); border:0; display:flex; flex-direction:column; align-items:center; justify-content:center; gap:10px; padding:12px; cursor:pointer; }
  .overlay img { max-width:100%; max-height:85vh; object-fit:contain; border-radius:8px; }
  .ocap { color:#e2e8f0; font-size:14px; text-transform:capitalize; }
</style>
