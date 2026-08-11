package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/entity"
)

// homeRadiusMeters is how close to the home coordinates counts as "home".
const homeRadiusMeters = 150.0

// mobileHomeFile persists the user-set precise home coordinates (the weather lat/lon is only a
// city-level reference and is too coarse for a geofence).
const mobileHomeFile = "/data/mobile-home.json"

// SetHome seeds the home-zone centre from integrations.weather, then applies any user-set
// precise home saved on disk (which overrides the coarse weather point).
func (s *Server) SetHome(lat, lon float64) {
	s.homeLat, s.homeLon = lat, lon
	if b, err := os.ReadFile(mobileHomeFile); err == nil {
		var h struct{ Latitude, Longitude float64 }
		if json.Unmarshal(b, &h) == nil && (h.Latitude != 0 || h.Longitude != 0) {
			s.homeLat, s.homeLon = h.Latitude, h.Longitude
		}
	}
}

// handleMobileConfig (GET) tells the app where home is, so it can register a geofence there.
func (s *Server) handleMobileConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"home": map[string]any{
			"latitude": s.homeLat, "longitude": s.homeLon, "radius": homeRadiusMeters,
		},
	})
}

// handleMobileConfigSet (POST) sets the precise home to the given coordinates (typically the
// phone's current location, captured while the user is home) and persists it.
func (s *Server) handleMobileConfigSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil ||
		req.Latitude == nil || req.Longitude == nil {
		http.Error(w, "latitude+longitude required", http.StatusBadRequest)
		return
	}
	s.homeLat, s.homeLon = *req.Latitude, *req.Longitude
	b, _ := json.Marshal(map[string]any{"latitude": s.homeLat, "longitude": s.homeLon})
	tmp := mobileHomeFile + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		_ = os.Rename(tmp, mobileHomeFile)
	}
	slog.Info("api: home set", "lat", s.homeLat, "lon", s.homeLon)
	writeJSON(w, map[string]any{"ok": true, "latitude": s.homeLat, "longitude": s.homeLon, "radius": homeRadiusMeters})
}

// haversineMeters is the great-circle distance between two lat/lon points, in metres.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

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
	// The app may send an explicit zone (from a geofence enter/exit) OR just lat/lon, in which
	// case we compute home/away against the configured home zone.
	if loc := req.Location; loc != nil {
		st := strings.ToLower(strings.TrimSpace(loc.Zone))
		switch st {
		case "home":
			st = "home"
		case "away", "not_home", "nothome":
			st = "not_home"
		default:
			// No explicit zone — derive it from coordinates if we have both.
			if loc.Latitude != nil && loc.Longitude != nil && (s.homeLat != 0 || s.homeLon != 0) {
				if haversineMeters(*loc.Latitude, *loc.Longitude, s.homeLat, s.homeLon) <= homeRadiusMeters {
					st = "home"
				} else {
					st = "not_home"
				}
			} else {
				st = "not_home"
			}
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
