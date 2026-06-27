# HomeForge

A Go-native home automation platform built to replace Home Assistant.
Single binary, embedded MQTT broker, Svelte 5 dashboard. Runs on a Raspberry Pi 4 or any server.

## Quick Start

```bash
cp config.example.yaml config.yaml
# edit config.yaml for your devices
docker compose up -d
```

Then open `http://localhost:8123`.

## Architecture

- **Go binary** — embeds the Svelte frontend, serves REST + WebSocket, runs the automation engine
- **Embedded MQTT broker** — no Mosquitto required (optional external broker supported)
- **SQLite** — default storage, zero config (Postgres optional)
- **Svelte 5 + Tailwind** — real-time dashboard via WebSocket push

## Supported Integrations

| Integration | Protocol | Status |
|-------------|---------|--------|
| Zigbee2MQTT | MQTT | ✅ |
| Z-Wave JS | MQTT | ✅ |
| Tasmota | MQTT | ✅ |
| ESPHome | Native API | 🚧 |
| WiZ bulbs | UDP | 🚧 |
| WLED | REST | 🚧 |
| Emporia Vue | Cloud API | 🚧 |
| Tigo Solar | REST | 🚧 |
| Sentinel NVR | MQTT | ✅ |

## License

MIT
