<script lang="ts">
  import { onMount } from 'svelte'
  // Embed the Zigbee2MQTT frontend (device management / pairing / network map / OTA). Z2M runs
  // standalone on the SAME host as HomeForge on port 8098. location.hostname keeps this working
  // after HF + Z2M move to the Beelink together (HA-exit endgame).
  let src = $state('')
  onMount(() => {
    src = `${location.protocol}//${location.hostname}:8098/`
  })
</script>

<div style="height: calc(100vh - 6.5rem)">
  {#if src}
    <iframe
      {src}
      title="Zigbee2MQTT"
      allow="fullscreen"
      style="width: 100%; height: 100%; border: 0; border-radius: 0.75rem; background: #000"
    ></iframe>
  {/if}
</div>
