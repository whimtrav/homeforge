// Package weather publishes local OUTDOOR conditions for the climate brain, from the
// free Open-Meteo API (no key required). Defaults to Billings, MT. This is the API-first
// outdoor-temp source; the camper-agm window probe folds in later as the primary once
// it's converted to LiquidFW (this stays as fallback/backfill).
package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const pollEvery = 10 * time.Minute

var wxHTTP = &http.Client{Timeout: 12 * time.Second}

type Manager struct {
	cfg   config.WeatherConfig
	store *entity.Store
}

func NewManager(cfg config.WeatherConfig, store *entity.Store) *Manager {
	return &Manager{cfg: cfg, store: store}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("weather: disabled")
		<-ctx.Done()
		return
	}
	lat, lon := m.cfg.Lat, m.cfg.Lon
	if lat == 0 && lon == 0 {
		lat, lon = 45.7833, -108.5007 // Billings, MT
	}
	slog.Info("weather: starting", "lat", lat, "lon", lon)
	m.poll(lat, lon)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.poll(lat, lon)
		}
	}
}

func (m *Manager) poll(lat, lon float64) {
	url := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f"+
		"&current=temperature_2m,relative_humidity_2m,apparent_temperature&temperature_unit=fahrenheit&timezone=auto",
		lat, lon)
	resp, err := wxHTTP.Get(url)
	if err != nil {
		slog.Warn("weather: poll failed", "err", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var r struct {
		Current struct {
			Temp     float64 `json:"temperature_2m"`
			Humidity float64 `json:"relative_humidity_2m"`
			Feels    float64 `json:"apparent_temperature"`
		} `json:"current"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		slog.Warn("weather: parse failed", "err", err)
		return
	}
	set := func(id, name string, v float64, unit string) {
		m.store.Set(entity.Entity{
			ID: id, Name: name, Domain: "sensor",
			State:      strconv.FormatFloat(v, 'f', 1, 64),
			Attributes: map[string]any{"device": "weather", "section": "outdoor", "unit_of_measurement": unit},
		})
	}
	set("sensor.outdoor_temperature", "Outdoor Temperature", r.Current.Temp, "°F")
	set("sensor.outdoor_humidity", "Outdoor Humidity", r.Current.Humidity, "%")
	set("sensor.outdoor_feels_like", "Outdoor Feels Like", r.Current.Feels, "°F")
	slog.Debug("weather: polled", "temp", r.Current.Temp, "rh", r.Current.Humidity)
}
