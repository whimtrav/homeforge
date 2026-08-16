<script lang="ts">
  import '../app.css'
  import { page } from '$app/stores'
  import { onMount } from 'svelte'
  import { goto, afterNavigate } from '$app/navigation'

  interface Props { children: any }
  let { children }: Props = $props()

  let drawerOpen = $state(false)

  const nav: [string, string][] = [
    ['/', 'Home'],
    ['/devices', 'Devices'],
    ['/climate', 'Climate'],
    ['/floorplan', 'Floor Plan'],
    ['/automations', 'Automations'],
    ['/energy', 'Energy'],
    ['/cameras', 'Cameras'],
    ['/events', 'Events'],
    ['/zigbee', 'Zigbee'],
    ['/ring', 'Ring'],
    ['/vitals', 'Health'],
    ['/assistant', 'AI'],
  ]

  function isActive(href: string): boolean {
    if (href === '/') return $page.url.pathname === '/'
    return $page.url.pathname.startsWith(href)
  }

  // Section title shown in the mobile top bar (desktop shows the full tab row instead).
  let currentLabel = $derived(
    (nav.find(([h]) => isActive(h))?.[1]) ??
    ($page.url.pathname.startsWith('/account') ? 'Account' : 'HomeForge')
  )

  // Close the drawer whenever navigation completes.
  afterNavigate(() => { drawerOpen = false })

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
    <nav class="border-b flex items-center px-3 sm:px-6 h-14 gap-2 sm:gap-4 shrink-0" style="border-color: var(--border); background: var(--surface); padding-top: env(safe-area-inset-top)">
      <!-- Mobile: hamburger opens the drawer -->
      <button
        class="sm:hidden shrink-0 grid place-items-center w-10 h-10 -ml-1 rounded-md text-2xl"
        style="color: var(--text); background: transparent"
        onclick={() => (drawerOpen = true)}
        aria-label="Open menu"
      >☰</button>

      <a href="/" class="font-bold text-lg tracking-tight shrink-0" style="color: var(--accent); text-decoration: none">⚒<span class="hidden sm:inline"> HomeForge</span></a>

      <!-- Desktop: full tab row (hidden on phones) -->
      <div class="tabs hidden sm:flex gap-1 overflow-x-auto flex-1">
        {#each nav as [href, label]}
          <a
            {href}
            class="px-3 py-1.5 rounded-md text-sm font-medium transition-colors whitespace-nowrap"
            style="color: {isActive(href) ? 'var(--text)' : 'var(--text-muted)'};
                   background: {isActive(href) ? 'var(--surface-2)' : 'transparent'}"
          >{label}</a>
        {/each}
      </div>

      <!-- Mobile: current section title, centered -->
      <span class="sm:hidden flex-1 text-center font-semibold truncate" style="color: var(--text)">{currentLabel}</span>

      <a
        href="/account"
        title="Account"
        class="shrink-0 grid place-items-center min-w-10 h-10 px-2.5 rounded-md text-sm font-medium transition-colors"
        style="text-decoration: none; color: {isActive('/account') ? 'var(--text)' : 'var(--text-muted)'};
               background: {isActive('/account') ? 'var(--surface-2)' : 'transparent'}"
      >⚙<span class="hidden sm:inline"> Account</span></a>
      <button
        onclick={logout}
        title="Sign out"
        class="hidden sm:inline-flex shrink-0 items-center px-2.5 py-1.5 rounded-md text-sm font-medium transition-colors"
        style="color: var(--text-muted); background: transparent"
      >⎋ Sign out</button>
    </nav>

    <!-- Mobile slide-in drawer (all sections, large touch targets) -->
    {#if drawerOpen}
      <button
        class="fixed inset-0 z-40 sm:hidden"
        style="background: rgba(0,0,0,0.55); border: none"
        onclick={() => (drawerOpen = false)}
        aria-label="Close menu"
      ></button>
      <aside
        class="fixed top-0 left-0 bottom-0 z-50 w-72 max-w-[82vw] flex flex-col sm:hidden"
        style="background: var(--surface); border-right: 1px solid var(--border); padding-top: env(safe-area-inset-top)"
      >
        <div class="h-14 flex items-center px-4 font-bold text-lg shrink-0" style="color: var(--accent); border-bottom: 1px solid var(--border)">⚒ HomeForge</div>
        <div class="flex-1 overflow-y-auto py-2">
          {#each nav as [href, label]}
            <a
              {href}
              class="block px-5 py-3.5 text-base font-medium"
              style="text-decoration: none;
                     color: {isActive(href) ? 'var(--text)' : 'var(--text-muted)'};
                     background: {isActive(href) ? 'var(--surface-2)' : 'transparent'};
                     border-left: 3px solid {isActive(href) ? 'var(--accent)' : 'transparent'}"
            >{label}</a>
          {/each}
        </div>
        <div class="border-t p-2 shrink-0" style="border-color: var(--border); padding-bottom: max(0.5rem, env(safe-area-inset-bottom))">
          <a href="/account" class="block px-5 py-3.5 text-base font-medium" style="text-decoration: none; color: var(--text-muted)">⚙ Account</a>
          <button onclick={logout} class="block w-full text-left px-5 py-3.5 text-base font-medium" style="color: var(--text-muted); background: transparent; border: none">⎋ Sign out</button>
        </div>
      </aside>
    {/if}

    <main class="flex-1 p-3 sm:p-6" style="padding-bottom: max(0.75rem, env(safe-area-inset-bottom))">
      {@render children()}
    </main>
  </div>
{/if}

<style>
  /* Let the desktop tab row scroll horizontally, hide the scrollbar */
  .tabs { -ms-overflow-style: none; scrollbar-width: none; }
  .tabs::-webkit-scrollbar { display: none; }
</style>
