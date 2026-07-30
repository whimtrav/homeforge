package api

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed zoneinfo so time.LoadLocation works in the slim container

	"github.com/gorilla/websocket"

	"github.com/whimtrav/homeforge/internal/automation"
	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
	"github.com/whimtrav/homeforge/internal/history"
)

func init() {
	// Alpine Linux has no /etc/mime.types — register essentials explicitly.
	mime.AddExtensionType(".js", "application/javascript")
	mime.AddExtensionType(".mjs", "application/javascript")
	mime.AddExtensionType(".css", "text/css; charset=utf-8")
	mime.AddExtensionType(".svg", "image/svg+xml")
	mime.AddExtensionType(".json", "application/json")
	mime.AddExtensionType(".woff2", "font/woff2")
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	cfg           config.APIConfig
	assistantCfg  config.AssistantConfig
	camerasCfg    config.CamerasConfig
	nvrProxy      *httputil.ReverseProxy
	mem           *assistantMemory
	auth          *authStore
	internalToken string
	store         *entity.Store
	bus           *bus.Bus

	reload  func() error
	history *history.History

	autoMu      sync.RWMutex
	automations []config.AutomationConfig
	autoState   *automation.StateStore

	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}

	writeMu sync.Mutex // serialises all websocket writes; gorilla panics on concurrent writes to a conn
}

func NewServer(cfg config.APIConfig, store *entity.Store, b *bus.Bus) *Server {
	s := &Server{
		cfg:     cfg,
		store:   store,
		bus:     b,
		clients: make(map[*websocket.Conn]struct{}),
	}

	// Health entities come from an infrequent source (phone, every ~12h), so unlike other
	// integrations they won't re-publish soon after a restart. Reload the last snapshot so the
	// data STAYS on the page across restarts.
	s.loadHealthSnapshot()

	// Broadcast every state change to all WebSocket clients.
	b.Subscribe(entity.TopicStateChanged, func(ev bus.Event) {
		payload, ok := ev.Payload.(entity.StateChangedPayload)
		if !ok {
			return
		}
		msg, _ := json.Marshal(map[string]any{
			"type":   "state_changed",
			"entity": payload.Entity,
		})
		s.broadcast(msg)
	})

	return s
}

func (s *Server) SetReload(fn func() error) { s.reload = fn }

func (s *Server) SetHistory(h *history.History) { s.history = h }

func (s *Server) SetAutomations(a []config.AutomationConfig) {
	s.autoMu.Lock()
	s.automations = a
	s.autoMu.Unlock()
}

func (s *Server) SetAutomationState(st *automation.StateStore) { s.autoState = st }

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.reload == nil {
		http.Error(w, "reload not configured", http.StatusNotImplemented)
		return
	}
	if err := s.reload(); err != nil {
		slog.Error("api: reload failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("api: config reloaded")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"reloaded"}`))
}

// handleHistory serves recorded state history for one entity.
//   GET /api/history/{id}?start=&end=&resolution=
// start/end accept RFC3339 or unix seconds (default: last 24h). resolution is
// raw|1m|1h (empty auto-selects by range).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "history disabled", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	q := r.URL.Query()
	end := time.Now()
	start := end.Add(-24 * time.Hour)
	parseTime := func(v string, def time.Time) time.Time {
		if v == "" {
			return def
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0)
		}
		return def
	}
	start = parseTime(q.Get("start"), start)
	end = parseTime(q.Get("end"), end)

	pts, err := s.history.Query(r.Context(), id, start, end, q.Get("resolution"))
	if err != nil {
		slog.Error("api: history query failed", "entity", id, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"entity_id": id, "points": pts})
}

// waterUsageTZ is the calendar timezone for the hour/day/week/month window
// boundaries (matches the timescaledb session TZ).
const waterUsageTZ = "America/Denver"

