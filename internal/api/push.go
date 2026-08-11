package api

import (
	"encoding/json"
	"net/http"

	"github.com/whimtrav/homeforge/internal/push"
)

// pushTokenReq is the body for register/unregister: the app's FCM device token.
type pushTokenReq struct {
	Token    string `json:"token"`
	Platform string `json:"platform,omitempty"` // "android" | "ios" (informational)
}

// handlePushRegister stores the app's FCM token so notify.* actions reach this device.
// Auth-gated (session cookie) like the rest of /api — only a logged-in app registers.
func (s *Server) handlePushRegister(w http.ResponseWriter, r *http.Request) {
	var req pushTokenReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	n := push.Add(req.Token)
	writeJSON(w, map[string]any{"ok": true, "count": n})
}

// handlePushUnregister removes a token (e.g. on logout / uninstall best-effort).
func (s *Server) handlePushUnregister(w http.ResponseWriter, r *http.Request) {
	var req pushTokenReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	push.Remove(req.Token)
	writeJSON(w, map[string]any{"ok": true})
}
