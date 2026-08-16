<script lang="ts">
  import { tick, onMount } from 'svelte'

  // ─────────── AI hub: one place, three modes ───────────
  type Mode = 'ask' | 'create' | 'terminal'
  let mode = $state<Mode>('ask')
  let isOwner = $state(false)
  let termSrc = $state('/aiterm')
  let termLoaded = $state(false)

  onMount(async () => {
    try {
      const d = await (await fetch('/api/auth/me')).json()
      isOwner = !!d.authenticated && !!d.email && d.email === d.ownerEmail
    } catch {}
  })

  // ═══════════ ASK (local model chat) ═══════════
  type Msg = { role: 'user' | 'assistant'; content: string; actions?: string[]; source?: string }
  let msgs = $state<Msg[]>([])
  let input = $state('')
  let busy = $state(false)
  let scroller: HTMLDivElement
  let ta: HTMLTextAreaElement
  let lastUser = $state('')

  function autoGrow() {
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = Math.min(ta.scrollHeight, 160) + 'px'
  }

  // ── voice ──
  let recording = $state(false)
  let voiceOn = $state(false)
  let recorder: MediaRecorder | null = null
  let chunks: Blob[] = []
  let audioCtx: AudioContext | null = null
  let vadRAF = 0
  let player: HTMLAudioElement | null = null

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
    const tick2 = () => {
      if (!recording || !audioCtx) return
      analyser.getByteTimeDomainData(buf)
      let sum = 0
      for (let i = 0; i < buf.length; i++) { const v = (buf[i] - 128) / 128; sum += v * v }
      const rms = Math.sqrt(sum / buf.length)
      const now = performance.now()
      if (rms > THRESH) { spoke = true; quietSince = now }
      if (spoke && now - start > MIN_MS && now - quietSince > SILENCE_MS) return stopRecording()
      if (now - start > MAX_MS) return stopRecording()
      vadRAF = requestAnimationFrame(tick2)
    }
    vadRAF = requestAnimationFrame(tick2)
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
          if (text?.trim()) await send(text, true)
          else msgs.push({ role: 'assistant', content: "(I didn't catch that — try again)" })
        } catch {
          busy = false
          msgs.push({ role: 'assistant', content: '⚠️ Could not transcribe audio.' })
        }
      }
      recorder.start()
      recording = true
      voiceOn = true
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
    lastUser = message
    if (ta) ta.style.height = 'auto'
    msgs.push({ role: 'user', content: message })
    busy = true
    scrollDown()
    const history = msgs.slice(0, -1).map((m) => ({ role: m.role, content: m.content }))
    try {
      const res = await fetch('/api/assistant', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message, history }),
      })
      const data = await res.json()
      msgs.push({ role: 'assistant', content: data.reply ?? '(no reply)', actions: data.actions ?? [], source: 'local' })
      if (spoken || voiceOn) speak(data.reply ?? '')
    } catch (e) {
      msgs.push({ role: 'assistant', content: '⚠️ Could not reach the assistant. Is the local model running?' })
    } finally {
      busy = false
      scrollDown()
    }
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(input) }
  }

  // ═══════════ escalate → Terminal (subscription) ═══════════
  function askClaude(q?: string) {
    const t = (q ?? input ?? '').trim() || lastUser
    if (!isOwner) return // Terminal is owner-gated
    termSrc = t ? '/aiterm?q=' + encodeURIComponent(t) : '/aiterm'
    termLoaded = true
    mode = 'terminal'
  }

  // ═══════════ CREATE (LED scenes — subscription-backed) ═══════════
  let scenePrompt = $state('')
  let sceneBusy = $state(false)
  let sceneDeluxe = $state(false)
  let sceneMsg = $state('')
  const sceneChips = ['a calm ocean sunset', 'minecraft world', 'a cozy campfire', 'northern lights', 'lava volcano']

  async function genScene(p?: string) {
    const prompt = (p ?? scenePrompt).trim()
    if (!prompt || sceneBusy) return
    scenePrompt = prompt
    sceneBusy = true
    sceneMsg = sceneDeluxe ? 'Designing with Claude…' : 'Designing locally…'
    try {
      const r = await fetch('/api/matrix/generate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ prompt, deluxe: sceneDeluxe }),
      })
      const d = await r.json()
      sceneMsg = d.ok ? `✓ ${d.source}: ${d.applied} — ${d.result}` : `⚠ ${d.note || 'could not generate'}`
    } catch { sceneMsg = '⚠ could not reach the scene generator' }
    finally { sceneBusy = false }
  }
  async function surprise() {
    if (sceneBusy) return
    sceneBusy = true
    sceneMsg = 'Surprising…'
    try {
      const d = await (await fetch('/api/matrix/surprise', { method: 'POST' })).json()
      sceneMsg = d.ok ? `✓ surprise — ${d.result || 'new scene'}` : `⚠ ${d.note || 'failed'}`
    } catch { sceneMsg = '⚠ could not reach the scene generator' }
    finally { sceneBusy = false }
  }

  const modes = $derived<[Mode, string, string][]>(
    isOwner
      ? [['ask', 'Ask', '💬'], ['create', 'Create', '✦'], ['terminal', 'Terminal', '⌘']]
      : [['ask', 'Ask', '💬'], ['create', 'Create', '✦']]
  )
