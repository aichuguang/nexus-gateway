#!/usr/bin/env bash
# Source and deployment syntax checks. Build the embedded UI first with
# scripts/build.mjs. This script does not install software or start services.
set -Eeuo pipefail
ROOT="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"
[[ "$(uname -s)" == Linux ]] || { echo 'Linux is required for this check entry point.' >&2; exit 1; }
command -v go >/dev/null
command -v node >/dev/null

for script in scripts/*.mjs; do
  node --check "$script"
done
bash -n deploy/install.sh deploy/replace.sh deploy/push.sh
go vet ./...
echo 'Source and deployment syntax checks passed.'
