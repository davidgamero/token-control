# token-control — Design

## Problem

Teams are adding LLM/model calls to workloads faster than platform teams can govern them.
On a shared Kubernetes cluster this creates four concrete problems:

1. **No allowlisting.** Any workload can call any provider/model. There is no cluster- or
   namespace-level control over *which* models may be used.
2. **Credential sprawl.** Every team copies a provider API key into its own `Secret` (often
   into git). Rotation is impossible, blast radius is the whole cluster, and nobody knows
   who holds which key.
3. **No budgets.** There is no per-namespace or per-workload token/request budget, so a
   single misbehaving job can exhaust an org-wide spend limit.
4. **No scoping.** Controls, where they exist, are global. There is no way to say "the
   `batch` workloads in `team-a` may use only `gpt-4o-mini`, at 10k TPM".

token-control treats LLM access as a **first-class, namespaced cluster resource** — the
governance analogue of `LimitRange`/`ResourceQuota`, but for models, credentials and tokens.

## Goals

- Cluster-, namespace- and workload-scoped **model allowlists** with deny-wins semantics.
- **Centrally-managed credentials**: one source-of-truth Secret, replicated only to
  authorized namespaces, injected into workloads automatically.
- **Token/request budgets** that compose down the hierarchy (narrow, never widen).
- **Admission-time enforcement** — reject or warn at `CREATE`/`UPDATE`, no proxy in the
  request hot path.
- Optional **gateway artifact generation** so live token rate-limiting can be delegated to
  an existing data plane (Envoy AI Gateway, Kuadrant).
- Be a **well-behaved operator**: leader-elected, least-privilege RBAC, distroless nonroot
  image, fail-open data-plane webhooks, Prometheus metrics, Helm-installable.

## Non-goals

- token-control is **not an inline proxy** and does not see request bodies or responses. It
  does not itself meter live token consumption; an external usage reporter (typically the
  gateway) feeds advisory consumption counters back for observability.
- It does not manage provider-side billing or model deployment.

## Architecture

```
                         +--------------------------------------------------+
                         |                 token-control manager            |
                         |                                                  |
  kube-apiserver  <----> |  Admission webhooks         Controllers          |
        |                |  - ValidatingWebhook        - ClusterTokenPolicy  |
        |  admission      |    * ClusterTokenPolicy      - TokenPolicy        |
        |  (CREATE/UPDATE)|    * TokenPolicy             - ModelCredential    |
        v                |    * ModelCredential                              |
   Pods / CRDs           |    * Pod (validate)         policy.Resolve()      |
                         |  - MutatingWebhook           (effective decision) |
                         |    * Pod (inject creds)                           |
                         +--------------------------------------------------+
                                   |                         |
                       generates   |                         | replicates
                       gateway      v                         v
                       artifacts  BackendTrafficPolicy /    tc-cred-* Secrets
                                  TokenRateLimitPolicy      in authorized namespaces
```

### Custom resources

| Kind                  | Scope      | Purpose |
|-----------------------|------------|---------|
| `ClusterTokenPolicy`  | Cluster    | Default allowlist, quota, enforcement and gateway integration for selected namespaces. Broadest tier. |
| `TokenPolicy`         | Namespaced | Namespace-default (empty selector) or workload-scoped (selector) policy. May only narrow what it inherits. |
| `ModelCredential`     | Cluster    | Binds a provider credential (one source Secret) to the namespaces/models permitted to use it, and how it is injected. |

CRDs are shipped in `deploy/helm/token-control/crds/` and installed by Helm. API group is
`governance.tokencontrol.io/v1alpha1`.

### The policy hierarchy and resolver

The resolver (`internal/policy`) is the heart of the system. It combines three tiers,
ordered broad → narrow:

1. **cluster** — every `ClusterTokenPolicy` whose `namespaceSelector` matches the namespace.
2. **namespace** — `TokenPolicy` objects in the namespace with an empty selector.
3. **workload** — `TokenPolicy` objects whose selector matches the pod's labels/service
   account.

The cardinal rule is **narrow, never widen**. For a given `provider/model` each tier yields:

- **Deny** — the tier explicitly denies it, *or* the tier publishes an allowlist (≥1 Allow
  rule) the model is not on (**deny-by-omission**).
- **Allow** — the tier matches it with an Allow rule, *or* the tier is a pure denylist that
  does not match it.
- **Abstain** — the tier declares no model rules and inherits.

A model is permitted **iff no tier denies it**. Because a narrower allowlist can only
introduce deny-by-omission, a narrower tier can never re-permit what a broader tier denied.
Model matching is case-insensitive with glob support (`path.Match`, e.g. `gpt-4o-*`, `*`).

