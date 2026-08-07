# HomeForge

A fast, self-hosted home-automation hub written in Go. A single static binary embeds a
SvelteKit dashboard and serves the UI, REST API, and a real-time WebSocket on one port — no
Python runtime, no cloud dependency. Runs on anything from a Raspberry Pi to a NAS.

> **Local-first by design: your home stays yours.** Nothing is required to phone home.

## Features

- **Entity store + real-time UI** — instant WebSocket push to the dashboard, no polling.
- **Automations** — simple `trigger → condition → action` rules in YAML (state, time, and
  sunrise/sunset triggers).
- **Integrations** — MQTT (Zigbee2MQTT, Tasmota, …) plus native clients for **ESPHome, WiZ,
  and WLED**. Cloud-vendor accounts (**Emporia, VeSync, Kidde, Hubspace, Ring**) run as
  optional docker-compose **sidecar bridges** that publish onto the internal MQTT bus.
- **History & energy** — long-term metrics in TimescaleDB with a dedicated energy dashboard.
- **Cameras** — reverse-proxy an NVR (Sentinel/Frigate) under `/nvr/` on the same origin,
  behind the login, plus a native live-frame grid and events browser.
- **Climate** — a thermostat controller and an optional "climate brain" (solar pre-cool,
  attic-fan A/B, heat-guard) for smarter HVAC.
- **Login** — built-in accounts (bcrypt, sessions, per-IP rate limiting) so the app protects
  itself over any access path — LAN, VPN, or a public tunnel.
- **Optional local AI assistant** — chat/voice control backed by a local LLM (ollama) plus
  Whisper (speech-to-text) and Piper (text-to-speech). All on-box, no cloud.
- **Alexa Smart Home** (optional) — a native skill endpoint so rooms/devices work with Alexa,
  with account-linking handled in-app (no third-party cloud relay).

## Quick start (Docker)

```bash
git clone https://github.com/whimtrav/homeforge.git
cd homeforge
cp .env.example .env                 # set MQTT_PASS + POSTGRES_PASSWORD
cp config.example.yaml config.yaml   # edit for your devices
docker compose up -d                 # core stack
```

Open `http://<your-host>:8093`. On first launch you'll create the owner account.

To also run the **local AI assistant** stack (ollama + Whisper + Piper), start the `ai` profile:

```bash
docker compose --profile ai up -d
```

> `config.yaml` and `.env` are gitignored — keep your real values there, never in git.

## Configuration

Everything is driven by `config.yaml` (see `config.example.yaml` for a fully-commented
template). Each integration has its own `enabled:` block, so you only turn on what you use.
Cloud-bridge credentials live in the sidecar env files (e.g. `hubspace/.env`) — also
gitignored.

## Remote access

HomeForge is tunnel-agnostic. Because the login lives *inside* the app, you can safely expose
it however you like — pick one:

- **Tailscale** — easiest; no domain or open ports needed.
- **Cloudflare Tunnel** — clean public HTTPS URL, no open ports (set `CF_TUNNEL_TOKEN`).
- **WireGuard** — your own VPN.
- **Reverse proxy** — Caddy/nginx with your own domain + cert.
- **Local-only** — the default; nothing exposed.

See [`docs/REMOTE_ACCESS.md`](docs/REMOTE_ACCESS.md).

## Project layout

```
cmd/homeforge/      main entry point
internal/           the hub: api, entity store, automation engine, mqtt broker,
                    history, climate, and per-vendor integrations
web/                SvelteKit dashboard (built and embedded into the binary)
emporia/ vesync/ kidde/ hubspace/ piper/   optional docker-compose sidecars
docker-compose.yml  full stack (core services + the `ai` profile)
```

## Building from source

```bash
cd web && npm install && npm run build && cd ..   # build the embedded UI
go build ./cmd/homeforge
```

## License

MIT (see [`LICENSE`](LICENSE)).

---

*HomeForge is self-hosted software you run yourself. It is not a hosted service — there is no
central server, and no support/warranty is implied. Configure your own remote access and
secure your own instance.*