// handleWaterUsage integrates a flow-rate entity (gal/min) over calendar-aligned
// windows and returns gallons used this hour/today/this week/this month.
//   GET /api/water/usage?entity=sensor.droplet_fe5c_flow
func (s *Server) handleWaterUsage(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "history disabled", http.StatusNotImplemented)
		return
	}
	eid := r.URL.Query().Get("entity")
	if eid == "" {
		eid = "sensor.droplet_fe5c_flow"
	}
	loc, err := time.LoadLocation(waterUsageTZ)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	y, mo, d := now.Date()
	hour := time.Date(y, mo, d, now.Hour(), 0, 0, 0, loc)
	day := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	week := day.AddDate(0, 0, -int(now.Weekday())) // week starts Sunday
	month := time.Date(y, mo, 1, 0, 0, 0, 0, loc)
	end := time.Now()

	calc := func(start time.Time) float64 {
		v, err := s.history.IntegrateFlow(r.Context(), eid, start, end)
		if err != nil {
			slog.Error("api: water integrate failed", "entity", eid, "err", err)
			return 0
		}
		return math.Round(v*1000) / 1000
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"entity_id": eid,
		"hour":      calc(hour),
		"today":     calc(day),
		"week":      calc(week),
		"month":     calc(month),
	})
}

type bcRow struct {
	metric, unit string
	val          float64
}

