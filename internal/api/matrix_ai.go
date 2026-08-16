package api

// AI scene generation for the D1 Matrix. A prompt goes to the LOCAL model first
// (qwen2.5:3b via ollama — free, unlimited), which emits either a built-in scene
// id or a custom scene spec (the mScene DSL). If the local model can't produce a
// usable result — or the caller asks for "deluxe" — it falls back to the Claude
// API (claude-haiku-4-5) under per-user limits: a daily cap, a cooldown, and a
// monthly-$ ceiling, so Victor can experiment without running up cost. The
// Anthropic key lives in /data/anthropic-matrix.key (gitignored, never logged).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	matrixKeyPath     = "/data/anthropic-matrix.key"
	matrixUsagePath   = "/data/matrix-usage.json"
	claudeMatrixModel = "claude-haiku-4-5" // cheap + JSON-capable; bump to claude-sonnet-5 for richer scenes
	claudeMatrixURL   = "https://api.anthropic.com/v1/messages"
	ollamaMatrixURL   = "http://127.0.0.1:11435/api/chat"
	ollamaMatrixModel = "qwen2.5:3b-instruct"

	// Per-user Claude limits (the local model is unmetered).
	matrixDailyCap    = 25    // Claude generations / user / day
	matrixCooldownSec = 15    // min seconds between Claude calls / user
	matrixMonthlyUSD  = 3.0   // ~$/user/month ceiling for Claude
	// claude-haiku-4-5 pricing per 1M tokens
	claudeInPer1M  = 1.0
	claudeOutPer1M = 5.0
)

// matrixSystemPrompt teaches the model the scene DSL + the panel's clarity rule.
const matrixSystemPrompt = `You design scenes for a 48x48 RGB LED panel in a kid's room.

Respond with ONLY a JSON object — no prose, no markdown fences. Two forms:
  {"builtin": <id>}                       to play a built-in on-device scene, OR
  {"scene": {"name": "...", "gamma": 2.5, "layers": [ ... ]}}   for a custom scene.

Built-in scene ids: 0 World, 1 Creeper(face), 2 Rainbow, 5 Plasma, 6 Village,
7 Pacman, 8 Minecraft, 9 Day(hot-air-balloons), 10 Volcano, 11 Cave(lava+bats).
Prefer a built-in when the request clearly matches one (e.g. "minecraft"->8,
"rainbow"->2, "pacman"->7, "volcano"->10). Use a custom scene for novel requests.

CLARITY RULE (most important): this panel looks best with a DARK/black background
and a FEW bright, distinct subjects. Do NOT fill the whole panel with bright color —
lit fills bloom into haze. Dark negative space reads as crisp.

Custom-scene layers (draw back-to-front; only include fields you need). Colors are
"#rrggbb":
- {"type":"background","kind":"black"}                      (best clarity; default)
- {"type":"background","kind":"gradient","colors":["#0a1030","#02030c"]}  (top,bottom; kept dim)
- {"type":"background","kind":"solid","color":"#0a0810"}
- {"type":"stars","count":50,"color":"#d2d7f0","h":32}     (twinkling; h = sky height)
- {"type":"celestial","kind":"moon","scale":4,"color":"#e6eaf5"}  (kind sun|moon; add "speed":1 to ARC it across the sky = rise, cross, set; the moon auto-follows the sun. Without speed it stays fixed at x,y)
- {"type":"terrain","kind":"grass","y":40,"colors":["#1e5a24","#123018"]}  (flat ground; kind grass|stone|sand; y=top row)
- {"type":"hills","kind":"blocky","y":32,"colors":["#3a8a3a","#5a3a1a"]}  (bumpy ground / "mountains"; kind blocky=Minecraft | rolling; "h"=height)
- {"type":"trees","count":4,"y":33,"color":"#2e8b2e"}  (a row of trees on ground level y)
- {"type":"houses","count":2,"y":33}  (a row of little houses with roofs, doors, windows)
- {"type":"creeper","count":1,"y":40}  (Minecraft creepers that WANDER, flash, then EXPLODE on a loop; y=feet)
- {"type":"pool","kind":"lava","x":0,"y":38,"w":48,"h":10}  (kind lava|water)
- {"type":"particles","kind":"rain","count":50,"speed":2,"color":"#5f8cdc"}  (kind rain|snow|embers|steam)

Game objects (count spreads them along the ground at y):
- {"type":"mob","kind":"zombie","count":1,"y":40}  (kind: steve|zombie|skeleton|villager|enderman|spider)
- {"type":"animals","kind":"sheep","count":2,"y":40}  (kind: sheep|pig|cow)
- {"type":"flowers","count":6,"y":42}
- {"type":"torch","count":3,"y":40}
- {"type":"clouds","count":3}
- {"type":"tnt","count":2,"y":40}
- {"type":"portal","x":20,"y":24,"w":8,"h":14}  (swirling purple portal)

Rules: "creeper"/"explode"/"blow up" -> creeper layer (it wanders and leaves a crater). "house"/"village" -> houses. "blocky"/"minecraft"/"mountains"/"hills" -> hills(kind blocky). "sun/moon rising, crossing, or setting" -> "speed":1 on those celestial layers. The user may describe MANY things in one paragraph — add one layer for EVERY item you recognize from the lists above, and skip anything not listed. Order back-to-front: sky/clouds/celestial, hills/terrain, portal/houses/trees, then mobs/animals/creepers/tnt on top. Up to ~10 layers. Output COMPACT JSON on one line.

Example ("houses and trees on blocky hills, sun and moon crossing the sky, a creeper that blows up"):
{"scene":{"name":"Blocky World","gamma":2.5,"layers":[{"type":"background","kind":"gradient","colors":["#1a2a4a","#0a0f1a"]},{"type":"celestial","kind":"sun","scale":3,"color":"#ffdd44","speed":1},{"type":"celestial","kind":"moon","scale":2,"color":"#e6eaf5","speed":1},{"type":"hills","kind":"blocky","y":30,"colors":["#3a8a3a","#5a3a1a"]},{"type":"houses","count":2,"y":31},{"type":"trees","count":3,"y":31,"color":"#2e8b2e"},{"type":"creeper","count":1,"y":41}]}}`

