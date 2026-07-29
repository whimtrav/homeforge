<script lang="ts">
  import { tick } from 'svelte'

  type Msg = { role: 'user' | 'assistant'; content: string; actions?: string[] }
  let msgs = $state<Msg[]>([])
  let input = $state('')
  let busy = $state(false)
  let scroller: HTMLDivElement
  let ta: HTMLTextAreaElement

  // grow the textarea with its content (wrap instead of scrolling right), up to a max then scroll
  function autoGrow() {
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = Math.min(ta.scrollHeight, 160) + 'px'
  }

  // ── voice ──
  let recording = $state(false)
  let voiceOn = $state(false) // speak replies aloud
  let recorder: MediaRecorder | null = null
  let chunks: Blob[] = []
  let audioCtx: AudioContext | null = null
  let vadRAF = 0
  let player: HTMLAudioElement | null = null

  // Neural voice via the local Piper service; falls back to the browser voice if it's down.
  // Playback works over plain HTTP (only mic capture needs HTTPS/localhost).
  async function speak(text: string) {
    if (!text) return
    try {
      const res = await fetch('/api/assistant/tts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      })
      if (!res.ok) throw new Error('tts')
      const url = URL.createObjectURL(new Blob([await res.arrayBuffer()], { type: 'audio/wav' }))
      player?.pause()
      player = new Audio(url)
      player.onended = () => URL.revokeObjectURL(url)
      await player.play()
    } catch {
      if ('speechSynthesis' in window) {
        speechSynthesis.cancel()
        speechSynthesis.speak(new SpeechSynthesisUtterance(text))
      }
    }
  }

  function stopRecording() {
    if (recorder && recorder.state !== 'inactive') recorder.stop()
  }

  function cleanupVAD() {
    if (vadRAF) cancelAnimationFrame(vadRAF)
    vadRAF = 0
    audioCtx?.close().catch(() => {})
    audioCtx = null
  }

  // Voice-activity detection: auto-stop ~1.2s after you go quiet (once you've spoken), 15s cap.
  function startVAD(stream: MediaStream) {
    audioCtx = new AudioContext()
    const analyser = audioCtx.createAnalyser()
    analyser.fftSize = 512
    audioCtx.createMediaStreamSource(stream).connect(analyser)
    const buf = new Uint8Array(analyser.fftSize)
    const start = performance.now()
    let spoke = false
    let quietSince = start
    const THRESH = 0.02, SILENCE_MS = 1200, MAX_MS = 15000, MIN_MS = 500
    const tick = () => {
      if (!recording || !audioCtx) return
      analyser.getByteTimeDomainData(buf)
      let sum = 0
      for (let i = 0; i < buf.length; i++) { const v = (buf[i] - 128) / 128; sum += v * v }
      const rms = Math.sqrt(sum / buf.length)
      const now = performance.now()
      if (rms > THRESH) { spoke = true; quietSince = now }
      if (spoke && now - start > MIN_MS && now - quietSince > SILENCE_MS) return stopRecording()
      if (now - start > MAX_MS) return stopRecording()
      vadRAF = requestAnimationFrame(tick)
    }
    vadRAF = requestAnimationFrame(tick)
  }

  async function toggleMic() {
    if (recording) { stopRecording(); return }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      recorder = new MediaRecorder(stream)
      chunks = []
      recorder.ondataavailable = (e) => { if (e.data.size) chunks.push(e.data) }
      recorder.onstop = async () => {
        stream.getTracks().forEach((t) => t.stop())
        cleanupVAD()
        recording = false
        if (!chunks.length) return
        const blob = new Blob(chunks, { type: recorder?.mimeType || 'audio/webm' })
        busy = true
        try {
          const res = await fetch('/api/assistant/stt', { method: 'POST', body: blob })
          const { text } = await res.json()
          busy = false
          if (text?.trim()) await send(text, true) // voice in → speak the reply back
          else msgs.push({ role: 'assistant', content: "(I didn't catch that — try again)" })
        } catch {
          busy = false
          msgs.push({ role: 'assistant', content: '⚠️ Could not transcribe audio.' })
        }
      }
      recorder.start()
      recording = true
      voiceOn = true // asking by voice implies you want to hear the answer
      startVAD(stream)
    } catch {
      msgs.push({ role: 'assistant', content: '⚠️ Microphone access was denied or is unavailable.' })
    }
  }

  const examples = [
    'What is the upstairs temperature?',
    'Is the office lamp on?',
    'Turn off the family room fan',
    'How much water have we used today?',
  ]

  async function scrollDown() {
    await tick()
    scroller?.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' })
  }

  async function send(text: string, spoken = false) {
    const message = text.trim()
    if (!message || busy) return
    input = ''
    if (ta) ta.style.height = 'auto' // shrink back after sending
    msgs.push({ role: 'user', content: message })
    busy = true
    scrollDown()
    // pass prior turns (role+content only) as conversation history
    const history = msgs
      .slice(0, -1)
      .map((m) => ({ role: m.role, content: m.content }))
    try {
      const res = await fetch('/api/assistant', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message, history }),
      })
      const data = await res.json()
      msgs.push({ role: 'assistant', content: data.reply ?? '(no reply)', actions: data.actions ?? [] })
      if (spoken || voiceOn) speak(data.reply ?? '')
    } catch (e) {
      msgs.push({ role: 'assistant', content: '⚠️ Could not reach the assistant. Is the local model running?' })
    } finally {
      busy = false
      scrollDown()
    }
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send(input)
    }
  }
