<script lang="ts">
  import { onMount } from 'svelte'
  import { fetchAutomations, setAutomationEnabled, type Automation } from '$lib/api'

  let automations = $state<Automation[]>([])
  let loading = $state(true)
  let error = $state('')

  async function load() {
    try {
      automations = await fetchAutomations()
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  onMount(load)

  async function toggle(a: Automation) {
    const next = !a.enabled
    a.enabled = next // optimistic
    try {
      await setAutomationEnabled(a.name, next)
    } catch {
      a.enabled = !next // revert on failure
    }
  }

  const enabledCount = $derived(automations.filter((a) => a.enabled).length)
</script>

<div class="max-w-4xl mx-auto">
  <div class="flex items-center justify-between mb-4">
    <h1 class="text-xl font-bold" style="color: var(--text)">Automations</h1>
    <span class="text-sm" style="color: var(--text-muted)">{enabledCount}/{automations.length} enabled</span>
  </div>

  {#if loading}
    <p style="color: var(--text-muted)">Loading…</p>
  {:else if error}
    <p style="color: #f87171">Error: {error}</p>
  {:else if automations.length === 0}
    <p style="color: var(--text-muted)">No automations configured.</p>
  {:else}
    <div class="flex flex-col gap-2">
      {#each automations as a (a.name)}
        <div
          class="flex items-center gap-4 p-3 rounded-lg border transition-opacity"
          style="border-color: var(--border); background: var(--surface); opacity: {a.enabled ? 1 : 0.5}"
        >
          <div class="flex-1 min-w-0">
            <div class="font-medium truncate" style="color: var(--text)">{a.name}</div>
            <div class="text-xs mt-0.5" style="color: var(--text-muted)">{a.trigger}</div>
            {#if a.actions.length}
              <div class="text-xs mt-1 flex flex-wrap gap-1">
                {#each a.actions as act}
                  <span class="px-1.5 py-0.5 rounded font-mono" style="background: var(--surface-2); color: var(--text-muted)">{act}</span>
                {/each}
              </div>
            {/if}
          </div>
          <button
            onclick={() => toggle(a)}
            class="relative w-12 h-7 rounded-full transition-colors shrink-0"
            style="background: {a.enabled ? 'var(--accent)' : 'var(--surface-2)'}"
            aria-label="toggle {a.name}"
            title={a.enabled ? 'Enabled — click to disable' : 'Disabled — click to enable'}
          >
            <span
              class="absolute top-0.5 left-0.5 w-6 h-6 rounded-full bg-white transition-transform"
              style="transform: translateX({a.enabled ? '20px' : '0'})"
            ></span>
          </button>
        </div>
      {/each}
    </div>
  {/if}
</div>