// ---- per-user usage limits ----

type matrixUserUsage struct {
	Day       string  `json:"day"`
	DayCount  int     `json:"day_count"`
	Month     string  `json:"month"`
	MonthUSD  float64 `json:"month_usd"`
	LastUnix  int64   `json:"last_unix"`
}

type matrixUsage struct {
	mu   sync.Mutex
	data map[string]*matrixUserUsage
}

func newMatrixUsage() *matrixUsage {
	u := &matrixUsage{data: map[string]*matrixUserUsage{}}
	if b, err := os.ReadFile(matrixUsagePath); err == nil {
		_ = json.Unmarshal(b, &u.data)
	}
	if u.data == nil {
		u.data = map[string]*matrixUserUsage{}
	}
	return u
}

func (u *matrixUsage) save() {
	b, _ := json.MarshalIndent(u.data, "", "  ")
	tmp := matrixUsagePath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, matrixUsagePath)
	}
}

// checkClaude reports whether a Claude call is allowed for email right now, and a
// reason if not. Caller must hold nothing; this locks internally.
func (u *matrixUsage) checkClaude(email string) (bool, string) {
	now := time.Now()
	day := now.Format("2006-01-02")
	month := now.Format("2006-01")
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.data[email]
	if e == nil {
		return true, ""
	}
	if e.Month != month {
		e.MonthUSD = 0
	}
	if e.Day != day {
		e.DayCount = 0
	}
	if e.MonthUSD >= matrixMonthlyUSD {
		return false, "monthly AI budget reached — using the local model or built-in scenes"
	}
	if e.DayCount >= matrixDailyCap {
		return false, "daily AI limit reached — try again tomorrow or use built-in scenes"
	}
	if now.Unix()-e.LastUnix < matrixCooldownSec {
		return false, fmt.Sprintf("slow down a moment (%ds cooldown)", matrixCooldownSec)
	}
	return true, ""
}

