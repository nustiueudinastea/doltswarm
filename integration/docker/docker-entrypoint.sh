#!/bin/bash
set -e

# Default values
PORT=${PORT:-10500}
LISTEN_ADDR=${LISTEN_ADDR:-0.0.0.0}
DB_PATH=${DB_PATH:-/data}
LOG_LEVEL=${LOG_LEVEL:-info}

# Build common args
COMMON_ARGS="--port $PORT --db $DB_PATH --log $LOG_LEVEL --listen-addr $LISTEN_ADDR"

# Add bootstrap peers if specified (comma-separated or multiple env vars)
if [ -n "$BOOTSTRAP_PEERS" ]; then
    IFS=',' read -ra PEERS <<< "$BOOTSTRAP_PEERS"
    for peer in "${PEERS[@]}"; do
        COMMON_ARGS="$COMMON_ARGS --bootstrap-peer $peer"
    done
fi

case "$1" in
    init)
        shift
        if [ "$1" = "--local" ]; then
            echo "Initializing local database..."
            exec /app/ddolt $COMMON_ARGS init --local
        elif [ -n "$INIT_PEER" ]; then
            echo "Initializing from peer $INIT_PEER..."
            exec /app/ddolt $COMMON_ARGS init --peer "$INIT_PEER"
        else
            echo "Error: init requires --local or INIT_PEER env var"
            exit 1
        fi
        ;;
    server)
        shift
        # Check if database is initialized
        if [ ! -d "$DB_PATH" ] || [ -z "$(ls -A $DB_PATH 2>/dev/null)" ]; then
            echo "Database not initialized. Initialize first with 'init --local' or 'init' with INIT_PEER"
            exit 1
        fi

        echo "Starting server on $LISTEN_ADDR:$PORT..."
        # If DEBUG_SQL=1, emit system table list before starting
        if [ "${DEBUG_SQL:-0}" = "1" ]; then
            echo "DEBUG: listing system tables (dolt_*):"
            /app/ddolt $COMMON_ARGS --no-gui --no-commits status >/dev/null 2>&1 || true
            /app/ddolt $COMMON_ARGS --no-gui --no-commits sql -q "SHOW TABLES LIKE 'dolt_%';" || true
        fi

        exec /app/ddolt $COMMON_ARGS --no-gui --no-commits server "$@"
        ;;
    status)
        exec /app/ddolt $COMMON_ARGS status
        ;;
    test)
        # Run tests inside container
        shift
        exec go test -v -timeout "${TEST_TIMEOUT:-30m}" "$@"
        ;;
    *)
        # Pass through to ddolt
        exec /app/ddolt "$@"
        ;;
esac
