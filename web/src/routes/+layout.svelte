<script lang="ts">
  import '../app.css'
  import { page } from '$app/stores'
  import { onMount } from 'svelte'
  import { goto } from '$app/navigation'

  interface Props { children: any }
  let { children }: Props = $props()

  const nav = [
    ['/', 'Home'],
    ['/devices', 'Devices'],
    ['/climate', 'Climate'],
    ['/floorplan', 'Floor Plan'],
    ['/automations', 'Automations'],
    ['/energy', 'Energy'],
    ['/cameras', 'Cameras'],
    ['/zigbee', 'Zigbee'],
    ['/ring', 'Ring'],
    ['/vitals', 'Health'],
    ['/assistant', 'Assistant'],
  ]

  function isActive(href: string): boolean {
    if (href === '/') return $page.url.pathname === '/'
    return $page.url.pathname.startsWith(href)
  }

  // Auth guard: if login is enabled and there's no valid session, go to /login.
  onMount(async () => {
    if ($page.url.pathname === '/login') return
    try {
      const r = await fetch('/api/auth/me')
      const d = await r.json()
      if (d.enabled && !d.authenticated) goto('/login')
    } catch {}
  })

  async function logout() {
    try { await fetch('/api/auth/logout', { method: 'POST' }) } catch {}
    goto('/login')
  }
</script>

<svelte:head>
  <title>HomeForge</title>
</svelte:head>

{#if $page.url.pathname === '/login'}
  {@render children()}
{:else}
  <div class="min-h-screen flex flex-col" style="background: var(--bg)">
    <nav class="border-b flex items-center px-3 sm:px-6 h-14 gap-2 sm:gap-4 shrink-0" style="border-color: var(--border); background: var(--surface)">
      <a href="/" class="font-bold text-lg tracking-tight shrink-0" style="color: var(--accent); text-decoration: none">⚒<span class="hidden sm:inline"> HomeForge</span></a>
      <div class="tabs flex gap-1 overflow-x-auto flex-1">
        {#each nav as [href, label]}
          <a
            {href}
            class="px-3 py-1.5 rounded-md text-sm font-medium transition-colors whitespace-nowrap"
            style="color: {isActive(href) ? 'var(--text)' : 'var(--text-muted)'};
                   background: {isActive(href) ? 'var(--surface-2)' : 'transparent'}"
          >{label}</a>
        {/each}
      </div>
      <a
        href="/account"
        title="Account"
        class="shrink-0 px-2.5 py-1.5 rounded-md text-sm font-medium transition-colors"
        style="text-decoration: none; color: {isActive('/account') ? 'var(--text)' : 'var(--text-muted)'};
               background: {isActive('/account') ? 'var(--surface-2)' : 'transparent'}"
      >⚙<span class="hidden sm:inline"> Account</span></a>
      <button
        onclick={logout}
        title="Sign out"
        class="shrink-0 px-2.5 py-1.5 rounded-md text-sm font-medium transition-colors"
        style="color: var(--text-muted); background: transparent"
      >⎋<span class="hidden sm:inline"> Sign out</span></button>
    </nav>

    <main class="flex-1 p-3 sm:p-6">
      {@render children()}
    </main>
  </div>
{/if}

<style>
  /* Let the tab row scroll horizontally on a phone instead of overflowing, hide the scrollbar */
  .tabs { -ms-overflow-style: none; scrollbar-width: none; }
  .tabs::-webkit-scrollbar { display: none; }
</style>
