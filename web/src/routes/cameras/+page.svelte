<script lang="ts">
  // The full Sentinel NVR UI, served by HomeForge under /nvr/ on this same domain (one login,
  // works over the tunnel + on mobile). HomeForge reverse-proxies + path-rewrites it — see
  // internal/api SetCameras / nvrRouter.
</script>

<div style="height: calc(100dvh - 6.5rem); min-height: 20rem">
  <iframe
    src="/nvr/"
    title="Sentinel NVR"
    allow="fullscreen; autoplay; camera; microphone"
    style="width: 100%; height: 100%; border: 0; border-radius: 0.75rem; background: #000"
  ></iframe>
</div>
