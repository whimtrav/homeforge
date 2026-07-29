<script lang="ts">
  import { onMount } from 'svelte'

  let me = $state('')
  let users = $state<{ email: string; created: string }[]>([])

  // change password
  let curPw = $state(''), newPw = $state(''), confPw = $state('')
  let pwMsg = $state(''), pwErr = $state(false), pwBusy = $state(false)

  // add user
  let addEmail = $state(''), addPw = $state('')
  let addMsg = $state(''), addErr = $state(false), addBusy = $state(false)

  async function loadUsers() {
    try {
      const r = await fetch('/api/auth/users')
      if (r.ok) users = await r.json()
    } catch {}
  }
  onMount(async () => {
    try { const d = await (await fetch('/api/auth/me')).json(); me = d.email || '' } catch {}
    loadUsers()
  })

  async function changePassword(e: Event) {
    e.preventDefault(); pwMsg = ''; pwErr = false
    if (newPw !== confPw) { pwMsg = 'New passwords do not match'; pwErr = true; return }
    pwBusy = true
    try {
      const r = await fetch('/api/auth/change-password', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ current_password: curPw, new_password: newPw }),
      })
      if (r.ok) { pwMsg = 'Password changed.'; curPw = newPw = confPw = '' }
      else { pwMsg = (await r.text()) || 'Could not change password'; pwErr = true }
    } catch { pwMsg = 'Could not reach the server'; pwErr = true }
    finally { pwBusy = false }
  }

  async function addUser(e: Event) {
    e.preventDefault(); addMsg = ''; addErr = false; addBusy = true
    try {
      const r = await fetch('/api/auth/users', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: addEmail, password: addPw }),
      })
      if (r.ok) { addMsg = `Added ${addEmail}.`; addEmail = addPw = ''; loadUsers() }
      else { addMsg = (await r.text()) || 'Could not add user'; addErr = true }
    } catch { addMsg = 'Could not reach the server'; addErr = true }
    finally { addBusy = false }
  }

  async function removeUser(email: string) {
    if (!confirm(`Remove ${email}? They will no longer be able to sign in.`)) return
    try {
      const r = await fetch(`/api/auth/users/${encodeURIComponent(email)}`, { method: 'DELETE' })
      if (r.ok) loadUsers()
      else alert((await r.text()) || 'Could not remove user')
    } catch {}
  }
</script>

<svelte:head><title>HomeForge · Account</title></svelte:head>

<div class="wrap">
  <h1>Account</h1>
  {#if me}<p class="who">Signed in as <strong>{me}</strong></p>{/if}

  <section class="card">
    <h2>Change password</h2>
    <form onsubmit={changePassword}>
      <input type="password" placeholder="Current password" bind:value={curPw} autocomplete="current-password" required />
      <input type="password" placeholder="New password (8+ chars)" bind:value={newPw} autocomplete="new-password" required />
      <input type="password" placeholder="Confirm new password" bind:value={confPw} autocomplete="new-password" required />
      {#if pwMsg}<div class="msg" class:err={pwErr}>{pwMsg}</div>{/if}
      <button type="submit" disabled={pwBusy || !curPw || !newPw}>{pwBusy ? '…' : 'Update password'}</button>
    </form>
  </section>

  <section class="card">
    <h2>People with access</h2>
    <ul class="users">
      {#each users as u}
        <li>
          <span>{u.email}{#if u.email === me}<span class="you"> · you</span>{/if}</span>
          {#if u.email !== me}<button class="rm" onclick={() => removeUser(u.email)}>Remove</button>{/if}
        </li>
      {/each}
      {#if users.length === 0}<li class="muted">Loading…</li>{/if}
    </ul>

    <h3>Add a person</h3>
    <form onsubmit={addUser}>
      <input type="email" placeholder="their@email.com" bind:value={addEmail} required />
      <input type="password" placeholder="Temporary password (8+ chars)" bind:value={addPw} autocomplete="new-password" required />
      {#if addMsg}<div class="msg" class:err={addErr}>{addMsg}</div>{/if}
      <button type="submit" disabled={addBusy || !addEmail || !addPw}>{addBusy ? '…' : 'Add person'}</button>
    </form>
    <p class="hint">They can sign in with this email + password, then change it here.</p>
  </section>
</div>

<style>
  .wrap { width: 100%; max-width: 520px; margin: 0 auto; display: flex; flex-direction: column; gap: 16px; }
  h1 { font-size: 20px; font-weight: 700; color: var(--text); }
  .who { font-size: 13px; color: var(--text-muted); margin-top: -8px; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 16px 18px; }
  h2 { font-size: 15px; font-weight: 600; color: var(--text); margin-bottom: 12px; }
  h3 { font-size: 13px; font-weight: 600; color: var(--text-muted); margin: 16px 0 8px; }
  form { display: flex; flex-direction: column; gap: 10px; }
  input { padding: 10px 12px; border-radius: 9px; border: 1px solid var(--border); background: var(--surface-2); color: var(--text); font-size: 15px; outline: none; }
  input:focus { border-color: var(--accent); }
  button { padding: 10px; border-radius: 9px; border: none; background: var(--accent); color: #fff; font-weight: 600; font-size: 14px; cursor: pointer; align-self: flex-start; padding-left: 18px; padding-right: 18px; }
  button:disabled { opacity: .5; cursor: default; }
  .msg { font-size: 13px; color: var(--success); }
  .msg.err { color: #fca5a5; }
  .users { list-style: none; display: flex; flex-direction: column; gap: 8px; }
  .users li { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 8px 10px; border-radius: 8px; background: var(--surface-2); font-size: 14px; color: var(--text); }
  .you { color: var(--text-muted); font-size: 12px; }
  .muted { color: var(--text-muted); background: transparent; }
  .rm { background: transparent; color: #fca5a5; border: 1px solid var(--border); padding: 4px 10px; font-size: 12px; font-weight: 500; }
  .hint { font-size: 11px; color: var(--text-muted); margin-top: 8px; }
</style>
