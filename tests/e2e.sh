#!/usr/bin/env bash
# End-to-end scenarios against the real stack on local Docker: real nginx,
# Prometheus, Alertmanager and healerd. Only Telegram is replaced by a local fake.
# Resets the local sandbox in .run/ (database contents are kept).
set -Euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 1

WORK="$ROOT/.run/e2e"
STATE="$ROOT/.run/state.json"
UPSTREAM="$ROOT/.run/upstream/active.conf"
TELEGRAM_LOG="$WORK/telegram.log"
FAILURES=0
LOAD_PID=""
TELEGRAM_PID=""

say() { printf '\n\033[1;36m=== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
check() { # check "description" command...
    local what=$1
    shift
    if "$@"; then ok "$what"; else bad "$what"; fi
}

cleanup() {
    [[ -n $LOAD_PID ]] && kill "$LOAD_PID" 2>/dev/null
    [[ -n $TELEGRAM_PID ]] && kill "$TELEGRAM_PID" 2>/dev/null
    return 0
}
trap cleanup EXIT

current() { jq -r '.current.version // empty' "$STATE"; }
current_color() { jq -r '.current.color // empty' "$STATE"; }
is_current() { [[ $(current) == "$1" ]]; }
is_quarantined() { jq -e --arg v "$1" '(.quarantined // []) | index($v)' "$STATE" >/dev/null; }
telegram_has() { grep -q -- "$1" "$TELEGRAM_LOG"; }
container_status() { docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null; }
is_stopped() { [[ $(container_status "$1") != running ]]; }
is_running() { [[ $(container_status "$1") == running ]]; }
edge_version() { curl -fsS -o /dev/null -D - --max-time 2 "http://127.0.0.1:$EDGE_PORT/healthz" | tr -d '\r' | awk 'tolower($1)=="x-app-version:"{print $2}'; }
edge_serves() { [[ $(edge_version) == "$1" ]]; }
report_is() { [[ $(jq -r "$2" "$1") == "$3" ]]; }

wait_current() { # wait_current VERSION TIMEOUT_SECONDS
    local deadline=$((SECONDS + $2))
    while ((SECONDS < deadline)); do
        is_current "$1" && return 0
        sleep 0.5
    done
    return 1
}

load_start() { # load_start REPORT_FILE
    "$WORK/loadgen" -url "http://127.0.0.1:$EDGE_PORT" -rps 60 -out "$1" &
    LOAD_PID=$!
}

load_stop() {
    kill -TERM "$LOAD_PID"
    wait "$LOAD_PID" 2>/dev/null
    LOAD_PID=""
}

build() { # build VERSION [docker build args...]
    local version=$1
    shift
    docker build --quiet --build-arg "VERSION=$version" "$@" -t "$APP_IMAGE_REPO:$version" app >/dev/null 2>&1
}

deploy() { SKIP_PULL=1 deploy/scripts/deploy.sh "$@"; }

say "Preparing the sandbox"
scripts/local-agent.sh stop
docker rm --force app-blue app-green >/dev/null 2>&1
rm -f "$STATE" "$UPSTREAM"
rm -rf "$WORK"
mkdir -p "$WORK"
scripts/local-init.sh
set -a
# shellcheck disable=SC1091
source deploy/.env
set +a

(cd tools && go build -o "$WORK/loadgen" ./loadgen && go build -o "$WORK/faketelegram" ./faketelegram) || exit 1
"$WORK/faketelegram" -listen 127.0.0.1:18081 -out "$TELEGRAM_LOG" &
TELEGRAM_PID=$!

docker compose -f deploy/compose/stack.yml --env-file deploy/.env up --detach --wait --wait-timeout 180 >/dev/null 2>&1 || exit 1
docker exec rs-nginx nginx -s reload >/dev/null 2>&1

TELEGRAM_BOT_TOKEN=e2e-token TELEGRAM_CHAT_ID=1 TELEGRAM_API_URL=http://127.0.0.1:18081 scripts/local-agent.sh start || exit 1

say "Building release images"
build e2e-good1 || exit 1
build e2e-good2 || exit 1
build e2e-good3 || exit 1
build e2e-good4 || exit 1
build e2e-bad-errors --build-arg GO_TAGS=chaos --build-arg CHAOS_MODE=errors || exit 1
build e2e-bad-leak --build-arg GO_TAGS=chaos --build-arg CHAOS_MODE=leak || exit 1
printf 'FROM alpine:3.22\nCMD ["sleep", "600"]\n' | docker build --quiet -t "$APP_IMAGE_REPO:e2e-broken" - >/dev/null 2>&1 || exit 1
ok "images built"

say "1. First deploy"
check "deploy succeeds" deploy e2e-good1
check "edge serves the release" edge_serves e2e-good1
check "/metrics is not public" test "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$EDGE_PORT/metrics")" = 404

