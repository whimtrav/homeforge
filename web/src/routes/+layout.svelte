<script lang="ts">
  import '../app.css'

  interface Props { children: any }
  let { children }: Props = $props()
</script>

<svelte:head>
  <title>HomeForge</title>
</svelte:head>

<div class="min-h-screen flex flex-col" style="background: var(--bg)">
  <nav class="border-b flex items-center px-6 h-14 gap-6 shrink-0" style="border-color: var(--border); background: var(--surface)">
    <span class="font-bold text-lg tracking-tight" style="color: var(--accent)">⚒ HomeForge</span>
    <div class="flex gap-1 ml-4">
      {#each [['/', 'Home'], ['/automations', 'Automations'], ['/energy', 'Energy'], ['/devices', 'Devices']] as [href, label]}
        <a
          {href}
          class="px-3 py-1.5 rounded-md text-sm font-medium transition-colors"
          style="color: var(--text-muted)"
        >{label}</a>
      {/each}
    </div>
  </nav>

  <main class="flex-1 p-6">
    {@render children()}
  </main>
</div>
