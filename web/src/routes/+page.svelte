<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS, callService, isOn, groupByDomain, entityIcon } from '$lib/api'
  import type { Entity, WSMessage } from '$lib/api'

  let entities = $state<Map<string, Entity>>(new Map())
  let connected = $state(false)

  onMount(() => {
    const disconnect = connectWS((msg: WSMessage) => {
      connected = true
      if (msg.type === 'snapshot' && msg.entities) {
        const m = new Map<string, Entity>()
        for (const e of msg.entities) m.set(e.id, e)
        entities = m
      } else if (msg.type === 'state_changed' && msg.entity) {
        const m = new Map(entities)
        m.set(msg.entity.id, msg.entity)
        entities = m
      }
    })
    return disconnect
  })

  async function toggle(entity: Entity) {
    const on = isOn(entity)
    // Optimistic update — respond instantly, revert on failure
    const m = new Map(entities)
    m.set(entity.id, { ...entity, state: on ? 'off' : 'on' })
    entities = m

    await callService(entity.domain, on ? 'turn_off' : 'turn_on', entity.id)
  }

  const domainOrder = ['light', 'switch', 'binary_sensor', 'sensor', 'lock', 'climate']

  let grouped = $derived.by(() => groupByDomain([...entities.values()]))
</script>

<div class="max-w-7xl mx-auto space-y-8">
  <div class="flex items-center gap-2 text-sm" style="color: var(--text-muted)">
    <span class="w-2 h-2 rounded-full inline-block" style="background: {connected ? 'var(--success)' : 'var(--danger)'}"></span>
    {connected ? `${entities.size} entities` : 'Connecting…'}
  </div>

  {#each domainOrder as domain}
    {#if grouped.has(domain)}
      <section>
        <h2 class="text-sm font-semibold uppercase tracking-wider mb-3" style="color: var(--text-muted)">
          {entityIcon(domain)} {domain.replace(/_/g, ' ')}
        </h2>
        <div class="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 gap-3">
          {#each grouped.get(domain)! as entity (entity.id)}
            {@const on = isOn(entity)}
            {@const interactive = domain === 'light' || domain === 'switch'}
            <button
              class="card text-left transition-all duration-150 hover:scale-[1.02] active:scale-95"
              style="cursor: {interactive ? 'pointer' : 'default'};
                     border-color: {on ? 'var(--accent)' : 'var(--border)'};
                     background: {on ? 'color-mix(in srgb, var(--accent) 12%, var(--surface))' : 'var(--surface)'}"
              onclick={() => interactive && toggle(entity)}
            >
              <div class="text-xl mb-2">{entityIcon(domain)}</div>
              <div class="text-xs font-medium truncate" style="color: var(--text)">{entity.name}</div>
              <div class="text-xs mt-1 font-semibold" style="color: {on ? 'var(--accent)' : 'var(--text-muted)'}">
                {entity.state}
              </div>
            </button>
          {/each}
        </div>
      </section>
    {/if}
  {/each}

  {#if entities.size === 0 && connected}
    <div class="text-center py-24" style="color: var(--text-muted)">
      <p class="text-5xl mb-4">🏠</p>
      <p class="text-lg font-medium" style="color: var(--text)">No entities yet</p>
      <p class="text-sm mt-1">Connect your devices in config.yaml to get started</p>
    </div>
  {/if}
</div>