say "2. Zero-downtime blue-green deploy under load"
load_start "$WORK/zero.json"
sleep 5
check "deploy succeeds" deploy e2e-good2
sleep 5
load_stop
jq -c '{total, transport_errors, gateway_errors, app_errors, by_version: (.by_version | map_values(add))}' "$WORK/zero.json"
check "no transport errors" report_is "$WORK/zero.json" .transport_errors 0
check "no gateway errors" report_is "$WORK/zero.json" .gateway_errors 0
check "no application errors" report_is "$WORK/zero.json" .app_errors 0
check "old release served traffic" jq -e '.by_version["e2e-good1"]' "$WORK/zero.json"
check "new release served traffic" jq -e '.by_version["e2e-good2"]' "$WORK/zero.json"
check "old release is kept as hot standby" grep -q 'backup' "$UPSTREAM"

say "3. Release with a 5xx spike is rolled back"
load_start "$WORK/errors.json"
sleep 3
started=$SECONDS
check "bad release passes the deploy gate" deploy e2e-bad-errors
check "agent rolled back to the stable release" wait_current e2e-good2 90
echo "  rollback finished $((SECONDS - started))s after the deploy started"
sleep 6
load_stop
jq -c '{total, transport_errors, gateway_errors, app_errors, by_version}' "$WORK/errors.json"
check "edge serves the stable release" edge_serves e2e-good2
check "bad release is quarantined" is_quarantined e2e-bad-errors
check "bad container is stopped" is_stopped app-blue
check "no gateway errors during the incident" report_is "$WORK/errors.json" .gateway_errors 0
check "no transport errors during the incident" report_is "$WORK/errors.json" .transport_errors 0
check "only the bad release produced 500s" jq -e '[.by_version | to_entries[] | select(.key != "e2e-bad-errors") | .value["500"] // 0] | add == 0' "$WORK/errors.json"
check "telegram report names both versions" telegram_has "Деплой e2e-bad-errors отменен: доля 5xx-ошибок выросла до .* Выполнен откат на версию e2e-good2. Uptime сохранен"

say "4. Quarantined release cannot be redeployed by accident"
deploy e2e-bad-errors >/dev/null 2>&1
check "deploy is refused with exit code 3" test $? -eq 3
check "live release is unchanged" is_current e2e-good2

say "5. Release with a memory leak is rolled back"
check "stable release deploys" deploy e2e-good3
load_start "$WORK/leak.json"
sleep 3
check "leaking release passes the deploy gate" deploy e2e-bad-leak
check "agent rolled back to the stable release" wait_current e2e-good3 90
sleep 6
load_stop
jq -c '{total, transport_errors, gateway_errors, app_errors, by_version}' "$WORK/leak.json"
check "edge serves the stable release" edge_serves e2e-good3
check "bad release is quarantined" is_quarantined e2e-bad-leak
check "no transport errors during the incident" report_is "$WORK/leak.json" .transport_errors 0
gateway=$(jq -r .gateway_errors "$WORK/leak.json")
total=$(jq -r .total "$WORK/leak.json")
echo "  gateway errors: $gateway of $total requests"
check "gateway errors stay below 0.5% of requests" test "$((gateway * 200))" -le "$total"
check "telegram report names the memory problem" telegram_has "Деплой e2e-bad-leak отменен: .*памят.* Выполнен откат на версию e2e-good3. Uptime сохранен"

say "6. Release that never becomes healthy never receives traffic"
before=$(cat "$UPSTREAM")
HEALTH_TIMEOUT=30 deploy e2e-broken >/dev/null 2>&1
check "deploy fails" test $? -eq 1
check "routing is untouched" test "$before" = "$(cat "$UPSTREAM")"
check "live release is unchanged" is_current e2e-good3
check "edge still serves the live release" edge_serves e2e-good3

say "7. Standby is retired after the watch window"
check "stable release deploys" deploy e2e-good4
standby="app-$(jq -r .previous.color "$STATE")"
check "standby runs during the watch window" is_running "$standby"
echo "  waiting for the 2 minute watch window to pass"
sleep 130
check "standby is stopped after the window" is_stopped "$standby"
check "nginx no longer lists a backup" bash -c "! grep -q backup '$UPSTREAM'"
check "edge still serves the live release" edge_serves e2e-good4

say "8. Webhook security and late failures"
TOKEN=$(cat .run/secrets/healer_token)
payload=$(jq -n --arg v e2e-good4 --arg c "$(current_color)" --arg at "$(date -u +%FT%TZ)" '{alerts: [{status: "firing",
    labels: {alertname: "ReleaseHighErrorRate", action: "rollback", reason: "high_error_rate", version: $v, color: $c, severity: "critical"},
    annotations: {value: "0.5"}, startsAt: $at}]}')
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d "$payload" http://172.30.0.1:9095/v1/alerts)
check "webhook without a token is rejected" test "$code" = 401
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -d "$payload" http://172.30.0.1:9095/v1/alerts)
check "webhook with the token is accepted" test "$code" = 202
sleep 3
check "failure outside the window does not roll back" is_current e2e-good4
check "operator is told why nothing was rolled back" telegram_has "Автооткат не выполнен"

say "Telegram messages received"
cat "$TELEGRAM_LOG"

echo
if ((FAILURES > 0)); then
    printf '\033[1;31m%d check(s) failed\033[0m\n' "$FAILURES"
    exit 1
fi
printf '\033[1;32mAll scenarios passed\033[0m\n'
