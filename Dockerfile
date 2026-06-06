# Build the token-control manager binary.
#
# Multi-stage: compile a static binary with the official Go toolchain, then ship it in a
# distroless nonroot base so the runtime image carries no shell, package manager, or libc
# attack surface and never runs as UID 0.
FROM golang:1.23 AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /workspace

# Cache module downloads independently of source changes.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the source tree needed to build the manager.
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO disabled for a fully static binary; trim symbols/DWARF and embed no VCS stamp so the
# build is reproducible inside the CI sandbox.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o manager ./cmd/main.go

# Runtime image.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
