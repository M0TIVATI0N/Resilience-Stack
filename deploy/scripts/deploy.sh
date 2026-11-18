#!/usr/bin/env bash
# Blue-green deploy of one application version.
#
#   deploy.sh <version>
#
# Traffic reaches the new release only after its health check passes, then moves
# over in weighted steps. The old release stays up as an nginx backup for the
# watch window; healerd retires it, or rolls back to it if the new release fails.
#
# Environment:
#   FORCE=1          deploy a version the agent has quarantined
#   SKIP_PULL=1      use a locally built image
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
RS_HOME=${RS_HOME:-$(dirname -- "$SCRIPT_DIR")}
ENV_FILE=${RS_ENV_FILE:-$RS_HOME/.env}

[[ -r $ENV_FILE ]] || { echo "deploy: cannot read $ENV_FILE" >&2; exit 2; }
set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

VERSION=${1:-}
[[ $VERSION =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] || { echo "usage: deploy.sh <version>" >&2; exit 64; }

: "${APP_IMAGE_REPO:?APP_IMAGE_REPO is not set}"
: "${RS_RUNTIME_DIR:?RS_RUNTIME_DIR is not set}"

IMAGE="$APP_IMAGE_REPO:$VERSION"
APP_COMPOSE="$RS_HOME/compose/app.yml"
STATE_FILE="$RS_RUNTIME_DIR/state.json"
LOCK_FILE="$RS_RUNTIME_DIR/deploy.lock"
UPSTREAM_FILE="$RS_RUNTIME_DIR/upstream/active.conf"
NGINX_CONTAINER=${NGINX_CONTAINER:-rs-nginx}
EDGE_URL=${EDGE_URL:-http://127.0.0.1:${EDGE_PORT:-80}/healthz}
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-60}
SHIFT_STEPS=${SHIFT_STEPS:-10 50}
SHIFT_PAUSE=${SHIFT_PAUSE:-3}

log() { printf '[deploy %s] %s\n' "$(date +%H:%M:%S)" "$*"; }

umask 0002
exec 9>"$LOCK_FILE"
flock -w 300 9 || { log "another deploy or rollback is still running"; exit 75; }

[[ -s $STATE_FILE ]] || echo '{}' >"$STATE_FILE"

ACTIVE=$(jq -r '.current.color // empty' "$STATE_FILE")
ACTIVE_VERSION=$(jq -r '.current.version // empty' "$STATE_FILE")
TARGET=blue
[[ $ACTIVE == blue ]] && TARGET=green

if [[ $VERSION == "$ACTIVE_VERSION" ]]; then
    log "version $VERSION is already live, nothing to do"
    exit 0
fi
if [[ ${FORCE:-0} != 1 ]] && jq -e --arg v "$VERSION" '(.quarantined // []) | index($v)' "$STATE_FILE" >/dev/null; then
    log "version $VERSION was rolled back automatically and is quarantined; set FORCE=1 to override"
    exit 3
fi

compose() {
    APP_COLOR=$TARGET APP_IMAGE=$IMAGE APP_VERSION=$VERSION \
        docker compose -f "$APP_COMPOSE" --env-file "$ENV_FILE" "$@"
}

# write_upstream SERVER_LINE...: atomically replaces the nginx routing include.
write_upstream() {
    local tmp
    tmp=$(mktemp "$UPSTREAM_FILE.XXXXXX")
    {
        echo "# Managed by deploy.sh and healerd. Do not edit."
        printf 'server %s;\n' "$@"
    } >"$tmp"
    chmod 0644 "$tmp"
    mv -f "$tmp" "$UPSTREAM_FILE"
}

reload_nginx() {
    docker exec "$NGINX_CONTAINER" nginx -t -q
    docker exec "$NGINX_CONTAINER" nginx -s reload >/dev/null 2>&1
}

TRAFFIC_MOVED=0

# Undo everything visible to users, then leave the failed container stopped.
abort() {
    local message=${1:-unexpected failure}
    trap - ERR
    log "ABORT: $message"
    if [[ $TRAFFIC_MOVED == 1 && -n $ACTIVE ]]; then
        log "returning all traffic to app-$ACTIVE"
        write_upstream "app-$ACTIVE:8080 resolve"
        reload_nginx || log "nginx reload failed while aborting"
    fi
    docker stop --time 10 "app-$TARGET" >/dev/null 2>&1 || true
    exit 1
}
trap 'abort "command failed on line $LINENO"' ERR

wait_healthy() {
    local deadline=$((SECONDS + HEALTH_TIMEOUT)) health
    while ((SECONDS < deadline)); do
        health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "app-$TARGET" 2>/dev/null || true)
        case $health in
            healthy) return 0 ;;
            unhealthy) return 1 ;;
        esac
        sleep 1
    done
    return 1
}

# Several consecutive answers are required: right after a reload nginx can still
# send a request to the standby while it resolves the new backend.
wait_edge_serves_target() {
    local attempt headers streak=0
    for attempt in {1..75}; do
        headers=$(curl -fsS -o /dev/null -D - --max-time 2 "$EDGE_URL" 2>/dev/null || true)
        if grep -qi "^x-app-version: $VERSION" <<<"${headers//$'\r'/}"; then
            streak=$((streak + 1))
            if ((streak >= 5)); then
                return 0
            fi
        else
            streak=0
        fi
        sleep 0.2
    done
    log "edge check gave up after $attempt attempts"
    return 1
}

commit_state() {
    local now tmp
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    tmp=$(mktemp "$STATE_FILE.XXXXXX")
    jq --arg version "$VERSION" --arg image "$IMAGE" --arg color "$TARGET" --arg now "$now" '
        .previous = .current
        | .current = {version: $version, image: $image, color: $color, deployed_at: $now}
        | .quarantined = ((.quarantined // []) - [$version])
        | .history = (((.history // []) + [{at: $now, kind: "deploy", version: $version, from: (.previous.version // "")}]) | .[-50:])
    ' "$STATE_FILE" >"$tmp"
    chmod 0664 "$tmp"
    mv -f "$tmp" "$STATE_FILE"
}

log "deploying $VERSION to $TARGET (live: ${ACTIVE_VERSION:-none})"

if [[ ${SKIP_PULL:-0} != 1 ]]; then
    compose pull --quiet app || abort "cannot pull $IMAGE"
fi
compose up --detach --no-build --force-recreate app >/dev/null || abort "cannot start app-$TARGET"

log "waiting for app-$TARGET to become healthy"
wait_healthy || abort "app-$TARGET did not become healthy within ${HEALTH_TIMEOUT}s; live traffic was not touched"

TRAFFIC_MOVED=1
if [[ -n $ACTIVE ]]; then
    for percent in $SHIFT_STEPS; do
        log "shifting $percent% of traffic to app-$TARGET"
        write_upstream "app-$TARGET:8080 weight=$percent resolve" "app-$ACTIVE:8080 weight=$((100 - percent)) resolve"
        reload_nginx
        sleep "$SHIFT_PAUSE"
    done
    log "shifting 100% of traffic to app-$TARGET, keeping app-$ACTIVE as hot standby"
    write_upstream "app-$TARGET:8080 resolve" "app-$ACTIVE:8080 resolve backup"
else
    write_upstream "app-$TARGET:8080 resolve"
fi
reload_nginx

wait_edge_serves_target || abort "edge does not serve $VERSION after the switch"

commit_state
trap - ERR
log "deployed $VERSION on $TARGET"
