package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/entity"
)

// Mobile companion-app sensor ingest. The HomeForge app pushes phone sensors here (battery,
// connectivity, device, activity) plus optional location/presence. Each reading becomes an
// entity under the phone's `device`, mirroring the health webhook. Booleans become
// binary_sensor.<phone>_<key>; scalars/strings become sensor.<phone>_<key>; location becomes
// device_tracker.<phone>. Auth-gated by the session cookie (the app is logged in). Entities
// persist via the store's own snapshot, so they survive restarts between the ~15-min pushes.
//
// Body:
//
//	{
//	  "device": "user_pixel",              // phone id (required, slugged)
//	  "name":   "the user's phone",            // friendly name (optional)
//	  "sensors": {
//	    "battery_level": {"state": 87, "unit": "%", "device_class": "battery"},
//	    "charging":      {"state": true},                    // -> binary_sensor
//	    "wifi_ssid":     {"state": "Liquid2g"}
//	  },
//	  "location": {"zone": "home", "latitude": 43.6, "longitude": -116.2}  // optional
//	}
type mobileSensor struct {
	State       any    `json:"state"`
	Unit        string `json:"unit,omitempty"`
	DeviceClass string `json:"device_class,omitempty"`
	Icon        string `json:"icon,omitempty"`
}

type mobileReq struct {
	Device   string                  `json:"device"`
	Name     string                  `json:"name,omitempty"`
	Sensors  map[string]mobileSensor `json:"sensors"`
	Location *struct {
		Zone      string   `json:"zone,omitempty"`
		Latitude  *float64 `json:"latitude,omitempty"`
		Longitude *float64 `json:"longitude,omitempty"`
	} `json:"location,omitempty"`
}

func mobileSlug(k string) string {
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

// mobilePretty turns "battery_level" into "Battery Level" for the friendly name.
func mobilePretty(k string) string {
	parts := strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func (s *Server) handleMobileSensors(w http.ResponseWriter, r *http.Request) {
	var req mobileReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	dev := mobileSlug(req.Device)
	if dev == "" {
		http.Error(w, "device required", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = mobilePretty(req.Device)
	}
	ts := time.Now().Format(time.RFC3339)
	n := 0

	for k, sv := range req.Sensors {
		key := mobileSlug(k)
		if key == "" || sv.State == nil {
			continue
		}
		domain, state := "sensor", ""
		switch v := sv.State.(type) {
		case bool:
			domain = "binary_sensor"
			if v {
				state = "on"
			} else {
				state = "off"
			}
		case float64:
			state = strconv.FormatFloat(v, 'f', -1, 64)
		case json.Number:
			state = v.String()
		case string:
			state = v
		default:
			b, _ := json.Marshal(v)
			state = string(b)
		}
		attr := map[string]any{
			"device": dev, "section": "phone", "phone_name": name,
			"friendly_name": name + " " + mobilePretty(k), "updated": ts,
		}
		if sv.Unit != "" {
			attr["unit_of_measurement"] = sv.Unit
		}
		if sv.DeviceClass != "" {
			attr["device_class"] = sv.DeviceClass
		}
		if sv.Icon != "" {
			attr["icon"] = sv.Icon
		}
		s.store.Set(entity.Entity{
			ID:         domain + "." + dev + "_" + key,
			Name:       name + " " + mobilePretty(k),
			Domain:     domain,
			State:      state,
			Attributes: attr,
		})
		n++
	}

	// Optional location -> device_tracker.<phone> (home / not_home), for presence automations.
	if loc := req.Location; loc != nil {
		st := strings.ToLower(strings.TrimSpace(loc.Zone))
		switch st {
		case "home":
			st = "home"
		case "", "away", "not_home", "nothome":
			st = "not_home"
		}
		attr := map[string]any{
			"device": dev, "section": "phone", "phone_name": name,
			"friendly_name": name, "source_type": "gps", "updated": ts,
		}
		if loc.Latitude != nil {
			attr["latitude"] = *loc.Latitude
		}
		if loc.Longitude != nil {
			attr["longitude"] = *loc.Longitude
		}
		s.store.Set(entity.Entity{
			ID: "device_tracker." + dev, Name: name, Domain: "device_tracker",
			State: st, Attributes: attr,
		})
		n++
	}

	slog.Info("api: mobile sensors", "device", dev, "count", n)
	writeJSON(w, map[string]any{"ok": true, "count": n})
}