</script>

<svelte:head><title>HomeForge · Assistant</title></svelte:head>

<div class="mx-auto flex flex-col" style="max-width: 820px; height: calc(100vh - 8rem)">
  <div class="mb-3 flex items-start justify-between gap-3">
    <div>
      <h1 class="text-xl font-bold" style="color: var(--text)">🤖 Home Assistant</h1>
      <p class="text-sm" style="color: var(--text-muted)">
        Local AI — runs entirely on your server. Type or tap the mic to talk.
      </p>
    </div>
    <button
      onclick={() => (voiceOn = !voiceOn)}
      title="Speak replies aloud"
      class="shrink-0 text-xs px-2.5 py-1.5 rounded-md border transition-colors"
      style="border-color: var(--border); color: var(--text); background: {voiceOn ? 'var(--surface-2)' : 'transparent'}"
    >{voiceOn ? '🔊 Speaking' : '🔇 Muted'}</button>
  </div>

  <div
    bind:this={scroller}
    class="flex-1 overflow-y-auto rounded-lg border p-4 flex flex-col gap-3"
    style="border-color: var(--border); background: var(--surface)"
  >
    {#if msgs.length === 0}
      <div class="m-auto text-center" style="color: var(--text-muted)">
        <div class="text-4xl mb-3">💬</div>
        <div class="text-sm mb-4">Try one of these:</div>
        <div class="flex flex-wrap gap-2 justify-center" style="max-width: 520px">
          {#each examples as ex}
            <button
              onclick={() => send(ex)}
              class="px-3 py-1.5 rounded-full text-sm border transition-colors"
              style="border-color: var(--border); color: var(--text); background: var(--surface-2)"
            >{ex}</button>
          {/each}
        </div>
      </div>
    {/if}

    {#each msgs as m}
      <div class="flex {m.role === 'user' ? 'justify-end' : 'justify-start'}">
        <div
          class="rounded-2xl px-4 py-2.5 text-sm leading-relaxed"
          style="max-width: 80%; white-space: pre-wrap;
                 background: {m.role === 'user' ? 'var(--accent)' : 'var(--surface-2)'};
                 color: {m.role === 'user' ? '#fff' : 'var(--text)'}"
        >
          {m.content}
          {#if m.actions && m.actions.length}
            <div class="mt-1.5 flex flex-wrap gap-1">
              {#each m.actions as a}
                <span
                  class="px-1.5 py-0.5 rounded text-xs font-mono"
                  style="background: var(--surface); color: var(--text-muted)"
                >⚙ {a}</span>
              {/each}
            </div>
          {/if}
        </div>
      </div>
    {/each}

    {#if busy}
      <div class="flex justify-start">
        <div class="rounded-2xl px-4 py-2.5 text-sm" style="background: var(--surface-2); color: var(--text-muted)">
          thinking…
        </div>
      </div>
    {/if}
  </div>

  <div class="mt-3 flex gap-2 items-end">
    <button
      onclick={toggleMic}
      disabled={busy && !recording}
      title="Tap to talk"
      class="shrink-0 px-3 py-2.5 rounded-lg text-sm font-medium transition-colors"
      style="background: {recording ? '#e5484d' : 'var(--surface-2)'}; color: {recording ? '#fff' : 'var(--text)'}"
    >{recording ? '● Listening' : '🎤'}</button>
    <textarea
      bind:this={ta}
      bind:value={input}
      onkeydown={onKey}
      oninput={autoGrow}
      rows="1"
      placeholder="Ask or command…  (Enter to send, Shift+Enter for a new line)"
      disabled={busy}
      class="flex-1 rounded-lg border px-4 py-2.5 text-sm outline-none resize-none leading-relaxed"
      style="border-color: var(--border); background: var(--surface); color: var(--text); max-height: 160px; overflow-y: auto"
    ></textarea>
    <button
      onclick={() => send(input)}
      disabled={busy || !input.trim()}
      class="px-5 py-2.5 rounded-lg text-sm font-medium transition-opacity"
      style="background: var(--accent); color: #fff; opacity: {busy || !input.trim() ? 0.5 : 1}"
    >Send</button>
  </div>
</div>
