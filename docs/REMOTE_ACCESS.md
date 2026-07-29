# Remote access

HomeForge has its own login (see `auth` in `config.yaml`), so the app protects itself no matter
how you reach it — the **first thing to do is enable auth and create your owner account**. Then
pick one of the methods below to reach your home from outside. HomeForge doesn't manage any of
these for you; you set up whichever you prefer.

> ⚠️ Never expose HomeForge to the internet with `auth.enabled: false`. It controls your home.

## Option 1 — Tailscale (easiest, recommended)

No domain, no open ports, no certificates. Install Tailscale on the machine running HomeForge
and on your phone/laptop; you then reach it over a private mesh at its Tailscale IP/name.

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
# then browse to http://<tailscale-name>:8093
```

Add [Tailscale Serve/Funnel] if you want an HTTPS name. Great default for most people.

## Option 2 — Cloudflare Tunnel (public HTTPS URL, no open ports)

Gives you `https://something.yourdomain.com` with no port-forwarding. You need a domain on
Cloudflare. Create a **Tunnel** in the Cloudflare Zero Trust dashboard, add a public hostname
routing to `http://localhost:8093`, copy the tunnel token into `.env` as `CF_TUNNEL_TOKEN`, and
the bundled `cloudflared` service in `docker-compose.yml` runs it:

```yaml
# already in docker-compose.yml:
cloudflared:
  image: cloudflare/cloudflared:latest
  network_mode: host
  command: tunnel --no-autoupdate run --token ${CF_TUNNEL_TOKEN}
```

The browser microphone (for the voice assistant) only works over HTTPS or localhost, so a
tunnel/HTTPS is what unlocks voice on a phone.

## Option 3 — WireGuard (your own VPN)

Run WireGuard (or your router's built-in VPN). Connect your phone to your home network, then
reach HomeForge at its LAN address. Full network access, fully self-hosted.

## Option 4 — Reverse proxy (advanced)

Point Caddy or nginx at `localhost:8093` with your own domain + TLS cert. Caddy example:

```
home.example.com {
    reverse_proxy localhost:8093
}
```

## Whichever you choose

- Keep `auth.enabled: true` and use a strong owner password.
- HomeForge sets `Secure` session cookies automatically when it sees HTTPS
  (`X-Forwarded-Proto: https`), so it works correctly behind a tunnel or proxy.
- WebSockets are used for live updates — make sure your proxy/tunnel forwards them (Cloudflare
  and the options above all do by default).
