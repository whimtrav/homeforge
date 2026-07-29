<script lang="ts">
  import { onMount } from 'svelte'
  import { goto } from '$app/navigation'

  let mode = $state<'loading' | 'login' | 'setup'>('loading')
  let email = $state('')
  let password = $state('')
  let error = $state('')
  let busy = $state(false)

  onMount(async () => {
    try {
      const r = await fetch('/api/auth/me')
      const d = await r.json()
      if (d.enabled === false) { goto('/'); return }
      if (d.authenticated) { goto('/'); return }
      if (d.needsSetup) { mode = 'setup'; email = d.ownerEmail || '' }
      else { mode = 'login' }
    } catch { mode = 'login' }
  })

  async function submit(e: Event) {
    e.preventDefault()
    error = ''; busy = true
    try {
      const url = mode === 'setup' ? '/api/auth/setup' : '/api/auth/login'
      const r = await fetch(url, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, password }),
      })
      if (r.ok) { goto('/'); return }
      const t = await r.text()
      error = t || (mode === 'setup' ? 'Setup failed' : 'Invalid email or password')
    } catch { error = 'Could not reach the server' }
    finally { busy = false }
  }
</script>

<svelte:head><title>HomeForge · Sign in</title></svelte:head>

<div class="screen">
  <form class="box" onsubmit={submit}>
    <div class="brand">⚒ HomeForge</div>
    {#if mode === 'loading'}
      <p class="sub">…</p>
    {:else}
      <p class="sub">{mode === 'setup' ? 'Create the owner account' : 'Sign in to your home'}</p>

      <label>Email
        <input type="email" bind:value={email} autocomplete="username" required placeholder="you@example.com" />
      </label>
      <label>Password
        <input type="password" bind:value={password} autocomplete={mode === 'setup' ? 'new-password' : 'current-password'} required placeholder={mode === 'setup' ? 'Choose a password (8+ chars)' : '••••••••'} />
      </label>

      {#if error}<div class="err">{error}</div>{/if}

      <button type="submit" disabled={busy || !email || !password}>
        {busy ? '…' : mode === 'setup' ? 'Create account' : 'Sign in'}
      </button>
      {#if mode === 'setup'}
        <p class="hint">This is the first account — it becomes the owner of this HomeForge.</p>
      {/if}
    {/if}
  </form>
</div>

<style>
  .screen { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 20px; background: var(--bg); }
  .box { width: 100%; max-width: 360px; background: var(--surface); border: 1px solid var(--border); border-radius: 16px; padding: 28px 24px; display: flex; flex-direction: column; gap: 14px; box-shadow: 0 10px 40px rgba(0,0,0,.35); }
  .brand { font-size: 22px; font-weight: 700; color: var(--accent); text-align: center; }
  .sub { text-align: center; color: var(--text-muted); font-size: 14px; margin-top: -6px; }
  label { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--text-muted); }
  input { padding: 10px 12px; border-radius: 10px; border: 1px solid var(--border); background: var(--surface-2); color: var(--text); font-size: 15px; outline: none; }
  input:focus { border-color: var(--accent); }
  .err { background: rgba(239,68,68,.12); color: #fca5a5; border: 1px solid rgba(239,68,68,.35); border-radius: 8px; padding: 8px 10px; font-size: 13px; }
  button { margin-top: 4px; padding: 11px; border-radius: 10px; border: none; background: var(--accent); color: #fff; font-size: 15px; font-weight: 600; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  .hint { font-size: 11px; color: var(--text-muted); text-align: center; }
</style>
