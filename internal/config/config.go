package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	API         APIConfig          `yaml:"api"`
	MQTT        MQTTConfig         `yaml:"mqtt"`
	Database    DatabaseConfig     `yaml:"database"`
	Automations []AutomationConfig `yaml:"automations"`
	Integrations IntegrationsConfig `yaml:"integrations"`
}

type APIConfig struct {
	Addr string `yaml:"addr"`
}

type MQTTConfig struct {
	// Embedded broker (default). Set external: true to use an external broker.
	External bool   `yaml:"external"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type DatabaseConfig struct {
	// Driver: "sqlite" (default) or "postgres"
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type AutomationConfig struct {
	Name    string          `yaml:"name"`
	Trigger TriggerConfig   `yaml:"trigger"`
	Condition *ConditionConfig `yaml:"condition,omitempty"`
	Action  []ActionConfig  `yaml:"action"`
}

type TriggerConfig struct {
	Type     string `yaml:"type"`   // state_change | time | mqtt
	Entity   string `yaml:"entity,omitempty"`
	To       string `yaml:"to,omitempty"`
	From     string `yaml:"from,omitempty"`
	Cron     string `yaml:"cron,omitempty"`
	Topic    string `yaml:"topic,omitempty"`
}

type ConditionConfig struct {
	Type   string `yaml:"type"`   // state | time_range
	Entity string `yaml:"entity,omitempty"`
	State  string `yaml:"state,omitempty"`
	After  string `yaml:"after,omitempty"`
	Before string `yaml:"before,omitempty"`
}

type ActionConfig struct {
	Service string         `yaml:"service"`
	Entity  string         `yaml:"entity"`
	Data    map[string]any `yaml:"data,omitempty"`
	Wait    string         `yaml:"wait,omitempty"`
}

type IntegrationsConfig struct {
	Zigbee2MQTT Zigbee2MQTTConfig `yaml:"zigbee2mqtt"`
	ESPHome     ESPHomeConfig     `yaml:"esphome"`
	WiZ         WiZConfig         `yaml:"wiz"`
	WLED        WLEDConfig        `yaml:"wled"`
	Emporia     EmporiaConfig     `yaml:"emporia"`
	Tigo        TigoConfig        `yaml:"tigo"`
}

type Zigbee2MQTTConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BaseTopic  string `yaml:"base_topic"`
}

type ESPHomeConfig struct {
	Enabled bool              `yaml:"enabled"`
	Devices []ESPHomeDevice   `yaml:"devices"`
}

type ESPHomeDevice struct {
	Name string `yaml:"name"`
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type WiZConfig struct {
	Enabled bool        `yaml:"enabled"`
	Bulbs   []WiZBulb   `yaml:"bulbs"`
}

type WiZBulb struct {
	Name string `yaml:"name"`
	IP   string `yaml:"ip"`
}

type WLEDConfig struct {
	Enabled  bool         `yaml:"enabled"`
	Devices  []WLEDDevice `yaml:"devices"`
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
	return cfg, nil
}
