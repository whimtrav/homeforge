#!/bin/sh
# One-time setup for the HomeForge AI assistant after a fresh deploy (e.g. the Beelink port).
# The model lives in the `ollama_models` docker volume, which starts empty on a new host.
# Run AFTER `docker compose up -d ollama`.
set -e
MODEL="${1:-qwen2.5:3b-instruct}"   # CPU-friendly tool-calling model; matches config.yaml assistant.model
echo "Pulling $MODEL into the homeforge-ollama container (CPU-only)..."
docker exec homeforge-ollama ollama pull "$MODEL"
echo "Done. Loaded models:"
docker exec homeforge-ollama ollama list
echo "The assistant will pre-warm on the next HomeForge start (see logs: 'assistant: prewarmed')."
