<script lang="ts">
  import { onMount } from 'svelte'
  import { connectWS } from '$lib/api'
  import type { WSMessage } from '$lib/api'

  let connected = $state(false)
  let count = $state(0)

  onMount(() => {
    return connectWS((msg: WSMessage) => {
      connected = true
      if (msg.type === 'snapshot' && msg.entities) count = msg.entities.length
      else if (msg.type === 'state_changed') count = Math.max(count, 1)
    })
  })
</script>

<div class="flex flex-col items-center justify-center py-32" style="color: var(--text-muted)">
  <p class="text-5xl mb-6">⚒</p>
  <p class="text-lg font-semibold mb-1" style="color: var(--text)">HomeForge</p>
  <p class="text-sm mb-8">
    <span class="inline-block w-2 h-2 rounded-full mr-1 align-middle" style="background:{connected ? 'var(--success)' : 'var(--danger)'}"></span>
    {connected ? `${count} entities connected` : 'Connecting…'}
  </p>
  <a href="/devices" class="px-4 py-2 rounded-lg text-sm font-medium" style="background: var(--accent); color: #fff">
    Go to Devices →
  </a>
</div>
