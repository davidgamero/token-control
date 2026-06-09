# AGENTS.md — token-control

Context file for AI coding agents. Read this before making any changes.

## Required reading

1. **[`docs/VISION.md`](docs/VISION.md)** — canonical project summary, vision, and backlog.
   Read this first. Every task should be consistent with the vision.
2. **[`docs/DESIGN.md`](docs/DESIGN.md)** — architecture, resolver semantics, CRD design
   decisions.
3. **[`docs/ENFORCEMENT.md`](docs/ENFORCEMENT.md)** — honest gap analysis of what is actually
   enforced today vs. what the docs imply. Do not paper over gaps; reference this file when
   discussing enforcement.

## Rule: `docs/VISION.md` must stay canonical

`docs/VISION.md` holds the **single source of truth** for the project's purpose and direction.
It must contain:

- A concise summary of what token-control is, why it matters, and how it fits into Kubernetes.
  **This summary section must stay under 1 000 words.**
- A `## Backlog` section with all outstanding implementation items, ordered by impact tier.

When completing a backlog item, remove it from the backlog section in `docs/VISION.md`.
When discovering a new gap or required feature, add it to the appropriate tier in the backlog.
Never let `docs/VISION.md` become stale: it is the living roadmap, not a one-time document.

## Project layout

```
api/v1alpha1/           CRD Go types + generated deepcopy
  common_types.go       shared types: TokenQuota, WorkloadSelector, ModelPermission, …
  clustertokenpolicy_types.go
  tokenpolicy_types.go
  modelcredential_types.go
  modelclaim_types.go
  zz_generated.deepcopy.go   (generated — do not edit by hand)

internal/policy/        hierarchy resolver (the core decision engine)
  resolver.go           Resolve(), Effective.Permit(), WorkloadSelectorMatches(), ValidGlob()

internal/webhook/v1alpha1/
  pod_webhook.go        validating + mutating pod admission
  webhook_common.go     shared helpers: declaredModelsForPod, resolveEffective, injection
  *_webhook.go          per-CRD structural validators

internal/controller/
  clustertokenpolicy_controller.go
  tokenpolicy_controller.go   also calls gateway.For() for artifact generation
  modelcredential_controller.go
  modelclaim_controller.go
  helpers.go

internal/gateway/       Envoy AI Gateway / Kuadrant artifact builders
internal/metrics/       Prometheus collectors (all registered in init())

cmd/main.go             manager wiring (flags, scheme, controller + webhook registration)
deploy/helm/token-control/   Helm chart (CRDs in crds/, templates/)
examples/               sample CRs, broad → narrow
test/integration/       kind integration tests + fake LLM server
docs/                   VISION.md, DESIGN.md, ENFORCEMENT.md, MODEL-CLAIM.md
```

## Architecture invariants

- **No proxy in the hot path.** token-control never intercepts inference requests. Enforcement
  is admission-time (webhooks) and config-generation-time (gateway artifacts). The gateway
  enforces live.
- **Deny-wins, narrow-only.** A model is permitted iff *no* tier denies it. Lower tiers can
  only narrow, never widen. This is enforced by `internal/policy/resolver.go`.
- **Three-tier hierarchy.** `ClusterTokenPolicy` → `TokenPolicy` (namespace default) →
  `TokenPolicy` (workload selector). `ModelClaim` declares workload intent; it does not grant
  permissions — it gets evaluated against the hierarchy.
- **Credential isolation.** Provider secrets live only in the operator namespace. The
  `ModelCredentialReconciler` copies only the key material to authorized namespaces as
  `tc-cred-<name>` Secrets. Pods never handle raw key material directly unless they mount the
  synced Secret.
- **Fail-open data-plane, fail-closed CRD webhooks.** Pod webhooks use `failurePolicy: Ignore`
  so a webhook outage does not block workloads. CRD webhooks use `failurePolicy: Fail` so
  invalid policy objects are rejected at creation time.

## Development conventions

### Tooling — Docker only (no host Go/Helm/make)

```sh
./hack/with-go.sh go build ./...          # compile
./hack/with-go.sh go test ./...           # unit tests
./hack/with-go.sh go test -tags integration ./test/integration/...  # integration tests
./hack/with-go.sh go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 \
    object:headerFile=hack/boilerplate.go.txt paths=./api/...   # regenerate deepcopy
./hack/with-go.sh go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 \
    crd:generateEmbeddedObjectMeta=true output:crd:dir=deploy/helm/token-control/crds \
    paths=./api/...                        # regenerate CRDs
./hack/with-helm.sh helm lint deploy/helm/token-control
```

Always regenerate after changing `api/v1alpha1/` types. Always run `go vet` before committing.

### Commit style

Follow the existing history: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:` prefixes.
Single-line subject ≤ 72 characters; wrap body at 80 characters.

### CRD changes

1. Edit types in `api/v1alpha1/`.
2. Regenerate deepcopy and CRDs (see above).
3. Update the Helm chart RBAC (`deploy/helm/token-control/templates/rbac.yaml`) if new
   resources or verbs are needed.
4. Update `deploy/helm/token-control/templates/validatingwebhookconfiguration.yaml` if a new
   webhook is added.
5. Add/update examples in `examples/`.

### Resolver changes

`internal/policy/resolver.go` is the single source of policy truth. Changes here affect every
webhook and controller. Run `go test ./internal/policy/...` after every edit.

### Webhook changes

The pod webhook is `failurePolicy: Ignore` — it must be safe to call multiple times (mutating
webhook `reinvocationPolicy: IfNeeded`). Do not make it stateful. Always test with
`internal/webhook/v1alpha1/*_test.go`.

## Testing

- **Unit tests:** `go test ./...` — no cluster required, uses `envtest`.
- **Integration tests:** `go test -tags integration ./test/integration/...` — requires a
  running cluster. See `test/integration/Makefile` for setup/teardown with kind.
- Before committing, always run `go build ./... && go vet ./... && go test ./...`.
- The integration test creates a fake LLM server (`test/integration/fake_llm/`) that
  implements `/v1/chat/completions` and verifies end-to-end credential injection and routing.

## Key types quick-reference

| Type | Package | Purpose |
|------|---------|---------|
| `policy.Resolve(ResolveInput)` | `internal/policy` | Full hierarchy resolution → `*Effective` |
| `Effective.Permit(provider, model)` | `internal/policy` | Per-model allow/deny decision → `ModelDecision` |
| `TokenQuota` | `api/v1alpha1` | TPM/RPM/day/month budget (limits today; requests/limits in v1alpha2) |
| `WorkloadSelector` | `api/v1alpha1` | Pod label selector + service accounts |
| `ModelPermission` | `api/v1alpha1` | A single allow/deny rule with optional credential + quota |
| `declaredModelsForPod` | `internal/webhook/v1alpha1` | Merge ModelClaims + annotation for a pod |
| `gateway.For(gw, tp, quota)` | `internal/gateway` | Build unstructured gateway artifact |
