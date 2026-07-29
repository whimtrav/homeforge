# HomeForge

A fast, self-hosted home-automation hub written in Go. A single static binary embeds a
SvelteKit dashboard and serves the UI, REST API, and a real-time WebSocket on one port — no
Python runtime, no cloud dependency. Runs on anything from a Raspberry Pi to a NAS.

> Local-first by design: your home stays yours. Nothing is required to phone home.

## Features

- **Entity store + real-time UI** — instant WebSocket push, no polling.
- **Automations** — simple `trigger → condition → action` rules in YAML.
- **Integrations** — MQTT (Zigbee2MQTT, Tasmota, …) plus native clients for ESPHome, WiZ,
  WLED, and more. Cloud-vendor bridges (Emporia, VeSync, Kidde, Hubspace, Ring) ship as
  optional docker-compose sidecars.
- **History & energy** — long-term metrics in TimescaleDB, with an energy dashboard.
- **Login** — built-in accounts (bcrypt, sessions, per-IP rate limiting) so the app protects
  itself over any access path (LAN, VPN, or a public tunnel). See below.
- **Optional local AI assistant** — chat/voice control backed by a local LLM (ollama) plus
  Whisper (speech-to-text) and Piper (text-to-speech). All on-box, no cloud.

## Quick start (Docker)

```bash
git clone https://github.com/whimtrav/homeforge.git
cd homeforge
cp .env.example .env                 # set MQTT_PASS + POSTGRES_PASSWORD
cp config.example.yaml config.yaml   # edit for your devices
docker compose up -d
```

Open `http://<your-host>:8093`. On first launch you'll create the owner account.

`config.yaml` and `.env` are gitignored — keep your real values there, never in git.

## Remote access

HomeForge is tunnel-agnostic. Because the login lives *inside* the app, you can safely expose
it however you like — pick one:

- **Tailscale** — easiest; no domain or open ports needed.
- **Cloudflare Tunnel** — clean public HTTPS URL, no open ports (set `CF_TUNNEL_TOKEN`).
- **WireGuard** — your own VPN.
- **Reverse proxy** — Caddy/nginx with your own domain + cert.
- **Local-only** — the default; nothing exposed.

See [`docs/REMOTE_ACCESS.md`](docs/REMOTE_ACCESS.md).

## Building from source

```bash
cd web && npm install && npm run build && cd ..   # build the embedded UI
go build ./cmd/homeforge
```

## License

MIT (see LICENSE).

---

*HomeForge is self-hosted software you run yourself. It is not a hosted service — there is no
central server, and no support/warranty is implied. Configure your own remote access and
secure your own instance.*
