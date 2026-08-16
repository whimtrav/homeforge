#!/bin/sh
# AI Terminal sidecar entrypoint. Wires the chosen AI CLI to HomeForge:
#  - reads HF's internal token (from the read-only /data mount) and writes an MCP config that
#    points the CLI at HF's owner-gated MCP server (the 14 house tools);
#  - drops a CLAUDE.md giving the AI its owner/admin context + full-admin API reach;
#  - unsets ANTHROPIC_API_KEY so Claude uses the user's SUBSCRIPTION, not pay-per-token API;
#  - launches aitermd (PTY<->ws bridge), which spawns AITERM_CMD.
set -e

HOME_DIR=/root
WS="$HOME_DIR/workspace"
mkdir -p "$WS" "$HOME_DIR/.config/homeforge"

TOKEN="$(cat /data/auth-users-internal-token 2>/dev/null || true)"

# MCP server config -> HF's HTTP MCP endpoint (host network: localhost:8093 is HomeForge).
cat > "$HOME_DIR/.config/homeforge/mcp.json" <<EOF
{"mcpServers":{"homeforge":{"type":"http","url":"http://localhost:8093/api/mcp","headers":{"X-HF-Internal":"$TOKEN"}}}}
EOF

# Admin context for the AI (literal $HF_INTERNAL_TOKEN — resolved from env at runtime, not baked).
cat > "$WS/CLAUDE.md" <<'EOF'
# HomeForge — AI Terminal (owner/admin)

You are the admin AI assistant for this HomeForge smart home. You have OWNER-LEVEL access.

## House control
Use the **homeforge** MCP tools for anything about the real house:
- read_sensor / get_state / query_history / climate_status / water_usage / energy_today / who_is_home / recent_events / find_devices
- set_switch / set_number (fan speed 0-3) / set_temperature / remember / forget

Prefer these tools over guessing — they read and act on the REAL house.

## Full admin API (for anything the tools don't cover)
The HomeForge admin API is at `$HF_URL` (http://localhost:8093). Authenticate ANY request with the
header `X-HF-Internal: $HF_INTERNAL_TOKEN` — this bypasses login and gives full admin. Example:
`curl -s -H "X-HF-Internal: $HF_INTERNAL_TOKEN" $HF_URL/api/entities | jq .`

## Ground rules (software-enforced limits still apply)
- You are read/act enabled on the whole house. **Confirm with the user before anything destructive**
  (restarting services, deleting data, bulk changes, editing automations/integrations).
- Report exactly what you did and the resulting state.
EOF

export HF_URL="http://localhost:8093"
export HF_INTERNAL_TOKEN="$TOKEN"
unset ANTHROPIC_API_KEY

# Pin the model to Opus 4.8 (owner's choice) so the CLI never auto-selects Opus 5 / Fable.
# ANTHROPIC_MODEL covers claude -p (the /gen scene path); the --model flag below pins the TUI.
: "${ANTHROPIC_MODEL:=claude-opus-4-8}"
export ANTHROPIC_MODEL

cd "$WS"

# Default CLI: Claude Code on the user's subscription, with the homeforge MCP server loaded and its
# tools pre-approved (read + house-control). bash/file ops still prompt = the software limit layer.
: "${AITERM_CMD:=claude --model claude-opus-4-8 --mcp-config $HOME_DIR/.config/homeforge/mcp.json --allowedTools mcp__homeforge}"
export AITERM_CMD

echo "HomeForge AI Terminal sidecar ready (cmd: $AITERM_CMD)"
exec aitermd
