package api

// HomeForge AI assistant: a local-LLM agent (ollama, CPU-only) that controls devices and
// answers questions about the home. The model is the language/orchestration layer; the real
// work is exact tools over the entity store + service bus ("capability = the system, not the
// model size"). Tuned to run on a no-GPU 4-core / 8GB box (the Beelink port target):
//   - static, cacheable system prompt (no live state) so ollama prompt-caches the big prefix,
//   - a small tool-calling model (qwen2.5:3b-instruct) with bounded context/output,
//   - fuzzy entity resolution to absorb the small model's entity_id sloppiness,
//   - pre-warm on startup so the first real request hits a warm model + primed cache.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/config"
)

type olMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
}
type olToolCall struct {
	Function olFunc `json:"function"`
}
type olFunc struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
type olTool struct {
	Type     string    `json:"type"`
	Function olToolDef `json:"function"`
}
type olToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type olRequest struct {
	Model    string         `json:"model"`
	Messages []olMessage    `json:"messages"`
	Tools    []olTool       `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}
type olResponse struct {
	Message olMessage `json:"message"`
	Error   string    `json:"error,omitempty"`
}

// SetAssistant stores the assistant config; if enabled + prewarm, it primes the model and
// prompt cache in the background so the first CPU-bound request isn't the slow cold one.
func (s *Server) SetAssistant(c config.AssistantConfig) {
	s.assistantCfg = c
	s.mem = newAssistantMemory(c.MemoryFile)
	if c.Enabled && c.Prewarm {
		go s.prewarmAssistant()
	}
}

func (s *Server) prewarmAssistant() {
	time.Sleep(8 * time.Second) // let the store fill + ollama come up
	for i := 0; i < 30; i++ {
		msgs := []olMessage{{Role: "system", Content: s.assistantSystemPrompt()}, {Role: "user", Content: "hello"}}
		if _, err := s.ollamaChat(msgs, assistantTools()); err == nil {
			slog.Info("assistant: prewarmed (model loaded + prompt cached)", "model", s.assistantCfg.Model)
			return
		}
		time.Sleep(10 * time.Second)
	}
	slog.Warn("assistant: prewarm gave up (ollama not reachable yet?)")
}

// rewarm re-primes the prompt cache after the remembered-facts prompt changes, so the user's
// next message isn't the slow cold call. Fire-and-forget; queues behind the current request.
func (s *Server) rewarm() {
	if !s.assistantCfg.Enabled {
		return
	}
	msgs := []olMessage{{Role: "system", Content: s.assistantSystemPrompt()}, {Role: "user", Content: "ok"}}
	_, _ = s.ollamaChat(msgs, assistantTools())
}

// handleAssistant runs one turn of the agent loop: static system prompt → ollama → execute
// tool calls → loop → final natural-language reply.
func (s *Server) handleAssistant(w http.ResponseWriter, r *http.Request) {
	if !s.assistantCfg.Enabled {
		http.Error(w, "assistant disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Message string      `json:"message"`
		History []olMessage `json:"history"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, "need {message}", http.StatusBadRequest)
		return
	}

	messages := []olMessage{{Role: "system", Content: s.assistantSystemPrompt()}}
	messages = append(messages, req.History...)
	messages = append(messages, olMessage{Role: "user", Content: req.Message})

	tools := assistantTools()
	var actions []string

	steps := s.assistantCfg.MaxSteps
	if steps <= 0 {
		steps = 5
	}
	for i := 0; i < steps; i++ {
		msg, err := s.ollamaChat(messages, tools)
		if err != nil {
			http.Error(w, "assistant: "+err.Error(), http.StatusBadGateway)
			return
		}
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			writeJSON(w, map[string]any{"reply": msg.Content, "actions": actions})
			return
		}
		for _, tc := range msg.ToolCalls {
			result := s.execTool(tc.Function.Name, tc.Function.Arguments)
			actions = append(actions, tc.Function.Name)
			messages = append(messages, olMessage{Role: "tool", Content: result, ToolName: tc.Function.Name})
		}
	}
	writeJSON(w, map[string]any{"reply": "(took too many steps — try rephrasing)", "actions": actions})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

var assistantDomains = map[string]bool{"switch": true, "light": true, "fan": true, "number": true, "climate": true}