// bodyComp estimates body composition (Xiaomi Mi Body Composition BIA formulas) from
// weight(kg) + impedance(Ω) + Bo's profile (183cm, 47y, male). BMI and BMR are exact; body-fat
// is an estimate that runs a few points off VeSync's proprietary value (~35% vs ~31%). Per-person
// profiles are a future add (whoever steps on).
func bodyComp(weight, impedance float64) []bcRow {
	const height, age = 183.0, 47.0 // male
	clamp := func(v, lo, hi float64) float64 {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	lbm := (height*9.058/100)*(height/100) + weight*0.32 + 12.226 - impedance*0.0068 - age*0.0542
	coef := 1.0
	if weight < 61 {
		coef = 0.98
	}
	fat := (1.0 - ((lbm-0.8)*coef)/weight) * 100
	if fat > 63 {
		fat = 75
	}
	fat = clamp(fat, 5, 75)
	water := (100 - fat) * 0.7
	wc := 0.98
	if water <= 50 {
		wc = 1.02
	}
	water = water * wc
	if water >= 65 {
		water = 75
	}
	water = clamp(water, 35, 75)
	bone := (0.18016894 - lbm*0.05158) * -1
	if bone > 2.2 {
		bone += 0.1
	} else {
		bone -= 0.1
	}
	bone = clamp(bone, 0.5, 8)
	muscle := clamp(weight-(fat*0.01)*weight-bone, 10, 120)
	var visc float64
	if height < weight*1.6 {
		sub := ((height * 0.4) - (height * (height * 0.0826))) * -1
		visc = ((weight*305)/(sub+48)) - 2.9 + age*0.15
	} else {
		sub := 0.765 + height*-0.0015
		visc = (((height*0.143)-(weight*sub))*-1) + age*0.15 - 5.0
	}
	visc = clamp(visc, 1, 50)
	bmr := clamp(877.8+weight*14.916-height*0.726-age*8.976, 500, 10000)
	bmi := clamp(weight/((height/100)*(height/100)), 10, 90)
	mage := clamp(height*-0.7471+weight*0.9161+age*0.4184+impedance*0.0517+54.2267, 15, 80)
	r := func(v float64) float64 { return math.Round(v*10) / 10 }
	const kgToLb = 2.2046226
	return []bcRow{
		{"bmi", "", r(bmi)}, {"body_fat", "%", r(fat)}, {"body_water", "%", r(water)},
		{"muscle_mass_lb", "lb", r(muscle * kgToLb)}, {"bone_mass_lb", "lb", r(bone * kgToLb)},
		{"visceral_fat", "", r(visc)}, {"bmr", "kcal", r(bmr)}, {"metabolic_age", "y", r(mage)},
	}
}

// handleScale ingests QN-scale BLE-client reports (GATT discovery / raw notifications /
// best-effort weight) from a LiquidFW scale reader. During bring-up it logs the full body so
// the exact QN frame format can be finalized from a real weigh-in; a confirmed weight is
// stored as sensor.health_<person>_weight_lb (person defaults to Bo, override with ?person=).
func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	device := r.PathValue("device")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32768))
	slog.Info("scale: report", "device", device, "body", string(body))

	id := strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			return c
		}
		return '_'
	}, strings.ToLower(device))

	trimmed := strings.TrimSpace(string(body))
	// keep the latest raw report visible for inspection while we finalize the decoder
	s.store.Set(entity.Entity{
		ID: "sensor.scale_" + id + "_raw", Name: device + " scale raw", Domain: "sensor",
		State: strconv.Itoa(len(trimmed)) + " bytes",
		Attributes: map[string]any{"device": "scale", "section": "health", "raw": trimmed},
	})

	// events array → store a confirmed weight (→ Vitals tab) + log best-effort ones
	if strings.HasPrefix(trimmed, "[") {
		var events []map[string]any
		if json.Unmarshal(body, &events) == nil {
			for _, ev := range events {
				if wl, ok := ev["weight_lb"].(float64); ok && wl > 10 && wl < 550 {
					slog.Info("scale: WEIGHT", "device", device, "lb", wl)
					setHealth := func(metric, unit string, val float64) {
						s.store.Set(entity.Entity{
							ID: "sensor.health_bo_" + metric, Name: "Bo " + metric, Domain: "sensor",
							State: strconv.FormatFloat(val, 'f', -1, 64),
							Attributes: map[string]any{"device": "health", "section": "health",
								"person": "bo", "person_name": "Bo", "person_figure": "man",
								"metric": metric, "unit_of_measurement": unit, "source": "ble-scale"},
						})
					}
					setHealth("weight_lb", "lb", math.Round(wl*10)/10)
					// body composition from weight + impedance (barefoot BIA)
					wk, _ := ev["weight_kg"].(float64)
					imp, _ := ev["impedance"].(float64)
					if wk > 20 && imp > 100 && imp < 3000 {
						for _, bc := range bodyComp(wk, imp) {
							setHealth(bc.metric, bc.unit, bc.val)
						}
						slog.Info("scale: body-comp stored", "device", device, "impedance", imp)
					}
				} else if wk, ok := ev["maybe_weight_kg"].(float64); ok && wk > 20 && wk < 250 {
					slog.Info("scale: maybe weight", "device", device, "kg", wk)
				}
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/entities", s.handleEntities)
	mux.HandleFunc("GET /api/entities/{id}", s.handleEntity)
	mux.HandleFunc("POST /api/services/{domain}/{service}", s.handleServiceCall)
	mux.HandleFunc("GET /api/ws", s.handleWebSocket)
	mux.HandleFunc("POST /api/reload", s.handleReload)
	mux.HandleFunc("GET /api/automations", s.handleAutomations)
	mux.HandleFunc("POST /api/automations/{name}/enabled", s.handleAutomationEnabled)
	mux.HandleFunc("GET /api/floorplan", s.handleFloorplanGet)
	mux.HandleFunc("PUT /api/floorplan", s.handleFloorplanPut)
	mux.HandleFunc("GET /api/floorplan/vents", s.handleVentsGet)
	mux.HandleFunc("PUT /api/floorplan/vents", s.handleVentsPut)
	mux.HandleFunc("GET /api/history/{id}", s.handleHistory)
	mux.HandleFunc("GET /api/water/usage", s.handleWaterUsage)
	mux.HandleFunc("POST /api/scale/{device}", s.handleScale)
	mux.HandleFunc("POST /api/comfort", s.handleComfort)
	mux.HandleFunc("POST /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/ble/{device}", s.handleBLE)
	mux.HandleFunc("POST /api/assistant", s.handleAssistant)
	mux.HandleFunc("POST /api/assistant/stt", s.handleSTT)
	mux.HandleFunc("POST /api/assistant/tts", s.handleTTS)
	mux.HandleFunc("GET /api/cameras", s.handleCamerasList)
	mux.HandleFunc("GET /api/cameras/{name}/frame", s.handleCameraFrame)
	mux.HandleFunc("GET /api/events", s.handleEventsList)
	mux.HandleFunc("GET /api/events/{id}/snapshot", s.handleEventSnapshot)
	mux.HandleFunc("GET /api/events/{id}/clip", s.handleEventClip)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/auth/me", s.handleAuthMe)
	mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/auth/change-password", s.handleChangePassword)
	mux.HandleFunc("GET /api/auth/users", s.handleListUsers)
	mux.HandleFunc("POST /api/auth/users", s.handleAddUser)
	mux.HandleFunc("DELETE /api/auth/users/{email}", s.handleDeleteUser)

	// Serve embedded frontend with SPA fallback.
	mux.Handle("/", spaHandler(webFS()))

	addr := s.cfg.Addr
	if addr == "" {
		addr = ":8123"
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: s.nvrRouter(s.authMiddleware(mux)),
	}

	slog.Info("api: listening", "addr", addr)

	go func() {
		<-ctx.Done()
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx2)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// automationView is the UI-facing shape of a configured automation.
type automationView struct {
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	Trigger string   `json:"trigger"`
	Actions []string `json:"actions"`
}

func (s *Server) handleAutomations(w http.ResponseWriter, r *http.Request) {
	s.autoMu.RLock()
	autos := s.automations
	s.autoMu.RUnlock()

	out := make([]automationView, 0, len(autos))
	for _, a := range autos {
		acts := make([]string, 0, len(a.Action))
		for _, ac := range a.Action {
			if ac.Wait != "" {
				acts = append(acts, "wait "+ac.Wait)
				continue
			}
			acts = append(acts, strings.TrimSpace(ac.Service+" "+ac.Entity))
		}
		out = append(out, automationView{
			Name:    a.Name,
			Enabled: s.autoState.Enabled(a.Name),
			Trigger: triggerSummary(a.Trigger),
			Actions: acts,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAutomationEnabled(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if s.autoState == nil {
		http.Error(w, "no automation state store", http.StatusNotImplemented)
		return
	}
	if err := s.autoState.SetEnabled(name, body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("api: automation toggled", "name", name, "enabled", body.Enabled)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"name": name, "enabled": body.Enabled})
}

// handleComfort logs a human comfort datapoint ("upstairs feels hot") with a full snapshot
// of the current climate context → training data for the climate brain's comfort model.
// Appends JSONL to /data/comfort-feedback.jsonl and surfaces the last tap as an entity.
func (s *Server) handleComfort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Zone   string `json:"zone"`
		Rating string `json:"rating"` // "hot" | "good" | "cold"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Zone == "" || body.Rating == "" {
		http.Error(w, "bad body (need zone + rating)", http.StatusBadRequest)
		return
	}
	ctx := map[string]any{}
	for _, id := range []string{
		"sensor.upstairs_temp_climate_temperature", "sensor.downstairs_temp_climate_temperature",
		"sensor.upstairs_temp_climate_humidity", "sensor.downstairs_temp_climate_humidity",
		"sensor.outdoor_temperature", "sensor.outdoor_humidity",
		"sensor.solar_pv_power", "sensor.solar_grid_power", "sensor.solar_battery_state_of_charge",
		"sensor.laundry_circuit",
	} {
		if e, ok := s.store.Get(id); ok {
			ctx[id] = e.State
		}
	}
	if e, ok := s.store.Get("climate.house"); ok {
		ctx["climate.house"] = e.State
		if e.Attributes != nil {
			ctx["hvac_action"] = e.Attributes["hvac_action"]
			ctx["current_temperature"] = e.Attributes["current_temperature"]
			ctx["setpoint"] = e.Attributes["temperature"]
		}
	}
	rec := map[string]any{
		"time": time.Now().Format(time.RFC3339), "zone": body.Zone, "rating": body.Rating, "context": ctx,
	}
	if line, err := json.Marshal(rec); err == nil {
		if f, ferr := os.OpenFile("/data/comfort-feedback.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); ferr == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	s.store.Set(entity.Entity{
		ID: "sensor.comfort_last_feedback", Name: "Comfort Last Feedback", Domain: "sensor",
		State:      body.Zone + ": " + body.Rating,
		Attributes: map[string]any{"device": "climate-brain", "section": "comfort", "zone": body.Zone, "rating": body.Rating, "at": rec["time"]},
	})
	slog.Info("api: comfort feedback", "zone", body.Zone, "rating", body.Rating)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "logged": rec})
}

// handleHealth ingests personal health metrics (from a phone Health-Connect exporter, a BP
// cuff, a smart scale, etc.). Body = a flat JSON object of metric:value (numbers), with two
// optional reserved keys: "timestamp" (RFC3339) and "units" ({metric: unit}). Each numeric
// metric → sensor.health_<metric> (logged to history automatically) + a raw JSONL audit line.
//   e.g. {"weight_kg":80.5,"bmi":24.2,"bp_systolic":121,"bp_diastolic":78,"heart_rate":61,
//         "steps":8432,"units":{"weight_kg":"kg","heart_rate":"bpm"}}
// defaultHealthPerson is who health data is attributed to when no ?person= is given.
const defaultHealthPerson = "Bo"

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body (JSON object of metric:value)", http.StatusBadRequest)
		return
	}
	ts := time.Now().Format(time.RFC3339)
	if t, ok := body["timestamp"].(string); ok && t != "" {
		ts = t
	}
	units, _ := body["units"].(map[string]any)
	healthNum := func(v any) (float64, bool) {
		switch n := v.(type) {
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
	sanitize := func(k string) string {
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
	// Health metrics are per-person. Person comes from ?person= or a body "person" field,
	// else the default (Bo). Each person's metrics live under sensor.health_<person>_<metric>
	// so multiple people never collide. Add more people by pointing their phone/webhook at
	// http://…/api/health?person=Name.
	person := defaultHealthPerson
	if p := strings.TrimSpace(r.URL.Query().Get("person")); p != "" {
		person = p
	}
	if p, ok := body["person"].(string); ok && strings.TrimSpace(p) != "" {
		person = strings.TrimSpace(p)
	}
	personSlug := sanitize(person)
	if personSlug == "" {
		personSlug, person = "unknown", "Unknown"
	}
	// figure = which silhouette to draw on the Vitals tab: man | woman | child. Default man.
	figure := "man"
	if f := strings.TrimSpace(r.URL.Query().Get("figure")); f != "" {
		figure = strings.ToLower(f)
	}
	if f, ok := body["figure"].(string); ok && strings.TrimSpace(f) != "" {
		figure = strings.ToLower(strings.TrimSpace(f))
	}
	n := 0
	for k, v := range body {
		if k == "timestamp" || k == "units" || k == "app_version" || k == "person" || k == "figure" {
			continue
		}
		// HC Webhook format: each data type is an ARRAY of records (steps/heart_rate/…).
		if arr, ok := v.([]any); ok {
			got := s.ingestHCArray(k, arr, ts, personSlug, person, figure)
			// log every type the webhook sends: record count + whether HF ingested it, plus a
			// field sample for any type HF doesn't parse yet (so new metrics are easy to add).
			sample := ""
			if got == 0 && len(arr) > 0 {
				b, _ := json.Marshal(arr[len(arr)-1])
				sample = string(b)
			}
			slog.Info("health: rx", "person", person, "key", k, "records", len(arr), "ingested", got, "sample", sample)
			n += got
			continue
		}
		// Flat scalar format: metric:value.
		f, ok := healthNum(v)
		if !ok {
			continue
		}
		unit := ""
		if units != nil {
			if u, ok := units[k].(string); ok {
				unit = u
			}
		}
		s.store.Set(entity.Entity{
			ID:     "sensor.health_" + personSlug + "_" + sanitize(k),
			Name:   person + " " + k,
			Domain: "sensor",
			State:  strconv.FormatFloat(f, 'f', -1, 64),
			Attributes: map[string]any{
				"device": "health", "section": "health", "person": personSlug, "person_name": person,
				"person_figure": figure, "metric": k, "unit_of_measurement": unit, "updated": ts,
			},
		})
		n++
	}
	if line, err := json.Marshal(map[string]any{"time": ts, "metrics": body}); err == nil {
		if f, e := os.OpenFile("/data/health-log.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); e == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	s.saveHealthSnapshot() // persist so data survives restarts + the 12h sync gap
	slog.Info("api: health ingest", "metrics", n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "metrics": n})
}

const healthSnapshotFile = "/data/health-latest.json"

// saveHealthSnapshot writes all sensor.health_* entities to disk so the data survives HF
// restarts + the long (12h) gap between phone syncs.
func (s *Server) saveHealthSnapshot() {
	var hs []entity.Entity
	for _, e := range s.store.All() {
		if strings.HasPrefix(e.ID, "sensor.health_") {
			hs = append(hs, e)
		}
	}
	if b, err := json.Marshal(hs); err == nil {
		os.WriteFile(healthSnapshotFile, b, 0644)
	}
}

// loadHealthSnapshot re-creates the health entities from the last snapshot at startup.
func (s *Server) loadHealthSnapshot() {
	b, err := os.ReadFile(healthSnapshotFile)
	if err != nil {
		return
	}
	var hs []entity.Entity
	if json.Unmarshal(b, &hs) != nil {
		return
	}
	for _, e := range hs {
		s.store.Set(e)
	}
}

// ── HC Webhook (Health Connect) format ───────────────────────────────────────
// The mcnaveen/health-connect-webhook Android app POSTs one JSON object: timestamp +
// app_version + optional snake_case ARRAYS of records per data type. We map each to a
// sensor.health_* entity — latest reading for instantaneous types, today's sum for
// cumulative ones. Feeds Samsung Health + A&D BP cuff data (both write to Health Connect).

type hcInst struct {
	field, metric, unit string
	conv                func(float64) float64
}

var hcInstMap = map[string]hcInst{
	"heart_rate":             {"bpm", "heart_rate", "bpm", nil},
	"resting_heart_rate":     {"bpm", "resting_heart_rate", "bpm", nil},
	"oxygen_saturation":      {"percentage", "spo2", "%", nil},
	"respiratory_rate":       {"rate", "respiratory_rate", "br/min", nil},
	"heart_rate_variability": {"rmssd_millis", "hrv", "ms", nil},
	"body_temperature":       {"celsius", "body_temp", "°F", func(c float64) float64 { return c*9/5 + 32 }},
	"weight":                 {"kilograms", "weight_lb", "lb", func(k float64) float64 { return k * 2.2046226 }},
	"blood_glucose":          {"mmol_per_liter", "blood_glucose", "mg/dL", func(m float64) float64 { return m * 18.0182 }},
	"body_fat":               {"percentage", "body_fat", "%", nil},
	"vo2_max":                {"vo2_milliliters_per_minute_kilogram", "vo2_max", "mL/kg/min", nil},
}

func (s *Server) ingestHCArray(key string, arr []any, fallbackTS, personSlug, personName, figure string) int {
	if len(arr) == 0 {
		return 0
	}
	set := func(metric string, val float64, unit, updated string) {
		if updated == "" {
			updated = fallbackTS
		}
		s.store.Set(entity.Entity{
			ID: "sensor.health_" + personSlug + "_" + metric, Name: personName + " " + metric, Domain: "sensor",
			State: strconv.FormatFloat(val, 'f', -1, 64),
			Attributes: map[string]any{"device": "health", "section": "health",
				"person": personSlug, "person_name": personName, "person_figure": figure,
				"metric": metric, "unit_of_measurement": unit, "updated": updated},
		})
	}
	switch key {
	case "blood_pressure":
		rec, t := hcLatest(arr)
		if rec == nil {
			return 0
		}
		n := 0
		if v, ok := hcFloat(rec["systolic"]); ok {
			set("bp_systolic", math.Round(v), "mmHg", t)
			n++
		}
		if v, ok := hcFloat(rec["diastolic"]); ok {
			set("bp_diastolic", math.Round(v), "mmHg", t)
			n++
		}
		return n
	case "sleep":
		rec, t := hcLatest(arr)
		if rec == nil {
			return 0
		}
		if v, ok := hcFloat(rec["duration_seconds"]); ok {
			set("sleep_hours", math.Round(v/3600*10)/10, "h", t)
			return 1
		}
		return 0
	case "steps":
		set("steps", math.Round(hcSumToday(arr, "count", "start_time")), "steps", fallbackTS)
		return 1
	case "distance":
		set("distance", math.Round(hcSumToday(arr, "meters", "start_time")/1609.344*10)/10, "mi", fallbackTS)
		return 1
	case "active_calories":
		set("active_calories", math.Round(hcSumToday(arr, "calories", "start_time")), "kcal", fallbackTS)
		return 1
	case "total_calories":
		set("total_calories", math.Round(hcSumToday(arr, "calories", "start_time")), "kcal", fallbackTS)
		return 1
	case "exercise":
		rec, t := hcLatest(arr)
		if rec == nil {
			return 0
		}
		n := 0
		if v, ok := hcFloat(rec["duration_seconds"]); ok {
			set("workout_min", math.Round(v/60), "min", t)
			n++
		}
		if v, ok := hcFloat(rec["distance_meters"]); ok && v > 0 {
			set("workout_mi", math.Round(v/1609.344*100)/100, "mi", t)
			n++
		}
		return n
	default:
		m, ok := hcInstMap[key]
		if !ok {
			return 0
		}
		rec, t := hcLatest(arr)
		if rec == nil {
			return 0
		}
		v, ok := hcFloat(rec[m.field])
		if !ok {
			return 0
		}
		if m.conv != nil {
			v = m.conv(v)
		}
		set(m.metric, math.Round(v*10)/10, m.unit, t)
		return 1
	}
}

func hcFloat(v any) (float64, bool) {
	switch n := v.(type) {
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

func hcRecTime(rec map[string]any) string {
	for _, k := range []string{"time", "end_time", "start_time", "session_end_time"} {
		if s, ok := rec[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// hcLatest returns the record with the newest timestamp + that timestamp string.
func hcLatest(arr []any) (map[string]any, string) {
	var best map[string]any
	var bestT time.Time
	var bestS string
	found := false
	for _, r := range arr {
		rec, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ts := hcRecTime(rec)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			if !found {
				best, bestS, found = rec, ts, true
			}
			continue
		}
		if !found || t.After(bestT) {
			best, bestT, bestS, found = rec, t, ts, true
		}
	}
	return best, bestS
}

// hcSumToday sums valField over records whose local date == the most recent record's date.
func hcSumToday(arr []any, valField, timeField string) float64 {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	type drec struct {
		v float64
		d string
	}
	var recs []drec
	maxD := ""
	for _, r := range arr {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		v, ok := hcFloat(m[valField])
		if !ok {
			continue
		}
		ts, _ := m[timeField].(string)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		d := t.In(loc).Format("2006-01-02")
		recs = append(recs, drec{v, d})
		if d > maxD {
			maxD = d
		}
	}
	sum := 0.0
	for _, r := range recs {
		if r.d == maxD {
			sum += r.v
		}
	}
	return sum
}

func triggerSummary(t config.TriggerConfig) string {
	switch t.Type {
	case "state_change":
		if t.To != "" {
			return "when " + t.Entity + " → " + t.To
		}
		return "when " + t.Entity + " changes"
	case "numeric":
		if t.Above != nil {
			return t.Entity + " rises above " + strconv.FormatFloat(*t.Above, 'f', -1, 64)
		}
		if t.Below != nil {
			return t.Entity + " drops below " + strconv.FormatFloat(*t.Below, 'f', -1, 64)
		}
	}
	return t.Type + " " + t.Entity
}

func (s *Server) handleEntities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.store.All())
}

func (s *Server) handleEntity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e, ok := s.store.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(e)
}

// SetCameras configures the optional NVR reverse-proxy. HomeForge serves the NVR (e.g.
// Sentinel/Frigate) under /nvr/ on its OWN domain (one domain, one login), rewriting the NVR's
// absolute asset/API paths to the /nvr/ prefix so the whole embedded UI works behind the tunnel.
func (s *Server) SetCameras(c config.CamerasConfig) {
	s.camerasCfg = c
	if c.NvrUpstream == "" {
		return
	}
	u, err := url.Parse(c.NvrUpstream)
	if err != nil {
		return
	}
	s.nvrProxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/nvr")
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			r.Header.Del("Accept-Encoding") // need uncompressed bodies to rewrite paths
		},
		ModifyResponse: func(resp *http.Response) error {
			ct := resp.Header.Get("Content-Type")
			if !strings.Contains(ct, "html") && !strings.Contains(ct, "javascript") && !strings.Contains(ct, "css") {
				return nil // pass images/video/json straight through
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			out := string(body)
			for _, pfx := range []string{"/assets/", "/api/", "/favicon", "/vod/", "/clips/"} {
				out = strings.ReplaceAll(out, `"`+pfx, `"/nvr`+pfx)
				out = strings.ReplaceAll(out, `'`+pfx, `'/nvr`+pfx)
				out = strings.ReplaceAll(out, "`"+pfx, "`/nvr"+pfx) // template-literal URLs (live frames, recordings, clips)
			}
			resp.Body = io.NopCloser(strings.NewReader(out))
			resp.ContentLength = int64(len(out))
			resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
			return nil
		},
	}
}

// nvrRouter serves the NVR under /nvr/ (auth-gated), else falls through to normal HomeForge.
func (s *Server) nvrRouter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.nvrProxy != nil && (r.URL.Path == "/nvr" || strings.HasPrefix(r.URL.Path, "/nvr/")) {
			if r.URL.Path == "/nvr" { // trailing slash so the NVR's relative assets resolve under /nvr/
				http.Redirect(w, r, "/nvr/", http.StatusMovedPermanently)
				return
			}
			if s.auth != nil && s.auth.enabled {
				ok := false
				if c, err := r.Cookie(sessionCookie); err == nil {
					_, ok = s.auth.valid(c.Value)
				}
				if !ok {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			s.nvrProxy.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// proxyToNVR forwards a GET to the NVR at upstreamPath (server-side, behind HomeForge's login),
// preserving the query string and forwarding/returning Range so video clips can seek.
func (s *Server) proxyToNVR(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	if s.camerasCfg.NvrUpstream == "" {
		http.Error(w, "no nvr configured", http.StatusServiceUnavailable)
		return
	}
	target := s.camerasCfg.NvrUpstream + upstreamPath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "nvr unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control", "Last-Modified", "Etag"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// Camera + event endpoints — native views, all behind HomeForge's login.
func (s *Server) handleCamerasList(w http.ResponseWriter, r *http.Request) {
	s.proxyToNVR(w, r, "/api/cameras")
}
func (s *Server) handleCameraFrame(w http.ResponseWriter, r *http.Request) {
	s.proxyToNVR(w, r, "/api/cameras/"+url.PathEscape(r.PathValue("name"))+"/latest-frame")
}
func (s *Server) handleEventsList(w http.ResponseWriter, r *http.Request) {
	s.proxyToNVR(w, r, "/api/events")
}
func (s *Server) handleEventSnapshot(w http.ResponseWriter, r *http.Request) {
	s.proxyToNVR(w, r, "/api/events/"+url.PathEscape(r.PathValue("id"))+"/snapshot.jpg")
}
func (s *Server) handleEventClip(w http.ResponseWriter, r *http.Request) {
	s.proxyToNVR(w, r, "/api/events/"+url.PathEscape(r.PathValue("id"))+"/clip.mp4")
}

func (s *Server) handleServiceCall(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	service := r.PathValue("service")

	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)

	entityID, _ := body["entity_id"].(string)
	data, _ := body["data"].(map[string]any)

	s.bus.Publish("service.call", map[string]any{
		"service": domain + "." + service,
		"entity":  entityID,
		"data":    data,
	})

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.clients[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
	}()

	// Send current state snapshot on connect.
	snapshot, _ := json.Marshal(map[string]any{
		"type":     "snapshot",
		"entities": s.store.All(),
	})
	s.writeConn(conn, snapshot)

	// Keep alive — read loop (discards client messages for now).
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// writeConn serialises writes to a websocket connection. gorilla/websocket panics
// on concurrent writes to the same conn, and state changes arrive from many
// goroutines (one per LiquidFW device), so every write must pass through writeMu.
// A write deadline bounds how long a stuck client can hold the lock.
func (s *Server) writeConn(conn *websocket.Conn, msg []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, msg)
}

func (s *Server) broadcast(msg []byte) {
	// Snapshot the client set under the read lock, then write outside it so a
	// slow write never blocks connect/disconnect (which take the write lock).
	s.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(s.clients))
	for conn := range s.clients {
		conns = append(conns, conn)
	}
	s.mu.RUnlock()
	for _, conn := range conns {
		s.writeConn(conn, msg)
	}
}

// spaHandler serves static files and falls back to index.html for unknown paths
// so SvelteKit client-side routing works correctly.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		_, err := fs.Stat(fsys, path)
		if err != nil {
			// File not found — serve index.html for SPA routing.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// floorplanPath stores the floor-plan tab's device pin positions (persisted on the /data volume).
const floorplanPath = "/data/floorplan.json"

func (s *Server) handleFloorplanGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	data, err := os.ReadFile(floorplanPath)
	if err != nil {
		w.Write([]byte("{}"))
		return
	}
	w.Write(data)
}

func (s *Server) handleFloorplanPut(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !json.Valid(data) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(floorplanPath, data, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
