package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
)

// Floor-plan HVAC vent annotations (supply/return registers + a typed measurement per vent).
// Stored SEPARATELY from device pins so it never touches /data/floorplan.json.
const ventsPath = "/data/floorplan-vents.json"

func (s *Server) handleVentsGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	data, err := os.ReadFile(ventsPath)
	if err != nil {
		w.Write([]byte("[]"))
		return
	}
	w.Write(data)
}

func (s *Server) handleVentsPut(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !json.Valid(data) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(ventsPath, data, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
