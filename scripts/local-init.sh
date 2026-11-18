#!/usr/bin/env bash
# Prepares a local sandbox in .run/ and generates deploy/.env on first use.
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
RUNTIME="$ROOT/.run"

mkdir -p "$RUNTIME/upstream" "$RUNTIME/secrets"

secret() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }

if [[ ! -s $RUNTIME/secrets/healer_token ]]; then
    secret >"$RUNTIME/secrets/healer_token"
fi
# Alertmanager runs as an unprivileged user inside its container.
chmod 0644 "$RUNTIME/secrets/healer_token"

if [[ ! -s $RUNTIME/upstream/active.conf ]]; then
    printf '# Managed by deploy.sh and healerd. Do not edit.\nserver app-blue:8080 resolve;\n' \
        >"$RUNTIME/upstream/active.conf"
fi

if [[ ! -s $ROOT/deploy/.env ]]; then
    cat >"$ROOT/deploy/.env" <<EOF
APP_IMAGE_REPO=resilience-local/parkd
RS_RUNTIME_DIR=$RUNTIME
EDGE_BIND=127.0.0.1
EDGE_PORT=8088
POSTGRES_PASSWORD=$(secret)
GRAFANA_ADMIN_PASSWORD=$(secret)
EOF
    chmod 0600 "$ROOT/deploy/.env"
fi
