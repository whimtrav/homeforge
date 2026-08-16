package api

// ai_activity.go: an owner-reviewable audit log of every AI prompt across the hub — who asked
// what, in which mode (ask / create / terminal), and the outcome. Requested so a parent can see
// what a scoped user (e.g. Victor) asks the assistant. Append-only JSONL in /data; read is
// owner-only (isOwnerReq). Create also has its own matrix-prompts.jsonl; this is the unified view.

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const aiActivityLog = "/data/ai-activity.jsonl"

var aiActivityMu sync.Mutex

type aiActivityEntry struct {
	Time    string   `json:"time"`
	Email   string   `json:"email"`
	Mode    string   `json:"mode"`
	Prompt  string   `json:"prompt"`
	Reply   string   `json:"reply,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// logAIActivity appends one record. Best-effort: never blocks or fails a request.
func (s *Server) logAIActivity(email, mode, prompt, reply string, actions []string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	if email == "" {
		email = "unknown"
	}
	if len(prompt) > 4000 {
		prompt = prompt[:4000]
	}
	if len(reply) > 2000 {
		reply = reply[:2000]
	}
	b, err := json.Marshal(aiActivityEntry{
		Time: time.Now().UTC().Format(time.RFC3339), Email: email, Mode: mode,
		Prompt: prompt, Reply: reply, Actions: actions,
	})
	if err != nil {
		return
	}
	aiActivityMu.Lock()
	defer aiActivityMu.Unlock()
	f, err := os.OpenFile(aiActivityLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// handleAIActivity returns recent AI activity, newest first. Owner-only. Optional ?email= filter
// (review one user) and ?limit= (default 200).
func (s *Server) handleAIActivity(w http.ResponseWriter, r *http.Request) {
	if !s.isOwnerReq(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))

	aiActivityMu.Lock()
	data, _ := os.ReadFile(aiActivityLog)
	aiActivityMu.Unlock()

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]aiActivityEntry, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e aiActivityEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if filter != "" && strings.ToLower(e.Email) != filter {
			continue
		}
		out = append(out, e)
	}
	writeJSON(w, map[string]any{"activity": out, "count": len(out)})
}
