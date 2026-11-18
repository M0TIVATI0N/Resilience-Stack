#!/usr/bin/env bash
# Forced command of the CI SSH key: the key can do nothing except deploy a commit.
set -euo pipefail

version=${SSH_ORIGINAL_COMMAND:-}
if [[ ! $version =~ ^[0-9a-f]{7,40}$ ]]; then
    echo "ci-deploy: expected a commit hash" >&2
    exit 64
fi
exec "$(dirname -- "${BASH_SOURCE[0]}")/deploy.sh" "$version"
