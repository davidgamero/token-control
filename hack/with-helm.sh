#!/usr/bin/env sh
# with-helm.sh runs the Helm CLI inside a container so a local Helm install is not required
# on the host. The repository is mounted at /workspace and that is the working directory, so
# chart paths are relative to the repo root (e.g. deploy/helm/token-control).
#
# Usage:
#   hack/with-helm.sh lint deploy/helm/token-control
#   hack/with-helm.sh template tc deploy/helm/token-control
set -eu

IMAGE="${HELM_IMAGE:-alpine/helm:3.16.3}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

exec docker run --rm \
  -v "${ROOT}":/workspace \
  -w /workspace \
  --entrypoint helm \
  "${IMAGE}" "$@"
