<script lang="ts">
  import '../app.css'
  import { page } from '$app/stores'

  interface Props { children: any }
  let { children }: Props = $props()

  const nav = [
    ['/', 'Home'],
    ['/devices', 'Devices'],
    ['/automations', 'Automations'],
    ['/energy', 'Energy'],
  ]

  function isActive(href: string): boolean {
    if (href === '/') return $page.url.pathname === '/'
    return $page.url.pathname.startsWith(href)
  }
</script>

<svelte:head>
  <title>HomeForge</title>
</svelte:head>

<div class="min-h-screen flex flex-col" style="background: var(--bg)">
  <nav class="border-b flex items-center px-6 h-14 gap-6 shrink-0" style="border-color: var(--border); background: var(--surface)">
    <a href="/" class="font-bold text-lg tracking-tight" style="color: var(--accent); text-decoration: none">⚒ HomeForge</a>
    <div class="flex gap-1 ml-2">
      {#each nav as [href, label]}
        <a
          {href}
          class="px-3 py-1.5 rounded-md text-sm font-medium transition-colors"
          style="color: {isActive(href) ? 'var(--text)' : 'var(--text-muted)'};
                 background: {isActive(href) ? 'var(--surface-2)' : 'transparent'}"
        >{label}</a>
      {/each}
    </div>
  </nav>

  <main class="flex-1 p-6">
    {@render children()}
  </main>
</div>