func (u *matrixUsage) recordClaude(email string, inTok, outTok int) {
	now := time.Now()
	day := now.Format("2006-01-02")
	month := now.Format("2006-01")
	cost := float64(inTok)/1e6*claudeInPer1M + float64(outTok)/1e6*claudeOutPer1M
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.data[email]
	if e == nil {
		e = &matrixUserUsage{}
		u.data[email] = e
	}
	if e.Month != month {
		e.Month, e.MonthUSD = month, 0
	}
	if e.Day != day {
		e.Day, e.DayCount = day, 0
	}
	e.DayCount++
	e.MonthUSD += cost
	e.LastUnix = now.Unix()
	u.save()
}

func (u *matrixUsage) snapshot(email string) map[string]any {
	now := time.Now()
	day := now.Format("2006-01-02")
	month := now.Format("2006-01")
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.data[email]
	dayCount, monthUSD := 0, 0.0
	if e != nil {
		if e.Day == day {
			dayCount = e.DayCount
		}
		if e.Month == month {
			monthUSD = e.MonthUSD
		}
	}
	return map[string]any{
		"day_used":       dayCount,
		"day_cap":        matrixDailyCap,
		"month_usd":      monthUSD,
		"month_cap_usd":  matrixMonthlyUSD,
	}
}

// ---- generation ----

type genResult struct {
	Builtin *int    `json:"builtin"`
	Scene   *mScene `json:"scene"`
}

// extractJSON pulls the first {...last} object out of a model reply that may be
// wrapped in prose or ```json fences.
func extractJSON(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j <= i {
		return ""
	}
	return s[i : j+1]
}

func parseGen(raw string) *genResult {
	js := extractJSON(raw)
	if js == "" {
		return nil
	}
	var g genResult
	if json.Unmarshal([]byte(js), &g) != nil {
		return nil
	}
	if g.Builtin != nil {
		ok := false
		for _, b := range matrixBuiltins {
			if b.ID == *g.Builtin {
				ok = true
				break
			}
		}
		if !ok {
			g.Builtin = nil
		}
	}
	if g.Builtin == nil && (g.Scene == nil || len(g.Scene.Layers) == 0) {
		// Tolerate a bare scene object (no "scene" wrapper) — a common LLM slip.
		var s mScene
		if json.Unmarshal([]byte(js), &s) == nil && len(s.Layers) > 0 {
			g.Scene = &s
		} else {
			return nil
		}
	}
	return &g
}

// keywordBuiltin maps a prompt to the closest on-device scene id, or -1. Used as a
// graceful fallback when the AI's scene can't be parsed, so the panel still changes.
func keywordBuiltin(prompt string) int {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "village") || strings.Contains(p, "house"):
		return 6
	case strings.Contains(p, "minecraft") || strings.Contains(p, "blocky"):
		return 8
	case strings.Contains(p, "creeper"):
		return 1
	case strings.Contains(p, "cave") || strings.Contains(p, "lava"):
		return 11
	case strings.Contains(p, "volcano"):
		return 10
	case strings.Contains(p, "rainbow"):
		return 2
	case strings.Contains(p, "pacman") || strings.Contains(p, "pac-man"):
		return 7
	case strings.Contains(p, "balloon") || strings.Contains(p, "daytime"):
		return 9
	case strings.Contains(p, "night") || strings.Contains(p, "star") || strings.Contains(p, "world"):
		return 0
	}
	return -1
}

func ollamaScene(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  ollamaMatrixModel,
		"stream": false,
		"format": "json",
		"messages": []map[string]string{
			{"role": "system", "content": matrixSystemPrompt},
			{"role": "user", "content": prompt},
		},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ollamaMatrixURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}

func claudeScene(ctx context.Context, key, prompt string) (string, int, int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      claudeMatrixModel,
		"max_tokens": 2048,
		"system":     matrixSystemPrompt,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, claudeMatrixURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", 0, 0, fmt.Errorf("claude %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", 0, 0, fmt.Errorf("claude decode")
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), out.Usage.InputTokens, out.Usage.OutputTokens, nil
}

