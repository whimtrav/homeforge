package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	API          APIConfig          `yaml:"api"`
	MQTT         MQTTConfig         `yaml:"mqtt"`
	Database     DatabaseConfig     `yaml:"database"`
	History      HistoryConfig      `yaml:"history"`
	Automations  []AutomationConfig `yaml:"automations"`
	Groups       []GroupConfig      `yaml:"groups"`
	Integrations IntegrationsConfig `yaml:"integrations"`
	Assistant    AssistantConfig    `yaml:"assistant"`
	Auth         AuthConfig         `yaml:"auth"`
	Cameras      CamerasConfig      `yaml:"cameras"`
}

// CamerasConfig lets HomeForge reverse-proxy an NVR (e.g. Sentinel/Frigate) so it's reachable
// over the same tunnel + login. When nvr_host is set, requests to that Host are auth-gated and
// proxied to nvr_upstream. Leave nvr_host blank to just embed the NVR directly on the LAN.
type CamerasConfig struct {
	NvrUpstream string `yaml:"nvr_upstream"` // NVR (Sentinel/Frigate) base URL; HomeForge serves it under /nvr/. Default http://localhost:5000
}

// AuthConfig — HomeForge's own login (protects the tunnel, VPN, and LAN alike).
type AuthConfig struct {
	Enabled      bool   `yaml:"enabled"`
	OwnerEmail   string `yaml:"owner_email"`   // pre-filled on first-run setup
	UsersFile    string `yaml:"users_file"`    // JSON store of accounts
	SessionDays  int    `yaml:"session_days"`  // cookie lifetime
	CookieDomain string `yaml:"cookie_domain"` // e.g. ".example.com" to share the session across subdomains (blank = host-only)
}

// AssistantConfig configures the local-LLM chat assistant. Defaults are tuned for a
// CPU-only 4-core / 8GB box (the Beelink port target) — no GPU dependency.
type AssistantConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Model       string  `yaml:"model"`       // ollama model tag
	OllamaURL   string  `yaml:"ollama_url"`  // ollama /api/chat endpoint
	NumCtx      int     `yaml:"num_ctx"`     // context window (KV cache); must fit prompt+history+reply
	NumPredict  int     `yaml:"num_predict"` // max generated tokens per step (bounds latency)
	NumThread   int     `yaml:"num_thread"`  // CPU threads (0 = ollama auto)
	NumGPU      int     `yaml:"num_gpu"`     // GPU layers; 0 = CPU-only
	Temperature float64 `yaml:"temperature"`
	MaxSteps    int     `yaml:"max_steps"`   // agent-loop bound
	DeviceCap   int     `yaml:"device_cap"`  // devices listed in the (static, cacheable) prompt
	Prewarm     bool    `yaml:"prewarm"`     // prime model + prompt cache on startup
	MemoryFile  string  `yaml:"memory_file"` // durable remembered-facts store (JSON)
	WhisperURL  string  `yaml:"whisper_url"` // local speech-to-text service (voice input)
	PiperURL    string  `yaml:"piper_url"`   // local neural TTS service (spoken replies)
}

// GroupConfig defines a virtual switch that fans service calls out to its members
// and reflects their combined state (on if any member is on). Members are entity IDs.
type GroupConfig struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

// HistoryConfig configures the TimescaleDB-backed state history recorder.
// Interval fields are Postgres interval literals ("30 days"); "" or "0" = forever.
type HistoryConfig struct {
	Enabled       bool   `yaml:"enabled"`
	DSN           string `yaml:"dsn"`
	RetentionRaw  string `yaml:"retention_raw"`
	Retention1m   string `yaml:"retention_1m"`
	Retention1h   string `yaml:"retention_1h"`
	CompressAfter string `yaml:"compress_after"`
}

type APIConfig struct {
	Addr string `yaml:"addr"`
}

type MQTTConfig struct {
	External     bool        `yaml:"external"`
	Host         string      `yaml:"host"`
	Port         int         `yaml:"port"`
	Username     string      `yaml:"username"`
	Password     string      `yaml:"password"`
	NtfyURL      string      `yaml:"ntfy_url"`       // ntfy topic URL for push alerts, e.g. https://ntfy.sh/<topic>
	PushRelayURL string      `yaml:"push_relay_url"` // FCM push-relay URL; empty = shared default (push.DefaultRelayURL)
	ZwaveLocks   []ZwaveLock `yaml:"zwave_locks"`
}

