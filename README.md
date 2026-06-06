# token-control

Kubernetes-native governance for LLM/model access. token-control makes "which workloads may
call which models, with which credentials, under what token budget" a **first-class,
namespaced cluster resource** — the governance analogue of `ResourceQuota`/`LimitRange`, but
for models, credentials and tokens.

It enforces at admission time and (optionally) generates rate-limit objects for your existing
gateway. **It is not an inline proxy and never sits in the request hot path.**

## Why

On a shared cluster, LLM usage tends to arrive faster than governance:

- any workload can call any provider/model — no allowlist;
- provider API keys get copied into every team's `Secret` (and into git) — no rotation, huge
  blast radius;
- there are no per-namespace/per-workload token or request budgets;
- where controls exist at all, they are global and un-scoped.

token-control addresses all four with a small hierarchy of CRDs and a controller-runtime
operator.

## Features

- **Hierarchical model allowlists** — cluster → namespace → workload, with **deny-wins** and
  **deny-by-omission** semantics. Narrower tiers can only *narrow*, never widen.
- **Centrally-managed credentials** — one source-of-truth `Secret` in the operator namespace,
  replicated only to authorized namespaces and **injected** into governed pods automatically.
  Teams declare intent; they never handle key material.
- **Token/request budgets** that compose down the hierarchy (field-wise minimum).
- **Admission enforcement** — `Enforce` (reject), `Audit` (warn) or `Disabled`, evaluated by
  validating/mutating pod webhooks.
- **Optional gateway integration** — generate `Envoy AI Gateway` `BackendTrafficPolicy` or
  `Kuadrant` `TokenRateLimitPolicy` from a policy's quota and let the data plane enforce live.
- **Operator hygiene** — leader election, least-privilege RBAC, distroless nonroot image,
  fail-open data-plane webhooks, Prometheus metrics, Helm install with self-signed *or*
  cert-manager webhook certs.

## Custom resources

| Kind | Scope | Purpose |
|------|-------|---------|
| `ClusterTokenPolicy` | Cluster | Default allowlist/quota/enforcement/gateway for selected namespaces (broadest tier). |
| `TokenPolicy` | Namespaced | Namespace-default (empty selector) or workload-scoped (selector) policy; may only narrow what it inherits. |
| `ModelCredential` | Cluster | Binds a provider credential to authorized namespaces/models and defines how it is injected. |

API group: `governance.tokencontrol.io/v1alpha1`. See [`docs/DESIGN.md`](docs/DESIGN.md) for
the full architecture and the resolver semantics.

## How it works

```
author applies a Deployment whose pod template declares its intent:
  annotations: { governance.tokencontrol.io/models: "openai/gpt-4o" }

  apiserver --CREATE pod--> mutating webhook   resolve effective policy;
                                               inject OPENAI_API_KEY from tc-cred-openai
  apiserver --CREATE pod--> validating webhook resolve effective policy;
                                               Enforce+violation -> 403; Audit -> warn
  pod scheduled with the injected credential
```

