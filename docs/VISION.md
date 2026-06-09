# token-control: vision

> **`ResourceQuota` and `LimitRange`, but for LLM tokens.**

---

## What it is

token-control is a Kubernetes-native governance layer for LLM access. It makes "which
workloads may call which models, on which credentials, under what token budget" a **first-class
cluster resource** — enforced at pod scheduling time, without sitting in the inference request
hot path.

Four CRDs form a three-tier hierarchy that mirrors how `LimitRange` + `ResourceQuota` work
today, but applied to model access instead of CPU and memory:

| CRD | Scope | Analogue |
|-----|-------|----------|
| `ClusterTokenPolicy` | cluster | `LimitRange` (cluster defaults) |
| `TokenPolicy` | namespace / workload | `ResourceQuota` / `LimitRange` |
| `ModelCredential` | cluster | `StorageClass` (supply-side capacity) |
| `ModelClaim` | namespace | `PersistentVolumeClaim` (consumer demand) |

---

## Why it matters

On any shared cluster running AI workloads, the following problems arrive before any governance
does:

1. **No allowlist.** Any pod can call any model at any provider. There is no namespace
   boundary, no deny-by-default.
2. **Credential sprawl.** Provider API keys get copied into team Secrets, committed to git, and
   replicated manually. Rotation is operationally infeasible; blast radius is unbounded.
3. **No budgets.** Token and request quotas exist at the provider level but are invisible to
   the cluster operator. There is no per-namespace or per-workload budget, no over-commitment
   detection, no "this team is burning the shared key".
4. **AI apps fail silently at runtime.** A pod that cannot reach its model — because a key is
   missing, revoked, or rate-limited — schedules, starts, and then fails to serve traffic. The
   scheduling system has no concept of "token capacity" as a schedulable resource.

token-control addresses all four. An AI-powered application **should not schedule at all**
unless the tokens it needs are available, just as a GPU workload does not schedule without
allocatable GPU capacity.

---

## The vision

**Tokens are a schedulable resource.** The full vision maps directly onto Kubernetes' existing
`requests`/`limits` semantics:

- `TokenQuota.requests` — the minimum token throughput a workload *requires* to be useful.
  A pod whose `ModelClaim` cannot be satisfied (denied model, or no capacity remaining) stays
  in `SchedulingGated` phase and never reaches the scheduler. It cannot fail silently at
  runtime because it never starts.
- `TokenQuota.limits` — the maximum rate enforced at the gateway. Workloads may burst up to
  limits but are rate-limited above them.
- `ModelCredential.spec.capacity` — the allocatable supply for a key, reconciled from the
  providing side (KAITO `Workspace` vLLM throughput, Azure Cognitive Services quota API, or
  static declaration). The controller tracks `allocated` vs `capacity` and flags `Oversubscribed`.

**The enforcement chain — no proxy in the hot path:**

```
pod admission → scheduling gate (ModelClaim Bound + capacity reserved)
             → egress NetworkPolicy (governed pods can only reach in-cluster gateway)
             → gateway rate-limit (BackendTrafficPolicy / QuotaPolicy per workload)
             → usage ingestion → status.usage → long-window caps (monthly)
```

The providing side (KAITO for self-hosted, Envoy AI Gateway `AIGatewayRoute` for external
providers) advertises capacity; token-control consumes it. Neither token-control nor any
component it manages needs to be in the per-request data path.

---

## How it fits into Kubernetes

token-control is a standard controller-runtime operator. It produces and consumes:

- **Gateway API** — `HTTPRoute`, `BackendTrafficPolicy` (Envoy Gateway), `QuotaPolicy` (Envoy
  AI Gateway), `TokenRateLimitPolicy` (Kuadrant), and in future `InferencePool` targets from
  the Gateway API Inference Extension.
- **Admission webhooks** — validating (model allowlist, `Enforce`/`Audit`) and mutating
  (credential injection, scheduling gate insertion).
- **Scheduling gates** — `pod.spec.schedulingGates` holds a pod until its `ModelClaim` is
  `Bound` and quota capacity is reserved, making token starvation a scheduling failure rather
  than a runtime failure.
- **NetworkPolicy** — egress rules generated alongside credential injection so governed pods
  cannot reach providers directly, making the gateway the mandatory enforcement point.
- **Standard RBAC** — workload teams `create` `ModelClaim`; cluster operators `create`
  `ClusterTokenPolicy` and `ModelCredential`. Kubernetes RBAC is the authorization mechanism;
  token-control adds no custom auth.

---

## Status

Alpha (`v1alpha1`). The admission gate and credential injection are operational. Scheduling
gates, the `requests`/`limits` API split, egress NetworkPolicy generation, and supply-side
reconcilers are in the backlog below.

