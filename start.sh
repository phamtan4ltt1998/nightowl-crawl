#!/bin/sh
set -eu

# ---------------------------------------------------------------------------
# nightowl-fetcher start script
# Usage:
#   ./start.sh            — build (if needed) then run
#   ./start.sh --build    — force rebuild then run
#   ./start.sh --dev      — go run (no binary, fast for dev)
#   ./start.sh --help     — show this help
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$SCRIPT_DIR/bin/fetcher"
ENV_FILE="$SCRIPT_DIR/.env"

# --- Go toolchain: try common install locations ---
for candidate in \
    "$HOME/go-sdk/bin" \
    "/usr/local/go/bin" \
    "/usr/lib/go/bin" \
    "/snap/go/current/bin"
do
    if [ -x "$candidate/go" ]; then
        PATH="$PATH:$candidate:$HOME/go/bin"
        export PATH
        break
    fi
done

if ! command -v go >/dev/null 2>&1; then
    echo "[start.sh] ERROR: Go toolchain not found." >&2
    echo "  Install: https://go.dev/dl/  or  sudo apt install golang-go" >&2
    exit 1
fi

# --- Load .env if present (simple KEY=VALUE, no spaces around =) ---
if [ -f "$ENV_FILE" ]; then
    echo "[start.sh] Loading $ENV_FILE"
    # Export each KEY=VALUE line, skip comments and blanks
    while IFS='=' read -r key val; do
        case "$key" in
            ''|\#*) continue ;;
        esac
        export "$key=$val"
    done < "$ENV_FILE"
fi

# --- Parse args ---
MODE="run"
for arg in "$@"; do
    case "$arg" in
        --build) MODE="build" ;;
        --dev)   MODE="dev"   ;;
        --help|-h)
            sed -n '3,8p' "$0" | sed 's/^# //'
            exit 0
            ;;
        *) echo "[start.sh] Unknown arg: $arg" >&2; exit 1 ;;
    esac
done

cd "$SCRIPT_DIR"

# --- Dev mode: go run, no binary ---
if [ "$MODE" = "dev" ]; then
    echo "[start.sh] Dev mode — go run ./cmd/server"
    exec go run ./cmd/server
fi

# --- Build if binary missing, stale, or --build flag ---
needs_build=false
if [ "$MODE" = "build" ]; then
    needs_build=true
elif [ ! -x "$BINARY" ]; then
    needs_build=true
elif find . -name '*.go' -newer "$BINARY" | grep -q .; then
    echo "[start.sh] Source changed — rebuilding..."
    needs_build=true
fi

if [ "$needs_build" = "true" ]; then
    echo "[start.sh] Building..."
    mkdir -p bin
    go build -ldflags="-s -w" -o "$BINARY" ./cmd/server
    echo "[start.sh] Build OK"
fi

# Auto-detect Python project root (try common sibling paths)
NIGHT_OWL_DIR=""
for _try in \
    "$SCRIPT_DIR/../nightowl/night-owl" \
    "$SCRIPT_DIR/../night-owl" \
    "$SCRIPT_DIR/../../nightowl/night-owl"
do
    if [ -f "$_try/scrape_sources.json" ]; then
        NIGHT_OWL_DIR="$(cd "$_try" && pwd)"
        break
    fi
done

if [ -z "${STORY_CONTENT_ROOT:-}" ] && [ -n "$NIGHT_OWL_DIR" ]; then
    export STORY_CONTENT_ROOT="$NIGHT_OWL_DIR/story-content"
    echo "[start.sh] STORY_CONTENT_ROOT=$STORY_CONTENT_ROOT"
fi

if [ -z "${SCRAPE_SOURCES_PATH:-}" ] && [ -n "$NIGHT_OWL_DIR" ]; then
    export SCRAPE_SOURCES_PATH="$NIGHT_OWL_DIR/scrape_sources.json"
    echo "[start.sh] SCRAPE_SOURCES_PATH=$SCRAPE_SOURCES_PATH"
fi

echo "[start.sh] Starting nightowl-fetcher on :${PORT:-8080}"
exec "$BINARY"