// assistantSystemPrompt is intentionally STATIC (id + name, no live state, no timestamps) so
// it is byte-identical across requests — that lets ollama reuse the KV cache for this large
// prefix instead of re-evaluating ~2-3k tokens on every CPU-bound call.
func (s *Server) assistantSystemPrompt() string {
	var b strings.Builder
	b.WriteString("You are the HomeForge home assistant for a smart home. Control devices and answer questions. Be concise and friendly.\n")
	b.WriteString("About you: you are the local AI built into HomeForge — a self-hosted smart-home hub written in Go, with a SvelteKit web UI and a TimescaleDB/Postgres history database, running entirely on the home's own server (no cloud). You are a small language model served locally by ollama on CPU. If asked what this system is made of or what language its code is written in: it is Go — the project rule is Go-first (Python only when no Go library exists), the database is TimescaleDB/Postgres, and device firmware is C++. You do not write code yourself; you act through your tools.\n")
	b.WriteString("Rules:\n")
	b.WriteString("- You have NO memory of live values or states. For ANY current reading or on/off state, call a tool THIS turn (read_sensor for readings, get_state for a known entity_id). Never state a value you did not just retrieve.\n")
	b.WriteString("- Report values EXACTLY as the tool shows them. Temperatures already include °F in parentheses — use that number, never do your own conversion math. If the tool shows no unit, give just the number.\n")
	b.WriteString("- read_sensor lists the best match FIRST — report that one value and stop. Do NOT mention the other matches, do NOT add caveats about sensors or units. get_state may return 'similar devices' — just pick the best fit.\n")
	b.WriteString("- For several distinct readings (e.g. a temperature AND a humidity), call read_sensor once per reading.\n")
	b.WriteString("- To control a device, use its entity_id from the list below (or find_devices if not listed), then call the tool.\n")
	b.WriteString("- When the user tells you to remember something (a preference, a name, how they refer to a place), call remember. When they say to forget something, call forget. Saved facts appear below and you should use them.\n")
	b.WriteString("- Answer EVERY part of the question, using the exact room/period asked.\n")
	b.WriteString("- After acting, confirm in ONE short sentence. If ambiguous, ask a brief clarifying question instead of guessing.\n\n")
	b.WriteString("Controllable devices (entity_id — name):\n")
	type row struct{ id, name string }
	var rows []row
	for _, e := range s.store.All() {
		if assistantDomains[strings.SplitN(e.ID, ".", 2)[0]] {
			rows = append(rows, row{e.ID, e.Name})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	limit := s.assistantCfg.DeviceCap
	if limit <= 0 {
		limit = 150
	}
	for i, r := range rows {
		if i >= limit {
			fmt.Fprintf(&b, "...(%d more — use find_devices)\n", len(rows)-limit)
			break
		}
		fmt.Fprintf(&b, "%s — %s\n", r.id, r.name)
	}
	b.WriteString("\nRead any sensor with read_sensor. The house thermostat is climate.house.\n")
	if s.mem != nil {
		if facts := s.mem.all(); len(facts) > 0 {
			b.WriteString("\nThings you've been asked to remember (known facts — use them directly, no tool needed to recall):\n")
			for i, f := range facts {
				if i >= 80 {
					break
				}
				fmt.Fprintf(&b, "- (#%d) %s\n", f.ID, f.Text)
			}
		}
	}
	return b.String()
}

func assistantTools() []olTool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	return []olTool{
		{Type: "function", Function: olToolDef{Name: "read_sensor",
			Description: "Search sensors by keyword and return their CURRENT values. Use for ANY temperature/humidity/power/water/air question. Never answer such questions from memory.",
			Parameters:  obj(map[string]any{"query": str("keywords, e.g. 'downstairs temperature' or 'living room humidity'")}, "query")}},
		{Type: "function", Function: olToolDef{Name: "get_state",
			Description: "Read the current state of ONE entity by its exact entity_id.",
			Parameters:  obj(map[string]any{"entity_id": str("full entity id, e.g. sensor.upstairs_temp_climate_temperature")}, "entity_id")}},
		{Type: "function", Function: olToolDef{Name: "set_switch",
			Description: "Turn a switch or light on or off.",
			Parameters: obj(map[string]any{"entity_id": str("switch/light entity id"),
				"state": map[string]any{"type": "string", "enum": []string{"on", "off"}}}, "entity_id", "state")}},
		{Type: "function", Function: olToolDef{Name: "set_number",
			Description: "Set a numeric control such as a ceiling-fan speed (0=off,1=low,2=med,3=high).",
			Parameters: obj(map[string]any{"entity_id": str("number entity id, e.g. number.familyroom_ceiling_fan"),
				"value": map[string]any{"type": "number"}}, "entity_id", "value")}},
		{Type: "function", Function: olToolDef{Name: "set_temperature",
			Description: "Set the house thermostat (climate.house) cool/heat setpoint in °F.",
			Parameters:  obj(map[string]any{"temperature": map[string]any{"type": "number", "description": "setpoint °F"}}, "temperature")}},
		{Type: "function", Function: olToolDef{Name: "find_devices",
			Description: "Search controllable devices by keyword (room or name) when unsure of the entity_id.",
			Parameters:  obj(map[string]any{"query": str("keyword like 'kitchen', 'fan', 'garage'")}, "query")}},
		{Type: "function", Function: olToolDef{Name: "water_usage",
			Description: "Get household water usage totals (gallons) for this hour, today, this week, and this month.",
			Parameters:  obj(map[string]any{})}},
		{Type: "function", Function: olToolDef{Name: "remember",
			Description: "Save a durable fact or preference the user asks you to remember (a name, a preference, how they refer to a room). It persists across restarts and future chats.",
			Parameters:  obj(map[string]any{"fact": str("the fact to remember, as a standalone statement")}, "fact")}},
		{Type: "function", Function: olToolDef{Name: "forget",
			Description: "Remove a remembered fact by a keyword it contains or by its number.",
			Parameters:  obj(map[string]any{"topic": str("keyword or number of the fact to forget")}, "topic")}},
	}
}

// resolveEntity maps a possibly-imperfect entity_id from the model (small models truncate or
// drop the domain, e.g. "office_ceiling" for switch.office_ceiling_light) to the best real
// controllable entity. Returns the input unchanged if nothing plausibly matches.
func (s *Server) resolveEntity(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	if _, ok := s.store.Get(id); ok {
		return id // exact hit
	}
	guess := strings.ToLower(id)
	if i := strings.Index(guess, "."); i >= 0 {
		guess = guess[i+1:] // strip any (possibly wrong) domain
	}
	gtoks := strings.FieldsFunc(guess, func(r rune) bool { return r == '_' || r == '.' || r == ' ' || r == '-' })
	bestID, bestScore := "", 0
	for _, e := range s.store.All() {
		if !assistantDomains[strings.SplitN(e.ID, ".", 2)[0]] {
			continue
		}
		lid := strings.ToLower(e.ID)
		hay := lid + " " + strings.ToLower(e.Name)
		score := 0
		if guess != "" && strings.Contains(lid, guess) {
			score += 5 // the model's guess is a substring of the real id
		}
		for _, t := range gtoks {
			if len(t) >= 2 && strings.Contains(hay, t) {
				score++
			}
		}
		if score > bestScore {
			bestScore, bestID = score, e.ID
		}
	}
	if bestScore >= 2 && bestID != "" {
		return bestID
	}
	return id
}

// sensorReading formats "id = state unit", and — because the household thinks in °F but the
// ESP sensors report °C — appends the °F conversion computed IN CODE so the model never has to
// do (and botch) the arithmetic. Tools do the hard thinking; the model just reports the number.
func sensorReading(id, state, unit string) string {
	line := strings.TrimSpace(fmt.Sprintf("%s = %s %s", id, state, unit))
	if strings.TrimSpace(unit) == "°C" {
		if c, err := strconv.ParseFloat(strings.TrimSpace(state), 64); err == nil {
			line += fmt.Sprintf(" (%.1f °F)", c*9/5+32)
		}
	}
	return line
}

// suggestDevices returns up to n controllable entities whose id/name overlaps the query —
// used to make tool misses self-healing for a weak model (it gets candidates inline).
func (s *Server) suggestDevices(query string, n int) string {
	q := strings.ToLower(query)
	if i := strings.Index(q, "."); i >= 0 {
		q = q[i+1:]
	}
	toks := strings.FieldsFunc(q, func(r rune) bool { return r == '_' || r == '.' || r == ' ' || r == '-' })
	var matches []string
	for _, e := range s.store.All() {
		if !assistantDomains[strings.SplitN(e.ID, ".", 2)[0]] {
			continue
		}
		hay := strings.ToLower(e.ID + " " + e.Name)
		for _, t := range toks {
			if len(t) >= 2 && strings.Contains(hay, t) {
				matches = append(matches, fmt.Sprintf("%s (%s)", e.ID, e.Name))
				break
			}
		}
		if len(matches) >= n {
			break
		}
	}
	return strings.Join(matches, "; ")
}

func (s *Server) execTool(name string, args map[string]any) string {
	getStr := func(k string) string { v, _ := args[k].(string); return v }
	getNum := func(k string) (float64, bool) {
		switch n := args[k].(type) {
		case float64:
			return n, true
		case json.Number:
			f, err := n.Float64()
			return f, err == nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
			return f, err == nil
		}
		return 0, false
	}
	call := func(service, entity string, data map[string]any) {
		s.bus.Publish("service.call", map[string]any{"service": service, "entity": entity, "data": data})
	}

	switch name {
	case "read_sensor":
		q := strings.ToLower(getStr("query"))
		words := strings.Fields(q)
		type hit struct {
			line    string
			score   int
			hasUnit bool
		}
		var hits []hit
		for _, e := range s.store.All() {
			if !strings.HasPrefix(e.ID, "sensor.") {
				continue
			}
			hay := strings.ToLower(e.ID + " " + e.Name)
			score := 0
			for _, w := range words {
				if strings.Contains(hay, w) {
					score++
				}
			}
			if score == 0 {
				continue
			}
			// prefer the primary LiquidFW room sensors (…_climate_*) and bury noisy/secondary
			// ones (a Kidde detector's IAQ temp reads hot; battery/signal/air-quality clutter)
			if strings.Contains(e.ID, "climate") {
				score += 2
			}
			for _, bad := range []string{"kidde", "uptime", "signal", "rssi", "battery", "eco2", "tvoc", "iaq", "linkquality", "_air_"} {
				if strings.Contains(e.ID, bad) {
					score -= 3
					break
				}
			}
			unit, _ := e.Attributes["unit_of_measurement"].(string)
			hits = append(hits, hit{sensorReading(e.ID, e.State, unit), score, unit != ""})
		}
		if len(hits) == 0 {
			return "no sensor matches: " + q
		}
		// best first; on a score tie, prefer the sensor that declares a unit (avoids the
		// unit-less duplicate that makes a small model guess/convert wrongly)
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].hasUnit && !hits[j].hasUnit
		})
		var out []string
		for i, h := range hits {
			if i >= 2 {
				break
			}
			out = append(out, h.line)
		}
		return "best match first: " + strings.Join(out, "; ")
	case "get_state":
		raw := getStr("entity_id")
		id := s.resolveEntity(raw)
		if e, ok := s.store.Get(id); ok {
			unit, _ := e.Attributes["unit_of_measurement"].(string)
			return sensorReading(id, e.State, unit)
		}
		// self-heal: hand the model similar devices inline so it needn't chain a find_devices
		if sug := s.suggestDevices(raw, 5); sug != "" {
			return "no exact entity '" + raw + "'. Similar devices: " + sug
		}
		return "no such entity: " + raw
	case "set_switch":
		id := s.resolveEntity(getStr("entity_id"))
		on := strings.ToLower(getStr("state")) == "on" || strings.ToLower(getStr("state")) == "true"
		svc := "switch.turn_off"
		if strings.HasPrefix(id, "light.") {
			svc = "light.turn_off"
			if on {
				svc = "light.turn_on"
			}
		} else if on {
			svc = "switch.turn_on"
		}
		call(svc, id, nil)
		return fmt.Sprintf("ok, turned %s %s", id, map[bool]string{true: "on", false: "off"}[on])
	case "set_number":
		id := s.resolveEntity(getStr("entity_id"))
		v, ok := getNum("value")
		if !ok {
			return "invalid value"
		}
		call("number.set_value", id, map[string]any{"value": v})
		return fmt.Sprintf("ok, set %s to %g", id, v)
	case "set_temperature":
		v, ok := getNum("temperature")
		if !ok {
			return "invalid temperature"
		}
		call("climate.set_temperature", "climate.house", map[string]any{"temperature": v})
		return fmt.Sprintf("ok, thermostat set to %g°F", v)
	case "find_devices":
		q := strings.ToLower(getStr("query"))
		var matches []string
		for _, e := range s.store.All() {
			if !assistantDomains[strings.SplitN(e.ID, ".", 2)[0]] {
				continue
			}
			if strings.Contains(strings.ToLower(e.ID), q) || strings.Contains(strings.ToLower(e.Name), q) {
				matches = append(matches, fmt.Sprintf("%s (%s) = %s", e.ID, e.Name, e.State))
				if len(matches) >= 25 {
					break
				}
			}
		}
		if len(matches) == 0 {
			return "no controllable devices match: " + q
		}
		return strings.Join(matches, "; ")
	case "water_usage":
		wreq, _ := http.NewRequest("GET", "http://127.0.0.1:8093/api/water/usage", nil)
		wreq.Header.Set("X-HF-Internal", s.internalToken) // bypass the auth gate for this server-internal call
		resp, err := http.DefaultClient.Do(wreq)
		if err != nil {
			return "water usage unavailable"
		}
		defer resp.Body.Close()
		var u struct {
			Hour  float64 `json:"hour"`
			Today float64 `json:"today"`
			Week  float64 `json:"week"`
			Month float64 `json:"month"`
		}
		json.NewDecoder(resp.Body).Decode(&u)
		return fmt.Sprintf("water used — this hour: %.1f gal; today: %.1f gal; this week: %.1f gal; this month: %.1f gal",
			u.Hour, u.Today, u.Week, u.Month)
	case "remember":
		if s.mem == nil {
			return "memory unavailable"
		}
		fact := strings.TrimSpace(getStr("fact"))
		if fact == "" {
			return "nothing to remember"
		}
		f := s.mem.add(fact)
		go s.rewarm() // re-prime the cache with the new fact in the prompt
		return fmt.Sprintf("remembered (#%d): %s", f.ID, f.Text)
	case "forget":
		if s.mem == nil {
			return "memory unavailable"
		}
		rm := s.mem.forget(getStr("topic"))
		if len(rm) == 0 {
			return "no remembered fact matched: " + getStr("topic")
		}
		go s.rewarm() // re-prime the cache with the fact removed from the prompt
		var ids []string
		for _, f := range rm {
			ids = append(ids, "#"+strconv.Itoa(f.ID))
		}
		return "forgot " + strings.Join(ids, ", ")
	}
	return "unknown tool: " + name
}

