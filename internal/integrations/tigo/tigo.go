// Package tigo is a HomeForge integration for the Tigo CCA (Cloud Connect Advanced),
// read LOCALLY — no Tigo cloud. The CCA exposes per-panel data at
//   GET http://<host>/cgi-bin/summary_data?date=YYYY-MM-DD&temp=pin&_=<epoch>
// behind HTTP Basic auth Tigo:$olar. Response: dataset[] time-series blocks, each with
// `order` (panel names A1..) and `data[]` where the last entry's `d[]` = latest per-panel
// watts (index-aligned to `order`). We poll it and publish read-only sensor entities.
package tigo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

const (
	pollEvery = 30 * time.Second
	// CCA local Basic auth = base64("Tigo:$olar"). Fixed device credential.
	authHeader = "Basic VGlnbzokb2xhcg=="
)

var tigoHTTP = &http.Client{Timeout: 12 * time.Second}

type Manager struct {
	cfg   config.TigoConfig
	store *entity.Store
	bus   *bus.Bus
}

func NewManager(cfg config.TigoConfig, store *entity.Store, b *bus.Bus) *Manager {
	return &Manager{cfg: cfg, store: store, bus: b}
}

func (m *Manager) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		slog.Info("tigo: disabled")
		<-ctx.Done()
		return
	}
	host := m.cfg.Host
	if host == "" {
		slog.Warn("tigo: no host configured — set integrations.tigo.host; skipping")
		return
	}
	slog.Info("tigo: starting", "host", host)
	m.poll(host)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.poll(host)
		}
	}
}

// summaryResp mirrors only the fields we consume from /cgi-bin/summary_data.
type summaryResp struct {
	Dataset []struct {
		Order []string `json:"order"`
		Data  []struct {
			T string `json:"t"`
			D []any  `json:"d"` // per-panel readings: number, string, or null
		} `json:"data"`
	} `json:"dataset"`
}

func (m *Manager) poll(host string) {
	now := time.Now()
	url := fmt.Sprintf("http://%s/cgi-bin/summary_data?date=%s&temp=pin&_=%d",
		host, now.Format("2006-01-02"), now.Unix())
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := tigoHTTP.Do(req)
	if err != nil {
		slog.Warn("tigo: poll failed", "err", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var sr summaryResp
	if err := json.Unmarshal(body, &sr); err != nil {
		slog.Warn("tigo: parse failed", "err", err)
		return
	}

	// Use the most recent block that actually has data, and its last sample.
	var order []string
	var latest []any
	for i := len(sr.Dataset) - 1; i >= 0; i-- {
		blk := sr.Dataset[i]
		if len(blk.Data) == 0 {
			continue
		}
		order = blk.Order
		latest = blk.Data[len(blk.Data)-1].D
		break
	}
	if len(order) == 0 || len(latest) == 0 {
		return
	}

	total := 0.0
	active := 0
	for i, name := range order {
		if i >= len(latest) {
			break
		}
		w, ok := asFloat(latest[i])
		if !ok {
			continue // null / offline reading this sample — keep last published value
		}
		total += w
		if w > 0 {
			active++
		}
		m.store.Set(entity.Entity{
			ID:     "sensor.tigo_" + strings.ToLower(name),
			Name:   "Tigo Panel " + name,
			Domain: "sensor",
			State:  strconv.Itoa(int(w)),
			Attributes: map[string]any{
				"device":              "tigo",
				"panel":               name,
				"section":             "panels",
				"unit_of_measurement": "W",
			},
		})
	}

	m.store.Set(entity.Entity{
		ID: "sensor.tigo_total_power", Name: "Tigo Total Power", Domain: "sensor",
		State:      strconv.Itoa(int(total)),
		Attributes: map[string]any{"device": "tigo", "section": "panels", "unit_of_measurement": "W"},
	})
	m.store.Set(entity.Entity{
		ID: "sensor.tigo_panels_active", Name: "Tigo Panels Active", Domain: "sensor",
		State:      fmt.Sprintf("%d/%d", active, len(order)),
		Attributes: map[string]any{"device": "tigo", "section": "panels"},
	})
	slog.Debug("tigo: polled", "panels", len(order), "active", active, "total_w", int(total))
}

func asFloat(v any) (float64, bool) {
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
