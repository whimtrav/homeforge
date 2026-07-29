package esphome

import (
	"context"
	"log/slog"

	"github.com/whimtrav/homeforge/internal/bus"
	"github.com/whimtrav/homeforge/internal/config"
	"github.com/whimtrav/homeforge/internal/entity"
)

// Manager starts and supervises one Client goroutine per configured device.
type Manager struct {
	cfg   config.ESPHomeConfig
	store *entity.Store
	bus   *bus.Bus
}

func NewManager(cfg config.ESPHomeConfig, store *entity.Store, b *bus.Bus) *Manager {
	return &Manager{cfg: cfg, store: store, bus: b}
}

func (m *Manager) Run(ctx context.Context) {
	if len(m.cfg.Devices) == 0 {
		slog.Info("esphome: no devices configured")
		<-ctx.Done()
		return
	}
	slog.Info("esphome: starting", "devices", len(m.cfg.Devices))
	for _, dev := range m.cfg.Devices {
		dev := dev
		go NewClient(dev, m.store, m.bus).Run(ctx)
	}
	<-ctx.Done()
}