</script>

<svelte:head><title>HomeForge · AI</title></svelte:head>

<div class="mx-auto flex flex-col" style="max-width: 900px; height: calc(100vh - 8rem)">
  <!-- header + mode switcher -->
  <div class="mb-3 flex items-center justify-between gap-3 flex-wrap">
    <h1 class="text-xl font-bold" style="color: var(--text)">🤖 HomeForge AI</h1>
    <div class="flex gap-1 p-1 rounded-lg" style="background: var(--surface-2)">
      {#each modes as [m, label, icon]}
        <button
          onclick={() => (mode = m)}
          class="px-3 py-1.5 rounded-md text-sm font-medium transition-colors"
          style="color: {mode === m ? '#fff' : 'var(--text-muted)'};
                 background: {mode === m ? 'var(--accent)' : 'transparent'}"
        >{icon} {label}</button>
      {/each}
    </div>
  </div>

  {#if mode === 'ask'}
    <p class="text-xs mb-2" style="color: var(--text-muted)">
      Local AI — runs entirely on your server, fast &amp; private.
      {#if isOwner}Need deeper reasoning? Tap <b style="color: var(--accent)">⚡ Claude</b>.{/if}
    </p>

    <div bind:this={scroller} class="flex-1 overflow-y-auto rounded-lg border p-4 flex flex-col gap-3"
      style="border-color: var(--border); background: var(--surface)">
      {#if msgs.length === 0}
        <div class="m-auto text-center" style="color: var(--text-muted)">
          <div class="text-4xl mb-3">💬</div>
          <div class="text-sm mb-4">Try one of these:</div>
          <div class="flex flex-wrap gap-2 justify-center" style="max-width: 520px">
            {#each examples as ex}
              <button onclick={() => send(ex)}
                class="px-3 py-1.5 rounded-full text-sm border transition-colors"
                style="border-color: var(--border); color: var(--text); background: var(--surface-2)">{ex}</button>
            {/each}
          </div>
        </div>
      {/if}

      {#each msgs as m}
        <div class="flex {m.role === 'user' ? 'justify-end' : 'justify-start'}">
          <div class="flex flex-col gap-1" style="max-width: 82%">
            <div class="rounded-2xl px-4 py-2.5 text-sm leading-relaxed" style="white-space: pre-wrap;
                   background: {m.role === 'user' ? 'var(--accent)' : 'var(--surface-2)'};
                   color: {m.role === 'user' ? '#fff' : 'var(--text)'}">
              {m.content}
              {#if m.actions && m.actions.length}
                <div class="mt-1.5 flex flex-wrap gap-1">
                  {#each m.actions as a}
                    <span class="px-1.5 py-0.5 rounded text-xs font-mono" style="background: var(--surface); color: var(--text-muted)">⚙ {a}</span>
                  {/each}
                </div>
              {/if}
            </div>
            {#if m.role === 'assistant' && m.source}
              <div class="flex items-center gap-2 px-1">
                <span class="text-xs font-mono" style="color: var(--text-muted)">◍ Local 1.5B</span>
                {#if isOwner}
                  <button onclick={() => askClaude(lastUser)} class="text-xs" style="color: var(--accent); background: transparent; border: none; cursor: pointer">⚡ Ask Claude →</button>
                {/if}
              </div>
            {/if}
          </div>
        </div>
      {/each}

      {#if busy}
        <div class="flex justify-start">
          <div class="rounded-2xl px-4 py-2.5 text-sm" style="background: var(--surface-2); color: var(--text-muted)">thinking…</div>
        </div>
      {/if}
    </div>

    <div class="mt-3 flex gap-2 items-end">
      <button onclick={toggleMic} disabled={busy && !recording} title="Tap to talk"
        class="shrink-0 px-3 py-2.5 rounded-lg text-sm font-medium transition-colors"
        style="background: {recording ? '#e5484d' : 'var(--surface-2)'}; color: {recording ? '#fff' : 'var(--text)'}">{recording ? '● Listening' : '🎤'}</button>
      <textarea bind:this={ta} bind:value={input} onkeydown={onKey} oninput={autoGrow} rows="1"
        placeholder="Ask or command…  (Enter to send, Shift+Enter = new line)" disabled={busy}
        class="flex-1 rounded-lg border px-4 py-2.5 text-sm outline-none resize-none leading-relaxed"
        style="border-color: var(--border); background: var(--surface); color: var(--text); max-height: 160px; overflow-y: auto"></textarea>
      {#if isOwner}
        <button onclick={() => askClaude()} title="Ask your Claude subscription (deep reasoning)"
          class="shrink-0 px-3 py-2.5 rounded-lg text-sm font-medium transition-colors"
          style="background: transparent; color: var(--accent); border: 1px solid var(--accent)">⚡ Claude</button>
      {/if}
      <button onclick={() => send(input)} disabled={busy || !input.trim()}
        class="px-5 py-2.5 rounded-lg text-sm font-medium transition-opacity"
        style="background: var(--accent); color: #fff; opacity: {busy || !input.trim() ? 0.5 : 1}">Send</button>
    </div>

  {:else if mode === 'create'}
    <p class="text-xs mb-3" style="color: var(--text-muted)">
      Design a scene for the LED matrix. Simple ideas render <b>locally &amp; free</b>; tick
      <b style="color: var(--accent)">Deluxe</b> to use your <b>subscription</b> (no API key) for richer scenes.
    </p>
    <div class="rounded-lg border p-5 flex flex-col gap-4" style="border-color: var(--border); background: var(--surface)">
      <div class="flex gap-2 items-center flex-wrap">
        <input bind:value={scenePrompt} onkeydown={(e) => e.key === 'Enter' && genScene()}
          placeholder="Describe a scene… e.g. a calm ocean sunset"
          class="flex-1 rounded-lg border px-4 py-2.5 text-sm outline-none" style="min-width: 220px; border-color: var(--border); background: var(--surface-2); color: var(--text)" />
        <button onclick={() => genScene()} disabled={sceneBusy || !scenePrompt.trim()}
          class="px-5 py-2.5 rounded-lg text-sm font-medium" style="background: var(--accent); color: #fff; opacity: {sceneBusy || !scenePrompt.trim() ? 0.5 : 1}">✦ Make</button>
        <button onclick={surprise} disabled={sceneBusy}
          class="px-4 py-2.5 rounded-lg text-sm font-medium" style="background: var(--surface-2); color: var(--text); border: 1px solid var(--border)">🎲 Surprise</button>
      </div>
      <label class="flex items-center gap-2 text-sm" style="color: var(--text-muted)">
        <input type="checkbox" bind:checked={sceneDeluxe} /> Deluxe (Claude subscription)
      </label>
      <div class="flex flex-wrap gap-2">
        {#each sceneChips as c}
          <button onclick={() => genScene(c)} disabled={sceneBusy}
            class="px-3 py-1.5 rounded-full text-sm border" style="border-color: var(--border); color: var(--text); background: var(--surface-2)">{c}</button>
        {/each}
      </div>
      {#if sceneMsg}
        <div class="text-sm rounded-lg px-3 py-2" style="background: var(--surface-2); color: var(--text)">{sceneMsg}</div>
      {/if}
    </div>

  {:else}
    <p class="text-xs mb-2" style="color: var(--text-muted)">
      Full agent session on your <b>subscription</b> — reads &amp; controls the whole house via HomeForge tools. You type; owner-gated; every action logged.
    </p>
    <div class="flex-1 rounded-lg overflow-hidden border" style="border-color: var(--border); background: #0b1220">
      {#if termLoaded || mode === 'terminal'}
        <iframe title="AI Terminal" src={termSrc} style="width:100%; height:100%; border:0; display:block"></iframe>
      {/if}
    </div>
  {/if}
</div>