The **resolver** combines all matching cluster/namespace/workload policies: a model is
permitted iff *no* tier denies it (an explicit `Deny`, or absence from a tier's allowlist).
Quotas take the field-wise minimum. Credentials and gateway/enforcement settings come from the
most specific tier.

## Quickstart

Requires a cluster and `helm` reachability to its API. The chart installs the CRDs, the
manager `Deployment`, RBAC, the webhook configurations and (by default) a self-signed webhook
certificate.

```sh
# 1) Install the operator
helm upgrade --install token-control deploy/helm/token-control \
  --namespace token-control-system --create-namespace

# 2) Create the source credential Secret (single source of truth, operator namespace only)
kubectl -n token-control-system create secret generic openai \
  --from-literal=apiKey=sk-REDACTED

# 3) Apply governance + a credential binding
kubectl apply -f examples/01-clustertokenpolicy.yaml
kubectl apply -f examples/02-modelcredential.yaml

# 4) Deploy a governed workload (declares intent; gets the key injected)
kubectl apply -f examples/05-workload-deployment.yaml
```

To use cert-manager instead of the built-in self-signed cert:

```sh
helm upgrade --install token-control deploy/helm/token-control \
  --namespace token-control-system --create-namespace \
  --set webhook.certManager.enabled=true
```

## Examples

Walk the hierarchy from broad to narrow in [`examples/`](examples/):

| File | What it shows |
|------|---------------|
| `01-clustertokenpolicy.yaml` | Cluster-wide default allowlist, quota, a global `Deny` guardrail. |
| `02-modelcredential.yaml` | Centrally-managed credential bound to authorized namespaces. |
| `03-tokenpolicy-namespace.yaml` | Namespace default narrowing the cluster policy. |
| `04-tokenpolicy-workload.yaml` | Workload-scoped policy (selector + priority) narrowing further. |
| `05-workload-deployment.yaml` | A governed workload declaring intent via annotation. |
| `06-gateway-integration.yaml` | Generating Envoy AI Gateway / Kuadrant rate-limit objects. |

## Configuration

Common `values.yaml` knobs (see the file for the full, inline-documented set):

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/token-control/token-control` | Manager image. |
| `image.tag` | `""` (chart `appVersion`) | Image tag. |
| `replicaCount` | `1` | Manager replicas (leader-elected; >1 for webhook HA). |
| `controller.leaderElect` | `true` | Enable leader election. |
| `controller.exemptNamespaces` | `[kube-system, kube-public, kube-node-lease]` | Namespaces excluded from pod governance (operator ns always exempt). |
| `webhook.enabled` | `true` | Run the webhook server and register configurations. |
| `webhook.failurePolicy` | `Ignore` | Pod webhook failure policy (fail-open). CRD webhooks always `Fail`. |
| `webhook.certManager.enabled` | `false` | Use cert-manager instead of the self-signed cert. |
| `webhook.certManager.issuerRef` | `{}` | Existing Issuer/ClusterIssuer; self-signed `Issuer` created when empty. |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. |
| `podDisruptionBudget.enabled` | `false` | Create a PDB (recommended with `replicaCount > 1`). |
| `resources` | `50m/128Mi` → `500m/256Mi` | Manager requests/limits. |

A namespace can also be excluded ad hoc by labeling it
`tokencontrol.io/governance=disabled`.

## Observability

The manager serves Prometheus metrics on `:8080`, including
`tokencontrol_admission_decisions_total`, `tokencontrol_model_violations_total`,
`tokencontrol_credentials_injected_total`, `tokencontrol_credential_synced_namespaces` and
`tokencontrol_gateway_artifacts`. Enable `metrics.serviceMonitor.enabled=true` to scrape with
the Prometheus Operator. `tokencontrol_tokens_consumed_total` is advisory and fed by an
external usage reporter (typically the gateway).

## Development

The only host dependency is **Docker**. All Go and Helm tooling runs in containers via
`hack/with-go.sh` (golang:1.23) and `hack/with-helm.sh` (alpine/helm), driven by the `Makefile`:

```sh
make help            # list targets
make generate        # DeepCopy methods (controller-gen)
make manifests       # regenerate CRDs into the chart's crds/
make fmt vet test    # format, vet, unit tests
make build           # build bin/manager
make docker-build    # build the container image (IMG=...)
make helm-lint       # lint the chart
make helm-template   # render the chart
make install         # helm upgrade --install into the cluster
```

### Layout

```
api/v1alpha1/        CRD Go types + generated deepcopy
internal/policy/     hierarchy resolver (the core decision engine) + tests
internal/webhook/    validating/mutating webhooks (CRDs + pods)
internal/controller/ ClusterTokenPolicy / TokenPolicy / ModelCredential reconcilers
internal/gateway/    Envoy AI Gateway / Kuadrant artifact generation
internal/metrics/    Prometheus collectors
cmd/main.go          manager wiring
deploy/helm/         the token-control Helm chart (CRDs + templates)
docs/DESIGN.md       architecture and design rationale
examples/            sample CRs, broad -> narrow
```

## Security

Least-privilege RBAC (a `ClusterRole` for governance CRDs/namespaces/secrets/optional gateway
CRDs; a namespaced leader-election `Role`). The manager runs as distroless `nonroot` (UID
65532) with `readOnlyRootFilesystem`, all capabilities dropped and `seccompProfile:
RuntimeDefault`. Secret IO uses an uncached client so the manager does not cache every Secret
in the cluster. Credential copies are owner-referenced and labeled for clean garbage
collection and bounded blast radius.

## Status

Alpha (`v1alpha1`). APIs may change. Live token metering depends on an external reporter or a
gateway; token-control itself does not inspect request bodies.
