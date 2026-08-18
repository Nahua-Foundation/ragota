#!/usr/bin/env bash
# One command from a cold machine to a serving index: start the stores and
# model services the config needs, prove the wiring, then run the server over
# a directory of repositories. Everything here is idempotent — a service that
# is already up is left alone — so this is safe to run every morning.
#
#   make up SOURCE=~/projects
#
# The model stack matches config.yaml: Qdrant in docker, the embedder through
# Ollama, and a llama.cpp reranker on :8090. The reranker's batch flags are
# deliberately modest: its physical batch must hold one query+document pair
# (the server caps documents at 4 KB, so 2048 tokens is generous), and larger
# buffers were measured as gigabytes of footprint the OS pushed to swap.
set -euo pipefail

SOURCE="${1:?usage: scripts/up.sh <directory-with-repositories>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG="${RAGOTA_CONFIG:-$ROOT/config.yaml}"
LOGDIR="$HOME/.ragota/logs"
mkdir -p "$LOGDIR"

say() { printf '\033[1m%s\033[0m\n' "$*"; }

say "qdrant"
if ! curl -sf --max-time 2 localhost:6333/ >/dev/null; then
    docker start qdrant >/dev/null 2>&1 ||
        docker run -d --name qdrant -p 6333:6333 -v qdrant_storage:/qdrant/storage qdrant/qdrant >/dev/null
    until curl -sf --max-time 2 localhost:6333/ >/dev/null; do sleep 1; done
fi
echo "  up"

say "embedder (ollama)"
if ! ollama list 2>/dev/null | grep -q '^qwen3-embedding:0.6b'; then
    ollama pull qwen3-embedding:0.6b
fi
echo "  qwen3-embedding:0.6b present"

say "reranker (llama.cpp on :8090)"
if ! curl -sf --max-time 2 localhost:8090/health >/dev/null; then
    nohup llama-server -hf ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF --rerank --port 8090 \
        -c 4096 -b 2048 -ub 2048 >"$LOGDIR/reranker.log" 2>&1 &
    until curl -sf --max-time 2 localhost:8090/health | grep -q ok; do sleep 1; done
fi
echo "  up (log: $LOGDIR/reranker.log)"

say "wiring"
"$ROOT/bin/ragota" --config "$CONFIG" --check-config

cat <<'EOF'

Connect an agent once the server is up (next command takes the terminal):
  claude mcp add ragota -e RAGOTA_URL=http://localhost:8080 -- <repo>/bin/ragota mcp
Repositories indexed before the vector index was enabled need one forced pass:
  for id in $(curl -s localhost:8080/api/v1/repos | jq -r '.[] | select(.active) | .id'); do
      curl -s -X POST localhost:8080/api/v1/repos/$id/index -H 'Content-Type: application/json' -d '{"force":true}'; echo; done

EOF

say "server"
exec "$ROOT/bin/ragota" --config "$CONFIG" --source "$SOURCE" --watch --interactive run