Other dimensions resolve as:

- **Quota** — field-wise minimum across all applicable tiers (`tokensPerMinute`,
  `requestsPerMinute`, `tokensPerDay`, `tokensPerMonth`). Unset = "no limit at this tier".
- **Enforcement** — most specific non-empty wins; default `Enforce`.
- **Credential** — most specific Allow that names a `credentialRef` wins, else the cluster
  `defaultCredentialRef`.
- **Gateway / Priority** — most specific gateway integration; among policies targeting the
  same workload the highest `Priority` supplies scalar fields.

`Resolve()` is used by the webhooks for a live pod; `ResolveForPolicy()` is used by the
controllers to populate a policy's `status.effectiveModels`/`effectiveQuota`.

### Enforcement model: admission gating + gateway generation

token-control enforces at two points, **neither of which is the request hot path**:

1. **Admission gating (always on).** The pod validating webhook reads the
   `governance.tokencontrol.io/models` annotation (a workload's declared intent) and checks
   each declaration against the effective policy:
   - `Enforce` → deny the pod with a `403`/Forbidden listing the violations.
   - `Audit` → admit but attach warnings and increment violation metrics.
   - `Disabled`/ungoverned/exempt namespace → admit unchanged.
2. **Gateway config generation (opt-in).** When a policy sets `spec.gateway.type`, the
   controller translates the resolved quota into the gateway's native object
   (`gateway.envoyproxy.io/BackendTrafficPolicy` or `kuadrant.io/TokenRateLimitPolicy`) and
   server-side-applies it (owned by the policy). The existing data plane then enforces token
   budgets live. token-control takes no compile-time dependency on those APIs — artifacts
   are `unstructured` and only applied when the CRDs exist.

This split keeps the controller off the latency path while still giving real, live token
limiting where a gateway is present.

### Credential management

`ModelCredential` removes Secret sprawl:

- The **source Secret** lives once, in the operator namespace, and is the single source of
  truth (never committed per-team).
- The controller computes the **authorized namespaces** (`allowedNamespaces` ∪
  `namespaceSelector`) and replicates the key into each as a Secret named `tc-cred-<name>`,
  labeled `app.kubernetes.io/managed-by=token-control` and **owned by the ModelCredential**
  (cluster-scoped owner of a namespaced dependent) so Kubernetes garbage-collects copies on
  delete. Replicas in de-authorized namespaces are pruned; drift is healed on a 10-minute
  resync.
- The **mutating pod webhook** injects the bound credential into governed pods — as an env
  var (`secretKeyRef` to `tc-cred-<name>`, provider-appropriate name like `OPENAI_API_KEY`)
  or as a projected volume. Existing env/volumes of the same name are never overwritten.

Workload teams therefore declare *intent* ("use the `openai` credential for `gpt-4o`") and
never handle key material.

### Capacity planning (supply vs demand)

A provider key has a finite throughput ceiling (its org/project rate limit). token-control
models this **supply** as `ModelCredential.spec.capacity` — the LLM analogue of a Node's
allocatable capacity. Where policy quotas are *demand* (what consumers may take), capacity is
*supply* (what the key can deliver):

| Kubernetes | token-control |
|------------|---------------|
| Node `status.allocatable` | `ModelCredential.spec.capacity` |
| Pod `requests` | `TokenPolicy` / `ClusterTokenPolicy` quotas |
| scheduler checks Σ requests ≤ allocatable | controller checks Σ allocated ≤ capacity |
| `Insufficient cpu` | `Oversubscribed` condition |

The ModelCredential controller rolls this up during its normal reconcile (reusing the pass
that computes `status.referencingPolicies`): every policy that **binds** the credential — an
Allow rule naming it via `credentialRef`, or a `ClusterTokenPolicy` whose
`defaultCredentialRef` is this key — contributes its `spec.quota`, summed field-wise into:

- `status.allocated` — committed demand (a planning estimate, not a hard reservation; each
  referencing policy's declared budget is summed once),
- `status.available` — `capacity − allocated`, floored at zero, only for windows where
  capacity declares a value,

and the `Oversubscribed` condition (plus `tokencontrol_credential_oversubscribed`,
`..._capacity_tokens_per_minute`, `..._allocated_tokens_per_minute` gauges) flips True when
any window's commitments exceed capacity. This is **advisory planning only** — capacity does
not rate-limit requests; live enforcement remains the gateway's responsibility. It exists so a
platform team can answer "have we handed out more of this key than it can serve?" from
declared values alone, without live metering.

### Request / admission flow

```
Author applies a Deployment whose pod template has:
  annotations: { governance.tokencontrol.io/models: "openai/gpt-4o" }

  apiserver --CREATE pod--> Mutating webhook (mpod)
                              resolve effective policy
                              if governed & allowed: inject OPENAI_API_KEY from tc-cred-openai
                              annotate effective-policy / credentials-bound
  apiserver --CREATE pod--> Validating webhook (vpod)
                              resolve effective policy
                              Enforce + violation  -> 403 Forbidden
                              Audit + violation     -> admit + Warning
                              allowed/ungoverned    -> admit
  pod scheduled; container starts with the injected credential
```

### Webhook configuration & failure policy

- **CRD webhooks** (`ClusterTokenPolicy`, `TokenPolicy`, `ModelCredential`) use
  `failurePolicy: Fail` — governance objects must be structurally valid. Hierarchy
  "widening" is surfaced as **warnings**, not hard errors: the runtime resolver is the real
  control, so an Allow that is dead due to a broader Deny is flagged but not rejected.
- **Pod webhooks** use `failurePolicy: Ignore` — a webhook outage must never block unrelated
  pod scheduling. The pod webhooks also exclude the operator namespace, the configured
  exempt namespaces (`kube-system`, `kube-public`, `kube-node-lease` by default) and any
  namespace labeled `tokencontrol.io/governance=disabled` via `namespaceSelector`.

### Webhook certificates

The chart provisions the serving certificate two ways:

- **Self-signed (default).** Helm generates a CA + serving cert (`genSignedCert`), stores
  them in a `kubernetes.io/tls` Secret, and injects the CA into every webhook's `caBundle`.
  Existing certs are reused across upgrades (via `lookup`) so rotation is opt-in and the
  running webhook is never disrupted. controller-runtime watches the mounted cert and
  hot-reloads on change.
- **cert-manager.** Set `webhook.certManager.enabled=true` to issue/rotate via a `Certificate`
  (self-signed `Issuer` by default, or a referenced `issuerRef`); the `cert-manager.io/
  inject-ca-from` annotation lets ca-injector fill the `caBundle`.

### Observability

Prometheus collectors (registered on the controller-runtime registry, served on `:8080`):

| Metric | Type | Labels |
|--------|------|--------|
| `tokencontrol_admission_decisions_total` | counter | decision, namespace, enforcement |
| `tokencontrol_model_violations_total` | counter | namespace, provider, model, enforcement |
| `tokencontrol_credentials_injected_total` | counter | namespace, credential |
| `tokencontrol_credential_synced_namespaces` | gauge | credential |
| `tokencontrol_credential_capacity_tokens_per_minute` | gauge | credential |
| `tokencontrol_credential_allocated_tokens_per_minute` | gauge | credential |
| `tokencontrol_credential_oversubscribed` | gauge | credential |
| `tokencontrol_effective_models` | gauge | namespace, policy |
| `tokencontrol_gateway_artifacts` | gauge | namespace, type |
| `tokencontrol_tokens_consumed_total` | counter | namespace, workload, provider, model |

`tokencontrol_tokens_consumed_total` is **advisory** and fed by an external usage reporter;
the rest are produced directly by the webhooks/controllers. A `ServiceMonitor`
(`metrics.serviceMonitor.enabled=true`) wires them into the Prometheus Operator.

## Security considerations

- Least-privilege RBAC: a `ClusterRole` for the governance CRDs, namespaces, secrets and the
  (optional) gateway CRDs; a namespaced `Role` for leader-election leases and events.
- Runtime: distroless `static:nonroot` image, `runAsNonRoot`, UID 65532,
  `readOnlyRootFilesystem`, all capabilities dropped, `seccompProfile: RuntimeDefault`.
- The manager uses an **uncached client** for all Secret IO and for gateway artifacts, so it
  does not cache every Secret in the cluster nor start informers for gateway CRDs that may
  not be installed.
- Key material is replicated only to authorized namespaces and tracked by owner references
  and labels, bounding blast radius and enabling clean GC.

## Implementation notes

- Module `github.com/token-control/token-control`, Go 1.23, controller-runtime v0.19,
  k8s.io/* v0.31.
- `cmd/main.go` wires the manager, the uncached client, the three controllers and (when
  `--enable-webhooks`) the webhook server.
- All Go and Helm tooling runs in containers (`hack/with-go.sh`, `hack/with-helm.sh`) so the
  only host dependency is Docker. See the `Makefile` for targets.

## Limitations / future work

- Live token metering depends on an external reporter or a gateway; token-control does not
  meter request bodies itself.
- Gateway artifact schemas track the upstream projects' evolving APIs and may need updates.
- `UsageStatus` counters in CRD status are advisory placeholders for a reporter to populate.
