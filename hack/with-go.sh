#!/usr/bin/env sh
# with-go.sh runs Go tooling inside the official golang image so that a local Go
# toolchain is not required on the host. Module and build caches are persisted in
# named Docker volumes so repeated invocations are fast.
#
# Usage:
#   hack/with-go.sh go build ./...
#   hack/with-go.sh go test ./...
#   hack/with-go.sh go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 ...
set -eu

IMAGE="${GO_IMAGE:-golang:1.23}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

exec docker run --rm \
  -v "${ROOT}":/workspace \
  -v token-control-gomod:/go/pkg/mod \
  -v token-control-gocache:/root/.cache/go-build \
  -w /workspace \
  -e GOFLAGS="${GOFLAGS:--buildvcs=false -mod=mod}" \
  -e CGO_ENABLED=0 \
  "${IMAGE}" "$@"