---

## Backlog

Ordered by impact. Items marked **[gap]** correspond to gaps documented in
[`docs/ENFORCEMENT.md`](ENFORCEMENT.md).

### Tier 1 — Close the trust gap (no gateway required)

- **Egress NetworkPolicy generation** `[gap C]` — when the mutating webhook injects a
  credential, also emit a `NetworkPolicy` that denies pod egress to provider IP ranges except
  the in-cluster gateway `Service`. Converts the admission gate from advisory to enforced.
  Single highest-leverage change in the project.

- **Scheduling gates** — insert `pod.spec.schedulingGates:
  [{name: tokencontrol.io/awaiting-token-reservation}]` from the mutating webhook when a
  governed pod is created. `ModelClaimReconciler` clears the gate once `phase=Bound` and
  `sum(requests) ≤ credential.capacity`. Pods that cannot get tokens never start.

- **`TokenQuota` requests/limits split** — rename `TokenQuota` to carry both `requests`
  (scheduling gate threshold) and `limits` (gateway rate-limit bucket). `Requests` drives the
  scheduling gate; `limits` drives the gateway artifact. Field-wise minimum still applies across
  tiers. This is an API-breaking change requiring a v1alpha2 bump.

### Tier 2 — Make gateway budgets real

- **Switch Envoy AI Gateway artifact to `QuotaPolicy`** `[gap E]` — replace `BackendTrafficPolicy`
  generation with `aigateway.envoyproxy.io/v1alpha1/QuotaPolicy` which has native
  `PerModelQuota` and per-workload scoping. Remove the dependency on manually configuring
  `clientSelectors`.

- **Generate `AIGatewayRoute` + `llmRequestCosts` (producer side)** `[gap E]` — currently
  token-control emits the consumer of `io.envoy.ai_gateway/llm_total_token` metadata but not
  the `AIGatewayRoute` that produces it. Generate both together when `gateway.type=EnvoyAIGateway`.

- **Cluster-tier gateway artifact generation** `[gap D]` — `ClusterTokenPolicyReconciler`
  currently writes status only; extend it to call `gateway.For` to emit a namespace-default
  artifact when `spec.gateway` is set.

- **Per-workload scoping in generated artifacts** `[gap E]` — add `clientSelectors` (Envoy) or
  `counters`/`when` (Kuadrant) using the `ModelClaim` service account or namespace as the
  counter key, so budgets are per-workload rather than a shared global bucket.

### Tier 3 — Close the usage loop

- **Usage ingestion** `[gap F]` — add a reconciler or sidecar that scrapes Limitador counters
  or Envoy AI Gateway usage metadata and writes `status.usage` + `tokencontrol_tokens_consumed_total`.

- **Long-window admission gate** — once `status.usage` is populated, extend the pod webhook to
  deny new pods when a namespace or workload has exhausted its monthly/daily cap.  This is the
  only enforcement mode that does not require a live request flowing through a gateway.

### Tier 4 — Supply-side integrations

- **KAITO `Workspace` reconciler** — watch `kaito.sh/v1alpha1/Workspace` resources; read vLLM
  `/metrics` for observed token throughput; update `ModelCredential.spec.capacity` dynamically.

- **Azure Cognitive Services quota reconciler** — periodic sync from the Azure Quota API
  (`Microsoft.CognitiveServices/locations/{loc}/quotas`) into `ModelCredential.spec.capacity`
  for managed API provider keys.

- **Envoy AI Gateway `BackendSecurityPolicy` emission** — optionally manage the provider API
  key at the gateway layer (via `BackendSecurityPolicy.spec.apiKey`) instead of pod env
  injection, so the credential never reaches the pod at all.

- **Gateway API Inference Extension `InferencePool` targeting** — allow `GatewayIntegration`
  to target a `inference.networking.k8s.io/InferencePool` (for KAITO-served models) so
  token-control governance can attach to self-hosted inference as naturally as to external APIs.

### Tier 5 — Hardening and operations

- **`v1alpha2` API with requests/limits** — schema migration, conversion webhook, and
  deprecation of flat `TokenQuota`.
- **Kind integration test CI** — automate the `test/integration` scenario (fake LLM, admission,
  routing) in GitHub Actions on pull requests.
- **Release pipeline** — goreleaser + ghcr.io image push + Helm chart release on tag.
- **DRA `ResourceClaim` support** — future opt-in: use `resource.k8s.io/v1beta1`
  `ResourceClaim` + a token-control DRA driver as an alternative scheduling gate mechanism
  (K8s ≥ 1.32). Enables quota-as-a-device semantics without a custom controller loop.
