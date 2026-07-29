package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/whimtrav/homeforge/internal/entity"
)

// bleObs is one advertisement observed by a LiquidFW ESP32 scanner in one window.
type bleObs struct {
	MAC  string `json:"mac"`
	RSSI int    `json:"rssi"`
	Name string `json:"name,omitempty"`
	Mfg  string `json:"mfg,omitempty"`
	Svc  string `json:"svc,omitempty"`
}

func bleSanitize(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	var b strings.Builder
	for _, c := range k {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// handleBLE ingests one BLE scan window from a LiquidFW ESP32 scanner:
//
//	POST /api/ble/{device}   body = [{"mac","rssi","name","mfg","svc"}, ...]
//
// Discovery phase: we keep the whole window (sorted by RSSI, capped) in the
// scanner's count-entity attributes so everything it hears is visible from
// /api/entities, and log the named devices. Format decoders (sensors, presence,
// oral-b) come later once we see what's actually broadcasting.
func (s *Server) handleBLE(w http.ResponseWriter, r *http.Request) {
	scanner := bleSanitize(r.PathValue("device"))
	if scanner == "" {
		http.Error(w, "missing device", http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	trimmed := bytes.TrimSpace(raw)

	// Diagnostic object (posted once at boot): {"status","enabled","heap","largest"}.
	// Lets us see the BLE memory picture per device without serial access.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var d map[string]any
		if err := json.Unmarshal(trimmed, &d); err != nil {
			http.Error(w, "bad diag", http.StatusBadRequest)
			return
		}
		slog.Info("ble: diag", "scanner", scanner, "status", d["status"],
			"enabled", d["enabled"], "attempted", d["attempted"], "reset", d["reset"],
			"heap", d["heap"], "largest", d["largest"])
		s.store.Set(entity.Entity{
			ID:     "sensor.ble_" + scanner + "_status",
			Name:   scanner + " BLE status",
			Domain: "sensor",
			State:  fmt.Sprintf("%v", d["status"]),
			Attributes: map[string]any{
				"source": "ble", "scanner": scanner, "section": "ble",
				"enabled": d["enabled"], "attempted": d["attempted"], "reset": d["reset"],
				"heap": d["heap"], "largest": d["largest"],
			},
		})
		w.WriteHeader(http.StatusOK)
		return
	}

	var obs []bleObs
	if err := json.Unmarshal(trimmed, &obs); err != nil {
		http.Error(w, "bad body (expected JSON array)", http.StatusBadRequest)
		return
	}
	sort.SliceStable(obs, func(i, j int) bool { return obs[i].RSSI > obs[j].RSSI })

	named := 0
	var sample []string
	for _, o := range obs {
		if o.Name != "" {
			named++
			if len(sample) < 8 {
				sample = append(sample, fmt.Sprintf("%s(%ddBm)", o.Name, o.RSSI))
			}
		}
	}
	slog.Info("ble: window", "scanner", scanner, "total", len(obs), "named", named,
		"top_named", strings.Join(sample, " "))

	top := obs
	if len(top) > 40 {
		top = top[:40]
	}
	s.store.Set(entity.Entity{
		ID:     "sensor.ble_" + scanner + "_count",
		Name:   scanner + " BLE devices",
		Domain: "sensor",
		State:  fmt.Sprintf("%d", len(obs)),
		Attributes: map[string]any{
			"source": "ble", "scanner": scanner, "section": "ble",
			"named": named, "observations": top,
		},
	})
	w.WriteHeader(http.StatusOK)
}
