<script lang="ts">
  import { onMount } from 'svelte'
  // Embed the Sentinel NVR web UI (full live views / recordings / event review) rather than
  // rebuilding it. Sentinel runs on the SAME host as HomeForge on port 5000 (Frigate-compatible,
  // no auth, no frame-blocking headers). Using location.hostname keeps this working after
  // HF + Sentinel move to the Beelink together (see the HA-exit endgame).
  let src = $state('')
  onMount(() => {
    src = `${location.protocol}//${location.hostname}:5000/`
  })
</script>

<div style="height: calc(100vh - 6.5rem)">
  {#if src}
    <iframe
      {src}
      title="Sentinel NVR"
      allow="fullscreen; autoplay; camera; microphone"
      style="width: 100%; height: 100%; border: 0; border-radius: 0.75rem; background: #000"
    ></iframe>
  {/if}
</div>