func matrixKey() string {
	b, err := os.ReadFile(matrixKeyPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// generate runs the local model, falls back to Claude when needed/allowed, applies
// the result to the panel, and returns a status map for the app.
func (m *matrixController) generate(email, prompt string, deluxe bool) (res map[string]any) {
	// Log every prompt + outcome (owner-reviewable). res is the named return, so
	// the defer captures whatever we return below.
	defer func() { logMatrixPrompt(email, prompt, res) }()
	apply := func(g *genResult) (string, any) {
		if g.Builtin != nil {
			_, _ = m.setBuiltin(*g.Builtin, "", "")
			return "builtin", *g.Builtin
		}
		m.startCustom(g.Scene)
		return "custom", g.Scene.Name
	}

	var localGen *genResult
	if !deluxe {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if raw, err := ollamaScene(ctx, prompt); err == nil {
			localGen = parseGen(raw)
		}
		cancel()
		if localGen != nil {
			kind, val := apply(localGen)
			return map[string]any{"ok": true, "source": "local", "applied": kind, "result": val, "usage": m.usage.snapshot(email)}
		}
	}

	// Escalate to the user's SUBSCRIPTION (via the aiterm sidecar's claude -p) — NO API key.
	// Deluxe request, or the local model failed. Per-user caps/cooldown still apply.
	genURL := os.Getenv("MATRIX_GEN_URL")
	if genURL == "" {
		return map[string]any{"ok": false, "source": "none", "note": "the smart AI isn't set up yet — try a simpler idea or a built-in scene", "usage": m.usage.snapshot(email)}
	}
	if ok, reason := m.usage.checkClaude(email); !ok {
		return map[string]any{"ok": false, "source": "limited", "note": reason, "usage": m.usage.snapshot(email)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	raw, err := subscriptionScene(ctx, genURL, prompt)
	cancel()
	if err != nil {
		return map[string]any{"ok": false, "source": "subscription", "note": "the AI had trouble with that — try rephrasing", "usage": m.usage.snapshot(email)}
	}
	m.usage.recordClaude(email, 0, 0) // count the generation (no per-token cost on a subscription)
	g := parseGen(raw)
	if g == nil {
		// The AI's scene didn't come out clean — fall back to the closest built-in so
		// the panel still changes instead of erroring.
		if id := keywordBuiltin(prompt); id >= 0 {
			m.setBuiltin(id, "", "")
			return map[string]any{"ok": true, "source": "builtin", "applied": "builtin", "result": id, "note": "used the closest built-in scene", "usage": m.usage.snapshot(email)}
		}
		return map[string]any{"ok": false, "source": "subscription", "note": "couldn't turn that into a scene — try a clearer description", "usage": m.usage.snapshot(email)}
	}
	kind, val := apply(g)
	return map[string]any{"ok": true, "source": "subscription", "applied": kind, "result": val, "usage": m.usage.snapshot(email)}
}

// subscriptionScene asks the aiterm sidecar to generate a scene using the user's AI SUBSCRIPTION
// (claude -p, no Anthropic API key). It sends the scene system prompt + the request and returns
// the raw model text (JSON) for parseGen.
func subscriptionScene(ctx context.Context, genURL, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"system": matrixSystemPrompt,
		"prompt": "Request: " + prompt + "\n\nRespond with ONLY the JSON object.",
	})
	u := genURL
	if sec := os.Getenv("AITERM_SECRET"); sec != "" {
		if strings.Contains(u, "?") {
			u += "&key=" + sec
		} else {
			u += "?key=" + sec
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gen %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// ---- handlers ----

func (s *Server) handleMatrixGenerate(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	email, ok := s.sessionEmail(r)
	if !ok || email == "" {
		email = "system"
	}
	var b struct {
		Prompt string `json:"prompt"`
		Deluxe bool   `json:"deluxe"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	prompt := strings.TrimSpace(b.Prompt)
	if prompt == "" {
		http.Error(w, "empty prompt", http.StatusBadRequest)
		return
	}
	res := s.matrix.generate(email, prompt, b.Deluxe)
	s.logAIActivity(email, "create", prompt, fmt.Sprintf("%v/%v", res["source"], res["result"]), nil)
	writeJSON(w, res)
}

func (s *Server) handleMatrixUsage(w http.ResponseWriter, r *http.Request) {
	if s.matrix == nil {
		http.Error(w, "matrix not configured", http.StatusNotImplemented)
		return
	}
	email, ok := s.sessionEmail(r)
	if !ok || email == "" {
		email = "system"
	}
	writeJSON(w, s.matrix.usage.snapshot(email))
}

// ---- prompt log (owner-reviewable) ----

const matrixPromptLog = "/data/matrix-prompts.jsonl"

// logMatrixPrompt appends one line per AI generation: who, the exact prompt, and
// the outcome. Append-only JSONL on the /data volume.
func logMatrixPrompt(email, prompt string, res map[string]any) {
	entry := map[string]any{
		"t":      time.Now().Format(time.RFC3339),
		"user":   email,
		"prompt": prompt,
	}
	if res != nil {
		entry["source"] = res["source"]
		entry["ok"] = res["ok"]
		if v, ok := res["result"]; ok {
			entry["result"] = v
		}
		if v, ok := res["note"]; ok {
			entry["note"] = v
		}
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(matrixPromptLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// isOwnerReq: the request is the owner (or an internal/device call). Scoped
// accounts (Victor) are not owners.
func (s *Server) isOwnerReq(r *http.Request) bool {
	if s.internalToken != "" && r.Header.Get("X-HF-Internal") == s.internalToken {
		return true
	}
	email, ok := s.sessionEmail(r)
	if !ok {
		return false
	}
	if s.auth != nil && s.auth.ownerEmail != "" && email == s.auth.ownerEmail {
		return true
	}
	prof, has := s.auth.profileFor(email)
	return !has || prof.IsOwner
}

// GET /api/matrix/history — owner-only; last 200 prompts, newest first.
func (s *Server) handleMatrixHistory(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerReq(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	data, err := os.ReadFile(matrixPromptLog)
	if err != nil {
		w.Write([]byte("[]"))
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	rows := make([]json.RawMessage, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			rows = append(rows, json.RawMessage(ln))
		}
	}
	if len(rows) > 200 {
		rows = rows[len(rows)-200:]
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	json.NewEncoder(w).Encode(rows)
}

// GET /aihistory — owner-only web page rendering the prompt log.
func (s *Server) handleAiHistoryPage(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerReq(r) {
		http.Redirect(w, r, "/download", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(aiHistoryHTML))
}

const aiHistoryHTML = `<!doctype html><html><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1"><title>AI Prompt Log</title>
<style>
 body{font-family:system-ui,-apple-system,sans-serif;background:#0f1117;color:#e2e8f0;margin:0;padding:16px}
 h1{font-size:18px;margin:0 0 4px}
 .sub{color:#64748b;font-size:13px;margin:0 0 16px}
 .row{background:#1a1d27;border:1px solid #262a38;border-radius:12px;padding:12px 14px;margin:8px 0}
 .p{font-size:15px;font-weight:600}
 .m{color:#64748b;font-size:12px;margin-top:4px}
 .ok{color:#10b981}.bad{color:#ef4444}
</style></head><body>
 <h1>AI Prompt Log</h1>
 <p class=sub>What each account has asked the LED-scene AI (newest first).</p>
 <div id=list>Loading…</div>
<script>
 fetch('/api/matrix/history',{credentials:'same-origin'}).then(function(r){return r.json();}).then(function(rows){
   var el=document.getElementById('list'); el.textContent='';
   if(!rows.length){ el.textContent='No AI prompts yet.'; return; }
   rows.forEach(function(x){
     var d=document.createElement('div'); d.className='row';
     var p=document.createElement('div'); p.className='p'; p.textContent=x.prompt||'(empty)';
     var m=document.createElement('div'); m.className='m';
     var outcome = x.ok ? ('made: '+(x.result||'a scene')) : ('failed: '+(x.note||''));
     m.innerHTML='<span class="'+(x.ok?'ok':'bad')+'">'+outcome+'</span> · '+(x.source||'')+' · '+(x.user||'')+' · '+(x.t||'');
     d.appendChild(p); d.appendChild(m); el.appendChild(d);
   });
 }).catch(function(){ document.getElementById('list').textContent='Could not load.'; });
</script></body></html>`