func (s *Server) ollamaChat(messages []olMessage, tools []olTool) (olMessage, error) {
	c := s.assistantCfg
	opts := map[string]any{
		"temperature": c.Temperature,
		"num_ctx":     c.NumCtx,
		"num_predict": c.NumPredict,
		"num_gpu":     c.NumGPU, // 0 = CPU-only
	}
	if c.NumThread > 0 {
		opts["num_thread"] = c.NumThread
	}
	body, _ := json.Marshal(olRequest{Model: c.Model, Messages: messages, Tools: tools, Stream: false, Options: opts})
	req, _ := http.NewRequest("POST", c.OllamaURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// generous timeout: the very first (cold) call also loads the model on a CPU box
	resp, err := (&http.Client{Timeout: 240 * time.Second}).Do(req)
	if err != nil {
		return olMessage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return olMessage{}, fmt.Errorf("ollama HTTP %d", resp.StatusCode)
	}
	var or olResponse
	if json.Unmarshal(raw, &or) != nil {
		return olMessage{}, fmt.Errorf("bad ollama response")
	}
	if or.Error != "" {
		return olMessage{}, fmt.Errorf("ollama: %s", or.Error)
	}
	return or.Message, nil
}

// handleSTT proxies uploaded audio to the local Whisper service and returns the transcript.
// Voice stays entirely on the box (faster-whisper on CPU) — nothing goes to the cloud.
func (s *Server) handleSTT(w http.ResponseWriter, r *http.Request) {
	if !s.assistantCfg.Enabled {
		http.Error(w, "assistant disabled", http.StatusServiceUnavailable)
		return
	}
	audio, err := io.ReadAll(io.LimitReader(r.Body, 25<<20)) // 25MB cap
	if err != nil || len(audio) == 0 {
		http.Error(w, "no audio", http.StatusBadRequest)
		return
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("audio_file", "audio.webm")
	fw.Write(audio)
	mw.Close()

	url := s.assistantCfg.WhisperURL + "/asr?encode=true&task=transcribe&language=en&output=txt"
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "speech-to-text unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "whisper HTTP "+strconv.Itoa(resp.StatusCode), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"text": strings.TrimSpace(string(body))})
}

// handleTTS proxies text to the local Piper service and streams back a WAV — a natural neural
// voice, generated entirely on the box. Playback (unlike mic capture) works over plain HTTP.
func (s *Server) handleTTS(w http.ResponseWriter, r *http.Request) {
	if !s.assistantCfg.Enabled {
		http.Error(w, "assistant disabled", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	text := strings.TrimSpace(body.Text)
	if text == "" {
		http.Error(w, "no text", http.StatusBadRequest)
		return
	}
	req, _ := http.NewRequest("POST", s.assistantCfg.PiperURL+"/tts", strings.NewReader(text))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "tts unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "piper HTTP "+strconv.Itoa(resp.StatusCode), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	io.Copy(w, resp.Body)
}
