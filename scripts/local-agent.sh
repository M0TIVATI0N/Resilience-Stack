#!/usr/bin/env bash
# Runs healerd on the local machine, the way systemd runs it on a server.
#
#   local-agent.sh start|stop
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUNTIME="$ROOT/.run"
PID_FILE="$RUNTIME/healerd.pid"

stop_agent() {
    if [[ -s $PID_FILE ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        kill "$(cat "$PID_FILE")"
        while kill -0 "$(cat "$PID_FILE")" 2>/dev/null; do sleep 0.1; done
    fi
    rm -f "$PID_FILE"
}

case ${1:-} in
    start)
        stop_agent
        (cd "$ROOT/agent" && CGO_ENABLED=0 go build -o "$RUNTIME/healerd" ./cmd/healerd)
        # shellcheck disable=SC1091
        source "$ROOT/deploy/.env"
        RS_RUNTIME_DIR=$RUNTIME \
            HEALER_LISTEN=172.30.0.1:9095 \
            HEALER_TOKEN_FILE=$RUNTIME/secrets/healer_token \
            HEALER_EDGE_URL=http://127.0.0.1:$EDGE_PORT/healthz \
            nohup "$RUNTIME/healerd" >>"$RUNTIME/healerd.log" 2>&1 &
        echo $! >"$PID_FILE"
        for _ in {1..50}; do
            curl -fs --max-time 1 http://172.30.0.1:9095/healthz >/dev/null 2>&1 && exit 0
            sleep 0.2
        done
        echo "healerd did not start, see $RUNTIME/healerd.log" >&2
        exit 1
        ;;
    stop)
        stop_agent
        ;;
    *)
        echo "usage: local-agent.sh start|stop" >&2
        exit 64
        ;;
esac