// ZwaveLock maps a zwave-js-ui named node segment to a HomeForge lock entity, for door
// locks paired locally to a Z-Wave stick (via zwave-js-ui) instead of a cloud hub.
// See internal/mqtt handleZwaveState.
type ZwaveLock struct {
	Node string `yaml:"node"` // zwave-js-ui named topic segment, e.g. "Front_Door"
	ID   string `yaml:"id"`   // entity id suffix -> lock.<id>
	Name string `yaml:"name"` // friendly name
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type AutomationConfig struct {
	Name      string           `yaml:"name"`
	Trigger   TriggerConfig    `yaml:"trigger"`
	Condition *ConditionConfig `yaml:"condition,omitempty"`
	Action    []ActionConfig   `yaml:"action"`
}

type TriggerConfig struct {
	Type   string   `yaml:"type"`
	Entity string   `yaml:"entity,omitempty"`
	To     string   `yaml:"to,omitempty"`
	From   string   `yaml:"from,omitempty"`
	Cron   string   `yaml:"cron,omitempty"`
	Topic  string   `yaml:"topic,omitempty"`
	Above  *float64 `yaml:"above,omitempty"`  // numeric trigger: fire on cross up through this value
	Below  *float64 `yaml:"below,omitempty"`  // numeric trigger: fire on cross down through this value
	Event  string   `yaml:"event,omitempty"`  // sun trigger: "sunrise" | "sunset"
	Offset int      `yaml:"offset,omitempty"` // sun trigger: minutes relative to the event (+after, -before)
}

type ConditionConfig struct {
	Type       string             `yaml:"type"`
	Entity     string             `yaml:"entity,omitempty"`
	State      string             `yaml:"state,omitempty"`
	After      string             `yaml:"after,omitempty"`
	Before     string             `yaml:"before,omitempty"`
	Above      *float64           `yaml:"above,omitempty"`      // numeric condition: value must be > this
	Below      *float64           `yaml:"below,omitempty"`      // numeric condition: value must be < this
	Conditions []*ConditionConfig `yaml:"conditions,omitempty"` // for type: and / or
}

type ActionConfig struct {
	Service   string           `yaml:"service"`
	Entity    string           `yaml:"entity"`
	Data      map[string]any   `yaml:"data,omitempty"`
	Wait      string           `yaml:"wait,omitempty"`
	Condition *ConditionConfig `yaml:"condition,omitempty"`
}

type IntegrationsConfig struct {
	Zigbee2MQTT  Zigbee2MQTTConfig  `yaml:"zigbee2mqtt"`
	ESPHome      ESPHomeConfig      `yaml:"esphome"`
	WiZ          WiZConfig          `yaml:"wiz"`
	WLED         WLEDConfig         `yaml:"wled"`
	Emporia      EmporiaConfig      `yaml:"emporia"`
	Tigo         TigoConfig         `yaml:"tigo"`
	LiquidFW     LiquidFWConfig     `yaml:"liquidfw"`
	Sonoff       SonoffConfig       `yaml:"sonoff"`
	Thermostat   ThermostatConfig   `yaml:"thermostat"`
	Disagg       DisaggConfig       `yaml:"disagg"`
	Weather      WeatherConfig      `yaml:"weather"`
	ClimateBrain ClimateBrainConfig `yaml:"climate_brain"`
	Rachio       RachioConfig       `yaml:"rachio"`
	Alexa        AlexaConfig        `yaml:"alexa"`
}

// AlexaConfig = native Alexa Smart Home skill support. HF exposes the curated
// Devices as Alexa endpoints and handles Smart Home directives. SharedToken
// authenticates the Lambda->HF hop (same pattern as the media skill).
type AlexaConfig struct {
	Enabled           bool          `yaml:"enabled"`
	SharedToken       string        `yaml:"shared_token"`
	OAuthClientID     string        `yaml:"oauth_client_id"`
	OAuthClientSecret string        `yaml:"oauth_client_secret"`
	EventClientID     string        `yaml:"event_client_id"`     // "Send Alexa Events" (Skill Messaging) creds
	EventClientSecret string        `yaml:"event_client_secret"` // for the Alexa event gateway (proactive reports)
	Devices           []AlexaDevice `yaml:"devices"`
}

// AlexaDevice = one exposed endpoint. Category defaults to LIGHT (FAN for *_fan);
// BrightnessController is auto-added when a number.<base>_brightness entity exists.
type AlexaDevice struct {
	Entity   string `yaml:"entity"`
	Name     string `yaml:"name"`
	Category string `yaml:"category,omitempty"`
}

// RachioConfig = Rachio smart irrigation via its public REST API (bearer api_key,
// the same key HA used). Native Go poller — no cloud sidecar.
type RachioConfig struct {
	Enabled     bool   `yaml:"enabled"`
	APIKey      string `yaml:"api_key"`
	PollSeconds int    `yaml:"poll_seconds"` // default 30
}

// ClimateBrainConfig = the adaptive HVAC brain (experiment → measure → learn → ACT).
// Phase 1: attic-fan A/B experiment + observability, no actuation. Phase 2: solar pre-cool
// actuation (nudges the setpoint DOWN only), attic-fan auto-run once the A/B has a verdict,
// and a humidity-aware comfort model (feels-like + learned comfortable setpoint).
type ClimateBrainConfig struct {
	Enabled         bool   `yaml:"enabled"`
	AtticExperiment bool   `yaml:"attic_experiment"` // run the attic-fan A/B test (low-risk)
	AtticFan        string `yaml:"attic_fan"`        // big attic exhaust switch entity
	BoxFan          string `yaml:"box_fan"`          // small box-fan switch entity
	BlockMin        int    `yaml:"block_min"`        // A/B arm length in minutes (default 30)

	// ── Phase 2: actuation + learning ──
	PrecoolActuate     bool    `yaml:"precool_actuate"`       // nudge the cool setpoint DOWN to bank solar coolth
	PrecoolOffset      float64 `yaml:"precool_offset"`        // max °F below the base setpoint (default 3)
	PrecoolExportW     float64 `yaml:"precool_export_w"`      // min solar export in W to trigger (default 400)
	PrecoolExportOffW  float64 `yaml:"precool_export_off_w"`  // hysteresis: only DISENGAGE below this export W (default exportW/3) — stops cloud flip-flop
	PrecoolMinDwellMin float64 `yaml:"precool_min_dwell_min"` // once pre-cooling, HOLD at least this many minutes (default 20)
	PrecoolMaxOut      float64 `yaml:"precool_max_outdoor"`   // above this °F outdoor the AC is too inefficient (default 92)
	PrecoolMinF        float64 `yaml:"precool_min_f"`         // hard floor °F — never pre-cool below (default 68)
	AtticAutoRun       bool    `yaml:"attic_auto_run"`        // once A/B says the fan helps, run it during hot+sun
	AtticMargin        float64 `yaml:"attic_margin"`          // min AC-duty improvement (fraction) to call it a win (default 0.05)
	ComfortModel       bool    `yaml:"comfort_model"`         // learn a comfortable setpoint from /data/comfort-feedback.jsonl
	// Humidity sources (room RH complements the probe RH; VeSync humidifiers report these).
	UpHumidity   []string `yaml:"up_humidity"`   // upstairs RH entity ids
	DownHumidity []string `yaml:"down_humidity"` // downstairs RH entity ids
	UpTempAlt    string   `yaml:"up_temp_alt"`   // optional extra upstairs temp (°F) cross-check, e.g. VeSync family room

	// ── Free cooling: when it's cooler outside than in, run the attic + box fans to
	// pull cool air through instead of running the AC (the wasted overnight/evening window).
	FreeCool      bool    `yaml:"free_cool"`
	FreeCoolDelta float64 `yaml:"free_cool_delta"`  // outdoor must be ≥ this °F below upstairs (default 4)
	FreeCoolMaxRH float64 `yaml:"free_cool_max_rh"` // skip if outdoor RH above this % (default 85)

	// ── Night fan test: overnight, back the AC setpoint up toward a comfort cap and run the
	// attic+box fans, testing whether fans (not the compressor) can hold the house. Stays in
	// cool mode so the AC is always armed as a fail-safe.
	NightFan       bool    `yaml:"night_fan"`
	NightFanMaxOut float64 `yaml:"night_fan_max_out"` // engage only when outdoor below this °F (default 80)
	NightFanCap    float64 `yaml:"night_fan_cap"`     // let upstairs float to this °F before the AC recovers (default 78)
	NightFanStrat  float64 `yaml:"night_fan_strat"`   // assumed upstairs-minus-average offset °F (default 3)
	NightFanStart  int     `yaml:"night_fan_start"`   // window start hour, local 0-23 (default 22)
	NightFanEnd    int     `yaml:"night_fan_end"`     // window end hour, local 0-23 (default 6)

	// ── Hot-day upstairs Heat-Guard: when it's hot out and upstairs is over its cap, drive the
	// setpoint DOWN to pull UPSTAIRS (not the up/down average) to the cap + run the fans. The
	// average-based thermostat won't do this on its own, so upstairs sits warm on hot afternoons.
	HeatGuard        bool    `yaml:"heat_guard"`
	HeatGuardHotOut  float64 `yaml:"heat_guard_hot_out"`  // engage only when outdoor ≥ this °F (default 88)
	HeatGuardCap     float64 `yaml:"heat_guard_cap"`      // max upstairs °F allowed on a hot day (default 75)
	HeatGuardFloor   float64 `yaml:"heat_guard_floor"`    // hard floor — never command below this °F (default 70)
	HeatGuardMaxDrop float64 `yaml:"heat_guard_max_drop"` // max °F below base the guard will command (default 4)

	// Manual-override hold: when the user sets the setpoint (UI/assistant), the brain + all
	// controllers hand off for this many minutes before resuming control (default 60).
	ManualHoldMin float64 `yaml:"manual_hold_min"`

	// ── Holistic per-room comfort: each room is {its own sensor, its own baseline, its own fan}.
	// Sensor accuracy is irrelevant — each gauge is used RELATIVE to itself (a "just right" tap
	// pins that room's comfortable reading; the brain moves that gauge with that room's levers,
	// learning each lever's effect by trial and error). Stage 1 = observe + publish only.
	Zones []ZoneConfig `yaml:"zones"`

	// ── Unified comfort-at-least-cost policy: ONE objective loop (keep the warm zone under the
	// comfort ceiling using the cheapest sufficient actuator) that will replace the pile of
	// individual modes. Runs in SHADOW (advisory, publishes what it WOULD do) until policy_actuate.
	Policy        bool    `yaml:"policy"`
	PolicyActuate bool    `yaml:"policy_actuate"`      // let the policy actually control (else shadow only)
	PolicyHigh    float64 `yaml:"policy_comfort_high"` // warm-zone comfort ceiling °F (default 78)
	PolicyLow     float64 `yaml:"policy_comfort_low"`  // cool-zone floor °F (default 68)
	PolicyTarget  float64 `yaml:"policy_target"`       // active-cooling aim °F (default = base setpoint)
}

// ZoneConfig = one room in the holistic comfort model. TempSensor is that room's own gauge
// (used relative to itself, so absolute accuracy doesn't matter); Fan is the room's comfort
// lever (a number 0..FanMax), empty for AC-only rooms like a hallway.
type ZoneConfig struct {
	Name       string `yaml:"name"`        // short room id, e.g. "bedroom2"
	TempSensor string `yaml:"temp_sensor"` // this room's temperature entity
	TempIsC    bool   `yaml:"temp_is_c"`   // true if the sensor reports °C (LiquidFW climate probes do)
	Humidity   string `yaml:"humidity"`    // optional RH entity for a feels-like
	Fan        string `yaml:"fan"`         // optional ceiling-fan number entity (the room's lever)
	FanMax     int    `yaml:"fan_max"`     // max fan speed (default 3)
}

// WeatherConfig = free Open-Meteo outdoor conditions for the climate brain (no API key).
type WeatherConfig struct {
	Enabled bool    `yaml:"enabled"`
	Lat     float64 `yaml:"lat"`
	Lon     float64 `yaml:"lon"`
}

// DisaggConfig drives the load-disaggregation of the shared washerdryer BL0939 meter
// (washer/dryer + sump pump + cat litter box on one circuit) into per-load entities.
type DisaggConfig struct {
	Enabled   bool   `yaml:"enabled"`
	MeterHost string `yaml:"meter_host"` // LiquidFW DualR3 whose /state.io.meter.power1 = the shared circuit
}

// ThermostatConfig drives the HVAC brain: it averages temp sensors (read off the
// HA MQTT broker), holds mode/setpoint/preset, and pushes them to a LiquidFW
// on-device thermostat (the DrZzs D1-mini relay board). Presets carry separate
// heat/cool setpoints so away/eco save in both seasons.
type ThermostatConfig struct {
	Enabled          bool               `yaml:"enabled"`
	Device           string             `yaml:"device"`    // LiquidFW device name (e.g. climate-control)
	MQTTHost         string             `yaml:"mqtt_host"` // broker carrying the temp topics (e.g. 10.0.0.5)
	MQTTPort         int                `yaml:"mqtt_port"` // default 1883
	MQTTUser         string             `yaml:"mqtt_user"`
	MQTTPass         string             `yaml:"mqtt_pass"`
	TempSensors      []ThermostatSensor `yaml:"temp_sensors"`      // topics to average
	PushIntervalSec  int                `yaml:"push_interval_sec"` // default 30
	MinTemp          float64            `yaml:"min_temp"`          // default 55
	MaxTemp          float64            `yaml:"max_temp"`          // default 88
	StateFile        string             `yaml:"state_file"`        // default /data/thermostat.json
	Presets          []ThermostatPreset `yaml:"presets"`           // home/away/sleep/eco
	PresenceEntity   string             `yaml:"presence_entity"`   // legacy single entity → auto-away when not home
	PresenceEntities []string           `yaml:"presence_entities"` // occupied if ANY reads home/on (phones + mmwave); away only when ALL clear
	AwayAfterMin     int                `yaml:"away_after_min"`    // minutes fully-empty before away (default 15)
	Circulate        CirculateConfig    `yaml:"circulate"`         // blower destratification policy
}

// CirculateConfig = HomeForge's blower-circulation (destratify) policy. When the two floors
// diverge HF requests the device run the blower; the device gates it to idle-only (safety).
type CirculateConfig struct {
	Enabled   bool    `yaml:"enabled"`
	OnDelta   float64 `yaml:"on_delta"`    // start when |up-down| >= this °F (default 3)
	OffDelta  float64 `yaml:"off_delta"`   // stop when |up-down| <= this °F (default 1.5)
	MaxRunMin int     `yaml:"max_run_min"` // cap a continuous run (default 15)
	MinOffMin int     `yaml:"min_off_min"` // rest between runs (default 5)
}

type ThermostatSensor struct {
	Topic     string `yaml:"topic"`       // MQTT topic (e.g. "zigbee2mqtt/Upstairs Temperature")
	Entity    string `yaml:"entity"`      // OR a HomeForge entity id to read from the store (LiquidFW probes)
	Field     string `yaml:"field"`       // MQTT JSON field (default "temperature")
	Celsius   bool   `yaml:"celsius"`     // convert °C→°F (Z2M + LiquidFW report °C)
	MaxAgeSec int    `yaml:"max_age_sec"` // staleness guard: ignore a reading older than this (0 = never stale)
}

type ThermostatPreset struct {
	Name string  `yaml:"name"`
	Cool float64 `yaml:"cool"` // cooling setpoint for this preset (°F)
	Heat float64 `yaml:"heat"` // heating setpoint for this preset (°F)
}

type Zigbee2MQTTConfig struct {
	Enabled   bool   `yaml:"enabled"`
	BaseTopic string `yaml:"base_topic"`
}

type ESPHomeConfig struct {
	Enabled bool            `yaml:"enabled"`
	Devices []ESPHomeDevice `yaml:"devices"`
}

type ESPHomeDevice struct {
	Name     string `yaml:"name"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
	NoisePSK string `yaml:"noise_psk"`
}

type WiZConfig struct {
	Enabled bool      `yaml:"enabled"`
	Bulbs   []WiZBulb `yaml:"bulbs"`
}

type WiZBulb struct {
	Name string `yaml:"name"`
	Mac  string `yaml:"mac"`
	IP   string `yaml:"ip"`
}

type WLEDConfig struct {
	Enabled bool         `yaml:"enabled"`
	Devices []WLEDDevice `yaml:"devices"`
}

type WLEDDevice struct {
	Name string `yaml:"name"`
	Host string `yaml:"host"`
}

type EmporiaConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
}

type TigoConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
}

type SonoffConfig struct {
	Enabled bool           `yaml:"enabled"`
	Devices []SonoffDevice `yaml:"devices"`
}

type SonoffDevice struct {
	Name     string         `yaml:"name"`
	DeviceID string         `yaml:"device_id"`
	IP       string         `yaml:"ip"`
	APIKey   string         `yaml:"api_key"`
	Entities []SonoffEntity `yaml:"entities"`
}

type SonoffEntity struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type LiquidFWPollConfig struct {
	Name       string `yaml:"name"`
	IP         string `yaml:"ip"`
	IntervalMs int    `yaml:"interval_ms"`
}

type LiquidFWConfig struct {
	Enabled      bool                 `yaml:"enabled"`
	KeyFile      string               `yaml:"key_file"`
	UDPPort      int                  `yaml:"udp_port"`
	RegistryFile string               `yaml:"registry_file"`
	HTTPPoll     []LiquidFWPollConfig `yaml:"http_poll"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{
		API:      APIConfig{Addr: ":8123"},
		MQTT:     MQTTConfig{Port: 1883},
		Database: DatabaseConfig{Driver: "sqlite", DSN: "homeforge.db"},
	}

	if err := yaml.NewDecoder(f).Decode(cfg); err != nil {
		return nil, err
	}

	// Defaults for LiquidFW
	if cfg.Integrations.LiquidFW.UDPPort == 0 {
		cfg.Integrations.LiquidFW.UDPPort = 7788
	}
	if cfg.Integrations.LiquidFW.KeyFile == "" {
		cfg.Integrations.LiquidFW.KeyFile = "liquidfw.key"
	}
	if cfg.Integrations.LiquidFW.RegistryFile == "" {
		cfg.Integrations.LiquidFW.RegistryFile = "liquidfw-devices.json"
	}

	// Assistant defaults — CPU-only, sized for the 4-core / 8GB Beelink target.
	a := &cfg.Assistant
	if a.Model == "" {
		a.Model = "qwen2.5:3b-instruct"
	}
	if a.OllamaURL == "" {
		a.OllamaURL = "http://127.0.0.1:11435/api/chat"
	}
	if a.NumCtx == 0 {
		a.NumCtx = 4096
	}
	if a.NumPredict == 0 {
		a.NumPredict = 320
	}
	if a.NumThread == 0 {
		a.NumThread = 4
	}
	// NumGPU defaults to 0 (CPU-only) — the zero value is exactly what we want.
	if a.Temperature == 0 {
		a.Temperature = 0.2
	}
	if a.MaxSteps == 0 {
		a.MaxSteps = 5
	}
	if a.DeviceCap == 0 {
		a.DeviceCap = 150
	}
	if a.MemoryFile == "" {
		a.MemoryFile = "/data/assistant-memory.json"
	}
	if a.WhisperURL == "" {
		a.WhisperURL = "http://127.0.0.1:9010"
	}
	if a.PiperURL == "" {
		a.PiperURL = "http://127.0.0.1:5111"
	}

	// Auth defaults
	if cfg.Auth.UsersFile == "" {
		cfg.Auth.UsersFile = "/data/auth-users.json"
	}
	if cfg.Auth.SessionDays == 0 {
		cfg.Auth.SessionDays = 30
	}

	// Cameras defaults
	if cfg.Cameras.NvrUpstream == "" {
		cfg.Cameras.NvrUpstream = "http://localhost:5000"
	}

	return cfg, nil
}
